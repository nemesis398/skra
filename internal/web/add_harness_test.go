// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package web

import (
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestAddAgainstHarness adds a queue through the page's own routes and checks
// what CUPS made of it. Virtual-MFC-3 belongs to the cups package's tests,
// which run at the same time, so this queue has a name of its own.
func TestAddAgainstHarness(t *testing.T) {
	s, c := harnessServer(t)
	h := s.Handler()
	const name = "Scratch-Web-A"
	t.Cleanup(func() { exec.Command("podman", "exec", "skra-harness", "lpadmin", "-x", name).Run() }) // gone already is fine
	get(h, "/")

	panel := get(h, "/printers/new?"+url.Values{"uri": {"ipp://localhost:8631/ipp/print"}, "info": {"Scratch Web"}}.Encode())
	if panel.Code != 200 || !strings.Contains(panel.Body.String(), `value="Scratch-Web"`) {
		t.Fatalf("panel: %d %q", panel.Code, panel.Body.String())
	}

	rec := postTo(s, h, "/printers/add", url.Values{
		"name": {name}, "info": {"Scratch Web"}, "location": {"Harness"}, "address": {"ipp://localhost:8631/ipp/print"},
		"driver": {"everywhere"}, "shared": {"no"}, "default": {"no"},
	})
	if rec.Code != 200 || rec.Body.String() != "Added "+name+". Send it a test page from the overview." {
		t.Fatalf("add: %d %q", rec.Code, rec.Body.String())
	}
	p, ok := queueNamed(t, c, name)
	if !ok {
		t.Fatal("CUPS has no such queue")
	}
	if p.Info != "Scratch Web" || p.Location != "Harness" || len(p.SidesSupported) == 0 {
		t.Errorf("queue = %+v", p)
	}
	if ev := lastEvent(t, s); ev.Action != "printer.add" || ev.Target != name || !ev.OK {
		t.Errorf("event = %+v", ev)
	}

	// The name is taken now, in any case.
	rec = postTo(s, h, "/printers/add", url.Values{"name": {"scratch-web-a"}, "address": {"ipp://localhost:8631/ipp/print"}, "driver": {"everywhere"}})
	if rec.Code != 409 {
		t.Errorf("a second add under the name: %d %q", rec.Code, rec.Body.String())
	}
}

// TestDriverListAgainstHarness lists the drivers of a maker CUPS ships and one
// it does not.
func TestDriverListAgainstHarness(t *testing.T) {
	s, _ := harnessServer(t)
	h := s.Handler()
	body := get(h, "/printers/drivers?make=Generic").Body.String()
	if !strings.Contains(body, `<select class="f" id="a-ppd" name="ppd">`) || !strings.Contains(body, "<option value=") {
		t.Errorf("no drivers for Generic:\n%.300s", body)
	}
	rec := get(h, "/printers/drivers?make="+url.QueryEscape("Zzyzx Imaginary Works"))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "<option") || !strings.Contains(rec.Body.String(), "CUPS has no driver for this model.") {
		t.Errorf("an unknown maker: %d %q", rec.Code, rec.Body.String())
	}
}

// TestSearchAgainstHarness runs the search the page runs. What the container's
// backends find is not fixed, so only the shape is checked.
func TestSearchAgainstHarness(t *testing.T) {
	s, _ := harnessServer(t)
	h := s.Handler()
	start := time.Now()
	rec := postTo(s, h, "/printers/search", nil)
	took := time.Since(start)
	if rec.Code != 200 {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<table") && !strings.Contains(body, "No printer answered.") {
		t.Errorf("neither a table nor the empty text:\n%.300s", body)
	}
	if took > 15*time.Second {
		t.Errorf("the search took %v, past its 15 s limit", took)
	}
	t.Logf("search took %v", took)
}
