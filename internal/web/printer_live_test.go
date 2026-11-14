// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/printer"
)

// fakeQuery stands in for printer.Query: a test sets the answer per device URI.
type fakeQuery struct {
	mu      sync.Mutex
	answers map[string]printer.Live
	asked   []string
	delay   time.Duration
}

func (q *fakeQuery) query(ctx context.Context, uri string) printer.Live {
	time.Sleep(q.delay)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.asked = append(q.asked, uri)
	return q.answers[uri]
}

func (q *fakeQuery) set(uri string, l printer.Live) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.answers == nil {
		q.answers = map[string]printer.Live{}
	}
	q.answers[uri] = l
}

func (q *fakeQuery) askedURIs() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.asked...)
}

func liveServer(f *fakeCUPS, q *fakeQuery) *Server {
	return New(config.Config{Listen: "127.0.0.1:7631", User: "mika"}, Deps{CUPS: f, Query: q.query})
}

// pageWithLive asks the printers once, as the hub's live loop does, then renders.
func pageWithLive(t *testing.T, s *Server) string {
	t.Helper()
	s.poll(context.Background(), true) // learn the queues
	s.livePoll(context.Background())
	rec := get(s.Handler(), "/")
	if rec.Code != 200 {
		t.Fatalf("GET /: status %d", rec.Code)
	}
	return rec.Body.String()
}

func liveInks() []cups.Marker {
	return []cups.Marker{{Name: "Foo toner", Level: 91, Low: -1}, {Name: "Bar drum", Level: 33, Low: -1}}
}

func TestLivePreferredOverCUPS(t *testing.T) {
	p := brother()
	q := &fakeQuery{}
	q.set(p.DeviceURI, printer.Live{Reachable: true, Markers: liveInks(), At: time.Now()})
	body := pageWithLive(t, liveServer(&fakeCUPS{printers: []cups.Printer{p}}, q))

	for _, want := range []string{`<span class="k">Fo</span>`, `<span class="k">Ba</span>`, "91 %", "33 %", "ready"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, unwanted := range []string{"ink as of", "68 %", "no ink levels", `class="st away"`, `<span class="k">C</span>`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page has %q although the printer answered", unwanted)
		}
	}
}

func TestLiveUnreachableIsAwayNotRed(t *testing.T) {
	p := brother()
	q := &fakeQuery{}
	q.set(p.DeviceURI, printer.Live{Reachable: false, At: time.Now()})
	body := pageWithLive(t, liveServer(&fakeCUPS{printers: []cups.Printer{p}}, q))

	for _, want := range []string{`class="pr away"`, `class="st away"`, "away", "ink as of", "the last time CUPS spoke to it"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Not a bare "bad": the page token is random hex and can spell it.
	if strings.Contains(body, "st bad") || strings.Contains(body, "pr bad") || strings.Contains(body, " ready") {
		t.Error("an unreachable printer must be away, never bad or ready")
	}
}

func TestLiveUnreachableBeatsWhateverCUPSThinks(t *testing.T) {
	p := brother()
	p.State, p.Reasons = 5, []string{"media-jam-error"}
	q := &fakeQuery{}
	q.set(p.DeviceURI, printer.Live{Reachable: false, At: time.Now()})
	body := pageWithLive(t, liveServer(&fakeCUPS{printers: []cups.Printer{p}}, q))
	if !strings.Contains(body, `class="st away"`) || strings.Contains(body, "paper jam") || strings.Contains(body, "needs you") {
		t.Errorf("an unreachable printer shows its stale CUPS state:\n%s", body)
	}
}

func TestLiveCUPSOfflineStaysAwayEvenWhenReachable(t *testing.T) {
	p := brother()
	p.Reasons = []string{"offline-report"}
	q := &fakeQuery{}
	q.set(p.DeviceURI, printer.Live{Reachable: true, Markers: liveInks(), At: time.Now()})
	body := pageWithLive(t, liveServer(&fakeCUPS{printers: []cups.Printer{p}}, q))
	if !strings.Contains(body, `class="st away"`) {
		t.Error("offline-report from CUPS should still read away")
	}
}

func TestLiveBeforeTheFirstAnswerShowsCUPS(t *testing.T) {
	// No live loop has run: the page falls back to CUPS's levels and says so.
	f := &fakeCUPS{printers: []cups.Printer{brother()}}
	body := get(liveServer(f, &fakeQuery{}).Handler(), "/").Body.String()
	for _, want := range []string{"ink as of", "the last time CUPS spoke to it", "68 %", `<span class="k">C</span>`, "ready"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, `class="st away"`) {
		t.Error("a printer nobody asked yet is not away")
	}
}

func TestLiveReachableWithoutMarkersKeepsCUPSLevels(t *testing.T) {
	p := brother()
	q := &fakeQuery{}
	q.set(p.DeviceURI, printer.Live{Reachable: true, At: time.Now()})
	body := pageWithLive(t, liveServer(&fakeCUPS{printers: []cups.Printer{p}}, q))
	if !strings.Contains(body, "68 %") || !strings.Contains(body, "ink as of") {
		t.Error("a printer that sends no markers should keep CUPS's levels with their age")
	}
	if strings.Contains(body, `class="st away"`) {
		t.Error("a printer that answered is not away")
	}
}

func TestLiveAsOfLineNeedsATime(t *testing.T) {
	p := brother()
	p.MarkersAt = time.Time{}
	body := pageWithLive(t, liveServer(&fakeCUPS{printers: []cups.Printer{p}}, &fakeQuery{}))
	if strings.Contains(body, "ink as of") || strings.Contains(body, "no ink levels") {
		t.Error("levels without marker-change-time get no age line")
	}
	if !strings.Contains(body, "68 %") {
		t.Error("the levels are missing")
	}
}

func TestLiveOnlyAsksPrintersItCanReach(t *testing.T) {
	mk := func(name, uri string) cups.Printer {
		p := brother()
		p.Name, p.Info, p.DeviceURI = name, name, uri
		return p
	}
	queried := map[string]string{
		"Ipp": "ipp://printer.local/ipp/print", "Ipps": "ipps://printer.local/ipp/print",
		"Dnssd": "dnssd://Brother%20X._ipp._tcp.local/", "Usbbridge": "ipp://localhost:60000/ipp/print",
	}
	skipped := map[string]string{
		"Usb": "usb://Brother/MFC", "Sock": "socket://10.0.0.5:9100", "Lpd": "lpd://10.0.0.5/q",
		"Cls": "implicitclass://Foo/", "File": "file:///dev/null", "Http": "http://10.0.0.5:631/ipp", "None": "",
	}
	var ps []cups.Printer
	q := &fakeQuery{}
	for n, u := range queried {
		ps = append(ps, mk(n, u))
		q.set(u, printer.Live{Reachable: false, At: time.Now()})
	}
	for n, u := range skipped {
		ps = append(ps, mk(n, u))
		q.set(u, printer.Live{Reachable: false, At: time.Now()}) // would read away if asked
	}
	s := liveServer(&fakeCUPS{printers: ps}, q)
	body := pageWithLive(t, s)

	asked := strings.Join(q.askedURIs(), " ")
	for n, u := range skipped {
		if u != "" && strings.Contains(asked, u) {
			t.Errorf("%s (%s) was queried", n, u)
		}
	}
	if len(q.askedURIs()) != len(queried) {
		t.Errorf("asked %d printers, want %d: %v", len(q.askedURIs()), len(queried), q.askedURIs())
	}
	snap := s.poll(context.Background(), false)
	for n := range skipped {
		if _, ok := snap.Live[n]; ok {
			t.Errorf("%s has a live entry", n)
		}
	}
	for n := range queried {
		if _, ok := snap.Live[n]; !ok {
			t.Errorf("%s has no live entry", n)
		}
	}
	if got := strings.Count(body, `class="pr away"`); got != len(queried) {
		t.Errorf("%d cells are away, want %d: only the queried printers can be away without CUPS saying so", got, len(queried))
	}
	if got := strings.Count(body, "ink as of"); got != len(queried)+len(skipped) {
		t.Errorf("%d cells show CUPS's levels, want %d", got, len(queried)+len(skipped))
	}
}

func TestLiveQueriesInParallel(t *testing.T) {
	var ps []cups.Printer
	q := &fakeQuery{delay: 200 * time.Millisecond}
	for _, n := range []string{"A", "B", "C", "D"} {
		p := brother()
		p.Name, p.Info, p.DeviceURI = n, n, "ipp://"+n+".local/ipp/print"
		ps = append(ps, p)
	}
	s := liveServer(&fakeCUPS{printers: ps}, q)
	s.poll(context.Background(), true)
	start := time.Now()
	s.livePoll(context.Background())
	if d := time.Since(start); d > 450*time.Millisecond {
		t.Errorf("four printers took %v, want about 200 ms", d)
	}
	if n := len(q.askedURIs()); n != 4 {
		t.Errorf("asked %d, want 4", n)
	}
}

func TestLiveKeepsTheLastAnswerAcrossPolls(t *testing.T) {
	p := brother()
	q := &fakeQuery{}
	q.set(p.DeviceURI, printer.Live{Reachable: false, At: time.Now()})
	s := liveServer(&fakeCUPS{printers: []cups.Printer{p}}, q)
	s.poll(context.Background(), true)
	s.livePoll(context.Background())
	for range 3 { // the 2 s CUPS ticks
		snap := s.poll(context.Background(), false)
		if l, ok := snap.Live[p.Name]; !ok || l.Reachable {
			t.Fatalf("poll lost the live answer: %+v", snap.Live)
		}
	}
	if n := len(q.askedURIs()); n != 1 {
		t.Errorf("a CUPS poll asked the printer: %d queries", n)
	}
}

func TestLiveAnswerGoesStale(t *testing.T) {
	p := brother()
	q := &fakeQuery{}
	q.set(p.DeviceURI, printer.Live{Reachable: true, Markers: liveInks(), At: time.Now().Add(-time.Hour)})
	s := liveServer(&fakeCUPS{printers: []cups.Printer{p}}, q)
	s.poll(context.Background(), true)
	s.livePoll(context.Background()) // the fake answers with an hour-old time
	snap := s.poll(context.Background(), false)
	if _, ok := snap.Live[p.Name]; ok {
		t.Error("an answer an hour old still counts")
	}
}

func TestLivePollReportsChanges(t *testing.T) {
	p := brother()
	q := &fakeQuery{}
	q.set(p.DeviceURI, printer.Live{Reachable: true, Markers: liveInks()})
	s := liveServer(&fakeCUPS{printers: []cups.Printer{p}}, q)
	s.poll(context.Background(), true)
	if !s.livePoll(context.Background()) {
		t.Error("first answer is a change")
	}
	if s.livePoll(context.Background()) {
		t.Error("the same answer again is no change")
	}
	q.set(p.DeviceURI, printer.Live{Reachable: false})
	if !s.livePoll(context.Background()) {
		t.Error("going away is a change")
	}
}

func TestLivePollWithoutQueryAsksNobody(t *testing.T) {
	s := serverFor(&fakeCUPS{printers: []cups.Printer{brother()}})
	if s.hub.live != nil {
		t.Error("a server without Query runs a live loop")
	}
	if s.livePoll(context.Background()) {
		t.Error("changed without a Query")
	}
}

func TestLivePollFindsTheQueuesItselfOnTheFirstRound(t *testing.T) {
	// A bare /events client subscribes before any CUPS poll has finished.
	p := brother()
	q := &fakeQuery{}
	q.set(p.DeviceURI, printer.Live{Reachable: true, Markers: liveInks()})
	s := liveServer(&fakeCUPS{printers: []cups.Printer{p}}, q)
	if !s.livePoll(context.Background()) {
		t.Error("no answer although CUPS knows a queue")
	}
}

func TestLiveEventsPushAwayAndBack(t *testing.T) {
	oldPoll, oldLive := pollEvery, liveEvery
	pollEvery, liveEvery = 20*time.Millisecond, 30*time.Millisecond
	defer func() { pollEvery, liveEvery = oldPoll, oldLive }()

	p := brother()
	q := &fakeQuery{}
	q.set(p.DeviceURI, printer.Live{Reachable: true, Markers: liveInks(), At: time.Now()})
	s := liveServer(&fakeCUPS{printers: []cups.Printer{p}}, q)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c := openEvents(t, ts)

	waitFor := func(what, want string, absent bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			name, data := c.event(t)
			if name == "printers" && strings.Contains(data, want) != absent {
				return
			}
		}
		t.Fatalf("no printers fragment %s", what)
	}
	waitFor("with the live inks", "91 %", false)
	q.set(p.DeviceURI, printer.Live{Reachable: false, At: time.Now()})
	waitFor("away", `class="st away"`, false)
	q.set(p.DeviceURI, printer.Live{Reachable: true, Markers: liveInks(), At: time.Now()})
	waitFor("back", `class="st away"`, true)
}

func TestConnectionLabel(t *testing.T) {
	for uri, want := range map[string]string{
		"usb://Brother/MFC-L2710DW?serial=1":             "USB",
		"ipp://localhost:60000/ipp/print":                "USB through ipp-usb",
		"ipp://127.0.0.1:60001/ipp/print":                "USB through ipp-usb",
		"ipp://localhost:65535/ipp/print":                "USB through ipp-usb",
		"ipp://localhost:8631/ipp/print":                 "network",
		"ipp://localhost:59999/ipp/print":                "network",
		"ipp://localhost/ipp/print":                      "network",
		"ipp://localhost:631/printers/x":                 "network",
		"ipps://127.0.0.1:8631/ipp/print":                "network",
		"dnssd://Brother%20MFC._ipp._tcp.local/":         "network",
		"dnssd://Brother%20MFC%20(USB)._ipp._tcp.local/": "USB through ipp-usb",
		"ipp://printer.local/ipp/print":                  "network",
		"socket://10.0.0.5:9100":                         "network",
		"implicitclass://Foo/":                           "implicitclass",
		"":                                               "",
		"ipp://localhost:notaport/":                      "network",
		"ipp://localhost.example.com:60000/ipp/print":    "network",
	} {
		if got := connection(uri); got != want {
			t.Errorf("connection(%q) = %q, want %q", uri, got, want)
		}
	}
}

func TestQueryable(t *testing.T) {
	for uri, want := range map[string]bool{
		"ipp://a/b": true, "ipps://a/b": true, "dnssd://x._ipp._tcp.local/": true,
		"usb://a": false, "socket://a:9100": false, "lpd://a/q": false, "implicitclass://a": false,
		"file:///x": false, "http://a/": false, "": false, "ipp": false,
	} {
		if got := queryable(uri); got != want {
			t.Errorf("queryable(%q) = %v, want %v", uri, got, want)
		}
	}
}
