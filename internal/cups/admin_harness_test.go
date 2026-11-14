// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package cups_test

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/testenv"
)

func ptr[T any](v T) *T { return &v }

// printer reads one queue.
func printer(t *testing.T, c *cups.Client, name string) cups.Printer {
	t.Helper()
	p, err := c.Printer(ctxFor(t), name)
	if err != nil {
		t.Fatalf("Printer(%s): %v", name, err)
	}
	return p
}

// scratch makes a queue in the container for the test and removes it at the
// end. Tests that reject, pause or empty a queue use their own, so a test of
// another package that prints on Virtual-MFC at the same moment is not hit.
// The names differ per package for the same reason.
func scratch(t *testing.T, name string) {
	t.Helper()
	podmanExec(t, "lpadmin", "-p", name, "-E", "-v", "ipp://localhost:8631/ipp/print", "-m", "everywhere")
	t.Cleanup(func() { exec.Command("podman", "exec", "skra-harness", "lpadmin", "-x", name).Run() }) // gone already is fine
}

// commandQueue makes a scratch queue from a generic PostScript driver that
// lists the three common maintenance commands, which a driverless queue does
// not. The queue is paused, so a command sent to it stays waiting.
func commandQueue(t *testing.T, name string) {
	t.Helper()
	podmanExec(t, "sh", "-c", "set -e; lpadmin -p "+name+" -v ipp://localhost:8631/ipp/print -m drv:///sample.drv/generic.ppd; "+
		"cp /etc/cups/ppd/"+name+".ppd /tmp/"+name+".ppd; "+
		"printf '*cupsFilter: \"application/vnd.cups-command 0 commandtops\"\n*cupsCommands: \"Clean PrintSelfTestPage ReportLevels\"\n' >> /tmp/"+name+".ppd; "+
		"lpadmin -p "+name+" -P /tmp/"+name+".ppd; rm /tmp/"+name+".ppd; lpadmin -p "+name+" -E; cupsdisable "+name)
	t.Cleanup(func() { exec.Command("podman", "exec", "skra-harness", "lpadmin", "-x", name).Run() })
}

// heldOn prints a held job on the queue and cancels it when the test ends.
func heldOn(t *testing.T, c *cups.Client, q, tag string) int {
	t.Helper()
	id, err := c.Print(ctxFor(t), q, "skra-test "+t.Name()+" "+tag, "text/plain", cups.JobOptions{Hold: true}, strings.NewReader("hello from skra\n"))
	if err != nil {
		t.Fatal(err)
	}
	cancelAtEnd(t, c, id)
	return id
}

// TestSettingsRoundTrip changes the defaults of Virtual-MFC, reads them back
// from CUPS and puts every one back.
func TestSettingsRoundTrip(t *testing.T) {
	c := testenv.Client(t)
	ctx := ctxFor(t)
	was := printer(t, c, queue)
	t.Cleanup(func() {
		undo := cups.Changes{Info: &was.Info, Location: &was.Location, Sides: &was.SidesDefault, Color: &was.ColorDefault,
			Media: &was.MediaDefault, ErrorPolicy: &was.ErrorPolicy, Shared: &was.Shared, Quality: &was.QualityDefault}
		if err := c.Modify(context.Background(), queue, undo); err != nil {
			t.Errorf("restoring %s: %v", queue, err)
		}
	})

	err := c.Modify(ctx, queue, cups.Changes{
		Sides: ptr("two-sided-long-edge"), Color: ptr("monochrome"), ErrorPolicy: ptr("abort-job"),
		Quality: ptr(5), Media: ptr("iso_a5_148x210mm"), Info: ptr("Skra test queue"), Location: ptr("Test lab"),
	})
	if err != nil {
		t.Fatal(err)
	}
	p := printer(t, c, queue)
	if p.SidesDefault != "two-sided-long-edge" || p.ColorDefault != "monochrome" || p.ErrorPolicy != "abort-job" ||
		p.QualityDefault != 5 || p.MediaDefault != "iso_a5_148x210mm" || p.Info != "Skra test queue" || p.Location != "Test lab" {
		t.Errorf("after Modify: %+v", p)
	}
	if p.Shared != was.Shared {
		t.Errorf("shared changed to %v although Modify did not send it", p.Shared)
	}
}

func TestAcceptRejectPause(t *testing.T) {
	c := testenv.Client(t)
	ctx := ctxFor(t)
	scratch(t, "Scratch-Cups-A")
	const q = "Scratch-Cups-A"

	if err := c.Accept(ctx, q, false); err != nil {
		t.Fatal(err)
	}
	if p := printer(t, c, q); p.Accepting {
		t.Error("the queue still accepts jobs after Accept(false)")
	}
	if err := c.Accept(ctx, q, true); err != nil {
		t.Fatal(err)
	}
	if p := printer(t, c, q); !p.Accepting {
		t.Error("the queue does not accept jobs after Accept(true)")
	}

	if err := c.Pause(ctx, q, true); err != nil {
		t.Fatal(err)
	}
	if p := printer(t, c, q); p.State != 5 {
		t.Errorf("state = %d after Pause(true), want 5", p.State)
	}
	if err := c.Pause(ctx, q, false); err != nil {
		t.Fatal(err)
	}
	if p := printer(t, c, q); p.State == 5 {
		t.Error("the queue is still stopped after Pause(false)")
	}
}

func TestCancelAllAndMoveAll(t *testing.T) {
	c := testenv.Client(t)
	ctx := ctxFor(t)
	scratch(t, "Scratch-Cups-B")
	scratch(t, "Scratch-Cups-C")
	ids := []int{heldOn(t, c, "Scratch-Cups-B", "1"), heldOn(t, c, "Scratch-Cups-B", "2")}

	if err := c.MoveAll(ctx, "Scratch-Cups-B", "Scratch-Cups-C"); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if j, ok := find(t, c, "not-completed", id); !ok || j.Printer != "Scratch-Cups-C" || j.State != 4 {
			t.Errorf("after MoveAll job %d: found %v, printer %q, state %d, want held on Scratch-Cups-C", id, ok, j.Printer, j.State)
		}
	}
	if err := c.MoveAll(ctx, "Scratch-Cups-B", "No-Such-Queue"); err == nil {
		t.Error("moving to a queue that does not exist succeeded")
	}

	if err := c.CancelAll(ctx, "Scratch-Cups-C"); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, ok := find(t, c, "not-completed", id); ok {
			t.Errorf("job %d is still waiting after CancelAll", id)
		}
		if j, ok := find(t, c, "completed", id); !ok || j.State != 7 {
			t.Errorf("job %d: found %v, state %d, want canceled (7)", id, ok, j.State)
		}
	}
}

func TestSetDefault(t *testing.T) {
	c := testenv.Client(t)
	ctx := ctxFor(t)
	before, err := c.Default(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scratch(t, "Scratch-Cups-D")
	// Deleting a queue that is the default clears it; when the harness had
	// one before, the test puts it back.
	t.Cleanup(func() {
		if before != "" {
			c.SetDefault(context.Background(), before)
		}
	})
	if err := c.SetDefault(ctx, "Scratch-Cups-D"); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Default(ctx); err != nil || got != "Scratch-Cups-D" {
		t.Errorf("Default() = %q, %v, want Scratch-Cups-D", got, err)
	}
}

func TestDeleteAQueue(t *testing.T) {
	c := testenv.Client(t)
	ctx := ctxFor(t)
	scratch(t, "Scratch-Cups-E")
	if err := c.Delete(ctx, "Scratch-Cups-E"); err != nil {
		t.Fatal(err)
	}
	ps, err := c.Printers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(ps, func(p cups.Printer) bool { return p.Name == "Scratch-Cups-E" }) {
		t.Error("the queue is still listed after Delete")
	}
	if err := c.Delete(ctx, "Scratch-Cups-E"); err == nil {
		t.Error("deleting a queue that is gone succeeded")
	}
}

// TestCommandMakesAJob sends a maintenance command to a paused queue, so the
// job stays waiting, and finds it.
func TestCommandMakesAJob(t *testing.T) {
	c := testenv.Client(t)
	ctx := ctxFor(t)
	commandQueue(t, "Scratch-Cups-F")
	if p := printer(t, c, "Scratch-Cups-F"); !slices.Equal(p.Commands, []string{"Clean", "PrintSelfTestPage", "ReportLevels"}) || p.State != 5 {
		t.Fatalf("the scratch queue: commands %v, state %d", p.Commands, p.State)
	}
	id, err := c.Command(ctx, "Scratch-Cups-F", "ReportLevels")
	if err != nil {
		t.Fatal(err)
	}
	cancelAtEnd(t, c, id)
	j, ok := find(t, c, "not-completed", id)
	if !ok || j.Name != "Printer command: ReportLevels" || j.Printer != "Scratch-Cups-F" {
		t.Errorf("job %d: found %v, %+v", id, ok, j)
	}
}
