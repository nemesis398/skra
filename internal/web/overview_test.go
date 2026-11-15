// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/printer"
)

// fakeCUPS is a CUPS whose answers a test sets and changes.
type fakeCUPS struct {
	mu       sync.Mutex
	printers []cups.Printer
	def      string
	active   []cups.Job
	done     []cups.Job
	err      error
	calls    int

	// The actions record what they were asked in acts ("cancel 12"); actErr
	// is what every action then answers. print stands in for Print.
	acts    []string
	actErr  error
	changes []cups.Changes // what Modify was asked, in order
	printed int
	print   func(printer, name, format string, o cups.JobOptions, doc io.Reader) (int, error)

	// Adding printers: what the search finds (and the timeout it was given and
	// the time its context had left), the drivers by device id and maker, and
	// the queues Add was asked for.
	devices    []cups.Device
	devErr     error
	devTimeout time.Duration
	devLeft    time.Duration
	ppdsFn     func(id, make string) ([]cups.PPD, error)
	ppdAsked   [][2]string
	adds       []cups.AddSpec
	addErr     error

	// The log level cupsd.conf has, what reading it answers, the levels
	// SetLogLevel was asked for with the passwords it got, and what it
	// answers.
	logLevel     string
	logErr       error
	setLogs      []string
	setPasswords []string
	setLogErr    error
	logReads     int
}

func (f *fakeCUPS) set(fn func(*fakeCUPS)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeCUPS) Printers(context.Context) ([]cups.Printer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.printers, f.err
}

// callCount is how often CUPS was asked for the printers.
func (f *fakeCUPS) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCUPS) Default(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.def, f.err
}

func (f *fakeCUPS) Jobs(_ context.Context, which string, _ int) ([]cups.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if which == "completed" {
		return f.done, f.err
	}
	return f.active, f.err
}

func serverFor(f *fakeCUPS) *Server {
	return New(config.Config{Listen: "127.0.0.1:7631", User: "mika"}, Deps{CUPS: f}, NewToken())
}

func pageFor(t *testing.T, f *fakeCUPS) string {
	t.Helper()
	rec := get(serverFor(f).Handler(), "/")
	if rec.Code != 200 {
		t.Fatalf("GET /: status %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func brother() cups.Printer {
	return cups.Printer{
		Name: "Virtual-MFC", Info: "Brother MFC-L2710DW", Location: "Study",
		DeviceURI: "ipp://printer.local/ipp/print", MakeModel: "Brother MFC, driverless", State: 3,
		Markers: []cups.Marker{
			{Name: "Black", Color: "#000000", Level: 68, Low: 10},
			{Name: "Cyan", Color: "#00FFFF", Level: 48, Low: 10},
			{Name: "Magenta", Color: "#FF00FF", Level: 6, Low: 10},
			{Name: "Yellow", Color: "#FFFF00", Level: 12, Low: 10},
		},
		MarkersAt: time.Now().Add(-time.Hour),
	}
}

func TestOverviewShowsPrintersQueueAndRecent(t *testing.T) {
	now := time.Now()
	f := &fakeCUPS{
		printers: []cups.Printer{brother()},
		def:      "Virtual-MFC",
		active: []cups.Job{
			{ID: 12, Name: "Invoice.pdf", Printer: "Virtual-MFC", State: 5, Sheets: 1, Created: now},
			{ID: 13, Name: "Held.pdf", Printer: "Virtual-MFC", State: 4, Created: now},
		},
		done: []cups.Job{
			{ID: 9, Name: "Old.pdf", Printer: "Virtual-MFC", State: 9, Sheets: 2, Completed: now.Add(-time.Hour)},
			{ID: 8, Name: "Broken.pdf", Printer: "Virtual-MFC", State: 8, Message: "paper jam", Completed: now.Add(-2 * time.Hour)},
		},
	}
	body := pageFor(t, f)
	for _, want := range []string{
		"Brother MFC-L2710DW", "default", "ready", "Study",
		`<span class="k">B</span>`, `<span class="k">C</span>`, "68 %", "48 %",
		`class="c low"`, // Magenta at 6 % is below its low mark of 10
		"print a file", "drop a PDF here",
		"Invoice.pdf", "printing · sheet 2", "held · waits for you",
		"Old.pdf", "printed", "Broken.pdf", "failed · paper jam",
		"1 held", // the bar says what needs you
		`href="http://localhost:631/printers/Virtual-MFC"`,
		"CUPS answers", "updated ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestOverviewEscapesNames(t *testing.T) {
	f := &fakeCUPS{
		printers: []cups.Printer{brother()},
		active:   []cups.Job{{ID: 1, Name: "<b>Fußball</b>", Printer: "Virtual-MFC", State: 3}},
	}
	body := pageFor(t, f)
	const want = "&lt;b&gt;Fußball&lt;/b&gt;"
	if n := strings.Count(body, want); n != 1 {
		t.Errorf("escaped job name appears %d times, want 1", n)
	}
	if strings.Contains(body, "<b>Fußball") {
		t.Error("job name reached the page unescaped")
	}
}

func TestOverviewEncodesQueueNameInLinks(t *testing.T) {
	p := brother()
	p.Name = "A b/c"
	body := pageFor(t, &fakeCUPS{printers: []cups.Printer{p}})
	if !strings.Contains(body, `href="http://localhost:631/printers/A%20b%2Fc"`) {
		t.Errorf("CUPS link does not encode the queue name:\n%s", body)
	}
}

func TestOverviewCUPSDown(t *testing.T) {
	f := &fakeCUPS{err: errors.New("dial unix /run/cups/cups.sock: no such file or directory")}
	rec := get(serverFor(f).Handler(), "/")
	if rec.Code != 200 {
		t.Fatalf("status %d, want 200 while CUPS is down", rec.Code)
	}
	body := rec.Body.String()
	foot := body[strings.Index(body, `<footer`):]
	if !strings.Contains(foot, "CUPS does not answer") {
		t.Errorf("footer lacks the failure:\n%s", foot)
	}
	if !strings.Contains(foot, `class="msg bad"`) {
		t.Error("the failure is not marked bad")
	}
}

func TestOverviewKeepsLastGoodDataWhileCUPSIsDown(t *testing.T) {
	f := &fakeCUPS{printers: []cups.Printer{brother()}}
	s := serverFor(f)
	if rec := get(s.Handler(), "/"); !strings.Contains(rec.Body.String(), "Brother MFC-L2710DW") {
		t.Fatal("first page lacks the printer")
	}
	f.set(func(f *fakeCUPS) { f.err = errors.New("boom") })
	body := get(s.Handler(), "/").Body.String()
	if !strings.Contains(body, "Brother MFC-L2710DW") || !strings.Contains(body, "CUPS does not answer") {
		t.Error("a failed poll should keep the last printers and say CUPS does not answer")
	}
}

func TestOverviewWithoutACUPSClient(t *testing.T) {
	rec := get(newTestServer().Handler(), "/")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "CUPS does not answer") {
		t.Errorf("status %d, body lacks the CUPS failure", rec.Code)
	}
}

func TestOverviewOddMarkers(t *testing.T) {
	none := brother()
	none.Name, none.Info, none.Markers, none.MarkersAt = "None", "No ink printer", nil, time.Time{}

	unknown := brother()
	unknown.Name, unknown.Info = "Unknown", "Unknown levels"
	unknown.Markers = []cups.Marker{{Level: -1, Low: -1}, {Level: -3, Low: -1}}

	six := brother()
	six.Name, six.Info = "Six", "Six markers"
	six.Markers = nil
	for range 6 {
		six.Markers = append(six.Markers, cups.Marker{Level: 50, Low: -1})
	}

	odd := brother()
	odd.Name, odd.Info = "Odd", "Odd colors"
	odd.Markers = []cups.Marker{{Name: "Black", Color: "not a color", Level: 10, Low: 5}, {Color: "javascript:alert(1)", Level: 20, Low: 5}}

	body := pageFor(t, &fakeCUPS{printers: []cups.Printer{none, unknown, six, odd}})

	if n := strings.Count(body, `<span class="v">·</span>`); n != 2 {
		t.Errorf("levels below 0 render · %d times, want 2", n)
	}
	for _, label := range []string{"#1", "#2", "#3", "#4", "#5", "#6"} {
		if !strings.Contains(body, `<span class="k">`+label+`</span>`) {
			t.Errorf("missing label %s", label)
		}
	}
	if !strings.Contains(body, "no ink levels reported yet") {
		t.Error("a printer without markers should say so")
	}
	if strings.Contains(body, "javascript:") || strings.Contains(body, "not a color") || strings.Contains(body, "ZgotmplZ") {
		t.Error("an odd marker color leaked into the page")
	}
}

func TestOverviewAwayIsNotRed(t *testing.T) {
	p := brother()
	p.Reasons = []string{"offline-report"}
	body := pageFor(t, &fakeCUPS{printers: []cups.Printer{p}})
	if !strings.Contains(body, `class="pr away"`) || !strings.Contains(body, `class="st away"`) {
		t.Error("an unreachable printer should render class away")
	}
	if !strings.Contains(body, "away") {
		t.Error("state word away is missing")
	}
	// Not a bare "bad": the page token is random hex and can spell it.
	if strings.Contains(body, `st bad`) || strings.Contains(body, `pr bad`) {
		t.Error("an unreachable printer must never render class bad")
	}
}

func TestOverviewStateWords(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mod     func(*cups.Printer)
		word    string
		class   string
		needBar bool
	}{
		{"ready", func(p *cups.Printer) {}, "ready", "st", false},
		{"printing", func(p *cups.Printer) { p.State = 4 }, "printing", "st", false},
		{"paused", func(p *cups.Printer) { p.State = 5 }, "paused", "st need", true},
		{"out of paper", func(p *cups.Printer) { p.Reasons = []string{"media-empty-error"} }, "out of paper", "st need", true},
		{"jam", func(p *cups.Printer) { p.Reasons = []string{"media-jam-report"} }, "paper jam", "st need", true},
		{"other error", func(p *cups.Printer) { p.Reasons = []string{"toner-low-warning", "cover-open-error"} }, "cover open", "st bad", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := brother()
			tc.mod(&p)
			body := pageFor(t, &fakeCUPS{printers: []cups.Printer{p}})
			if !strings.Contains(body, `class="`+tc.class+`">`) || !strings.Contains(body, tc.word) {
				t.Errorf("want class %q and word %q in:\n%s", tc.class, tc.word, body)
			}
			if got := strings.Contains(body, "needs you"); got != tc.needBar {
				t.Errorf("bar says needs you = %t, want %t", got, tc.needBar)
			}
		})
	}
}

func TestPollRefreshesFinishedJobsWhenTheQueueChanges(t *testing.T) {
	f := &fakeCUPS{printers: []cups.Printer{brother()}}
	s := serverFor(f)
	ctx := context.Background()
	s.poll(ctx, false) // the first poll fetches the completed jobs too

	// Only the finished list changes: it is not refetched until the fifth poll.
	f.set(func(f *fakeCUPS) { f.done = []cups.Job{{ID: 1, State: 9}} })
	if got := s.poll(ctx, false); len(got.Done) != 0 {
		t.Errorf("completed jobs refetched on an ordinary poll: %+v", got.Done)
	}

	// A job leaves the queue: the completed list is fetched at once.
	f.set(func(f *fakeCUPS) { f.active = []cups.Job{{ID: 2, State: 5}} })
	s.poll(ctx, false)
	f.set(func(f *fakeCUPS) { f.active = nil; f.done = []cups.Job{{ID: 2, State: 9}, {ID: 1, State: 9}} })
	if got := s.poll(ctx, false); len(got.Done) != 2 {
		t.Errorf("a job ended but the completed list was not refreshed: %+v", got.Done)
	}
}

// --- /events ---

type sseClient struct {
	resp   *http.Response
	cancel context.CancelFunc
	r      *bufio.Reader
}

func openEvents(t *testing.T, ts *httptest.Server) *sseClient {
	t.Helper()
	return openEventsAt(t, ts, "/events")
}

// openEventsAt opens the event stream at path, such as /events?page=scan.
func openEventsAt(t *testing.T, ts *httptest.Server, path string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+path, nil)
	req.Host = "localhost:7631"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &sseClient{resp: resp, cancel: cancel, r: bufio.NewReader(resp.Body)}
	t.Cleanup(c.close)
	return c
}

func (c *sseClient) close() {
	c.cancel()
	io.Copy(io.Discard, c.resp.Body)
	c.resp.Body.Close()
}

// event reads one event and returns its name and data lines joined by newline.
func (c *sseClient) event(t *testing.T) (name, data string) {
	t.Helper()
	type result struct{ name, data string }
	ch := make(chan result, 1)
	go func() {
		var r result
		var lines []string
		for {
			line, err := c.r.ReadString('\n')
			if err != nil {
				ch <- result{}
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case line == "":
				r.data = strings.Join(lines, "\n")
				ch <- r
				return
			case strings.HasPrefix(line, "event: "):
				r.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				lines = append(lines, strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	select {
	case r := <-ch:
		if r.name == "" {
			t.Fatal("stream ended")
		}
		return r.name, r.data
	case <-time.After(2 * time.Second):
		t.Fatal("no event within 2 s")
	}
	return "", ""
}

func TestEventsSendsFullSnapshotOnConnect(t *testing.T) {
	f := &fakeCUPS{
		printers: []cups.Printer{brother()},
		active:   []cups.Job{{ID: 1, Name: "Held.pdf", Printer: "Virtual-MFC", State: 4}},
	}
	old := pollEvery
	pollEvery = time.Hour
	defer func() { pollEvery = old }()
	s := serverFor(f)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := openEvents(t, ts)
	if ct := c.resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type %q", ct)
	}
	if cc := c.resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}
	if c.resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Error("the stream lacks the guard's headers")
	}
	got := map[string]string{}
	for range 7 {
		name, data := c.event(t)
		got[name] = data
	}
	for _, name := range []string{"printers", "opts", "queue", "queues", "recent", "need", "foot"} {
		if _, ok := got[name]; !ok {
			t.Errorf("no %q event on connect", name)
		}
	}
	if !strings.Contains(got["printers"], "Brother MFC-L2710DW") || !strings.Contains(got["queue"], "Held.pdf") ||
		!strings.Contains(got["need"], "1 held") || !strings.Contains(got["foot"], "updated ") {
		t.Errorf("fragments are missing content: %+v", got)
	}
	if !strings.Contains(got["printers"], "\n") {
		t.Error("expected a multi-line fragment to test the data framing")
	}

	// A second tab joins the same poller and gets the whole state at once.
	c2 := openEvents(t, ts)
	for range 7 {
		c2.event(t)
	}
	if n := s.hub.Subscribers(); n != 2 {
		t.Errorf("Subscribers() = %d, want 2", n)
	}
}

func TestEventsPushesChangesAndStopsPolling(t *testing.T) {
	f := &fakeCUPS{printers: []cups.Printer{brother()}}
	old := pollEvery
	pollEvery = 10 * time.Millisecond
	defer func() { pollEvery = old }()
	s := serverFor(f)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := openEvents(t, ts)
	for range 7 {
		c.event(t)
	}
	f.set(func(f *fakeCUPS) {
		f.active = []cups.Job{{ID: 7, Name: "Fresh.pdf", Printer: "Virtual-MFC", State: 5}}
	})
	for {
		name, data := c.event(t)
		if name == "queue" && strings.Contains(data, "Fresh.pdf") {
			break
		}
	}

	c.close()
	deadline := time.Now().Add(time.Second)
	for s.hub.Subscribers() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the subscriber did not leave after the client closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	f.mu.Lock()
	before := f.calls
	f.mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	f.mu.Lock()
	after := f.calls
	f.mu.Unlock()
	if before != after {
		t.Errorf("CUPS was polled %d times with nobody watching", after-before)
	}
}

func TestEventsRejectsAForeignHost(t *testing.T) {
	ts := httptest.NewServer(serverFor(&fakeCUPS{}).Handler())
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest("GET", ts.URL+"/events", nil)
	req.Host = "evil.example:7631"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}
}

func TestOverviewInkBarsAreNeutral(t *testing.T) {
	p := brother()
	p.Markers = []cups.Marker{
		{Name: "Green", Color: "#00FF00", Level: 80, Low: 10},
		{Name: "Cyan", Color: "#00FFFF", Level: 50, Low: 10},
	}
	body := pageFor(t, &fakeCUPS{printers: []cups.Printer{p}})
	if strings.Contains(strings.ToLower(body), "00ff") || strings.Contains(body, "background:") {
		t.Errorf("a marker color reached the page; the bars are neutral:\n%s", body)
	}
	if !strings.Contains(body, `style="width:80%"`) {
		t.Error("the bar lost its width")
	}
}

// parseSSE reads frames the way EventSource does: a line ends at \r\n, \r or
// \n, a blank line dispatches the event.
func parseSSE(stream string) (events []fragment) {
	var cur fragment
	var data []string
	norm := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(stream)
	for _, line := range strings.Split(norm, "\n") {
		switch {
		case line == "":
			if cur.event != "" || len(data) > 0 {
				cur.html = strings.Join(data, "\n")
				events = append(events, cur)
			}
			cur, data = fragment{}, nil
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line, "data:"))
		}
	}
	return events
}

func TestWriteEventKeepsOneFrameForCarriageReturns(t *testing.T) {
	for _, name := range []string{"a\r\rfoo", "a\r\nfoo", "a\rfoo", "a\nfoo"} {
		html := "<td>" + name + "</td>\n<td>end</td>"
		rec := httptest.NewRecorder()
		if err := writeEvent(rec, "queue", html); err != nil {
			t.Fatal(err)
		}
		evs := parseSSE(rec.Body.String())
		if len(evs) != 1 || evs[0].event != "queue" {
			t.Fatalf("%q: got %d events %+v, want one queue event", name, len(evs), evs)
		}
		want := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(html)
		if evs[0].html != want {
			t.Errorf("%q: data %q, want %q", name, evs[0].html, want)
		}
		if !strings.Contains(evs[0].html, "<td>end</td>") {
			t.Errorf("%q: the fragment was cut short: %q", name, evs[0].html)
		}
	}
}

func TestEventsSurviveCarriageReturnInJobName(t *testing.T) {
	f := &fakeCUPS{
		printers: []cups.Printer{brother()},
		active:   []cups.Job{{ID: 1, Name: "a\r\rfoo\r\nbar", Printer: "Virtual-MFC", State: 3}},
	}
	ts := httptest.NewServer(serverFor(f).Handler())
	t.Cleanup(ts.Close)
	c := openEvents(t, ts)
	// The reader splits on \n only, so read raw bytes until all six events are in.
	var raw strings.Builder
	deadline := time.Now().Add(2 * time.Second)
	for len(parseSSE(raw.String())) < 6 {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than six events:\n%q", raw.String())
		}
		b, err := c.r.ReadByte()
		if err != nil {
			t.Fatal(err)
		}
		raw.WriteByte(b)
	}
	evs := parseSSE(raw.String())
	if len(evs) != 6 {
		t.Fatalf("got %d events, want 6: %+v", len(evs), evs)
	}
	for _, e := range evs {
		if e.event == "queue" && (!strings.Contains(e.html, "foo") || !strings.Contains(e.html, "bar") || !strings.HasSuffix(strings.TrimSpace(e.html), "</div>")) {
			t.Errorf("queue fragment cut short: %q", e.html)
		}
	}
}

// msgLine finds the message line of a printer card: its class and text.
var msgLine = regexp.MustCompile(`<div class="why( [a-z]+)?" title="[^"]*">([^<]*)</div>`)

// A queue that CUPS stopped, or that has a reason ending in -error or
// -warning, says why under its state: the printer's own message first, else
// CUPS's, in the color of the state.
func TestOverviewShowsThePrinterMessage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mod       func(*cups.Printer)
		live      *printer.Live
		class     string // "" for no line
		text      string
		stateWord string
	}{
		{"stopped after a failed job", func(p *cups.Printer) {
			p.State, p.Message = 5, "Unable to send data to printer."
		}, nil, " need", "Unable to send data to printer.", "paused"},
		{"an error", func(p *cups.Printer) {
			p.Reasons, p.Message = []string{"cover-open-error"}, "Close the front cover."
		}, nil, " bad", "Close the front cover.", "cover open"},
		{"a warning on a ready printer", func(p *cups.Printer) {
			p.Reasons, p.Message = []string{"toner-low-warning"}, "Toner is low."
		}, nil, " need", "Toner is low.", "ready"},
		{"the printer's own message first", func(p *cups.Printer) {
			p.State, p.Message = 5, "CUPS says this."
		}, &printer.Live{Reachable: true, Message: "The printer says this.", At: time.Now()}, " need", "The printer says this.", "paused"},
		{"a live warning", func(p *cups.Printer) {}, &printer.Live{Reachable: true, Reasons: []string{"media-low-warning"}, Message: "Paper is low.", At: time.Now()},
			" need", "Paper is low.", "ready"},
		{"escaped and on one line", func(p *cups.Printer) {
			p.State, p.Message = 5, "<b>bold</b>\nsecond   line"
		}, nil, " need", "&lt;b&gt;bold&lt;/b&gt; second line", "paused"},
		{"away stays Overlay0", func(p *cups.Printer) {
			p.State, p.Reasons, p.Message = 5, []string{"other-error"}, "Unable to locate printer."
		}, &printer.Live{Reachable: false, At: time.Now()}, " away", "Unable to locate printer.", "away"},
		{"an empty message adds no line", func(p *cups.Printer) { p.State = 5 }, nil, "", "", "paused"},
		{"a message that repeats the state adds no line", func(p *cups.Printer) { p.State, p.Message = 5, "Paused" }, nil, "", "", "paused"},
		{"a ready printer adds no line", func(p *cups.Printer) { p.Message = "Ready to print." }, nil, "", "", "ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := brother()
			tc.mod(&p)
			f := &fakeCUPS{printers: []cups.Printer{p}}
			var body string
			if tc.live == nil {
				body = pageFor(t, f) // no printer is asked: what CUPS knows
			} else {
				q := &fakeQuery{}
				q.set(p.DeviceURI, *tc.live)
				body = pageWithLive(t, liveServer(f, q))
			}
			m := msgLine.FindAllStringSubmatch(body, -1)
			if tc.class == "" {
				if len(m) != 0 || strings.Contains(body, `class="why`) {
					t.Errorf("want no message line, got %q", m)
				}
				return
			}
			if len(m) != 1 || m[0][1] != tc.class || m[0][2] != tc.text {
				t.Errorf("message line = %q, want class %q text %q", m, tc.class, tc.text)
			}
			if !strings.Contains(body, tc.stateWord) {
				t.Errorf("state word %q is missing", tc.stateWord)
			}
			if tc.class == " away" && (strings.Contains(body, `why bad`) || strings.Contains(body, `st bad`)) {
				t.Error("an away printer's message must not be red")
			}
		})
	}
}
