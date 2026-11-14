// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package web

import (
	"context"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/nemesis398/skra/internal/cups"
)

// scratchQueue makes a queue in the container and removes it at the end. The
// web tests that pause, empty or remove a queue use their own, so a test of
// another package that prints on Virtual-MFC at the same moment is not hit.
func scratchQueue(t *testing.T, name string) {
	t.Helper()
	out, err := exec.Command("podman", "exec", "skra-harness", "lpadmin", "-p", name, "-E", "-v",
		"ipp://localhost:8631/ipp/print", "-m", "everywhere").CombinedOutput()
	if err != nil {
		t.Fatalf("lpadmin -p %s: %v\n%s", name, err, out)
	}
	t.Cleanup(func() { exec.Command("podman", "exec", "skra-harness", "lpadmin", "-x", name).Run() }) // gone already is fine
}

func queueNamed(t *testing.T, c *cups.Client, name string) (cups.Printer, bool) {
	t.Helper()
	ps, err := c.Printers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ps, func(p cups.Printer) bool { return p.Name == name })
	if i < 0 {
		return cups.Printer{}, false
	}
	return ps[i], true
}

// TestSettingsAgainstHarness saves two settings of Virtual-MFC through the
// page's own route, reads them back from CUPS and puts them back.
func TestSettingsAgainstHarness(t *testing.T) {
	s, c := harnessServer(t)
	h := s.Handler()
	was, ok := queueNamed(t, c, "Virtual-MFC")
	if !ok {
		t.Fatal("no Virtual-MFC")
	}
	t.Cleanup(func() {
		c.Modify(context.Background(), "Virtual-MFC", cups.Changes{Sides: &was.SidesDefault, Color: &was.ColorDefault,
			ErrorPolicy: &was.ErrorPolicy, Location: &was.Location, Info: &was.Info})
	})

	body := get(h, "/printers/Virtual-MFC/settings").Body.String()
	for _, want := range []string{"default sides", "default color", "when printing fails", "A4 · 210 × 297mm", `value="ipp://localhost:8631/ipp/print"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel lacks %q", want)
		}
	}
	if strings.Contains(body, `data-act="command"`) {
		t.Error("a driverless queue lists maintenance commands")
	}

	rec := postTo(s, h, "/printers/Virtual-MFC/settings", url.Values{
		"sides": {"two-sided-long-edge"}, "color": {"monochrome"}, "policy": {"abort-job"}, "location": {"Test lab"}})
	if rec.Code != 200 || rec.Body.String() != "Saved Virtual-MFC: 4 settings." {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	p, _ := queueNamed(t, c, "Virtual-MFC")
	if p.SidesDefault != "two-sided-long-edge" || p.ColorDefault != "monochrome" || p.ErrorPolicy != "abort-job" || p.Location != "Test lab" {
		t.Errorf("CUPS has %+v", p)
	}
	if ev := lastEvent(t, s); ev.Action != "printer.modify" || !ev.OK {
		t.Errorf("event = %+v", ev)
	}
	if rec = postTo(s, h, "/printers/Virtual-MFC/settings", url.Values{"sides": {"sideways"}}); rec.Code != 400 {
		t.Errorf("a wrong value: status %d", rec.Code)
	}
	if p, _ = queueNamed(t, c, "Virtual-MFC"); p.SidesDefault != "two-sided-long-edge" {
		t.Errorf("sides-default = %q after a refused post", p.SidesDefault)
	}
}

func TestPrintersPageAgainstHarness(t *testing.T) {
	s, _ := harnessServer(t)
	rec := get(s.Handler(), "/printers")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `data-act="configure" data-q="Virtual-MFC"`) || strings.Contains(body, "CUPS does not answer") {
		t.Errorf("status %d, body:\n%s", rec.Code, body)
	}
}

// TestQueueActionsAgainstHarness runs the queue actions on queues of its own:
// pause, reject, move, cancel and remove.
func TestQueueActionsAgainstHarness(t *testing.T) {
	s, c := harnessServer(t)
	h := s.Handler()
	scratchQueue(t, "Scratch-Web-A")
	scratchQueue(t, "Scratch-Web-B")
	cancelNamed(t, c, "skra-test web queue")
	get(h, "/") // learn the queue names

	if rec := postTo(s, h, "/printers/Scratch-Web-A/pause", nil); rec.Code != 200 {
		t.Fatalf("pause: status %d, body %q", rec.Code, rec.Body.String())
	}
	if p, _ := queueNamed(t, c, "Scratch-Web-A"); p.State != 5 {
		t.Errorf("state %d after pause, want 5", p.State)
	}
	if rec := postTo(s, h, "/printers/Scratch-Web-A/reject", nil); rec.Code != 200 {
		t.Fatalf("reject: status %d, body %q", rec.Code, rec.Body.String())
	}
	if p, _ := queueNamed(t, c, "Scratch-Web-A"); p.Accepting {
		t.Error("still accepting after reject")
	}
	rec := postTo(s, h, "/printers/Scratch-Web-A/settings", url.Values{"state": {"running"}, "accept": {"accepted"}})
	if rec.Code != 200 || rec.Body.String() != "Saved Scratch-Web-A: accepts new jobs, running." {
		t.Errorf("settings: status %d, body %q", rec.Code, rec.Body.String())
	}
	if p, _ := queueNamed(t, c, "Scratch-Web-A"); !p.Accepting || p.State == 5 {
		t.Errorf("after the settings post: accepting %v, state %d", p.Accepting, p.State)
	}

	for _, tag := range []string{"1", "2"} {
		if _, err := c.Print(context.Background(), "Scratch-Web-A", "skra-test web queue "+tag, "text/plain", cups.JobOptions{Hold: true}, strings.NewReader("hi\n")); err != nil {
			t.Fatal(err)
		}
	}
	rec = postTo(s, h, "/printers/Scratch-Web-A/move-all", url.Values{"to": {"Scratch-Web-B"}})
	if rec.Code != 200 || rec.Body.String() != "Moved 2 jobs from Scratch-Web-A to Scratch-Web-B." {
		t.Errorf("move-all: status %d, body %q", rec.Code, rec.Body.String())
	}
	if n := len(jobsOn(t, c, "Scratch-Web-B")); n != 2 {
		t.Errorf("%d jobs on the target, want 2", n)
	}
	rec = postTo(s, h, "/printers/Scratch-Web-B/cancel-all", nil)
	if rec.Code != 200 || rec.Body.String() != "Canceled 2 jobs on Scratch-Web-B." {
		t.Errorf("cancel-all: status %d, body %q", rec.Code, rec.Body.String())
	}
	if n := len(jobsOn(t, c, "Scratch-Web-B")); n != 0 {
		t.Errorf("%d jobs left after cancel-all", n)
	}

	if rec = postTo(s, h, "/printers/Scratch-Web-A/delete", nil); rec.Code != 200 {
		t.Errorf("delete: status %d, body %q", rec.Code, rec.Body.String())
	}
	if _, ok := queueNamed(t, c, "Scratch-Web-A"); ok {
		t.Error("the queue is still there after delete")
	}
	if rec = postTo(s, h, "/printers/Scratch-Web-A/delete", nil); rec.Code != 502 || rec.Body.Len() == 0 {
		t.Errorf("second delete: status %d, body %q", rec.Code, rec.Body.String())
	}
}

func jobsOn(t *testing.T, c *cups.Client, queue string) []cups.Job {
	t.Helper()
	jobs, err := c.Jobs(context.Background(), "not-completed", 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []cups.Job
	for _, j := range jobs {
		if j.Printer == queue {
			out = append(out, j)
		}
	}
	return out
}

func TestSetDefaultAgainstHarness(t *testing.T) {
	s, c := harnessServer(t)
	h := s.Handler()
	before, err := c.Default(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	scratchQueue(t, "Scratch-Web-C")
	t.Cleanup(func() {
		if before != "" {
			c.SetDefault(context.Background(), before)
		}
	})
	if rec := postTo(s, h, "/printers/Scratch-Web-C/default", nil); rec.Code != 200 {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got, _ := c.Default(context.Background()); got != "Scratch-Web-C" {
		t.Errorf("Default() = %q", got)
	}
}
