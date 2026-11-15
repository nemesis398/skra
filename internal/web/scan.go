// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// scanLimit is how long one scan job may run: a full feeder of 40 sides at
// 600 dpi fits. A stalled job ends sooner, as each page has 60 seconds, and
// a runaway one at 200 pages. A variable so a test can lower it.
var scanLimit = 60 * time.Minute

// saveScanFiles writes a scan into a folder. A variable so a test can hold a
// save while it runs.
var saveScanFiles = scan.Save

const (
	scanRunningText   = "A scan is running. Wait for it or stop it."
	scanBusyText      = "That scan is being saved or discarded. Wait a moment."
	scanGoneJobText   = "This scan was saved or discarded."
	unknownScanner    = "That scanner is not in the list. Reload the page and pick it again."
	noScansText       = "Scanning is not set up here: skrá has no place to keep scans."
	scanStartingText  = "Starting the scan…"
	scanNoPagesText   = "The scanner sent no pages."
	scanTooLongFormat = "The scan ran for %g minutes, the longest one may run, and was stopped. Scan more for the rest."
	scanGoneText      = "That scan is no longer here: it was saved or discarded. Reload the page."
	scanUnreadable    = "skrá cannot read that scan. Reload the page."
	noPageText        = "There is no such page in this scan. Reload the page."
	noPagesText       = "There are no pages to save."
	noFolderText      = "Scans have no folder to go to. Set scan_folder in the config."
	pickFormatText    = "Pick PDF, PNG or JPEG."
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

// scanReply answers a start or a stop with msg and tells the page, in the
// X-Skra-Class header, which color to show it in: "need", "away" or "bad",
// as the scan messages of the event stream. A scanner that is away is never
// a failure, so its answer must not come out red.
func scanReply(w http.ResponseWriter, status int, msg, class string) {
	w.Header().Set("X-Skra-Class", class)
	reply(w, status, msg)
}

// scanErrText is what the page says for err, and its class. A session
// discarded under a job, which the claims of save and discard should leave
// no room for, still reads as a sentence.
func scanErrText(err error) (msg, class string) {
	if errors.Is(err, scan.ErrGone) {
		return scanGoneJobText, "need"
	}
	for _, e := range scanErrors {
		if errors.Is(err, e.err) {
			return e.err.Error(), e.class
		}
	}
	return "The scan failed: " + err.Error(), "bad"
}

// scanJob is the last scan of one session. Only one runs per session; the
// fields below done change with s.scanMu held. created says the job's start
// made the session; such a session is discarded again when the job took no
// page, and gone says that happened. busy says a save, discard, turn or drop
// holds the session, and no scan may start into it meanwhile.
type scanJob struct {
	session *scan.Session
	scanner escl.Scanner
	started time.Time
	created bool
	cancel  context.CancelFunc
	done    chan struct{} // closed when the job has ended

	running    bool
	gone       bool
	busy       bool
	msg, class string
}

func (s *Server) scanRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /scan/scanners", s.scannerList)
	mux.HandleFunc("GET /scan/form", s.scanForm)
	mux.HandleFunc("POST /scan/start", s.startScan)
	mux.HandleFunc("POST /scan/{id}/stop", s.stopScan)
	mux.HandleFunc("GET /scan/{id}/page/{n}", s.pageImage)
	mux.HandleFunc("POST /scan/{id}/page/{n}/rotate", s.rotatePage)
	mux.HandleFunc("POST /scan/{id}/page/{n}/drop", s.dropPage)
	mux.HandleFunc("GET /scan/{id}/name", s.saveName)
	mux.HandleFunc("POST /scan/{id}/save", s.saveScan)
	mux.HandleFunc("POST /scan/{id}/discard", s.discardScan)
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

// scanJobView is one scan as the event stream draws it. Scanner and DPI are
// the eSCL root and the resolution the session's pages share, which the page
// holds its form to while the scan has pages. Gone marks a scan whose
// session was discarded because it took no page.
type scanJobView struct {
	ID, Msg, Class, Scanner string
	DPI                     int
	Running, Gone           bool
	Pages                   []pageView
}

// pageView is one page of the strip: its number, counted from 1, its turn
// clockwise in degrees, and its file, which tells the picture's URL apart
// from that of the page that had the number before a drop.
type pageView struct {
	N, Rotate int
	File      string
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
		rows = append(rows, row{scanJobView{ID: id, Msg: j.msg, Class: j.class, Running: j.running, Gone: j.gone,
			Scanner: j.session.Scanner.URL, DPI: j.session.Settings.Resolution}, j.session, j.started})
	}
	s.scanMu.Unlock()
	slices.SortFunc(rows, func(a, b row) int { return a.started.Compare(b.started) })
	out := make([]scanJobView, len(rows))
	for i, r := range rows {
		for i, p := range r.sess.PageList() {
			r.v.Pages = append(r.v.Pages, pageView{N: i + 1, Rotate: p.Rotate, File: p.File})
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

// scanBusy reports whether a save, discard, turn or drop holds the session.
func (s *Server) scanBusy(id string) bool {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	j := s.scanJobs[id]
	return j != nil && j.busy
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
		scanReply(w, http.StatusServiceUnavailable, noScansText, "bad")
		return
	}
	if err := r.ParseForm(); err != nil {
		scanReply(w, http.StatusBadRequest, "The form could not be read. Reload the page and try again.", "bad")
		return
	}
	req, refused := parseScanForm(r.PostForm)
	if refused != "" {
		scanReply(w, http.StatusBadRequest, refused, "bad")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()

	// A session that is gone, such as one a scan discarded because it took
	// no page, leaves nothing to scan more into: the scan starts a new one.
	var sess *scan.Session
	if id := r.PostForm.Get("session"); id != "" {
		var err error
		sess, err = s.deps.Scans.Get(id)
		switch {
		case errors.Is(err, os.ErrNotExist):
			sess = nil
		case err != nil:
			scanReply(w, http.StatusBadRequest, "That is not a scan. Reload the page and try again.", "bad")
			return
		case s.scanRunning(id):
			scanReply(w, http.StatusConflict, scanRunningText, "need")
			return
		case s.scanBusy(id):
			scanReply(w, http.StatusConflict, scanBusyText, "need")
			return
		}
	}
	if sess != nil && len(sess.PageList()) == 0 {
		// Every page was dropped, so there are no pages before to match.
		// A page takes the resolution the session was opened with, so the
		// scan starts a new session and the empty one goes.
		if err := s.deps.Scans.Discard(sess.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("skra: discarding the empty scan %s: %v", sess.ID, err)
		}
		s.forget(sess.ID)
		sess = nil
	}
	if sess != nil {
		// A page's size in the PDF comes from the resolution the session
		// was opened with, so the pages of one scan share it.
		if sess.Scanner.URL != req.scanner || sess.Settings.Resolution != req.resolution {
			scanReply(w, http.StatusBadRequest, fmt.Sprintf("Scan more on %s at %d dpi, as the pages before, or save this scan first.",
				sess.Scanner.Name, sess.Settings.Resolution), "need")
			return
		}
	}
	sc, ok := s.knownScanner(ctx, req.scanner)
	if !ok {
		scanReply(w, http.StatusBadRequest, unknownScanner, "bad")
		return
	}
	c, err := s.scannerClient(sc.URL)
	if err != nil {
		scanReply(w, http.StatusBadRequest, "skrá cannot speak to "+sc.Name+": "+err.Error(), "bad")
		return
	}
	caps, err := s.scannerCaps(ctx, c, sc.URL, false)
	if err != nil {
		msg, class := scanErrText(err)
		scanReply(w, http.StatusBadGateway, msg, class)
		return
	}
	set, refused := scanSettingsFor(req, caps)
	if refused != "" {
		scanReply(w, http.StatusBadRequest, refused, "bad")
		return
	}
	created := sess == nil
	if created {
		if sess, err = s.deps.Scans.New(sc, set); err != nil {
			scanReply(w, http.StatusInternalServerError, "Could not keep the scan: "+err.Error(), "bad")
			return
		}
	}

	// The job outlives this request: it ends on stop or after scanLimit.
	jctx, jcancel := context.WithTimeout(context.Background(), scanLimit)
	j := &scanJob{session: sess, scanner: sc, started: time.Now(), created: created, cancel: jcancel,
		done: make(chan struct{}), running: true, msg: scanStartingText}
	// The claim of a save or discard and this one exclude each other, so no
	// page comes into a session that a save is about to delete. A session
	// that went in the meantime is checked under the same lock.
	s.scanMu.Lock()
	refuse, msg := 0, ""
	switch old := s.scanJobs[sess.ID]; {
	case old != nil && old.running:
		refuse, msg = http.StatusConflict, scanRunningText
	case old != nil && old.busy:
		refuse, msg = http.StatusConflict, scanBusyText
	case !created:
		if _, err := s.deps.Scans.Get(sess.ID); err != nil {
			refuse, msg = http.StatusNotFound, scanGoneText
		}
	}
	if refuse != 0 {
		s.scanMu.Unlock()
		jcancel()
		scanReply(w, refuse, msg, "need")
		return
	}
	s.scanJobs[sess.ID] = j
	s.scanMu.Unlock()

	s.record(r, "scan.start", sc.Name, fmt.Sprintf("%s, %s, %d dpi, %s · scan %s",
		sourceWords(set), scanColorWords[set.Color], set.Resolution, req.size.Name, sess.ID), nil)
	go s.runScan(jctx, j, c, set, s.holdIdle())
	s.hub.Refresh()
	scanReply(w, http.StatusAccepted, sess.ID, "")
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
	// A scan that took nothing into a session it made leaves no session
	// behind: there is nothing to save, and the next start is free to pick
	// another scanner or resolution.
	gone := j.created && n == 0 && len(j.session.PageList()) == 0 && s.deps.Scans.Discard(j.session.ID) == nil
	msg, class := scanOutcome(ctx, err, n)
	s.scanMu.Lock()
	j.gone = gone
	s.scanMu.Unlock()
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
		scanReply(w, http.StatusConflict, "No scan is running there.", "need")
		return
	}
	j.cancel()
	select {
	case <-j.done:
	case <-time.After(stopWait):
	}
	s.record(r, "scan.stop", j.scanner.Name, "scan "+id, nil)
	msg, class, _, _ := s.scanState(id)
	scanReply(w, http.StatusOK, msg, class)
}

// --- the pages, saving and unsaved scans ---

// The formats a scan saves in, as scan.Save names them, with their labels
// and file extensions.
var (
	saveFormats = []choice{{Value: "pdf", Label: "PDF"}, {Value: "png", Label: "PNG"}, {Value: "jpeg", Label: "JPEG"}}
	saveExts    = map[string]string{"pdf": "pdf", "png": "png", "jpeg": "jpg"}
)

// scanPageView is what the Scan tab draws itself: the folder scans are saved
// to, with ~ for the home, the format buttons, and the scans that were not
// saved, to continue or discard.
type scanPageView struct {
	Folder  string
	Format  segView
	Unsaved []unsavedView
}

// unsavedView is one scan kept from before: when it began and its pages.
type unsavedView struct {
	ID, From, Pages string
	Count           int
}

// scanPage is the Scan tab's own part. It lists the scans that wait to be
// saved and offers each to continue, which it can because each is then in
// the event stream's scan fragment.
func (s *Server) scanPage() *scanPageView {
	v := &scanPageView{
		Folder: homeShort(s.cfg.ScanFolder) + "/",
		Format: segFirst("format", slices.Clone(saveFormats), "pdf"),
	}
	now := s.now()
	for _, sess := range s.keptScans() {
		n := len(sess.PageList())
		v.Unsaved = append(v.Unsaved, unsavedView{ID: sess.ID, From: scanFrom(sess.Created, now),
			Pages: plural(n, "page", "pages"), Count: n})
	}
	return v
}

// keptScans takes the scans with pages that the store keeps into this
// process's scans, so the scan fragment draws them, and returns them oldest
// first. A page open across an idle exit needs them before it has drawn the
// Scan tab again, so a scan event stream takes them too when it connects. A
// scan with no page from an earlier process, as a crash leaves one, goes
// without a word; one that runs or is held is the page's to adopt, and is
// not returned.
func (s *Server) keptScans() []*scan.Session {
	if s.deps.Scans == nil {
		return nil
	}
	list, err := s.deps.Scans.List()
	if err != nil {
		log.Printf("skra: listing the scans: %v", err)
		return nil
	}
	var out []*scan.Session
	for _, sess := range list {
		n := len(sess.PageList())
		s.scanMu.Lock()
		j := s.scanJobs[sess.ID]
		known, held := j != nil, j != nil && (j.running || j.busy)
		s.scanMu.Unlock()
		switch {
		case held:
			continue
		case n == 0:
			if !known && sess.Created.Before(s.born) {
				if err := s.deps.Scans.Discard(sess.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
					log.Printf("skra: discarding the empty scan %s: %v", sess.ID, err)
				}
			}
			continue
		}
		s.adopt(sess)
		out = append(out, sess)
	}
	return out
}

// scanFrom says when a scan began: the time on the day of now, or the day
// and the time before it.
func scanFrom(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Mon 2 Jan 15:04")
}

// homeShort writes a path under the home directory with ~, as the config
// may name it.
func homeShort(p string) string {
	home := os.Getenv("HOME")
	if home == "" || home == "/" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+"/"); ok {
		return "~/" + rest
	}
	return p
}

// adopt puts a session the service did not scan into in this process, such
// as one kept from before, among its scans, so the scan fragment draws it.
func (s *Server) adopt(sess *scan.Session) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	s.jobOf(sess)
}

// jobOf is the session's job, made as an idle one that has ended when the
// service has none. The caller holds s.scanMu.
func (s *Server) jobOf(sess *scan.Session) *scanJob {
	if j := s.scanJobs[sess.ID]; j != nil {
		return j
	}
	done := make(chan struct{})
	close(done)
	j := &scanJob{session: sess, scanner: sess.Scanner, started: sess.Created, cancel: func() {}, done: done}
	s.scanJobs[sess.ID] = j
	return j
}

// forget drops the service's record of a session that was saved or discarded.
func (s *Server) forget(id string) {
	s.scanMu.Lock()
	delete(s.scanJobs, id)
	s.scanMu.Unlock()
}

// claim finds the session a request names and holds it for one save,
// discard, turn or drop until release is called. It refuses a session whose
// scan runs, as a discard during a page on its way could leave a session of
// that one page, and one another request holds; a start refuses a held one
// in turn. When it answers false it has answered the request.
func (s *Server) claim(w http.ResponseWriter, id string) (sess *scan.Session, release func(), ok bool) {
	if sess, ok = s.findScan(w, id); !ok {
		return nil, nil, false
	}
	s.scanMu.Lock()
	j := s.jobOf(sess)
	switch {
	case j.running:
		s.scanMu.Unlock()
		scanReply(w, http.StatusConflict, scanRunningText, "need")
		return nil, nil, false
	case j.busy:
		s.scanMu.Unlock()
		scanReply(w, http.StatusConflict, scanBusyText, "need")
		return nil, nil, false
	}
	j.busy = true
	s.scanMu.Unlock()
	release = func() {
		s.scanMu.Lock()
		j.busy = false
		s.scanMu.Unlock()
	}
	// The one that held it before may have saved or discarded it.
	if _, err := s.deps.Scans.Get(id); err != nil {
		release()
		s.forget(id)
		scanReply(w, http.StatusNotFound, scanGoneText, "need")
		return nil, nil, false
	}
	return sess, release, true
}

// findScan finds the session a request names. When it answers false it has
// answered the request with 404, or 503 when nothing can be scanned.
func (s *Server) findScan(w http.ResponseWriter, id string) (*scan.Session, bool) {
	if s.deps.Scans == nil {
		scanReply(w, http.StatusServiceUnavailable, noScansText, "bad")
		return nil, false
	}
	sess, err := s.deps.Scans.Get(id)
	switch {
	case errors.Is(err, os.ErrNotExist):
		scanReply(w, http.StatusNotFound, scanGoneText, "need")
		return nil, false
	case err != nil:
		log.Printf("skra: reading scan %q: %v", id, err)
		scanReply(w, http.StatusNotFound, scanUnreadable, "bad")
		return nil, false
	}
	return sess, true
}

// pageNumber reads the page number of the request, counted from 1, and
// answers 404 when the session has no such page.
func pageNumber(w http.ResponseWriter, r *http.Request, sess *scan.Session) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || n > len(sess.PageList()) {
		scanReply(w, http.StatusNotFound, noPageText, "need")
		return 0, false
	}
	return n, true
}

// pageImage answers the JPEG of a page, for its thumbnail. A cross-site read
// is refused by the guard, as this is no page another site may open.
func (s *Server) pageImage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.findScan(w, r.PathValue("id"))
	if !ok {
		return
	}
	n, ok := pageNumber(w, r, sess)
	if !ok {
		return
	}
	path, err := sess.PagePath(n)
	var f *os.File
	if err == nil {
		f, err = os.Open(path)
	}
	if err != nil { // dropped or discarded since
		scanReply(w, http.StatusNotFound, noPageText, "need")
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", time.Time{}, f)
}

// editPage turns or drops a page with edit and answers done. A failure the
// page number explains, as when another tab dropped the page first, is a
// 404; anything else is logged and answered with failed.
func (s *Server) editPage(w http.ResponseWriter, r *http.Request, edit func(*scan.Session, int) error, done, failed string) {
	sess, release, ok := s.claim(w, r.PathValue("id"))
	if !ok {
		return
	}
	defer release()
	n, ok := pageNumber(w, r, sess)
	if !ok {
		return
	}
	if err := edit(sess, n); err != nil {
		if errors.Is(err, scan.ErrGone) {
			s.forget(sess.ID)
			scanReply(w, http.StatusNotFound, scanGoneText, "need")
			return
		}
		if n > len(sess.PageList()) {
			scanReply(w, http.StatusNotFound, noPageText, "need")
			return
		}
		log.Printf("skra: scan %s, page %d: %v", sess.ID, n, err)
		scanReply(w, http.StatusInternalServerError, failed, "bad")
		return
	}
	s.hub.Refresh()
	scanReply(w, http.StatusOK, fmt.Sprintf(done, n), "")
}

// rotatePage turns a page a quarter, by 90 (clockwise) or -90.
func (s *Server) rotatePage(w http.ResponseWriter, r *http.Request) {
	by, err := strconv.Atoi(r.PostFormValue("by"))
	if err != nil || (by != 90 && by != -90) {
		scanReply(w, http.StatusBadRequest, "A page turns by 90 or -90 degrees.", "bad")
		return
	}
	s.editPage(w, r, func(sess *scan.Session, n int) error { return sess.Rotate(n, by) },
		"Turned page %d.", "Could not turn the page. Try again.")
}

// dropPage removes a page from the scan and deletes its file.
func (s *Server) dropPage(w http.ResponseWriter, r *http.Request) {
	s.editPage(w, r, (*scan.Session).Drop, "Dropped page %d.", "Could not drop the page. Try again.")
}

// andMore names the first of count files, and how many follow it.
func andMore(first string, count int) string {
	if count < 2 {
		return first
	}
	return fmt.Sprintf("%s and %d more", first, count-1)
}

// saveName answers the path a save would write now, with ~ for the home, so
// the page shows it as the title is typed and before anything is written. It
// is the name scan.Save picks, " (2)" included when the plain one is taken.
func (s *Server) saveName(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.findScan(w, r.PathValue("id"))
	if !ok {
		return
	}
	q := r.URL.Query()
	format := q.Get("format")
	if _, ok := saveExts[format]; !ok {
		scanReply(w, http.StatusBadRequest, pickFormatText, "bad")
		return
	}
	if s.cfg.ScanFolder == "" {
		scanReply(w, http.StatusServiceUnavailable, noFolderText, "bad")
		return
	}
	paths, err := scan.Names(s.cfg.ScanFolder, q.Get("title"), format, len(sess.PageList()), s.now())
	if err != nil {
		scanReply(w, http.StatusBadRequest, noPagesText, "need")
		return
	}
	scanReply(w, http.StatusOK, andMore(homeShort(paths[0]), len(paths)), "")
}

// saveScan writes the scan into the scan folder under its title, and then
// forgets it. A failed save keeps the scan, so it can be saved again.
func (s *Server) saveScan(w http.ResponseWriter, r *http.Request) {
	sess, release, ok := s.claim(w, r.PathValue("id"))
	if !ok {
		return
	}
	defer release()
	format := r.PostFormValue("format")
	if _, ok := saveExts[format]; !ok {
		scanReply(w, http.StatusBadRequest, pickFormatText, "bad")
		return
	}
	folder := s.cfg.ScanFolder
	if folder == "" {
		scanReply(w, http.StatusServiceUnavailable, noFolderText, "bad")
		return
	}
	n := len(sess.PageList())
	if n == 0 {
		scanReply(w, http.StatusBadRequest, noPagesText, "need")
		return
	}
	detail := fmt.Sprintf("%s, %s · scan %s", strings.ToUpper(format), plural(n, "page", "pages"), sess.ID)
	paths, err := saveScanFiles(sess, folder, r.PostFormValue("title"), format, s.now())
	if err != nil {
		log.Printf("skra: saving scan %s in %s: %v", sess.ID, folder, err)
		s.record(r, "scan.save", folder, detail, err)
		scanReply(w, http.StatusInternalServerError, "Could not save the scan in "+filepath.Base(folder)+
			". Check that the folder exists or can be made and that skrá may write there, then save again.", "bad")
		return
	}
	if err := s.deps.Scans.Discard(sess.ID); err != nil {
		log.Printf("skra: forgetting the saved scan %s: %v", sess.ID, err)
	}
	s.forget(sess.ID)
	s.record(r, "scan.save", strings.Join(paths, ", "), detail, nil)
	s.hub.Refresh()
	scanReply(w, http.StatusOK, "Saved "+andMore(`"`+filepath.Base(paths[0])+`"`, len(paths))+
		" in "+filepath.Base(folder)+".", "")
}

// discardScan deletes the scan and its pages.
func (s *Server) discardScan(w http.ResponseWriter, r *http.Request) {
	sess, release, ok := s.claim(w, r.PathValue("id"))
	if !ok {
		return
	}
	defer release()
	n := len(sess.PageList())
	if err := s.deps.Scans.Discard(sess.ID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.forget(sess.ID)
			scanReply(w, http.StatusNotFound, scanGoneText, "need")
			return
		}
		log.Printf("skra: discarding scan %s: %v", sess.ID, err)
		scanReply(w, http.StatusInternalServerError, "Could not discard the scan. Try again.", "bad")
		return
	}
	s.forget(sess.ID)
	s.record(r, "scan.discard", sess.Scanner.Name, plural(n, "page", "pages")+" · scan "+sess.ID, nil)
	s.hub.Refresh()
	scanReply(w, http.StatusOK, "Discarded the scan and its "+plural(n, "page", "pages")+".", "")
}
