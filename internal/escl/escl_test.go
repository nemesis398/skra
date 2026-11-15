// SPDX-License-Identifier: AGPL-3.0-or-later

package escl

import (
	"context"
	"errors"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/escl/fakescanner"
)

// No test in this file ever opens a TCP connection to port 631. The tests
// that point the client at it swap the transport for one whose dials are
// recorded and never reach this machine's CUPS.

var errStopped = errors.New("test: stopped before connect()")

// dials records the addresses the client's transport dialed.
type dials struct {
	mu  sync.Mutex
	got []string
}

func (d *dials) add(addr string) {
	d.mu.Lock()
	d.got = append(d.got, addr)
	d.mu.Unlock()
}

func (d *dials) list() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.got)
}

// swapTransport makes every client the test builds use the transport that
// wrap makes from the one New uses in production, and records each address
// dialed.
func swapTransport(t *testing.T, wrap func(orig func(context.Context, string, string) (net.Conn, error), d *dials) func(context.Context, string, string) (net.Conn, error)) *dials {
	t.Helper()
	d := &dials{}
	old := newTransport
	newTransport = func(skipVerify bool) *http.Transport {
		tr := old(skipVerify)
		tr.DialContext = wrap(tr.DialContext, d)
		return tr
	}
	t.Cleanup(func() { newTransport = old })
	return d
}

func fake(t *testing.T, o fakescanner.Options) (*fakescanner.Scanner, *Client) {
	t.Helper()
	s := fakescanner.New(o)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL + "/eSCL")
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestParseTheBrother(t *testing.T) {
	doc, err := os.ReadFile("testdata/brother-capabilities.xml")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/eSCL/ScannerCapabilities" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		w.Write(doc)
	}))
	defer srv.Close()
	c, err := New(srv.URL + "/eSCL")
	if err != nil {
		t.Fatal(err)
	}
	caps, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Model != "Brother MFC-J6975DW" {
		t.Errorf("Model = %q", caps.Model)
	}
	if want := []Source{{Name: "Platen"}, {Name: "Feeder", Duplex: true}}; !slices.Equal(caps.Sources, want) {
		t.Errorf("Sources = %+v, want %+v", caps.Sources, want)
	}
	if want := []string{"RGB24", "Grayscale8", "BlackAndWhite1"}; !slices.Equal(caps.Colors, want) {
		t.Errorf("Colors = %q, want %q", caps.Colors, want)
	}
	if want := []int{100, 200, 300, 600}; !slices.Equal(caps.Resolutions, want) {
		t.Errorf("Resolutions = %v, want %v", caps.Resolutions, want)
	}
	if want := []string{"application/pdf", "image/jpeg"}; !slices.Equal(caps.Formats, want) {
		t.Errorf("Formats = %q, want %q", caps.Formats, want)
	}
	if caps.MaxWidth != 3507 || caps.MaxHeight != 5100 {
		t.Errorf("MaxWidth, MaxHeight = %d, %d, want 3507, 5100", caps.MaxWidth, caps.MaxHeight)
	}
}

func TestScanThreeFromTheFeeder(t *testing.T) {
	_, c := fake(t, fakescanner.Options{AdfPages: 3})
	ctx := context.Background()
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "Idle" || st.Adf != "ScannerAdfLoaded" {
		t.Errorf("Status = %+v", st)
	}
	job, err := c.Start(ctx, Settings{Source: "Feeder", Color: "Grayscale8", Resolution: 100, Width: 2480, Height: 3508})
	if err != nil {
		t.Fatal(err)
	}
	if job != "/eSCL/ScanJobs/1" {
		t.Errorf("job = %q, want the Location path", job)
	}
	for i := range 3 {
		page, err := c.NextPage(ctx, job)
		if err != nil {
			t.Fatalf("page %d: %v", i+1, err)
		}
		if _, err := jpeg.DecodeConfig(page); err != nil {
			t.Errorf("page %d is not a JPEG: %v", i+1, err)
		}
		page.Close()
	}
	if page, err := c.NextPage(ctx, job); err != io.EOF {
		if page != nil {
			page.Close()
		}
		t.Fatalf("after three pages: %v, want io.EOF", err)
	}
	if err := c.Cancel(ctx, job); err != nil {
		t.Errorf("Cancel: %v", err)
	}
}

func TestStartSendsTheSettings(t *testing.T) {
	s, c := fake(t, fakescanner.Options{})
	if _, err := c.Start(context.Background(), Settings{
		Source: "Feeder", Duplex: true, Color: "Grayscale8", Resolution: 300, Width: 2480, Height: 3508,
	}); err != nil {
		t.Fatal(err)
	}
	got := s.LastSettings()
	got.ContentRegionUnits = strings.TrimPrefix(got.ContentRegionUnits, "escl:")
	want := fakescanner.Settings{
		InputSource: "Feeder", Duplex: true, ColorMode: "Grayscale8", XResolution: 300, YResolution: 300,
		DocumentFormatExt: "image/jpeg", Width: 2480, Height: 3508, ContentRegionUnits: "ThreeHundredthsOfInches",
	}
	if got != want {
		t.Errorf("the scanner got %+v, want %+v", got, want)
	}
}

func TestErrorsSayWhatToDo(t *testing.T) {
	ctx := context.Background()
	feeder := Settings{Source: "Feeder", Color: "Grayscale8", Resolution: 100, Width: 2480, Height: 3508}
	check := func(name string, err, want error, text string) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Errorf("%s: error %v, want %v", name, err, want)
		}
		if want.Error() != text {
			t.Errorf("%s: the error reads %q, want %q", name, want.Error(), text)
		}
	}

	s, c := fake(t, fakescanner.Options{})
	s.SetState("empty", false)
	_, err := c.Start(ctx, feeder)
	check("empty feeder", err, ErrFeederEmpty, "The feeder is empty. Put the pages in and scan more.")

	s.SetState("loaded", true)
	_, err = c.Start(ctx, feeder)
	check("busy", err, ErrBusy, "The scanner is busy. Try again in a moment.")

	s.SetState("loaded", false)
	job, err := c.Start(ctx, feeder)
	if err != nil {
		t.Fatal(err)
	}
	s.SetState("jam", false)
	page, err := c.NextPage(ctx, job)
	if page != nil {
		page.Close()
	}
	check("jam", err, ErrJam, "The paper jammed in the feeder. Clear it and scan the rest.")

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	away, err := New("http://" + addr + "/eSCL")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	_, err = away.Capabilities(ctx)
	if d := time.Since(t0); d > 5*time.Second {
		t.Errorf("a closed port took %v to give up", d)
	}
	check("closed port", err, ErrAway, "The scanner does not answer. It may be off or on another network.")
}

func TestNewRefusesOddAddresses(t *testing.T) {
	for _, base := range []string{"ftp://x/eSCL", "http://u:p@x/eSCL", "http:///eSCL", "http://u@x/eSCL", "x/eSCL", "://x"} {
		if _, err := New(base); err == nil {
			t.Errorf("New(%q) took it", base)
		}
	}
	for _, base := range []string{"http://scanner.local:80/eSCL", "https://192.0.2.7/eSCL", "http://[2001:db8::7]:8080/eSCL/"} {
		if _, err := New(base); err != nil {
			t.Errorf("New(%q): %v", base, err)
		}
	}
}

func TestNoRedirects(t *testing.T) {
	d := swapTransport(t, func(orig func(context.Context, string, string) (net.Conn, error), d *dials) func(context.Context, string, string) (net.Conn, error) {
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			d.add(addr)
			if strings.HasSuffix(addr, ":631") {
				return nil, errStopped
			}
			return orig(ctx, network, addr)
		}
	})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://127.0.0.1:631/eSCL/ScannerCapabilities", http.StatusFound)
	}))
	defer srv.Close()
	c, err := New(srv.URL + "/eSCL")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Capabilities(context.Background()); err == nil {
		t.Error("a redirect counted as capabilities")
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("the scanner got %d requests, want 1", n)
	}
	if got, want := d.list(), []string{srv.Listener.Addr().String()}; !slices.Equal(got, want) {
		t.Errorf("dialed %q, want only %q", got, want)
	}
}

// TestNeverDialsOwnCUPS runs the real guard of the real transport, but every
// dial goes out as UDP. The guard checks the address before connect() whatever
// the network is, and a UDP connect() sends nothing, so if the guard ever let
// 127.0.0.1:631 through, nothing would reach CUPS and the test would see
// errStopped instead of the guard's refusal.
func TestNeverDialsOwnCUPS(t *testing.T) {
	d := swapTransport(t, func(orig func(context.Context, string, string) (net.Conn, error), d *dials) func(context.Context, string, string) (net.Conn, error) {
		return func(ctx context.Context, _, addr string) (net.Conn, error) {
			d.add(addr)
			conn, err := orig(ctx, "udp", addr)
			if err != nil {
				return nil, err
			}
			conn.Close()
			return nil, errStopped
		}
	})
	c, err := New("http://127.0.0.1:631/eSCL")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Capabilities(context.Background())
	if err == nil || errors.Is(err, errStopped) || !strings.Contains(err.Error(), "this machine's own CUPS") {
		t.Errorf("Capabilities = %v, want the guard's refusal", err)
	}
	if len(d.list()) == 0 {
		t.Error("nothing was dialed, so the guard was never asked")
	}
}

// locationScanner answers every scan with loc as its Location and serves one
// page for the job /eSCL/ScanJobs/9, wherever loc says that job is.
func locationScanner(t *testing.T, loc string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/eSCL/ScanJobs":
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/eSCL/ScanJobs/9/NextDocument":
			pages.Add(1)
			w.Write([]byte("page"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &pages
}

// A scanner may put its job under another name or address than the one skrá
// knows it by. Only the path of the Location counts, and the job is fetched
// from the scanner's address.
func TestStartTakesTheJobPathFromAnyLocation(t *testing.T) {
	d := swapTransport(t, func(orig func(context.Context, string, string) (net.Conn, error), d *dials) func(context.Context, string, string) (net.Conn, error) {
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			d.add(addr)
			if strings.HasSuffix(addr, ":631") || strings.HasPrefix(addr, "192.0.2.") || strings.HasPrefix(addr, "brother.") {
				return nil, errStopped
			}
			return orig(ctx, network, addr)
		}
	})
	feeder := Settings{Source: "Feeder", Color: "Grayscale8", Resolution: 100, Width: 2480, Height: 3508}
	for _, loc := range []string{
		"http://192.0.2.7/eSCL/ScanJobs/9",
		"https://brother.local.:443/eSCL/ScanJobs/9/",
		"http://127.0.0.1:631/eSCL/ScanJobs/9",
		"/eSCL/ScanJobs/9",
		"ScanJobs/9",
	} {
		srv, pages := locationScanner(t, loc)
		c, err := New(srv.URL + "/eSCL")
		if err != nil {
			t.Fatal(err)
		}
		job, err := c.Start(context.Background(), feeder)
		if err != nil {
			t.Errorf("Location %q: %v", loc, err)
			continue
		}
		if job != "/eSCL/ScanJobs/9" {
			t.Errorf("Location %q: job %q, want /eSCL/ScanJobs/9", loc, job)
		}
		page, err := c.NextPage(context.Background(), job)
		if err != nil {
			t.Errorf("Location %q: NextPage: %v", loc, err)
			continue
		}
		page.Close()
		if n := pages.Load(); n != 1 {
			t.Errorf("Location %q: the scanner served %d pages, want 1", loc, n)
		}
	}
	for _, addr := range d.list() {
		if !strings.HasPrefix(addr, "127.0.0.1:") || strings.HasSuffix(addr, ":631") {
			t.Errorf("dialed %q, want only the scanner's address", addr)
		}
	}
}

func TestStartRefusesAJobOutsideScanJobs(t *testing.T) {
	feeder := Settings{Source: "Feeder", Color: "Grayscale8", Resolution: 100, Width: 2480, Height: 3508}
	for _, loc := range []string{
		"/eSCL/ScannerStatus",
		"http://192.0.2.7/admin/9",
		"/eSCL/ScanJobs/",
		"/eSCL/ScanJobsX/9",
		"/ScanJobs/9",
		"/eSCL/ScanJobs/%2e%2e/ScannerStatus",
	} {
		srv, _ := locationScanner(t, loc)
		c, err := New(srv.URL + "/eSCL")
		if err != nil {
			t.Fatal(err)
		}
		if job, err := c.Start(context.Background(), feeder); err == nil {
			t.Errorf("Location %q taken as job %q", loc, job)
		}
	}
	_, c := fake(t, fakescanner.Options{})
	for _, job := range []string{"/eSCL/ScannerStatus", "http://192.0.2.7/eSCL/ScanJobs/1", "/eSCL/ScanJobs/1/../../ScannerStatus"} {
		if page, err := c.NextPage(context.Background(), job); err == nil {
			page.Close()
			t.Errorf("NextPage took job %q", job)
		}
		if err := c.Cancel(context.Background(), job); err == nil {
			t.Errorf("Cancel took job %q", job)
		}
	}
}
