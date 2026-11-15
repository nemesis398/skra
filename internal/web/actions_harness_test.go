// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package web

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/store"
	"github.com/nemesis398/skra/internal/testenv"
)

// harnessServer is a server over the harness CUPS with a real store.
func harnessServer(t *testing.T) (*Server, *cups.Client) {
	t.Helper()
	c := testenv.Client(t)
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(config.Config{Listen: "127.0.0.1:7631", User: "root"}, Deps{CUPS: c, Store: db}, NewToken()), c
}

// jobsNamed lists the jobs of both lists whose name has the prefix.
func jobsNamed(t *testing.T, c *cups.Client, prefix string) []cups.Job {
	t.Helper()
	var out []cups.Job
	for _, which := range []string{"not-completed", "completed"} {
		jobs, err := c.Jobs(context.Background(), which, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			if strings.HasPrefix(j.Name, prefix) {
				out = append(out, j)
			}
		}
	}
	return out
}

// cancelNamed cancels every job with the prefix when the test ends.
func cancelNamed(t *testing.T, c *cups.Client, prefix string) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, j := range jobsNamed(t, c, prefix) {
			c.Cancel(ctx, j.ID)
		}
	})
}

func TestPrintAndCancelAgainstHarness(t *testing.T) {
	s, c := harnessServer(t)
	h := s.Handler()
	name := fmt.Sprintf("skra-test web print %d.txt", time.Now().UnixNano())
	cancelNamed(t, c, name)

	fields := [][2]string{{"printer", "Virtual-MFC"}, {"sides", "one-sided"}, {"hold", "true"}}
	rec := upload(s, h, fields, name, strings.NewReader("hello from the web test\n"), -1)
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), `Sent "`+name+`" to Virtual-MFC as job `) {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	jobs := jobsNamed(t, c, name)
	if len(jobs) != 1 || jobs[0].State != 4 {
		t.Fatalf("jobs = %+v, want one held job", jobs)
	}

	get(h, "/") // learn the job name
	rec = postTo(s, h, "/jobs/"+strconv.Itoa(jobs[0].ID)+"/cancel", nil)
	if rec.Code != 200 || rec.Body.String() != `Canceled "`+name+`".` {
		t.Errorf("cancel: status %d, body %q", rec.Code, rec.Body.String())
	}
	if jobs = jobsNamed(t, c, name); len(jobs) != 1 || jobs[0].State != 7 {
		t.Errorf("after cancel: %+v", jobs)
	}
	rec = postTo(s, h, "/jobs/"+strconv.Itoa(jobs[0].ID)+"/cancel", nil)
	if rec.Code != 502 || rec.Body.Len() == 0 {
		t.Errorf("second cancel: status %d, body %q", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, s); ev.Action != "job.cancel" || ev.OK {
		t.Errorf("event = %+v", ev)
	}
}

func TestPrintToAMissingQueueAgainstHarness(t *testing.T) {
	s, _ := harnessServer(t)
	rec := upload(s, s.Handler(), [][2]string{{"printer", "No-Such-Queue"}}, "a.txt", strings.NewReader("hi\n"), -1)
	if rec.Code != 502 || rec.Body.Len() == 0 {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	t.Logf("CUPS said: %s", rec.Body.String())
}

func TestTestPageAgainstHarness(t *testing.T) {
	s, c := harnessServer(t)
	rec := postTo(s, s.Handler(), "/printers/Virtual-MFC/testpage", url.Values{"hold": {"true"}})
	const prefix = "Test page sent to Virtual-MFC as job "
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), prefix) {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rec.Body.String(), prefix), "."))
	if err != nil {
		t.Fatalf("no job id in %q", rec.Body.String())
	}
	t.Cleanup(func() { c.Cancel(context.Background(), id) })
	jobs, err := c.Jobs(context.Background(), "not-completed", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == id {
			if j.State != 4 {
				t.Errorf("test page job state %d, want 4 (held)", j.State)
			}
			return
		}
	}
	t.Errorf("job %d is not in the queue", id)
}

// TestOversizedUploadLeavesNoJobAgainstHarness lowers the limit to 1 MiB and
// sends 3 MiB with no length, so CUPS is already receiving the document when
// the limit trips. The Print-Job must be aborted and leave no job behind.
func TestOversizedUploadLeavesNoJobAgainstHarness(t *testing.T) {
	s, c := harnessServer(t)
	old := maxUpload
	maxUpload = 1 << 20
	defer func() { maxUpload = old }()
	name := fmt.Sprintf("skra-test oversized %d.txt", time.Now().UnixNano())
	cancelNamed(t, c, name)

	fields := [][2]string{{"printer", "Virtual-MFC"}, {"hold", "true"}}
	rec := upload(s, s.Handler(), fields, name, &fileBody{head: []byte("some plain text\n"), fill: 'a', n: 3 << 20}, -1)
	if rec.Code != 413 || rec.Body.String() != tooLargeText {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, s); ev.Action != "print.file" || ev.OK {
		t.Errorf("event = %+v", ev)
	}
	time.Sleep(time.Second) // cupsd notices the closed connection
	if jobs := jobsNamed(t, c, name); len(jobs) != 0 {
		t.Errorf("CUPS kept %d job(s) of an aborted upload: %+v", len(jobs), jobs)
	}
}
