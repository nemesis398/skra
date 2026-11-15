// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package web

import (
	"bytes"
	"context"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
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

// fileState is what a walk records of a path. A directory's size and time
// change with what is in it, so only its mode counts.
type fileState struct {
	mode    os.FileMode
	size    int64
	modTime time.Time
}

// walkHome records every path under root.
func walkHome(t *testing.T, root string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		st := fileState{mode: fi.Mode()}
		if !fi.IsDir() {
			st.size, st.modTime = fi.Size(), fi.ModTime()
		}
		out[path] = st
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// TestThreePageFeederScanBecomesOnePDF is Verification 5 and 6: three
// feeder pages from the fake become one PDF in the folder the page names,
// and nothing else under the home changes but the state directory.
func TestThreePageFeederScanBecomesOnePDF(t *testing.T) {
	root := startFakeScanner(t) // built before HOME moves, so the build cache stays put
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".local", "state", "skra")
	folder := filepath.Join(home, "Nextcloud", "Documents", "Scans")
	if err := os.MkdirAll(filepath.Join(home, "Nextcloud", "Documents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "Nextcloud", "Documents", "Brief.odt"), []byte("not a scan"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Listen: "127.0.0.1:7631", User: "root", StateDir: stateDir, ScanFolder: folder,
		Scanners: []config.Scanner{{Name: "skrá fake scanner", URL: root}}}
	db, err := store.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	scans, err := scan.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, Deps{CUPS: testenv.Client(t), Store: db, Scans: scans,
		Scanners: func(ctx context.Context) []escl.Scanner { return escl.Find(ctx, cfg.Scanners, nil) },
	}, NewToken())
	t.Cleanup(s.stopScans)
	// One clock for the server and the name the test expects, so a run
	// across midnight does not see two dates.
	at := time.Now()
	s.now = func() time.Time { return at }
	h := s.Handler()
	before := walkHome(t, home)

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
	shown := get(h, "/scan/"+id+"/name?title=Krankenkasse+Beitragsbescheid&format=pdf").Body.String()
	name := scan.FileName("Krankenkasse Beitragsbescheid", at, "pdf")
	if shown != "~/Nextcloud/Documents/Scans/"+name {
		t.Errorf("the page shows %q before the save", shown)
	}
	rec = postTo(s, h, "/scan/"+id+"/save", url.Values{"title": {"Krankenkasse Beitragsbescheid"}, "format": {"pdf"}})
	if rec.Code != 200 || rec.Body.String() != `Saved "`+name+`" in Scans.` {
		t.Fatalf("save: status %d, %q", rec.Code, rec.Body.String())
	}

	dest := filepath.Join(folder, name)
	doc, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(doc, []byte("/Type /Page ")); n != 3 || !bytes.Contains(doc, []byte("/Count 3 ")) {
		t.Errorf("the PDF has %d pages, want 3", n)
	}
	if ents, _ := os.ReadDir(folder); len(ents) != 1 {
		t.Errorf("the folder holds %d entries, want the one PDF", len(ents))
	}

	after := walkHome(t, home)
	for path, st := range after {
		if was, ok := before[path]; ok && was == st {
			continue
		}
		if path != dest && path != folder && !within(path, stateDir) {
			t.Errorf("%s changed, and it is neither the scan nor the state directory", path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok && !within(path, stateDir) {
			t.Errorf("%s went away", path)
		}
	}
}
