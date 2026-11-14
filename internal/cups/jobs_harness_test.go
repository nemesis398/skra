// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package cups_test

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/testenv"
)

const queue = "Virtual-MFC"

func ctxFor(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// held prints a held text job named for the test and cancels it when the test
// ends, so later runs do not see stale jobs.
func held(t *testing.T, c *cups.Client, tag string) int {
	t.Helper()
	id, err := c.Print(ctxFor(t), queue, "skra-test "+t.Name()+" "+tag, "text/plain", cups.JobOptions{Hold: true}, strings.NewReader("hello from skra\n"))
	if err != nil {
		t.Fatal(err)
	}
	cancelAtEnd(t, c, id)
	return id
}

func cancelAtEnd(t *testing.T, c *cups.Client, id int) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c.Cancel(ctx, id) // refused when the job is already over; that is fine
	})
}

// find returns the job with the id from the not-completed or completed list.
func find(t *testing.T, c *cups.Client, which string, id int) (cups.Job, bool) {
	t.Helper()
	jobs, err := c.Jobs(ctxFor(t), which, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == id {
			return j, true
		}
	}
	return cups.Job{}, false
}

func TestHeldJobLifecycle(t *testing.T) {
	c := testenv.Client(t)
	ctx := ctxFor(t)

	id := held(t, c, "release")
	j, ok := find(t, c, "not-completed", id)
	if !ok || j.State != 4 {
		t.Fatalf("held job: found %v, state %d, want 4", ok, j.State)
	}
	if err := c.Release(ctx, id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		j, ok = find(t, c, "not-completed", id)
		if !ok || j.State != 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d is still held after Release", id)
		}
		time.Sleep(200 * time.Millisecond)
	}

	id2 := held(t, c, "cancel")
	if err := c.Cancel(ctx, id2); err != nil {
		t.Fatal(err)
	}
	done, ok := find(t, c, "completed", id2)
	if !ok || done.State != 7 {
		t.Fatalf("canceled job: found %v, state %d, want 7", ok, done.State)
	}
	if err := c.Cancel(ctx, id2); err == nil {
		t.Error("canceling a canceled job succeeded")
	}

	// Hold on a job that is already held keeps it held.
	id3 := held(t, c, "hold")
	if err := c.Hold(ctx, id3); err != nil {
		t.Fatal(err)
	}
	if j, ok := find(t, c, "not-completed", id3); !ok || j.State != 4 {
		t.Errorf("after Hold: found %v, state %d, want 4", ok, j.State)
	}
}

func TestHeldTestPage(t *testing.T) {
	c := testenv.Client(t)
	id, err := c.PrintTestPage(ctxFor(t), queue, true)
	if err != nil {
		t.Fatal(err)
	}
	cancelAtEnd(t, c, id)
	j, ok := find(t, c, "not-completed", id)
	if !ok || j.State != 4 || j.Name != "Test Page" {
		t.Errorf("test page: found %v, state %d, name %q, want a held job named Test Page", ok, j.State, j.Name)
	}
}

func TestRestartAJob(t *testing.T) {
	c := testenv.Client(t)
	ctx := ctxFor(t)
	id := held(t, c, "restart")
	if err := c.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := c.Restart(ctx, id); err != nil {
		t.Skipf("this CUPS does not restart a canceled job: %v", err)
	}
	if j, ok := find(t, c, "completed", id); ok && j.State == 7 {
		t.Errorf("job %d is still canceled after Restart", id)
	}
}

func podmanExec(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("podman", append([]string{"exec", "skra-harness"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("podman exec %v: %v\n%s", args, err, out)
	}
}

func TestMoveJob(t *testing.T) {
	c := testenv.Client(t)
	ctx := ctxFor(t)
	// The add-queue API arrives in a later task, so the second queue is made
	// inside the container.
	podmanExec(t, "lpadmin", "-p", "Virtual-MFC-2", "-E", "-v", "ipp://localhost:8631/ipp/print", "-m", "everywhere")
	t.Cleanup(func() { podmanExec(t, "lpadmin", "-x", "Virtual-MFC-2") })

	id := held(t, c, "move")
	if err := c.Move(ctx, id, "Virtual-MFC-2"); err != nil {
		t.Fatal(err)
	}
	j, ok := find(t, c, "not-completed", id)
	if !ok || j.Printer != "Virtual-MFC-2" {
		t.Errorf("after Move: found %v, printer %q, want Virtual-MFC-2", ok, j.Printer)
	}
	if err := c.Move(ctx, id, "No-Such-Queue"); err == nil {
		t.Error("moving to a queue that does not exist succeeded")
	}
}

// brokenAfter yields n bytes and then fails, like a browser that went away.
type brokenAfter struct{ n int }

func (b *brokenAfter) Read(p []byte) (int, error) {
	if b.n <= 0 {
		return 0, errors.New("the upload broke off")
	}
	n := min(len(p), b.n)
	b.n -= n
	for i := range p[:n] {
		p[i] = 'a'
	}
	return n, nil
}

// TestAbortedPrintJobLeavesNoJob streams a document that fails midway and
// checks that CUPS keeps no job of it, neither waiting nor in its history. It
// looks for the job by its name, because other packages' tests use the same
// CUPS at the same time.
func TestAbortedPrintJobLeavesNoJob(t *testing.T) {
	c := testenv.Client(t)
	name := fmt.Sprintf("skra-test aborted %d", time.Now().UnixNano())
	_, err := c.Print(ctxFor(t), queue, name, "text/plain", cups.JobOptions{Hold: true}, &brokenAfter{n: 3 << 20})
	if err == nil {
		t.Fatal("a broken upload counted as success")
	}
	t.Logf("Print error: %v", err)
	time.Sleep(time.Second) // cupsd notices the closed connection
	for _, which := range []string{"not-completed", "completed"} {
		jobs, err := c.Jobs(ctxFor(t), which, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			if j.Name == name {
				cancelAtEnd(t, c, j.ID)
				t.Errorf("job %d is left in state %d (%s list) after an aborted upload", j.ID, j.State, which)
			}
		}
	}
}
