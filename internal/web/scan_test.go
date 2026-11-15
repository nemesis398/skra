// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/escl"
	"github.com/nemesis398/skra/internal/escl/fakescanner"
	"github.com/nemesis398/skra/internal/scan"
	"github.com/nemesis398/skra/internal/store"
)

const fakeScannerName = "Fake MFC"

// scanRig is a server whose one scanner is the fake, served over HTTP so the
// real eSCL client speaks to it. wrap, when set, stands between the client
// and the fake, so a test can slow a page down or hang up.
type scanRig struct {
	s    *Server
	h    http.Handler
	fake *fakescanner.Scanner
	ts   *httptest.Server
	url  string // the eSCL root

	mu     sync.Mutex
	listed int      // calls of Deps.Scanners
	made   int      // calls of Deps.Scanner
	reqs   []string // "METHOD path" of every request the scanner got
}

func newScanRig(t *testing.T, pages int, wrap func(next http.Handler) http.Handler) *scanRig {
	t.Helper()
	rig := &scanRig{fake: fakescanner.New(fakescanner.Options{AdfPages: pages})}
	var inner http.Handler = rig.fake
	if wrap != nil {
		inner = wrap(rig.fake)
	}
	rig.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.mu.Lock()
		rig.reqs = append(rig.reqs, r.Method+" "+r.URL.Path)
		rig.mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(rig.ts.Close)
	rig.url = rig.ts.URL + "/eSCL"

	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	scans, err := scan.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		CUPS:  &fakeCUPS{},
		Store: db,
		Scans: scans,
		Scanners: func(context.Context) []escl.Scanner {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.listed++
			return []escl.Scanner{{Name: fakeScannerName, URL: rig.url, Declared: true}}
		},
		Scanner: func(u string) (ScannerClient, error) {
			rig.mu.Lock()
			rig.made++
			rig.mu.Unlock()
			c, err := escl.New(u)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
	}
	rig.s = New(config.Config{Listen: "127.0.0.1:7631", User: "mika"}, deps, NewToken())
	rig.h = rig.s.Handler()
	// Runs before the fake's server closes, which waits for the requests
	// a running job still has open.
	t.Cleanup(rig.s.stopScans)
	return rig
}

// stopScans stops every running scan and waits for it to end, so no job
// outlives its test.
func (s *Server) stopScans() {
	s.scanMu.Lock()
	var jobs []*scanJob
	for _, j := range s.scanJobs {
		jobs = append(jobs, j)
	}
	s.scanMu.Unlock()
	for _, j := range jobs {
		j.cancel()
		select {
		case <-j.done:
		case <-time.After(stopWait):
		}
	}
}

func (rig *scanRig) counts() (listed, made int, reqs []string) {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return rig.listed, rig.made, append([]string(nil), rig.reqs...)
}

func (rig *scanRig) got(req string) bool {
	_, _, reqs := rig.counts()
	for _, r := range reqs {
		if r == req {
			return true
		}
	}
	return false
}

// feeder is the form of a feeder scan in gray at 100 dpi on A4: the fake
// draws a page at 300 dpi slowly under the race detector.
func (rig *scanRig) feeder(session string) url.Values {
	return url.Values{
		"session": {session}, "scanner": {rig.url}, "source": {"Feeder"}, "duplex": {"false"},
		"color": {"Grayscale8"}, "resolution": {"100"}, "size": {"A4"},
	}
}

// start posts the form and wants 202 with a session ID.
func (rig *scanRig) start(t *testing.T, form url.Values) string {
	t.Helper()
	rec := postTo(rig.s, rig.h, "/scan/start", form)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /scan/start: status %d: %s", rec.Code, rec.Body.String())
	}
	id := strings.TrimSpace(rec.Body.String())
	if len(id) != 32 {
		t.Fatalf("POST /scan/start answered %q, want a session ID", id)
	}
	return id
}

// pages is the number of pages the session holds.
func (rig *scanRig) pages(t *testing.T, id string) int {
	t.Helper()
	sess, err := rig.s.deps.Scans.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return len(sess.PageList())
}

// waitDone waits for the job of the session to end and returns its last
// message and class.
func (rig *scanRig) waitDone(t *testing.T, id string) (msg, class string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msg, class, running, ok := rig.s.scanState(id)
		if ok && !running {
			return msg, class
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the scan of %s did not end within 3 s", id)
	return "", ""
}

// slowPages delays every page request by d.
func slowPages(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/NextDocument") {
				select {
				case <-time.After(d):
				case <-r.Context().Done():
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// scanEvent reads the stream until a scan event holds want, and fails after
// 3 s. It returns that event's HTML.
func scanEvent(t *testing.T, c *sseClient, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		name, data := c.event(t)
		if name == "scan" && strings.Contains(data, want) {
			return data
		}
	}
	t.Fatalf("no scan event with %q within 3 s", want)
	return ""
}

// The Scan page draws without asking any scanner: the list of scanners and
// the form are fragments the page loads once it is open.
func TestScanPageReadsNoScanner(t *testing.T) {
	rig := newScanRig(t, 3, nil)
	rec := get(rig.h, "/scan")
	if rec.Code != 200 {
		t.Fatalf("GET /scan: status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<body data-page="scan">`, `class="tab on" id="tab-scan" href="/scan"`,
		`data-src="/scan/scanners"`, `id="scan-form"`, `id="scan-job"`, `data-scan="start"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scan page lacks %q", want)
		}
	}
	if i, j := strings.Index(body, `id="tab-overview"`), strings.Index(body, `id="tab-scan"`); i < 0 || j < i || j > strings.Index(body, `id="tab-printers"`) {
		t.Error("the Scan tab is not between Overview and Printers")
	}
	if listed, made, reqs := rig.counts(); listed != 0 || made != 0 || len(reqs) != 0 {
		t.Errorf("GET /scan listed scanners %d times, made %d clients, sent %v", listed, made, reqs)
	}
	if body := get(rig.h, "/").Body.String(); !strings.Contains(body, `class="tab" id="tab-scan" href="/scan"`) {
		t.Error("the overview lacks the Scan tab")
	}
}

func TestScannerList(t *testing.T) {
	rig := newScanRig(t, 3, nil)
	rec := get(rig.h, "/scan/scanners")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `<option value="`+rig.url+`">`+fakeScannerName+`</option>`) {
		t.Errorf("scanner list lacks the fake:\n%s", body)
	}
	if _, made, reqs := rig.counts(); made != 0 || len(reqs) != 0 {
		t.Errorf("listing scanners spoke to one: made %d, sent %v", made, reqs)
	}
}

// smallerGlass makes the fake say its glass and feeder are 8.5 inches wide.
func smallerGlass(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/ScannerCapabilities") {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		w.Header().Set("Content-Type", "text/xml")
		w.Write(bytes.ReplaceAll(rec.Body.Bytes(), []byte("<scan:MaxWidth>3507</scan:MaxWidth>"), []byte("<scan:MaxWidth>2550</scan:MaxWidth>")))
	})
}

func TestScannerForm(t *testing.T) {
	rig := newScanRig(t, 3, nil)
	rec := get(rig.h, "/scan/form?scanner="+url.QueryEscape(rig.url))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-group="source"`, `data-pick="Platen" aria-pressed="false">glass<`,
		`class="on" data-pick="Feeder" aria-pressed="true">feeder<`, `data-pick="Duplex" aria-pressed="false">feeder · both sides<`,
		`data-group="color"`, `data-pick="RGB24" aria-pressed="false">color<`,
		`class="on" data-pick="Grayscale8" aria-pressed="true">gray<`, `data-pick="BlackAndWhite1" aria-pressed="false">black and white<`,
		`data-group="resolution"`, `data-pick="100" aria-pressed="false">100<`, `data-pick="200" aria-pressed="false">200<`,
		`class="on" data-pick="300" aria-pressed="true">300<`, `data-pick="600" aria-pressed="false">600<`,
		`data-group="size"`, `class="on" data-pick="A4" aria-pressed="true">A4<`, `data-pick="Letter" aria-pressed="false">Letter<`,
		`data-pick="A3" aria-pressed="false">A3<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("form lacks %q", want)
		}
	}
	if strings.Contains(body, `data-pick="1200"`) {
		t.Error("form offers 1200 dpi")
	}

	small := newScanRig(t, 3, smallerGlass)
	body = get(small.h, "/scan/form?scanner="+url.QueryEscape(small.url)).Body.String()
	if !strings.Contains(body, `data-pick="A4"`) || strings.Contains(body, `data-pick="A3"`) {
		t.Errorf("a scanner 8.5 in wide must offer A4 and not A3:\n%s", body)
	}
	form := small.feeder("")
	form.Set("size", "A3")
	if rec := postTo(small.s, small.h, "/scan/start", form); rec.Code != 400 {
		t.Errorf("A3 on the smaller scanner: status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if small.got("POST /eSCL/ScanJobs") {
		t.Error("the refused A3 scan reached the scanner")
	}
}

func TestScannerFormRefusesAnUnknownScanner(t *testing.T) {
	rig := newScanRig(t, 3, nil)
	rec := get(rig.h, "/scan/form?scanner="+url.QueryEscape("http://192.0.2.9/eSCL"))
	if rec.Code != 400 {
		t.Errorf("status %d, want 400", rec.Code)
	}
	if _, made, _ := rig.counts(); made != 0 {
		t.Errorf("made %d clients for a scanner not in the list", made)
	}
}

func TestStartScansIntoTheSession(t *testing.T) {
	rig := newScanRig(t, 3, slowPages(150*time.Millisecond))
	ts := httptest.NewServer(rig.h)
	t.Cleanup(ts.Close)
	ev := openEventsAt(t, ts, "/events?page=scan")
	scanEvent(t, ev, `id="scans"`) // the full set on connect

	begun := time.Now()
	id := rig.start(t, rig.feeder(""))
	second := scanEvent(t, ev, "Scanning page 2 of the feeder…")
	if !strings.Contains(second, `data-session="`+id+`"`) || !strings.Contains(second, `data-running="true"`) {
		t.Errorf("progress event does not name the running session:\n%s", second)
	}
	last := scanEvent(t, ev, "3 pages scanned.")
	if !strings.Contains(last, `data-running="false"`) || strings.Count(last, `class="pg"`) != 3 {
		t.Errorf("last event does not show three pages and an ended job:\n%s", last)
	}
	if n := rig.pages(t, id); n != 3 || time.Since(begun) > 2*time.Second {
		t.Errorf("session holds %d pages after %v, want 3 within 2 s", n, time.Since(begun))
	}
	set := rig.fake.LastSettings()
	if set.InputSource != "Feeder" || set.Duplex || set.ColorMode != "Grayscale8" || set.XResolution != 100 || set.Width != 2480 || set.Height != 3507 {
		t.Errorf("the scanner was asked for %+v", set)
	}
	e := lastEvent(t, rig.s)
	if e.Action != "scan.start" || e.Actor != "mika" || e.Target != fakeScannerName || !e.OK {
		t.Errorf("logged %+v, want scan.start by mika on %s", e, fakeScannerName)
	}
}

func TestScanMoreAppends(t *testing.T) {
	rig := newScanRig(t, 3, nil)
	id := rig.start(t, rig.feeder(""))
	rig.waitDone(t, id)
	if again := rig.start(t, rig.feeder(id)); again != id {
		t.Fatalf("scan more answered session %s, want %s", again, id)
	}
	if msg, _ := rig.waitDone(t, id); msg != "3 pages scanned." {
		t.Errorf("message %q", msg)
	}
	if n := rig.pages(t, id); n != 6 {
		t.Errorf("session holds %d pages, want 6", n)
	}
	form := rig.feeder(id)
	form.Set("resolution", "600")
	if rec := postTo(rig.s, rig.h, "/scan/start", form); rec.Code != 400 {
		t.Errorf("scan more at another resolution: status %d, want 400", rec.Code)
	}
	if rec := postTo(rig.s, rig.h, "/scan/start", form); rec.Header().Get("X-Skra-Class") != "need" {
		t.Errorf("the refusal of another resolution has class %q, want need", rec.Header().Get("X-Skra-Class"))
	}
	frag, err := rig.s.scanFragment()
	if err != nil || !strings.Contains(frag, `data-gone="false" data-scanner="`+rig.url+`" data-dpi="100"`) {
		t.Errorf("the fragment does not say which scanner and resolution the scan keeps (%v):\n%s", err, frag)
	}
}

func TestStartRefusesWhatTheScannerLacks(t *testing.T) {
	rig := newScanRig(t, 3, nil)
	for name, change := range map[string][2]string{
		"1200 dpi":        {"resolution", "1200"},
		"CMYK":            {"color", "CMYK"},
		"unknown scanner": {"scanner", "http://192.0.2.9/eSCL"},
		"no source":       {"source", "Tray"},
		"odd duplex":      {"duplex", "maybe"},
		"no size":         {"size", "A0"},
	} {
		form := rig.feeder("")
		form.Set(change[0], change[1])
		if rec := postTo(rig.s, rig.h, "/scan/start", form); rec.Code != 400 {
			t.Errorf("%s: status %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
	if _, made, reqs := rig.counts(); made != 0 || len(reqs) != 0 {
		t.Errorf("refusals spoke to the scanner: made %d, sent %v", made, reqs)
	}
	if sessions, _ := rig.s.deps.Scans.List(); len(sessions) != 0 {
		t.Errorf("refusals left %d sessions", len(sessions))
	}
}

func TestSecondStartWhileOneRunsIsRefused(t *testing.T) {
	rig := newScanRig(t, 3, slowPages(time.Minute))
	id := rig.start(t, rig.feeder(""))
	rec := postTo(rig.s, rig.h, "/scan/start", rig.feeder(id))
	if rec.Code != 409 || rec.Body.String() != "A scan is running. Wait for it or stop it." {
		t.Errorf("second start: status %d, %q", rec.Code, rec.Body.String())
	}
}

func TestFeederEmptyKeepsThePages(t *testing.T) {
	rig := newScanRig(t, 2, nil)
	ts := httptest.NewServer(rig.h)
	t.Cleanup(ts.Close)
	ev := openEventsAt(t, ts, "/events?page=scan")
	id := rig.start(t, rig.feeder(""))
	scanEvent(t, ev, "2 pages scanned.")

	rig.fake.SetState("empty", false)
	rig.start(t, rig.feeder(id))
	got := scanEvent(t, ev, escl.ErrFeederEmpty.Error())
	if !strings.Contains(got, `<div class="scanmsg need">The feeder is empty. Put the pages in and scan more.</div>`) {
		t.Errorf("feeder empty is not in Peach:\n%s", got)
	}
	if n := rig.pages(t, id); n != 2 {
		t.Errorf("session holds %d pages, want the 2 from before", n)
	}
	if strings.Count(got, `class="pg"`) != 2 {
		t.Errorf("strip does not show the 2 pages:\n%s", got)
	}
}

// hangUpAfterFirstPage answers the first page and then hangs up on every
// request for another, as a scanner does that left the network mid-scan.
func hangUpAfterFirstPage(next http.Handler) http.Handler {
	var mu sync.Mutex
	served := 0
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/NextDocument") {
			mu.Lock()
			served++
			n := served
			mu.Unlock()
			if n > 1 {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func TestAwayIsGray(t *testing.T) {
	rig := newScanRig(t, 3, hangUpAfterFirstPage)
	id := rig.start(t, rig.feeder(""))
	msg, class := rig.waitDone(t, id)
	if msg != escl.ErrAway.Error() || class != "away" {
		t.Errorf("away gives %q in %q, want the sentinel's own text in away", msg, class)
	}
	frag, err := rig.s.scanFragment()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frag, `<div class="scanmsg away">`+escl.ErrAway.Error()+`</div>`) || strings.Contains(frag, "bad") {
		t.Errorf("fragment:\n%s", frag)
	}
	if n := rig.pages(t, id); n != 1 {
		t.Errorf("session holds %d pages, want the 1 that came", n)
	}
}

// blockSecondPage holds the second page until the request is given up.
func blockSecondPage(next http.Handler) http.Handler {
	var mu sync.Mutex
	served := 0
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/NextDocument") {
			mu.Lock()
			served++
			n := served
			mu.Unlock()
			if n > 1 {
				<-r.Context().Done()
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func TestStopCancelsTheJob(t *testing.T) {
	rig := newScanRig(t, 3, blockSecondPage)
	id := rig.start(t, rig.feeder(""))
	deadline := time.Now().Add(2 * time.Second)
	for rig.pages(t, id) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	rec := postTo(rig.s, rig.h, "/scan/"+id+"/stop", url.Values{})
	if rec.Code != 200 {
		t.Fatalf("stop: status %d: %s", rec.Code, rec.Body.String())
	}
	if !rig.got("DELETE /eSCL/ScanJobs/1") {
		_, _, reqs := rig.counts()
		t.Errorf("stop sent no DELETE to the job: %v", reqs)
	}
	if msg, _, running, _ := rig.s.scanState(id); running || msg != "Stopped after 1 page." {
		t.Errorf("after stop: %q, running %t", msg, running)
	}
	if e := lastEvent(t, rig.s); e.Action != "scan.stop" || e.Target != fakeScannerName {
		t.Errorf("logged %+v, want scan.stop on %s", e, fakeScannerName)
	}
	if rec := postTo(rig.s, rig.h, "/scan/"+id+"/stop", url.Values{}); rec.Code != 409 {
		t.Errorf("stopping an ended scan: status %d, want 409", rec.Code)
	}
}

// A running scan keeps the service up after its request is over, until the
// last page is in.
func TestScanHoldsTheIdleClock(t *testing.T) {
	rig := newScanRig(t, 3, slowPages(300*time.Millisecond))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	done := make(chan error, 1)
	go func() { done <- rig.s.Serve(ln, 100*time.Millisecond) }()

	addr := ln.Addr().String()
	req, _ := http.NewRequest("POST", "http://"+addr+"/scan/start", strings.NewReader(rig.feeder("").Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Skra-Token", rig.s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var id bytes.Buffer
	id.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start: status %d: %s", resp.StatusCode, id.String())
	}
	http.DefaultClient.CloseIdleConnections()

	select {
	case err := <-done:
		t.Fatalf("Serve returned (%v) while the scan ran", err)
	case <-time.After(800 * time.Millisecond):
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after the scan ended")
	}
	if n := rig.pages(t, strings.TrimSpace(id.String())); n != 3 {
		t.Errorf("session holds %d pages when the service exits, want 3", n)
	}
}

// hangUpOnPages hangs up on every page request, as a scanner does that left
// the network before the first sheet.
func hangUpOnPages(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/NextDocument") {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// A first scan that takes no page leaves no session: the page forgets it, and
// a start on another resolution, even one that still names it, begins anew.
func TestAFailedFirstScanLeavesNoSession(t *testing.T) {
	rig := newScanRig(t, 3, hangUpOnPages)
	id := rig.start(t, rig.feeder(""))
	if msg, class := rig.waitDone(t, id); msg != escl.ErrAway.Error() || class != "away" {
		t.Fatalf("first scan ended with %q in %q", msg, class)
	}
	if sessions, _ := rig.s.deps.Scans.List(); len(sessions) != 0 {
		t.Errorf("an empty first scan left %d sessions", len(sessions))
	}
	frag, err := rig.s.scanFragment()
	if err != nil || !strings.Contains(frag, `data-session="`+id+`" data-running="false" data-gone="true"`) {
		t.Errorf("the fragment does not mark the scan gone (%v):\n%s", err, frag)
	}
	form := rig.feeder(id)
	form.Set("resolution", "200")
	rec := postTo(rig.s, rig.h, "/scan/start", form)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start on another resolution after an empty scan: status %d: %s", rec.Code, rec.Body.String())
	}
	if again := strings.TrimSpace(rec.Body.String()); again == id {
		t.Error("the new scan reuses the discarded session")
	}
}

// A scan whose every page was dropped has no pages before to match: a start
// that names it at another resolution starts a new scan, and the empty one
// goes.
func TestScanAfterDroppingEveryPageStartsAfresh(t *testing.T) {
	rig := newScanRig(t, 1, nil)
	id := rig.start(t, rig.feeder(""))
	rig.waitDone(t, id)
	if rec := postTo(rig.s, rig.h, "/scan/"+id+"/page/1/drop", nil); rec.Code != 200 {
		t.Fatalf("drop: status %d, %q", rec.Code, rec.Body.String())
	}
	form := rig.feeder(id)
	form.Set("resolution", "200")
	rec := postTo(rig.s, rig.h, "/scan/start", form)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start at 200 dpi after dropping every page: status %d: %s", rec.Code, rec.Body.String())
	}
	again := strings.TrimSpace(rec.Body.String())
	if again == id {
		t.Error("the new scan reuses the emptied session")
	}
	rig.waitDone(t, again)
	if _, err := rig.s.deps.Scans.Get(id); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the emptied session is still kept: %v", err)
	}
	if _, _, _, ok := rig.s.scanState(id); ok {
		t.Error("the service still lists the emptied scan")
	}
	sess, err := rig.s.deps.Scans.Get(again)
	if err != nil || sess.Settings.Resolution != 200 {
		t.Errorf("the new scan: %v, at %d dpi, want 200", err, sess.Settings.Resolution)
	}
}

// A scan more that takes no page keeps the session and the pages before.
func TestScanMoreThatTakesNothingKeepsTheSession(t *testing.T) {
	rig := newScanRig(t, 2, nil)
	id := rig.start(t, rig.feeder(""))
	rig.waitDone(t, id)
	rig.fake.SetState("empty", false)
	rig.start(t, rig.feeder(id))
	rig.waitDone(t, id)
	if n := rig.pages(t, id); n != 2 {
		t.Errorf("session holds %d pages, want 2", n)
	}
	if frag, _ := rig.s.scanFragment(); !strings.Contains(frag, `data-gone="false"`) {
		t.Errorf("the session is marked gone:\n%s", frag)
	}
}

// hangUpOnEverything is a scanner that answers nothing at all.
func hangUpOnEverything(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			conn.Close()
		}
	})
}

// The answer to a start names its color as the scan messages do, so the page
// never shows a scanner that is away, or a scan that runs, in red.
func TestStartAnswersCarryTheirColor(t *testing.T) {
	away := newScanRig(t, 3, hangUpOnEverything)
	rec := postTo(away.s, away.h, "/scan/start", away.feeder(""))
	if rec.Code != http.StatusBadGateway || rec.Body.String() != escl.ErrAway.Error() || rec.Header().Get("X-Skra-Class") != "away" {
		t.Errorf("away at start: status %d, %q, class %q; want 502, the sentinel's text, away",
			rec.Code, rec.Body.String(), rec.Header().Get("X-Skra-Class"))
	}

	busy := newScanRig(t, 3, slowPages(time.Minute))
	id := busy.start(t, busy.feeder(""))
	rec = postTo(busy.s, busy.h, "/scan/start", busy.feeder(id))
	if rec.Code != http.StatusConflict || rec.Header().Get("X-Skra-Class") != "need" {
		t.Errorf("start while one runs: status %d, class %q; want 409, need", rec.Code, rec.Header().Get("X-Skra-Class"))
	}

	form := busy.feeder("")
	form.Set("resolution", "1200")
	if rec := postTo(busy.s, busy.h, "/scan/start", form); rec.Header().Get("X-Skra-Class") != "bad" {
		t.Errorf("a refused form has class %q, want bad", rec.Header().Get("X-Skra-Class"))
	}
}

// --- rotate, drop, save and unsaved scans ---

// tinyJPEG is a small gray page.
func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 21, 30))
	for i := range img.Pix {
		img.Pix[i] = uint8(i)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// saveDay is the day of the brief's example, and the time the page asks.
var saveDay = time.Date(2026, 11, 14, 10, 5, 0, 0, time.Local)

// newSaveRig is a scan rig whose home is a directory of its own, which holds
// the scan folder ~/Nextcloud/Documents/Scans, and whose clock says saveDay.
func newSaveRig(t *testing.T, wrap func(http.Handler) http.Handler) (rig *scanRig, folder string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	rig = newScanRig(t, 3, wrap)
	folder = filepath.Join(home, "Nextcloud", "Documents", "Scans")
	rig.s.cfg.ScanFolder = folder
	rig.s.now = func() time.Time { return saveDay }
	return rig, folder
}

// session makes a session of n pages on the fake, as a scan at 100 dpi
// leaves it.
func (rig *scanRig) session(t *testing.T, n int) *scan.Session {
	t.Helper()
	return addPages(t, rig.s.deps.Scans, rig.url, n)
}

func addPages(t *testing.T, st *scan.Store, url string, n int) *scan.Session {
	t.Helper()
	sess, err := st.New(escl.Scanner{Name: fakeScannerName, URL: url},
		escl.Settings{Source: "Feeder", Color: "Grayscale8", Resolution: 100, Width: 2480, Height: 3507})
	if err != nil {
		t.Fatal(err)
	}
	for range n {
		if _, err := sess.AddPage(bytes.NewReader(tinyJPEG(t))); err != nil {
			t.Fatal(err)
		}
	}
	return sess
}

// The save row shows the path a save will write, made from the title as it
// is typed, before anything is written.
func TestSaveShowsThePathFirst(t *testing.T) {
	rig, folder := newSaveRig(t, nil)
	sess := rig.session(t, 2)
	name := func(title, format string) *httptest.ResponseRecorder {
		return get(rig.h, "/scan/"+sess.ID+"/name?"+url.Values{"title": {title}, "format": {format}}.Encode())
	}
	for _, c := range []struct{ title, format, want string }{
		{"Krankenkasse Beitragsbescheid", "pdf", "~/Nextcloud/Documents/Scans/2026-11-14 Krankenkasse Beitragsbescheid.pdf"},
		{"Krankenkasse Mahnung", "pdf", "~/Nextcloud/Documents/Scans/2026-11-14 Krankenkasse Mahnung.pdf"},
		{"a/b", "png", "~/Nextcloud/Documents/Scans/2026-11-14 a b 1.png and 1 more"},
		{"X", "jpeg", "~/Nextcloud/Documents/Scans/2026-11-14 X 1.jpg and 1 more"},
	} {
		rec := name(c.title, c.format)
		if rec.Code != 200 || rec.Body.String() != c.want || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%q as %s: status %d, %q; want %q", c.title, c.format, rec.Code, rec.Body.String(), c.want)
		}
	}
	if rec := name("X", "tiff"); rec.Code != 400 {
		t.Errorf("tiff: status %d, want 400", rec.Code)
	}
	if rec := get(rig.h, "/scan/"+strings.Repeat("0", 32)+"/name?format=pdf"); rec.Code != 404 {
		t.Errorf("a gone scan: status %d, want 404", rec.Code)
	}
	body := get(rig.h, "/scan").Body.String()
	for _, want := range []string{
		`id="scan-save"`, `id="scan-title"`, `data-group="format"`,
		`<button type="button" class="on" data-pick="pdf" aria-pressed="true">PDF</button>`,
		`data-pick="png" aria-pressed="false">PNG</button>`, `data-pick="jpeg" aria-pressed="false">JPEG</button>`,
		`id="scan-path">~/Nextcloud/Documents/Scans/</span>`, `data-scan="save"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scan page lacks %q", want)
		}
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(folder))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("showing the path wrote to the home: %v", err)
	}
}

func TestSaveWritesAndForgets(t *testing.T) {
	rig, folder := newSaveRig(t, nil)
	sess := rig.session(t, 2)
	rig.s.adopt(sess)
	rec := postTo(rig.s, rig.h, "/scan/"+sess.ID+"/save", url.Values{"title": {"X"}, "format": {"pdf"}})
	if rec.Code != 200 || rec.Body.String() != `Saved "2026-11-14 X.pdf" in Scans.` {
		t.Fatalf("save: status %d, %q", rec.Code, rec.Body.String())
	}
	file := filepath.Join(folder, "2026-11-14 X.pdf")
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the saved file: %v, %v; want mode 0600", fi, err)
	}
	if _, err := rig.s.deps.Scans.Get(sess.ID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the session is still kept: %v", err)
	}
	if _, _, _, ok := rig.s.scanState(sess.ID); ok {
		t.Error("the service still lists the saved scan")
	}
	if e := lastEvent(t, rig.s); e.Action != "scan.save" || e.Target != file || e.Actor != "mika" || !e.OK {
		t.Errorf("logged %+v, want scan.save of %s", e, file)
	}

	three := rig.session(t, 3)
	rec = postTo(rig.s, rig.h, "/scan/"+three.ID+"/save", url.Values{"title": {"Y"}, "format": {"jpeg"}})
	if rec.Code != 200 || rec.Body.String() != `Saved "2026-11-14 Y 1.jpg" and 2 more in Scans.` {
		t.Errorf("save as JPEG: status %d, %q", rec.Code, rec.Body.String())
	}
	if e := lastEvent(t, rig.s); e.Action != "scan.save" || !strings.Contains(e.Target, filepath.Join(folder, "2026-11-14 Y 3.jpg")) {
		t.Errorf("logged %+v, want every path", e)
	}

	for _, c := range []struct {
		name, path string
		form       url.Values
		code       int
		want       string
	}{
		{"saved before", "/scan/" + sess.ID + "/save", url.Values{"format": {"pdf"}}, 404, ""},
		{"no pages", "/scan/" + rig.session(t, 0).ID + "/save", url.Values{"format": {"pdf"}}, 400, "There are no pages to save."},
		{"bad format", "/scan/" + rig.session(t, 1).ID + "/save", url.Values{"format": {"tiff"}}, 400, ""},
		{"not an ID", "/scan/nope/save", url.Values{"format": {"pdf"}}, 404, ""},
	} {
		rec := postTo(rig.s, rig.h, c.path, c.form)
		if rec.Code != c.code || (c.want != "" && rec.Body.String() != c.want) {
			t.Errorf("%s: status %d, %q; want %d %q", c.name, rec.Code, rec.Body.String(), c.code, c.want)
		}
	}
}

// A save that fails says so in a sentence of its own and keeps the scan.
func TestSaveFailureKeepsTheScan(t *testing.T) {
	rig, folder := newSaveRig(t, nil)
	if err := os.MkdirAll(filepath.Dir(folder), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(folder, nil, 0o600); err != nil { // a file where the folder goes
		t.Fatal(err)
	}
	sess := rig.session(t, 1)
	rec := postTo(rig.s, rig.h, "/scan/"+sess.ID+"/save", url.Values{"title": {"X"}, "format": {"pdf"}})
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "scan:") || !strings.Contains(rec.Body.String(), "Scans") {
		t.Errorf("failed save: status %d, %q", rec.Code, rec.Body.String())
	}
	if n := rig.pages(t, sess.ID); n != 1 {
		t.Errorf("the failed save left %d pages, want 1", n)
	}
}

func TestRotateAndDrop(t *testing.T) {
	rig, _ := newSaveRig(t, nil)
	sess := rig.session(t, 3)
	id := sess.ID
	edit := func(path string, form url.Values) *httptest.ResponseRecorder {
		return postTo(rig.s, rig.h, "/scan/"+id+"/page/"+path, form)
	}
	frag := func() string {
		f, err := rig.s.scanFragment()
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if rec := edit("2/rotate", url.Values{"by": {"90"}}); rec.Code != 200 {
		t.Fatalf("rotate: status %d, %q", rec.Code, rec.Body.String())
	}
	if f := frag(); !strings.Contains(f, `<img class="turn90" src="/scan/`+id+`/page/2?`) || strings.Count(f, "turn") != 1 {
		t.Errorf("page 2 is not shown turned:\n%s", f)
	}
	edit("2/rotate", url.Values{"by": {"-90"}})
	edit("2/rotate", url.Values{"by": {"-90"}})
	if f := frag(); !strings.Contains(f, `<img class="turn270" src="/scan/`+id+`/page/2?`) {
		t.Errorf("page 2 is not turned back past upright:\n%s", f)
	}
	if rec := edit("2/rotate", url.Values{"by": {"45"}}); rec.Code != 400 {
		t.Errorf("rotate by 45: status %d, want 400", rec.Code)
	}

	first, err := sess.PagePath(1)
	if err != nil {
		t.Fatal(err)
	}
	if rec := edit("1/drop", nil); rec.Code != 200 {
		t.Fatalf("drop: status %d, %q", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dropped page is still on disk: %v", err)
	}
	if f := frag(); strings.Count(f, `class="pg"`) != 2 || !strings.Contains(f, `<img class="turn270" src="/scan/`+id+`/page/1?`) {
		t.Errorf("the strip does not show the 2 pages left, the turned one first:\n%s", f)
	}

	for _, path := range []string{"3/rotate", "0/rotate", "x/rotate", "3/drop", "0/drop", "-1/drop"} {
		if rec := edit(path, url.Values{"by": {"90"}}); rec.Code != 404 {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
	}
	if rec := get(rig.h, "/scan/"+id+"/page/3"); rec.Code != 404 {
		t.Errorf("GET page 3: status %d, want 404", rec.Code)
	}
	rec := get(rig.h, "/scan/"+id+"/page/2")
	want, _ := os.ReadFile(mustPath(t, sess, 2))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" || rec.Header().Get("Cache-Control") != "no-store" || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Errorf("GET page 2: status %d, %q, %q, %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"), rec.Body.Len())
	}
}

func mustPath(t *testing.T, sess *scan.Session, n int) string {
	t.Helper()
	p, err := sess.PagePath(n)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A running scan's session takes no save, discard, turn or drop: a discard
// during a page on its way could leave a session of that one page.
func TestPageEditsWaitForTheScan(t *testing.T) {
	rig, _ := newSaveRig(t, slowPages(time.Minute))
	sess := rig.session(t, 1)
	rig.start(t, rig.feeder(sess.ID))
	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"save", url.Values{"format": {"pdf"}}},
		{"discard", nil},
		{"page/1/rotate", url.Values{"by": {"90"}}},
		{"page/1/drop", nil},
	} {
		rec := postTo(rig.s, rig.h, "/scan/"+sess.ID+"/"+c.path, c.form)
		if rec.Code != 409 || rec.Body.String() != "A scan is running. Wait for it or stop it." || rec.Header().Get("X-Skra-Class") != "need" {
			t.Errorf("%s while scanning: status %d, %q, class %q", c.path, rec.Code, rec.Body.String(), rec.Header().Get("X-Skra-Class"))
		}
	}
	if n := rig.pages(t, sess.ID); n != 1 {
		t.Errorf("the session holds %d pages, want 1", n)
	}
}

// restartWith is the rig's server as it starts again over the scans in st.
func restartWith(rig *scanRig, st *scan.Store) *Server {
	deps := rig.s.deps
	deps.Scans = st
	s := New(rig.s.cfg, deps, NewToken())
	s.now = rig.s.now
	return s
}

// setCreated rewrites when the session in dir/scans/id says it began.
func setCreated(t *testing.T, dir, id string, at time.Time) {
	t.Helper()
	path := filepath.Join(dir, "scans", id, "session.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m["Created"] = at
	if data, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A scan that was not saved before skrá exited shows on the Scan tab again,
// to continue or discard. One that holds no page, as a crash can leave, goes
// without a word.
func TestUnsavedScanComesBack(t *testing.T) {
	rig, _ := newSaveRig(t, nil)
	dir := t.TempDir()
	before, err := scan.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	kept := addPages(t, before, rig.url, 3)
	empty := addPages(t, before, rig.url, 0)
	setCreated(t, dir, kept.ID, time.Date(2026, 11, 14, 9, 41, 0, 0, time.Local))
	after, err := scan.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := restartWith(rig, after)
	t.Cleanup(s.stopScans)
	h := s.Handler()

	body := get(h, "/scan").Body.String()
	for _, want := range []string{
		`An unsaved scan from 09:41 with 3 pages.`,
		`data-scan="continue" data-session="` + kept.ID + `"`,
		`data-scan="discard" data-session="` + kept.ID + `"`,
		`data-confirm="The scan and its 3 pages are deleted, not saved."`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scan page lacks %q", want)
		}
	}
	if strings.Count(body, "An unsaved scan") != 1 {
		t.Errorf("the page lists the empty scan too:\n%s", body)
	}
	if _, err := after.Get(empty.ID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the empty scan is still kept: %v", err)
	}
	if f, _ := s.scanFragment(); !strings.Contains(f, `data-session="`+kept.ID+`"`) || strings.Count(f, `class="pg"`) != 3 {
		t.Errorf("the scan fragment lacks the scan to continue:\n%s", f)
	}

	rec := postTo(s, h, "/scan/"+kept.ID+"/discard", nil)
	if rec.Code != 200 {
		t.Fatalf("discard: status %d, %q", rec.Code, rec.Body.String())
	}
	if _, err := after.Get(kept.ID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the discarded scan is still kept: %v", err)
	}
	if _, _, _, ok := s.scanState(kept.ID); ok {
		t.Error("the service still lists the discarded scan")
	}
	if e := lastEvent(t, s); e.Action != "scan.discard" || !e.OK {
		t.Errorf("logged %+v, want scan.discard", e)
	}
	if body := get(h, "/scan").Body.String(); strings.Contains(body, "An unsaved scan") {
		t.Error("the discarded scan still shows")
	}
	if rec := postTo(s, h, "/scan/"+kept.ID+"/discard", nil); rec.Code != 404 {
		t.Errorf("a second discard: status %d, want 404", rec.Code)
	}
}

// A Scan page left open across an idle exit connects again to a fresh
// process, which has not drawn the Scan tab. Its first scan fragment still
// holds the scan kept from before, with its pages, and says it does not run,
// so the page shows it as kept and not as scanning.
func TestEventsShowAKeptScanAfterARestart(t *testing.T) {
	rig, _ := newSaveRig(t, nil)
	dir := t.TempDir()
	before, err := scan.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	kept := addPages(t, before, rig.url, 2)
	after, err := scan.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := restartWith(rig, after)
	t.Cleanup(s.stopScans)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	ev := openEventsAt(t, ts, "/events?page=scan")
	got := scanEvent(t, ev, "")
	if !strings.Contains(got, `data-session="`+kept.ID+`" data-running="false" data-gone="false"`) {
		t.Errorf("the first scan fragment lacks the kept scan, not running:\n%s", got)
	}
	if n := strings.Count(got, `class="pg"`); n != 2 {
		t.Errorf("the first scan fragment shows %d pages, want 2:\n%s", n, got)
	}
}

// Another site cannot read a scanned page, not even as a picture of its own.
func TestPagesAreNotForOtherSites(t *testing.T) {
	rig, _ := newSaveRig(t, nil)
	sess := rig.session(t, 1)
	for _, h := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image"},
		{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image"},
		{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"},
	} {
		req := httptest.NewRequest("GET", "/scan/"+sess.ID+"/page/1", nil)
		req.Host = "127.0.0.1:7631"
		for k, v := range h {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		rig.h.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("%v: status %d, want 403", h, rec.Code)
		}
	}
	req := httptest.NewRequest("GET", "/scan/"+sess.ID+"/page/1", nil)
	req.Host = "127.0.0.1:7631"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Dest", "image")
	rec := httptest.NewRecorder()
	rig.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("the page's own picture: status %d", rec.Code)
	}
}

// The path the page shows before a save is the one the save writes: for a
// title long enough to be cut into numbered pictures, and when the plain
// name is taken.
func TestShownPathIsTheWrittenPath(t *testing.T) {
	rig, folder := newSaveRig(t, nil)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "2026-11-14 Taken.pdf"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("HOME")
	long := strings.Repeat("Beitragsbescheid ", 18) // about 300 characters
	for _, c := range []struct {
		title, format, more string
	}{{long, "jpeg", " and 2 more"}, {"Taken", "pdf", ""}} {
		sess := rig.session(t, 3)
		q := url.Values{"title": {c.title}, "format": {c.format}}
		shown := get(rig.h, "/scan/"+sess.ID+"/name?"+q.Encode()).Body.String()
		if rec := postTo(rig.s, rig.h, "/scan/"+sess.ID+"/save", q); rec.Code != 200 {
			t.Fatalf("save: status %d, %q", rec.Code, rec.Body.String())
		}
		first, _, _ := strings.Cut(lastEvent(t, rig.s).Target, ", ")
		if want := "~" + strings.TrimPrefix(first, home) + c.more; shown != want {
			t.Errorf("%.20q as %s: shown %q, written %q", c.title, c.format, shown, want)
		}
	}
}

// A save holds its scan from start to end: scan more, turn, drop and discard
// wait for it, so no page comes in that the save then deletes, and the job of
// a scan that started is never forgotten while it runs.
func TestScanMoreWaitsForTheSave(t *testing.T) {
	rig, _ := newSaveRig(t, nil)
	sess := rig.session(t, 2)
	begun, release := make(chan struct{}), make(chan struct{})
	saveScanFiles = func(s *scan.Session, folder, title, format string, now time.Time) ([]string, error) {
		close(begun)
		<-release
		return scan.Save(s, folder, title, format, now)
	}
	t.Cleanup(func() { saveScanFiles = scan.Save })

	saved := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		saved <- postTo(rig.s, rig.h, "/scan/"+sess.ID+"/save", url.Values{"title": {"X"}, "format": {"pdf"}})
	}()
	select {
	case <-begun:
	case rec := <-saved:
		t.Fatalf("the save did not go through saveScanFiles: status %d, %q", rec.Code, rec.Body.String())
	}
	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/scan/start", rig.feeder(sess.ID)},
		{"/scan/" + sess.ID + "/discard", nil},
		{"/scan/" + sess.ID + "/save", url.Values{"format": {"pdf"}}},
		{"/scan/" + sess.ID + "/page/1/rotate", url.Values{"by": {"90"}}},
		{"/scan/" + sess.ID + "/page/1/drop", nil},
	} {
		rec := postTo(rig.s, rig.h, c.path, c.form)
		if rec.Code != 409 || rec.Header().Get("X-Skra-Class") != "need" || rec.Body.String() != scanBusyText {
			t.Errorf("%s during the save: status %d, %q, class %q", c.path, rec.Code, rec.Body.String(), rec.Header().Get("X-Skra-Class"))
		}
	}
	close(release)
	if rec := <-saved; rec.Code != 200 {
		t.Fatalf("save: status %d, %q", rec.Code, rec.Body.String())
	}
	if _, made, _ := rig.counts(); made != 0 {
		t.Errorf("a scan reached the scanner during the save (%d clients)", made)
	}
	// The saved scan is gone, so scan more starts a new one.
	if id := rig.start(t, rig.feeder(sess.ID)); id == sess.ID {
		t.Error("scan more went into the saved scan")
	}
}

// A job whose session went away under it says so in a sentence of its own.
func TestAGoneScanSaysSo(t *testing.T) {
	msg, class := scanOutcome(context.Background(), fmt.Errorf("adding a page: %w", scan.ErrGone), 1)
	if msg != "This scan was saved or discarded." || class != "need" {
		t.Errorf("a gone scan ends with %q in %q", msg, class)
	}
}
