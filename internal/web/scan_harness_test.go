// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package web

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/escl"
	"github.com/nemesis398/skra/internal/scan"
	"github.com/nemesis398/skra/internal/store"
	"github.com/nemesis398/skra/internal/testenv"
)

// startFakeScanner builds cmd/fakescanner, runs it on a free loopback port and
// returns its eSCL root once it answers. The process ends with the test.
func startFakeScanner(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakescanner")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/nemesis398/skra/cmd/fakescanner").CombinedOutput(); err != nil {
		t.Fatalf("building the fake scanner: %v\n%s", err, out)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cmd := exec.Command(bin, "-listen", addr)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	root := "http://" + addr + "/eSCL"
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(root + "/ScannerStatus")
		if err == nil {
			resp.Body.Close()
			return root
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake scanner on %s did not answer: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestScanThroughTheFake drives a feeder scan from the Scan tab's routes to
// the fake scanner's own process, wired as cmd/skra wires it: the scanner
// comes from the config through escl.Find and is spoken to by escl.New.
func TestScanThroughTheFake(t *testing.T) {
	root := startFakeScanner(t)
	cfg := config.Config{Listen: "127.0.0.1:7631", User: "root",
		Scanners: []config.Scanner{{Name: "skrá fake scanner", URL: root}}}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	scans, err := scan.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, Deps{CUPS: testenv.Client(t), Store: db, Scans: scans,
		Scanners: func(ctx context.Context) []escl.Scanner { return escl.Find(ctx, cfg.Scanners, nil) },
	}, NewToken())
	t.Cleanup(s.stopScans)
	h := s.Handler()

	if rec := get(h, "/scan"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "CUPS answers") {
		t.Fatalf("GET /scan: status %d", rec.Code)
	}
	if body := get(h, "/scan/scanners").Body.String(); !strings.Contains(body, `value="`+root+`"`) {
		t.Fatalf("scanner list lacks the fake:\n%s", body)
	}
	if body := get(h, "/scan/form?scanner="+url.QueryEscape(root)).Body.String(); !strings.Contains(body, `data-pick="Duplex"`) {
		t.Fatalf("form lacks the duplex feeder:\n%s", body)
	}
	form := url.Values{"scanner": {root}, "source": {"Feeder"}, "duplex": {"false"},
		"color": {"Grayscale8"}, "resolution": {"300"}, "size": {"A4"}}
	rec := postTo(s, h, "/scan/start", form)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: status %d: %s", rec.Code, rec.Body.String())
	}
	id := strings.TrimSpace(rec.Body.String())
	deadline := time.Now().Add(10 * time.Second)
	for {
		msg, _, running, _ := s.scanState(id)
		if !running {
			if msg != "3 pages scanned." {
				t.Fatalf("the scan ended with %q", msg)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scan did not end within 10 s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	sess, err := scans.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	pages := sess.PageList()
	if len(pages) != 3 {
		t.Fatalf("%d pages, want 3", len(pages))
	}
	for i, p := range pages {
		if p.Width != 2480 || p.Height != 3507 || p.DPI != 300 {
			t.Errorf("page %d is %+v, want A4 at 300 dpi", i+1, p)
		}
	}
}
