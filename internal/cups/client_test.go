// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/ipp"
	"github.com/nemesis398/skra/internal/ipp/ipptest"
)

// fake is a CUPS stand-in on a unix socket. Socket paths are limited to about
// 108 bytes, so the directory is short and lives under os.TempDir.
func fake(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "skra")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(h)
	s.Listener = l
	s.Start()
	t.Cleanup(s.Close)
	return sock
}

func reply(w http.ResponseWriter, m *ipptest.Wire) {
	w.Header().Set("Content-Type", "application/ipp")
	w.Write(m.Bytes())
}

func TestDoSendsPeerCredAndUser(t *testing.T) {
	var gotAuth, gotType, gotPath string
	var gotBody []byte
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotType, gotPath = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		reply(w, ipptest.NewWire(0))
	})
	c := New(sock, "mika")
	req := ipp.NewRequest(ipp.OpGetPrinterAttributes).Op(ipp.URI("printer-uri", "ipp://localhost/"))
	if _, err := c.Do(context.Background(), "/admin/", req, nil); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "PeerCred mika" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotType != "application/ipp" {
		t.Errorf("Content-Type = %q", gotType)
	}
	if gotPath != "/admin/" {
		t.Errorf("path = %q", gotPath)
	}
	dec, err := ipp.Decode(gotBody)
	if err != nil {
		t.Fatal(err)
	}
	if got := dec.Of(ipp.GroupOperation)[0].String("requesting-user-name"); got != "mika" {
		t.Errorf("requesting-user-name = %q", got)
	}
}

// userNames counts the requesting-user-name values in an encoded request.
func userNames(t *testing.T, b []byte) int {
	t.Helper()
	dec, err := ipp.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range dec.Of(ipp.GroupOperation)[0].Attrs {
		if a.Name == "requesting-user-name" {
			n += len(a.Values)
		}
	}
	return n
}

// Do sends the user name once and leaves the caller's request as it was, so
// sending it again does not add a second one.
func TestDoLeavesTheRequestAlone(t *testing.T) {
	var bodies [][]byte
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		reply(w, ipptest.NewWire(0))
	})
	c := New(sock, "mika")
	req := ipp.NewRequest(ipp.OpCUPSGetPrinters)
	for range 2 {
		if _, err := c.Do(context.Background(), "/", req, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i, b := range bodies {
		if n := userNames(t, b); n != 1 {
			t.Errorf("request %d carries %d requesting-user-name values, want 1", i+1, n)
		}
	}
	if n := userNames(t, req.Encode()); n != 0 {
		t.Errorf("the caller's request gained %d requesting-user-name values", n)
	}
	// A request that cannot even be built leaves it alone too.
	if _, err := c.Do(context.Background(), "\x7f bad path", req, nil); err == nil {
		t.Error("a bad path was sent")
	}
	if n := userNames(t, req.Encode()); n != 0 {
		t.Errorf("a failed Do left %d requesting-user-name values in the request", n)
	}
}

func TestDoStreamsTheDocumentAfterTheRequest(t *testing.T) {
	var gotBody []byte
	var gotTE []string
	var gotLen int64
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		gotTE, gotLen = r.TransferEncoding, r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		reply(w, ipptest.NewWire(0))
	})
	c := New(sock, "mika")
	if _, err := c.Do(context.Background(), "/printers/x", ipp.NewRequest(ipp.OpPrintJob), strings.NewReader("%PDF-doc")); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(gotBody), string(rune(ipp.GroupEnd))+"%PDF-doc") {
		t.Errorf("body does not end with the end tag and the document: %q", gotBody)
	}
	// Streamed, not read into memory first to learn its length.
	if !slices.Equal(gotTE, []string{"chunked"}) || gotLen != -1 {
		t.Errorf("Transfer-Encoding = %q, Content-Length = %d, want chunked and -1", gotTE, gotLen)
	}
}

func TestDoWithoutUser(t *testing.T) {
	called := false
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	_, err := New(sock, "").Do(context.Background(), "/", ipp.NewRequest(ipp.OpCUPSGetPrinters), nil)
	if err == nil || !strings.Contains(err.Error(), "no user configured") {
		t.Errorf("err = %v", err)
	}
	if called {
		t.Error("CUPS was contacted without a user")
	}
}

func TestDoMapsHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{401, "CUPS refused the login"},
		{403, "CUPS refused this"},
		{500, "CUPS answered HTTP 500"},
	} {
		sock := fake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) })
		_, err := New(sock, "mika").Do(context.Background(), "/", ipp.NewRequest(ipp.OpCUPSGetPrinters), nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("HTTP %d: err = %v, want %q", tc.status, err, tc.want)
		}
	}
}

// A socket that is missing or refuses the connect means CUPS is not running.
// The error names the cause without the url.Error wrapping around it.
func TestDoSaysCUPSIsNotRunning(t *testing.T) {
	refused := filepath.Join(t.TempDir(), "r.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: refused, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close() // the file stays, nothing listens: connect is refused
	for name, sock := range map[string]string{
		"missing socket": filepath.Join(t.TempDir(), "no-such.sock"),
		"refused":        refused,
	} {
		c := New(sock, "mika")
		_, err := c.Do(context.Background(), "/", ipp.NewRequest(ipp.OpCUPSGetPrinters), nil)
		checkNotRunning(t, name+", Do", err)
		_, err = c.raw(context.Background(), http.MethodGet, "/admin/conf/cupsd.conf", nil, "")
		checkNotRunning(t, name+", raw", err)
	}
}

func checkNotRunning(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, ErrNotRunning) || errors.Is(err, ErrBusy) {
		t.Errorf("%s: err = %v, want ErrNotRunning", name, err)
		return
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "CUPS does not answer: dial unix") || strings.Contains(msg, "http://") {
		t.Errorf("%s: err = %q, want the dial error without the URL", name, msg)
	}
}

// An answer over 32 MiB is reported as too large, not as unreadable.
func TestDoRefusesAnAnswerOver32MiB(t *testing.T) {
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/ipp")
		w.Write(ipptest.NewWire(0).Bytes())
		w.Write(make([]byte, maxResponse))
	})
	_, err := New(sock, "mika").Do(context.Background(), "/", ipp.NewRequest(ipp.OpCUPSGetPrinters), nil)
	if err == nil || !strings.Contains(err.Error(), "over 32 MiB") {
		t.Errorf("err = %v, want an answer over 32 MiB", err)
	}
}

func TestDoReturnsIPPStatusErrors(t *testing.T) {
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, ipptest.NewWire(ipp.StatusNotFound).Str(ipp.TagText, "status-message", "The printer or class does not exist."))
	})
	res, err := New(sock, "mika").Do(context.Background(), "/", ipp.NewRequest(ipp.OpGetPrinterAttributes), nil)
	se, ok := err.(*ipp.StatusError)
	if !ok || se.Status != ipp.StatusNotFound || se.Message != "The printer or class does not exist." {
		t.Errorf("err = %#v", err)
	}
	if res == nil || res.Status != ipp.StatusNotFound {
		t.Errorf("res = %+v, want the decoded response alongside the error", res)
	}
}

func TestPrintersParses(t *testing.T) {
	var asked []string
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, _ := ipp.Decode(body)
		asked = req.Of(ipp.GroupOperation)[0].Strings("requested-attributes")
		reply(w, ipptest.NewWire(0).
			Group(ipp.GroupPrinter).
			Str(ipp.TagName, "printer-name", "Zeta").
			Num(ipp.TagEnum, "printer-state", 3).
			Group(ipp.GroupPrinter).
			Str(ipp.TagName, "printer-name", "Brother-MFC-J6975DW").
			Str(ipp.TagText, "printer-info", "Brother").
			Str(ipp.TagText, "printer-location", "Study").
			Str(ipp.TagText, "printer-make-and-model", "Brother MFC").
			Str(ipp.TagURI, "device-uri", "ipp://192.168.1.5/ipp/print").
			Str(ipp.TagText, "printer-state-message", "Idle").
			Str(ipp.TagKeyword, "printer-state-reasons", "none", "marker-supply-low-warning").
			Num(ipp.TagEnum, "printer-state", 4).
			Num(ipp.TagInteger, "queued-job-count", 2).
			Boolean("printer-is-accepting-jobs", true).
			Boolean("printer-is-shared", false).
			Str(ipp.TagKeyword, "sides-supported", "one-sided", "two-sided-long-edge").
			Str(ipp.TagKeyword, "sides-default", "two-sided-long-edge").
			Str(ipp.TagKeyword, "print-color-mode-supported", "color", "monochrome").
			Str(ipp.TagKeyword, "print-color-mode-default", "color").
			Str(ipp.TagKeyword, "media-supported", "iso_a4_210x297mm", "na_letter_8.5x11in").
			Str(ipp.TagKeyword, "media-default", "iso_a4_210x297mm").
			Num(ipp.TagEnum, "print-quality-supported", 3, 4, 5).
			Num(ipp.TagEnum, "print-quality-default", 4).
			Str(ipp.TagName, "printer-error-policy", "retry-job").
			Str(ipp.TagName, "printer-error-policy-supported", "retry-job", "abort-job").
			Str(ipp.TagKeyword, "printer-commands", "Clean", "PrintSelfTestPage").
			Num(ipp.TagInteger, "marker-levels", 80, 5, -3, 40).
			Str(ipp.TagKeyword, "marker-colors", "#000000", "#00FFFF", "#FF00FF", "#FFFF00").
			Str(ipp.TagKeyword, "marker-types", "toner", "toner", "toner", "toner").
			Num(ipp.TagInteger, "marker-low-levels", 10, 10, 10, 10).
			Num(ipp.TagInteger, "marker-change-time", 1700000000))
	})
	ps, err := New(sock, "mika").Printers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"printer-name", "printer-commands", "marker-levels", "media-supported", "printer-is-shared", "print-quality-default", "sides-default"} {
		found := false
		for _, a := range asked {
			found = found || a == want
		}
		if !found {
			t.Errorf("requested-attributes lacks %q", want)
		}
	}
	if len(ps) != 2 || ps[0].Name != "Brother-MFC-J6975DW" || ps[1].Name != "Zeta" {
		t.Fatalf("not sorted by name: %+v", ps)
	}
	p := ps[0]
	if p.Info != "Brother" || p.Location != "Study" || p.MakeModel != "Brother MFC" || p.DeviceURI != "ipp://192.168.1.5/ipp/print" ||
		p.Message != "Idle" || p.State != 4 || p.QueuedJobs != 2 || !p.Accepting || p.Shared {
		t.Errorf("scalars wrong: %+v", p)
	}
	if strings.Join(p.Reasons, ",") != "none,marker-supply-low-warning" ||
		strings.Join(p.SidesSupported, ",") != "one-sided,two-sided-long-edge" || p.SidesDefault != "two-sided-long-edge" ||
		strings.Join(p.ColorSupported, ",") != "color,monochrome" || p.ColorDefault != "color" ||
		len(p.MediaSupported) != 2 || p.MediaDefault != "iso_a4_210x297mm" ||
		len(p.QualitySupported) != 3 || p.QualityDefault != 4 ||
		p.ErrorPolicy != "retry-job" || len(p.ErrorPolicies) != 2 ||
		strings.Join(p.Commands, ",") != "Clean,PrintSelfTestPage" {
		t.Errorf("lists wrong: %+v", p)
	}
	want := []Marker{
		{Name: "", Color: "#000000", Type: "toner", Level: 80, Low: 10},
		{Name: "", Color: "#00FFFF", Type: "toner", Level: 5, Low: 10},
		{Name: "", Color: "#FF00FF", Type: "toner", Level: -3, Low: 10},
		{Name: "", Color: "#FFFF00", Type: "toner", Level: 40, Low: 10},
	}
	if len(p.Markers) != 4 {
		t.Fatalf("markers = %+v", p.Markers)
	}
	for i := range want {
		if p.Markers[i] != want[i] {
			t.Errorf("marker %d = %+v, want %+v", i, p.Markers[i], want[i])
		}
	}
	if !p.MarkersAt.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("MarkersAt = %v", p.MarkersAt)
	}
	if len(ps[1].Markers) != 0 || !ps[1].MarkersAt.IsZero() {
		t.Errorf("printer without markers: %+v", ps[1])
	}
}

func TestMarkersFromUnevenLists(t *testing.T) {
	g := ipp.AttrGroup{Group: ipp.GroupPrinter, Attrs: []ipp.Attr{
		ipp.Name("marker-names", "Black"),
		ipp.Int("marker-levels", 50, 60, 70),
		ipp.Keyword("marker-colors", "#000000"),
		ipp.Int("marker-low-levels", 10),
	}}
	ms := markers(g)
	want := []Marker{
		{Name: "Black", Color: "#000000", Level: 50, Low: 10},
		{Level: 60, Low: -1},
		{Level: 70, Low: -1},
	}
	if len(ms) != 3 {
		t.Fatalf("markers = %+v", ms)
	}
	for i := range want {
		if ms[i] != want[i] {
			t.Errorf("marker %d = %+v, want %+v", i, ms[i], want[i])
		}
	}
	// names longer than levels: level and low level fall back to -1
	g = ipp.AttrGroup{Attrs: []ipp.Attr{ipp.Name("marker-names", "A", "B")}}
	ms = markers(g)
	if len(ms) != 2 || ms[1].Name != "B" || ms[1].Level != -1 || ms[1].Low != -1 {
		t.Errorf("names only: %+v", ms)
	}
	if markers(ipp.AttrGroup{}) != nil {
		t.Error("no markers should give nil")
	}
}

// A marker-levels value of the wrong length stays in its place, so the levels
// after it stay with their names.
func TestMarkersKeepPositionsPastABadLevel(t *testing.T) {
	g := ipp.AttrGroup{Attrs: []ipp.Attr{
		ipp.Name("marker-names", "Cyan", "Magenta", "Yellow"),
		{Tag: ipp.TagInteger, Name: "marker-levels", Values: []any{int32(50), nil, int32(70)}},
		{Tag: ipp.TagName, Name: "marker-types", Values: []any{nil, "ink", "ink"}},
	}}
	want := []Marker{
		{Name: "Cyan", Level: 50, Low: -1},
		{Name: "Magenta", Type: "ink", Level: -1, Low: -1},
		{Name: "Yellow", Type: "ink", Level: 70, Low: -1},
	}
	if ms := markers(g); !slices.Equal(ms, want) {
		t.Errorf("markers = %+v, want %+v", ms, want)
	}
}

func TestPrinterURIEscapes(t *testing.T) {
	if got, want := PrinterURI("A b/ü"), "ipp://localhost/printers/A%20b%2F%C3%BC"; got != want {
		t.Errorf("PrinterURI = %q, want %q", got, want)
	}
}

func TestPrinterAsksForOneQueue(t *testing.T) {
	var path, uri string
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, _ := ipp.Decode(body)
		path, uri = r.URL.EscapedPath(), req.Of(ipp.GroupOperation)[0].String("printer-uri")
		reply(w, ipptest.NewWire(0).Group(ipp.GroupPrinter).Str(ipp.TagName, "printer-name", "A b"))
	})
	p, err := New(sock, "mika").Printer(context.Background(), "A b")
	if err != nil || p.Name != "A b" {
		t.Fatalf("p = %+v, err = %v", p, err)
	}
	if path != "/printers/A%20b" || uri != "ipp://localhost/printers/A%20b" {
		t.Errorf("path = %q, uri = %q", path, uri)
	}
}

func TestPrinterNotFoundAndEmpty(t *testing.T) {
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) { reply(w, ipptest.NewWire(0)) })
	if _, err := New(sock, "mika").Printer(context.Background(), "x"); err == nil {
		t.Error("an answer without a printer group should be an error")
	}
}

func TestDefault(t *testing.T) {
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, ipptest.NewWire(0).Group(ipp.GroupPrinter).Str(ipp.TagName, "printer-name", "Brother"))
	})
	if got, err := New(sock, "mika").Default(context.Background()); err != nil || got != "Brother" {
		t.Errorf("Default = %q, %v", got, err)
	}
	none := fake(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, ipptest.NewWire(ipp.StatusNotFound).Str(ipp.TagText, "status-message", "No default"))
	})
	if got, err := New(none, "mika").Default(context.Background()); err != nil || got != "" {
		t.Errorf("no default: %q, %v", got, err)
	}
}

func TestJobPrinterName(t *testing.T) {
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, ipptest.NewWire(0).Group(ipp.GroupJob).
			Num(ipp.TagInteger, "job-id", 12).
			Str(ipp.TagName, "job-name", "Karte").
			Num(ipp.TagEnum, "job-state", 9).
			Str(ipp.TagKeyword, "job-state-reasons", "job-completed-successfully").
			Str(ipp.TagURI, "job-printer-uri", "ipp://localhost:631/printers/Brother-MFC-J6975DW").
			Str(ipp.TagText, "job-printer-state-message", "ok").
			Num(ipp.TagInteger, "job-media-sheets-completed", 3).
			Num(ipp.TagInteger, "time-at-creation", 1700000000).
			Num(ipp.TagInteger, "time-at-completed", 1700000060).
			Str(ipp.TagName, "job-originating-user-name", "mika"))
	})
	js, err := New(sock, "mika").Jobs(context.Background(), "completed", 10)
	if err != nil || len(js) != 1 {
		t.Fatalf("js = %+v, err = %v", js, err)
	}
	j := js[0]
	if j.Printer != "Brother-MFC-J6975DW" {
		t.Errorf("Printer = %q", j.Printer)
	}
	if j.ID != 12 || j.State != 9 || j.Sheets != 3 || j.Name != "Karte" || j.User != "mika" || j.Message != "ok" ||
		len(j.Reasons) != 1 || !j.Created.Equal(time.Unix(1700000000, 0)) || !j.Completed.Equal(time.Unix(1700000060, 0)) {
		t.Errorf("job = %+v", j)
	}
}

func TestJobPrinterNameEscapedAndMissing(t *testing.T) {
	for in, want := range map[string]string{
		"ipp://localhost/printers/A%20b%2F%C3%BC": "A b/ü",
		"ipp://localhost/printers/x":              "x",
		"":                                        "",
		"nonsense":                                "nonsense",
	} {
		if got := printerFromURI(in); got != want {
			t.Errorf("printerFromURI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJobsRequestAndOrder(t *testing.T) {
	var op ipp.AttrGroup
	var which string
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, _ := ipp.Decode(body)
		op = req.Of(ipp.GroupOperation)[0]
		which = op.String("which-jobs")
		m := ipptest.NewWire(0)
		if which == "completed" {
			m.Group(ipp.GroupJob).Num(ipp.TagInteger, "job-id", 1).Num(ipp.TagInteger, "time-at-completed", 100).
				Group(ipp.GroupJob).Num(ipp.TagInteger, "job-id", 2).Num(ipp.TagInteger, "time-at-completed", 300).
				Group(ipp.GroupJob).Num(ipp.TagInteger, "job-id", 3)
		} else {
			m.Group(ipp.GroupJob).Num(ipp.TagInteger, "job-id", 7).
				Group(ipp.GroupJob).Num(ipp.TagInteger, "job-id", 5)
		}
		reply(w, m)
	})
	c := New(sock, "mika")
	active, err := c.Jobs(context.Background(), "not-completed", 0)
	if err != nil || len(active) != 2 || active[0].ID != 5 || active[1].ID != 7 {
		t.Fatalf("active = %+v, %v", active, err)
	}
	if _, has := op.Int("limit"); has {
		t.Error("limit 0 should not be sent")
	}
	if op.String("printer-uri") != "ipp://localhost/" {
		t.Errorf("printer-uri = %q", op.String("printer-uri"))
	}
	done, err := c.Jobs(context.Background(), "completed", 200)
	if err != nil || len(done) != 3 || done[0].ID != 2 || done[1].ID != 1 || done[2].ID != 3 {
		t.Fatalf("done = %+v, %v", done, err)
	}
	if n, _ := op.Int("limit"); n != 200 {
		t.Errorf("limit = %d", n)
	}
	if !done[2].Completed.IsZero() {
		t.Error("a missing time-at-completed should be the zero time")
	}
	if _, err := c.Jobs(context.Background(), "all", 0); err == nil {
		t.Error("an unknown which should be an error")
	}
}

func TestDoGivesUpOnACupsThatNeverAnswers(t *testing.T) {
	old := headerTimeout
	headerTimeout = 100 * time.Millisecond
	t.Cleanup(func() { headerTimeout = old })
	release := make(chan struct{})
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-release // read the request, never answer
	})
	// Registered after fake, so it runs before the server's Close waits for the handler.
	t.Cleanup(func() { close(release) })
	// The context is only a backstop; the header timeout has to fire first.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := New(sock, "mika").Do(ctx, "/", ipp.NewRequest(ipp.OpCUPSGetPrinters), nil)
	if !errors.Is(err, ErrBusy) || errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "CUPS does not answer") || strings.Contains(msg, "http://") {
		t.Errorf("err = %q, want the cause without the URL", msg)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Do waited %v for a CUPS that never answers", d)
	}
}

// A deadline on ctx that runs out while CUPS works is ErrBusy too.
func TestDoCountsTheDeadlineOfCtxAsBusy(t *testing.T) {
	release := make(chan struct{})
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-release
	})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := New(sock, "mika").Do(ctx, "/", ipp.NewRequest(ipp.OpCUPSGetPrinters), nil)
	if !errors.Is(err, ErrBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want ErrBusy around the deadline", err)
	}
}

// An idle connection to cupsd is closed after 30 s, so the client does not
// keep Fedora's on-demand cupsd from exiting.
func TestClientClosesIdleConnections(t *testing.T) {
	tr, ok := New("/nonexistent/cups.sock", "mika").http.Transport.(*http.Transport)
	if !ok {
		t.Fatal("the client has no *http.Transport")
	}
	if tr.IdleConnTimeout != 30*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 30s", tr.IdleConnTimeout)
	}
}
