// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	form = rig.feeder(strings.Repeat("0", 32))
	if rec := postTo(rig.s, rig.h, "/scan/start", form); rec.Code != 404 {
		t.Errorf("scan more into a missing session: status %d, want 404", rec.Code)
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
