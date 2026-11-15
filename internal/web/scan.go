// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/nemesis398/skra/internal/escl"
	"github.com/nemesis398/skra/internal/scan"
)

const (
	// findTimeout bounds a listing of the scanners: the configured ones are
	// there at once, and avahi answers on a LAN well within it.
	findTimeout = 4 * time.Second
	// cancelTimeout bounds the cancel a job sends the scanner when it ends
	// early, and stopWait how long a stop waits for the job to end.
	cancelTimeout = 5 * time.Second
	stopWait      = 10 * time.Second
)

// scanLimit is how long one scan job may run. A variable so a test can lower it.
var scanLimit = 10 * time.Minute

const (
	scanRunningText   = "A scan is running. Wait for it or stop it."
	unknownScanner    = "That scanner is not in the list. Reload the page and pick it again."
	noScansText       = "Scanning is not set up here: skrá has no place to keep scans."
	scanGoneText      = "That scan is gone. Reload the page to start a new one."
	scanStartingText  = "Starting the scan…"
	scanNoPagesText   = "The scanner sent no pages."
	scanTooLongFormat = "The scan ran for %g minutes, the longest one may run, and was stopped. Scan more for the rest."
)

// The color modes the form offers, richest first, and their words.
var (
	scanColors     = []string{"RGB24", "Grayscale8", "BlackAndWhite1"}
	scanColorWords = map[string]string{"RGB24": "color", "Grayscale8": "gray", "BlackAndWhite1": "black and white"}
)

// The resolutions the form offers, in dots per inch.
const minDPI, maxDPI = 100, 600

// paperSize is a size the form offers, in three-hundredths of an inch rounded
// down, as eSCL counts. A large size shows only for a scanner that takes it
// whole; the others are cut to the scanner's largest when it is smaller.
type paperSize struct {
	Name  string
	W, H  int
	Large bool
}

var scanSizes = []paperSize{
	{Name: "A4", W: 2480, H: 3507},
	{Name: "Letter", W: 2550, H: 3300},
	{Name: "A3", W: 3507, H: 4960, Large: true},
}

func (p paperSize) offered(c escl.Caps) bool {
	return !p.Large || (c.MaxWidth >= p.W && c.MaxHeight >= p.H)
}

// A scan error that the person at the scanner can act on shows as its own
// sentence, never with the Go error behind it, and in its color: "need" for
// Peach, "away" for Overlay0 and "bad" for Red.
var scanErrors = []struct {
	err   error
	class string
}{
	{escl.ErrFeederEmpty, "need"},
	{escl.ErrJam, "need"},
	{escl.ErrBusy, "need"},
	{escl.ErrAway, "away"},
	{scan.ErrTooManyPages, "bad"},
	{scan.ErrNotJPEG, "bad"},
	{scan.ErrPageTooBig, "bad"},
}

// scanErrText is what the page says for err, and its class.
func scanErrText(err error) (msg, class string) {
	for _, e := range scanErrors {
		if errors.Is(err, e.err) {
			return e.err.Error(), e.class
		}
	}
	return "The scan failed: " + err.Error(), "bad"
}

// scanJob is the last scan of one session. Only one runs per session; the
// fields below done change with s.scanMu held.
type scanJob struct {
	session *scan.Session
	scanner escl.Scanner
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{} // closed when the job has ended

	running    bool
	msg, class string
}

func (s *Server) scanRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /scan/scanners", s.scannerList)
	mux.HandleFunc("GET /scan/form", s.scanForm)
	mux.HandleFunc("POST /scan/start", s.startScan)
	mux.HandleFunc("POST /scan/{id}/stop", s.stopScan)
}

// listScanners asks for the scanners afresh and remembers them.
func (s *Server) listScanners(ctx context.Context) []escl.Scanner {
	if s.deps.Scanners == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, findTimeout)
	defer cancel()
	list := s.deps.Scanners(ctx)
	s.scanMu.Lock()
	s.scanners = list
	s.scanMu.Unlock()
	return list
}

// knownScanner finds the scanner at u among those last listed, and lists them
// afresh when it is not there. Only a listed scanner is ever spoken to, so no
// request can make skrá connect to an address of its choosing.
func (s *Server) knownScanner(ctx context.Context, u string) (escl.Scanner, bool) {
	find := func(list []escl.Scanner) (escl.Scanner, bool) {
		i := slices.IndexFunc(list, func(sc escl.Scanner) bool { return sc.URL == u })
		if i < 0 {
			return escl.Scanner{}, false
		}
		return list[i], true
	}
	if u == "" {
		return escl.Scanner{}, false
	}
	s.scanMu.Lock()
	sc, ok := find(s.scanners)
	s.scanMu.Unlock()
	if ok {
		return sc, true
	}
	return find(s.listScanners(ctx))
}

func (s *Server) scannerClient(u string) (ScannerClient, error) {
	if s.deps.Scanner != nil {
		return s.deps.Scanner(u)
	}
	c, err := escl.New(u)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// scannerCaps is what the scanner at u can do: what the form last read, or a
// fresh answer when fresh is set or nothing was read yet.
func (s *Server) scannerCaps(ctx context.Context, c ScannerClient, u string, fresh bool) (escl.Caps, error) {
	if !fresh {
		s.scanMu.Lock()
		caps, ok := s.scanCaps[u]
		s.scanMu.Unlock()
		if ok {
			return caps, nil
		}
	}
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return escl.Caps{}, err
	}
	s.scanMu.Lock()
	s.scanCaps[u] = caps
	s.scanMu.Unlock()
	return caps, nil
}

// --- the fragments ---

func (s *Server) scannerList(w http.ResponseWriter, r *http.Request) {
	s.fragment(w, "scanners", s.listScanners(r.Context()), renderFailedText)
}

type scanFormView struct {
	Model, Away                     string
	Source, Color, Resolution, Size segView
}

// segFirst offers the choices with the first of want on, or the first choice
// when none of them is there. Unlike segOf it keeps a single choice, so the
// page always has one to send.
func segFirst(group string, choices []choice, want ...string) segView {
	on := 0
	for _, w := range want {
		if i := slices.IndexFunc(choices, func(c choice) bool { return c.Value == w }); i >= 0 {
			on = i
			break
		}
	}
	if len(choices) > 0 {
		choices[on].On = true
	}
	return segView{Group: group, Choices: choices}
}

// formFor is the form for a scanner that can do caps. The feeder is on when
// there is one, gray at 300 dpi, and A4.
func formFor(caps escl.Caps) scanFormView {
	var sources, colors, dpis, sizes []choice
	for _, src := range caps.Sources {
		switch src.Name {
		case "Platen":
			sources = append(sources, choice{Value: "Platen", Label: "glass"})
		case "Feeder":
			sources = append(sources, choice{Value: "Feeder", Label: "feeder"})
			if src.Duplex {
				sources = append(sources, choice{Value: "Duplex", Label: "feeder · both sides"})
			}
		}
	}
	for _, m := range scanColors {
		if slices.Contains(caps.Colors, m) {
			colors = append(colors, choice{Value: m, Label: scanColorWords[m]})
		}
	}
	dpiOn := ""
	for _, d := range caps.Resolutions {
		if d >= minDPI && d <= maxDPI {
			v := strconv.Itoa(d)
			dpis = append(dpis, choice{Value: v, Label: v})
			if d <= 300 {
				dpiOn = v // the highest up to 300
			}
		}
	}
	for _, p := range scanSizes {
		if p.offered(caps) {
			sizes = append(sizes, choice{Value: p.Name, Label: p.Name})
		}
	}
	return scanFormView{
		Model:      caps.Model,
		Source:     segFirst("source", sources, "Feeder"),
		Color:      segFirst("color", colors, "Grayscale8"),
		Resolution: segFirst("resolution", dpis, dpiOn),
		Size:       segFirst("size", sizes, "A4"),
	}
}

func (s *Server) scanForm(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	sc, ok := s.knownScanner(ctx, r.URL.Query().Get("scanner"))
	if !ok {
		reply(w, http.StatusBadRequest, unknownScanner)
		return
	}
	c, err := s.scannerClient(sc.URL)
	if err != nil {
		reply(w, http.StatusBadRequest, "skrá cannot speak to "+sc.Name+": "+err.Error())
		return
	}
	caps, err := s.scannerCaps(ctx, c, sc.URL, true)
	if err != nil {
		msg, class := scanErrText(err)
		if class == "away" { // a state of the scanner, not a failure
			s.fragment(w, "scanform", scanFormView{Away: msg}, renderFailedText)
			return
		}
		reply(w, http.StatusBadGateway, sc.Name+" did not say what it can do: "+err.Error())
		return
	}
	s.fragment(w, "scanform", formFor(caps), renderFailedText)
}

// scanJobView is one scan as the event stream draws it.
type scanJobView struct {
	ID, Msg, Class string
	Running        bool
	Pages          []int // page numbers, counted from 1
}

// scanViews lists the scans the service knows, oldest first.
func (s *Server) scanViews() []scanJobView {
	type row struct {
		v       scanJobView
		sess    *scan.Session
		started time.Time
	}
	s.scanMu.Lock()
	rows := make([]row, 0, len(s.scanJobs))
	for id, j := range s.scanJobs {
		rows = append(rows, row{scanJobView{ID: id, Msg: j.msg, Class: j.class, Running: j.running}, j.session, j.started})
	}
	s.scanMu.Unlock()
	slices.SortFunc(rows, func(a, b row) int { return a.started.Compare(b.started) })
	out := make([]scanJobView, len(rows))
	for i, r := range rows {
		for n := range r.sess.PageList() {
			r.v.Pages = append(r.v.Pages, n+1)
		}
		out[i] = r.v
	}
	return out
}

// scanFragment is the event stream's scan fragment: every scan the service
// knows, each with its last message and its pages. The page shows the one it
// started.
func (s *Server) scanFragment() (string, error) {
	var buf bytes.Buffer
	err := s.tpl.ExecuteTemplate(&buf, "scan", s.scanViews())
	return buf.String(), err
}

// scanState is the last message and class of the session's scan, and whether
// it runs; ok is false when the service knows no scan of it.
func (s *Server) scanState(id string) (msg, class string, running, ok bool) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	j := s.scanJobs[id]
	if j == nil {
		return "", "", false, false
	}
	return j.msg, j.class, j.running, true
}

// scanRunning reports whether a scan of the session runs.
func (s *Server) scanRunning(id string) bool {
	_, _, running, _ := s.scanState(id)
	return running
}

// --- starting and stopping ---

// scanRequest is a start form that passed the checks that need no scanner.
type scanRequest struct {
	scanner, source, color string
	duplex                 bool
	resolution             int
	size                   paperSize
}

// parseScanForm reads the start form. A value no form offers is refused
// with the sentence to show, before any scanner is asked.
func parseScanForm(f url.Values) (scanRequest, string) {
	req := scanRequest{scanner: f.Get("scanner"), source: f.Get("source"), color: f.Get("color")}
	if req.source != "Platen" && req.source != "Feeder" {
		return req, "Pick the glass or the feeder."
	}
	switch f.Get("duplex") {
	case "true":
		req.duplex = true
	case "false", "":
	default:
		return req, "Both sides is true or false."
	}
	if req.duplex && req.source != "Feeder" {
		return req, "Only the feeder scans both sides."
	}
	if _, ok := scanColorWords[req.color]; !ok {
		return req, "Pick color, gray or black and white."
	}
	dpi, err := strconv.Atoi(f.Get("resolution"))
	if err != nil || dpi < minDPI || dpi > maxDPI {
		return req, fmt.Sprintf("Pick a resolution from %d to %d dpi.", minDPI, maxDPI)
	}
	req.resolution = dpi
	i := slices.IndexFunc(scanSizes, func(p paperSize) bool { return p.Name == f.Get("size") })
	if i < 0 {
		return req, "Pick A4, Letter or A3."
	}
	req.size = scanSizes[i]
	return req, ""
}

// scanSettingsFor turns the request into the settings of a scan on a scanner that
// can do caps, or refuses it with the sentence to show.
func scanSettingsFor(req scanRequest, caps escl.Caps) (escl.Settings, string) {
	i := slices.IndexFunc(caps.Sources, func(src escl.Source) bool { return src.Name == req.source })
	switch {
	case i < 0 && req.source == "Feeder":
		return escl.Settings{}, "This scanner has no feeder."
	case i < 0:
		return escl.Settings{}, "This scanner has no glass."
	case req.duplex && !caps.Sources[i].Duplex:
		return escl.Settings{}, "This scanner's feeder scans one side only."
	case !slices.Contains(caps.Colors, req.color):
		return escl.Settings{}, "This scanner does not scan in " + scanColorWords[req.color] + "."
	case !slices.Contains(caps.Resolutions, req.resolution):
		return escl.Settings{}, fmt.Sprintf("This scanner does not scan at %d dpi.", req.resolution)
	case !req.size.offered(caps):
		return escl.Settings{}, req.size.Name + " is larger than this scanner takes."
	}
	set := escl.Settings{Source: req.source, Duplex: req.duplex, Color: req.color, Resolution: req.resolution,
		Width: req.size.W, Height: req.size.H}
	if caps.MaxWidth > 0 {
		set.Width = min(set.Width, caps.MaxWidth)
	}
	if caps.MaxHeight > 0 {
		set.Height = min(set.Height, caps.MaxHeight)
	}
	return set, ""
}

// sourceWords says where a scan takes its paper from.
func sourceWords(set escl.Settings) string {
	switch {
	case set.Source == "Platen":
		return "glass"
	case set.Duplex:
		return "feeder · both sides"
	}
	return "feeder"
}

// startScan starts a scan into a new session, or into the one named, and
// answers 202 with the session's ID while the pages come in the background.
func (s *Server) startScan(w http.ResponseWriter, r *http.Request) {
	if s.deps.Scans == nil {
		reply(w, http.StatusServiceUnavailable, noScansText)
		return
	}
	if err := r.ParseForm(); err != nil {
		reply(w, http.StatusBadRequest, "The form could not be read. Reload the page and try again.")
		return
	}
	req, refused := parseScanForm(r.PostForm)
	if refused != "" {
		reply(w, http.StatusBadRequest, refused)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()

	var sess *scan.Session
	if id := r.PostForm.Get("session"); id != "" {
		var err error
		if sess, err = s.deps.Scans.Get(id); err != nil {
			reply(w, http.StatusNotFound, scanGoneText)
			return
		}
		if s.scanRunning(id) {
			reply(w, http.StatusConflict, scanRunningText)
			return
		}
		// A page's size in the PDF comes from the resolution the session
		// was opened with, so the pages of one scan share it.
		if sess.Scanner.URL != req.scanner || sess.Settings.Resolution != req.resolution {
			reply(w, http.StatusBadRequest, fmt.Sprintf("Scan more on %s at %d dpi, as the pages before, or save this scan first.",
				sess.Scanner.Name, sess.Settings.Resolution))
			return
		}
	}
	sc, ok := s.knownScanner(ctx, req.scanner)
	if !ok {
		reply(w, http.StatusBadRequest, unknownScanner)
		return
	}
	c, err := s.scannerClient(sc.URL)
	if err != nil {
		reply(w, http.StatusBadRequest, "skrá cannot speak to "+sc.Name+": "+err.Error())
		return
	}
	caps, err := s.scannerCaps(ctx, c, sc.URL, false)
	if err != nil {
		msg, _ := scanErrText(err)
		reply(w, http.StatusBadGateway, msg)
		return
	}
	set, refused := scanSettingsFor(req, caps)
	if refused != "" {
		reply(w, http.StatusBadRequest, refused)
		return
	}
	if sess == nil {
		if sess, err = s.deps.Scans.New(sc, set); err != nil {
			reply(w, http.StatusInternalServerError, "Could not keep the scan: "+err.Error())
			return
		}
	}

	// The job outlives this request: it ends on stop or after scanLimit.
	jctx, jcancel := context.WithTimeout(context.Background(), scanLimit)
	j := &scanJob{session: sess, scanner: sc, started: time.Now(), cancel: jcancel, done: make(chan struct{}),
		running: true, msg: scanStartingText}
	s.scanMu.Lock()
	if old := s.scanJobs[sess.ID]; old != nil && old.running {
		s.scanMu.Unlock()
		jcancel()
		reply(w, http.StatusConflict, scanRunningText)
		return
	}
	s.scanJobs[sess.ID] = j
	s.scanMu.Unlock()

	s.record(r, "scan.start", sc.Name, fmt.Sprintf("%s, %s, %d dpi, %s · scan %s",
		sourceWords(set), scanColorWords[set.Color], set.Resolution, req.size.Name, sess.ID), nil)
	go s.runScan(jctx, j, c, set, s.holdIdle())
	s.hub.Refresh()
	reply(w, http.StatusAccepted, sess.ID)
}

// scanningText is the message while page n of a job is on its way.
func scanningText(set escl.Settings, n int) string {
	if set.Source == "Platen" {
		return "Scanning the glass…"
	}
	return fmt.Sprintf("Scanning page %d of the feeder…", n)
}

// setScan changes what the job says and tells the open Scan pages.
func (s *Server) setScan(j *scanJob, msg, class string, running bool) {
	s.scanMu.Lock()
	j.msg, j.class, j.running = msg, class, running
	s.scanMu.Unlock()
	s.hub.Refresh()
}

// runScan is the background job: it starts the scan and takes the pages into
// the session one by one, until the scanner has no more, something goes
// wrong, or ctx ends. The pages that came stay in the session whatever ends
// it. release lets go of the idle clock once the job has ended.
func (s *Server) runScan(ctx context.Context, j *scanJob, c ScannerClient, set escl.Settings, release func()) {
	defer release()
	defer j.cancel()
	n := 0
	job, err := c.Start(ctx, set)
	for err == nil {
		s.setScan(j, scanningText(set, n+1), "", true)
		var page io.ReadCloser
		if page, err = c.NextPage(ctx, job); err == io.EOF {
			err = nil
			break
		} else if err != nil {
			break
		}
		_, err = j.session.AddPage(page)
		page.Close()
		if err == nil {
			n++
		}
	}
	// A job that ended early is ended on the scanner too, unless the
	// scanner is away and would only make the stop wait.
	if err != nil && job != "" && !errors.Is(err, escl.ErrAway) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
		_ = c.Cancel(cctx, job) // the scan has ended here either way
		cancel()
	}
	msg, class := scanOutcome(ctx, err, n)
	s.setScan(j, msg, class, false)
	close(j.done)
}

// scanOutcome is the last message of a job that took n pages and ended with
// err, and its class.
func scanOutcome(ctx context.Context, err error, n int) (msg, class string) {
	switch {
	case err == nil && n == 0:
		return scanNoPagesText, "need"
	case err == nil:
		return plural(n, "page", "pages") + " scanned.", ""
	case errors.Is(ctx.Err(), context.Canceled):
		if n == 0 {
			return "Stopped before the first page.", ""
		}
		return "Stopped after " + plural(n, "page", "pages") + ".", ""
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf(scanTooLongFormat, scanLimit.Minutes()), "need"
	}
	return scanErrText(err)
}

// stopScan ends the session's running scan and waits for it to end, so the
// answer says how it ended.
func (s *Server) stopScan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.scanMu.Lock()
	j := s.scanJobs[id]
	running := j != nil && j.running
	s.scanMu.Unlock()
	if !running {
		reply(w, http.StatusConflict, "No scan is running there.")
		return
	}
	j.cancel()
	select {
	case <-j.done:
	case <-time.After(stopWait):
	}
	s.record(r, "scan.stop", j.scanner.Name, "scan "+id, nil)
	msg, _, _, _ := s.scanState(id)
	reply(w, http.StatusOK, msg)
}
