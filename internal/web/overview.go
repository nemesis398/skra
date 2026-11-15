// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/printer"
)

// pollEvery is how often the hub asks CUPS while somebody watches. A variable
// so a test can lower it.
var pollEvery = 2 * time.Second

const (
	// pollTimeout bounds one whole poll. CUPS answers a listing in
	// milliseconds, so a poll that takes longer means CUPS is stuck.
	pollTimeout = 10 * time.Second
	// doneEvery fetches the completed jobs on every fifth poll: they change
	// only when a job ends.
	doneEvery = 5
	doneLimit = 200
	// recentJobs is how many finished jobs the overview shows.
	recentJobs = 5
	maxInks    = 8
)

// Glyphs from the Nerd Font, as in the prototype.
const (
	glyphHeld = "\U000F03E4"
	glyphNeed = "\U000F002A"
)

// poll asks CUPS for everything the overview shows. It never fails: when CUPS
// does not answer, the snapshot keeps the last good data and carries the
// reason in Err, so the page tells the truth instead of going blank. Polls are
// serialized because the hub's poller and a page load can overlap.
func (s *Server) poll(ctx context.Context, withDone bool) Snapshot {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	withDone = withDone || !s.haveDone || s.ticks%doneEvery == 0
	s.ticks++

	snap := s.good
	snap.Err = ""
	snap.Live = s.liveNow()
	fail := func(err error) Snapshot {
		msg := err.Error()
		if !strings.HasPrefix(msg, "CUPS") {
			msg = "CUPS does not answer: " + msg
		}
		snap.Err = msg
		snap.At = time.Now()
		return snap
	}
	c := s.deps.CUPS
	if c == nil {
		return fail(fmt.Errorf("no CUPS client is set up"))
	}
	var err error
	if snap.Printers, err = c.Printers(ctx); err != nil {
		snap.Printers = s.good.Printers
		return fail(err)
	}
	if snap.Default, err = c.Default(ctx); err != nil {
		snap.Default = s.good.Default
		return fail(err)
	}
	if snap.Active, err = c.Jobs(ctx, "not-completed", 0); err != nil {
		snap.Active = s.good.Active
		return fail(err)
	}
	// A job that just ended is in the completed list, so refresh it as soon
	// as the active jobs change instead of waiting for the fifth poll.
	if withDone || !sameJobs(snap.Active, s.good.Active) {
		if snap.Done, err = c.Jobs(ctx, "completed", doneLimit); err != nil {
			snap.Done = s.good.Done
			return fail(err)
		}
		s.haveDone = true
	}
	snap.At = time.Now()
	s.goodMu.Lock()
	s.good = snap
	s.goodMu.Unlock()
	return snap
}

func sameJobs(a, b []cups.Job) bool {
	return slices.EqualFunc(a, b, func(x, y cups.Job) bool { return x.ID == y.ID && x.State == y.State })
}

// tickPoll is the hub's poll function.
func (s *Server) tickPoll(ctx context.Context) Snapshot { return s.poll(ctx, false) }

// page renders the whole overview from a fresh poll, so it works before the
// event stream connects.
func (s *Server) page(w http.ResponseWriter, r *http.Request) { s.render(w, r, "overview") }

// render draws a full page, the overview or the printers tab, with the shared
// tab bar around it.
func (s *Server) render(w http.ResponseWriter, r *http.Request, which string) {
	snap := s.poll(r.Context(), true)
	var buf bytes.Buffer
	data := struct {
		Token, Page string
		V           view
	}{s.token, which, s.view(snap)}
	if err := s.tpl.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		http.Error(w, "Could not render the page.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

type fragment struct{ event, html string }

// fragments renders the pieces of the overview that the event stream swaps.
func (s *Server) fragments(snap Snapshot) []fragment {
	v := s.view(snap)
	var out []fragment
	for _, name := range []string{"printers", "opts", "queue", "queues", "recent", "need", "foot"} {
		var buf bytes.Buffer
		if err := s.tpl.ExecuteTemplate(&buf, name, v); err != nil {
			continue // a template bug must not end the stream
		}
		out = append(out, fragment{name, buf.String()})
	}
	return out
}

// view is what the overview templates draw.
type view struct {
	Printers     []printerView
	PrinterCount string
	NoPrinters   string
	Queue        []jobView
	QueueCount   string
	Queues       []queueView // the table of the printers tab
	Recent       []jobView
	Need         string
	Msg          string
	MsgBad       bool
	Fresh        string
	// Opts is the JSON the print form is drawn from, and DefaultName the
	// queue it starts on.
	Opts        string
	DefaultName string
}

// moveTarget is a queue the move dialog offers.
type moveTarget struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// optPrinter is one queue in the print form: what it can do for the
// choices the form offers.
type optPrinter struct {
	Name         string   `json:"name"`
	Label        string   `json:"label"`
	Sides        []string `json:"sides"`
	Color        []string `json:"color"`
	SidesDefault string   `json:"sidesDefault"`
	ColorDefault string   `json:"colorDefault"`
}

type printerView struct {
	Name, Display, State, Class, Facts, AsOf, Link string
	Default, Away, Paused                          bool
	Inks                                           []inkView
}

type inkView struct {
	Label, Value string
	Pct          int
	Low          bool
}

type jobView struct {
	ID                                       int
	Name, Printer, State, Class, When, Pages string
	// CanHold and CanRelease say which of the two buttons the row offers. A
	// job that is printing carries the confirm texts of the dialog that
	// stands before its cancel.
	CanHold, CanRelease                     bool
	ConfirmTitle, ConfirmText, ConfirmLabel string
	// MoveTo is the JSON of the other queues a waiting or held job can go to,
	// and MoveTitle the heading of the dialog that asks; both are empty when
	// there is nowhere to move it.
	MoveTo, MoveTitle string
}

func (s *Server) view(snap Snapshot) view {
	now := snap.At
	if now.IsZero() {
		now = time.Now()
	}
	v := view{
		PrinterCount: plural(len(snap.Printers), "queue", "queues"),
		QueueCount:   plural(len(snap.Active), "job", "jobs"),
	}
	if snap.Err != "" {
		v.NoPrinters = "No printers to show until CUPS answers."
	} else {
		v.NoPrinters = "CUPS has no printers. Add one in the CUPS tab."
	}

	held, stuck := 0, 0
	for _, p := range snap.Printers {
		pv := printerCell(p, snap.Default, now, snap.Live)
		if pv.Class == "need" {
			stuck++
		}
		v.Printers = append(v.Printers, pv)
	}
	names := shortNames(snap.Printers)
	opts := make([]optPrinter, 0, len(snap.Printers))
	for _, p := range snap.Printers {
		opts = append(opts, optPrinter{Name: p.Name, Label: names[p.Name], Sides: p.SidesSupported, Color: p.ColorSupported,
			SidesDefault: p.SidesDefault, ColorDefault: p.ColorDefault})
	}
	if raw, err := json.Marshal(opts); err == nil {
		v.Opts = string(raw)
	}
	v.DefaultName = snap.Default
	v.Queues = s.queueRows(snap, now)
	for _, j := range snap.Active {
		if j.State == 4 {
			held++
		}
		jv := jobRow(j, names, now, true)
		if jv.MoveTo = moveTargets(j, snap.Printers, names); jv.MoveTo != "" {
			jv.MoveTitle = "Move " + quoted(jv.Name)
		}
		v.Queue = append(v.Queue, jv)
	}
	for i, j := range snap.Done {
		if i == recentJobs {
			break
		}
		v.Recent = append(v.Recent, jobRow(j, names, now, false))
	}

	var need []string
	if held > 0 {
		need = append(need, fmt.Sprintf("%d held", held))
	}
	switch {
	case stuck == 1:
		need = append(need, "1 printer needs you")
	case stuck > 1:
		need = append(need, fmt.Sprintf("%d printers need you", stuck))
	}
	if len(need) > 0 {
		v.Need = glyphNeed + " " + strings.Join(need, " · ")
	}

	if snap.Err != "" {
		v.Msg, v.MsgBad = snap.Err+" · trying again", true
	} else {
		v.Msg = "CUPS answers · acting as " + s.cfg.User
	}
	if !snap.At.IsZero() {
		v.Fresh = fmt.Sprintf("updated %s · every %g s while a page is open", snap.At.Format("15:04:05"), pollEvery.Seconds())
	}
	return v
}

// moveTargets is the JSON of the other queues a waiting or held job can move
// to, or "" when it cannot move.
func moveTargets(j cups.Job, printers []cups.Printer, names map[string]string) string {
	if (j.State != 3 && j.State != 4) || len(printers) < 2 {
		return ""
	}
	return otherQueues(printers, j.Printer, names)
}

// otherQueues is the JSON of the move dialog's choices: every queue but the
// one named, or "" when there is none.
func otherQueues(printers []cups.Printer, except string, names map[string]string) string {
	var targets []moveTarget
	for _, p := range printers {
		if p.Name != except {
			targets = append(targets, moveTarget{Name: p.Name, Label: names[p.Name]})
		}
	}
	if raw, err := json.Marshal(targets); err == nil && len(targets) > 0 {
		return string(raw)
	}
	return ""
}

// shortNames maps each queue name to the short name the overview uses for it:
// the first word of its description, or the queue name when it has none.
func shortNames(printers []cups.Printer) map[string]string {
	names := make(map[string]string, len(printers))
	for _, p := range printers {
		names[p.Name] = p.Name
		if f := strings.Fields(p.Info); len(f) > 0 {
			names[p.Name] = f[0]
		}
	}
	return names
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

var reasonSuffix = regexp.MustCompile(`-(report|warning|error)$`)

// printerState turns the IPP state and reasons into a state word and a class:
// "" for fine, "need" for Peach, "bad" for Red and "away" for Overlay0. An
// unreachable printer is away and never red.
func printerState(p cups.Printer) (word, class string) {
	var reasons, plain []string
	for _, r := range p.Reasons {
		if r == "none" || strings.HasPrefix(r, "cups-") {
			continue
		}
		reasons = append(reasons, r)
		plain = append(plain, reasonSuffix.ReplaceAllString(r, ""))
	}
	has := func(names ...string) bool {
		return slices.ContainsFunc(plain, func(r string) bool { return slices.Contains(names, r) })
	}
	switch {
	case has("offline"):
		return "away", "away"
	case has("media-empty", "media-needed"):
		return "out of paper", "need"
	case has("media-jam"):
		return "paper jam", "need"
	case has("marker-supply-empty"):
		return "ink empty", "need"
	}
	for i, r := range reasons {
		if strings.HasSuffix(r, "-error") {
			return strings.ReplaceAll(plain[i], "-", " "), "bad"
		}
	}
	switch {
	case p.State == 5:
		return "paused", "need"
	case p.State == 4:
		return "printing", ""
	}
	return "ready", ""
}

// printerCell draws one queue. live holds the answers of the printers
// themselves, keyed by queue name; a queue with no entry was not asked, or not
// yet, and shows what CUPS knows.
func printerCell(p cups.Printer, def string, now time.Time, live map[string]printer.Live) printerView {
	l, asked := live[p.Name]
	state := p
	if asked && l.Reachable {
		// The printer just answered, which is fresher than a CUPS offline
		// report that CUPS clears only when it next probes the queue.
		state.Reasons = slices.DeleteFunc(slices.Clone(p.Reasons), func(r string) bool {
			return reasonSuffix.ReplaceAllString(r, "") == "offline"
		})
	}
	word, class := printerState(state)
	if asked && !l.Reachable {
		word, class = "away", "away" // it did not answer: not on this network, which is no fault
	}
	pv := printerView{
		Name:    p.Name,
		Display: p.Info,
		State:   word,
		Class:   class,
		Away:    class == "away",
		Paused:  p.State == 5,
		Default: p.Name == def,
		Link:    "http://localhost:631/printers/" + url.PathEscape(p.Name),
	}
	if pv.Display == "" {
		pv.Display = p.Name
	}
	var facts []string
	for _, f := range []string{p.Location, connection(p.DeviceURI), model(p.MakeModel)} {
		if f != "" {
			facts = append(facts, f)
		}
	}
	pv.Facts = strings.Join(facts, " · ")

	markers := p.Markers
	switch {
	case asked && l.Reachable && len(l.Markers) > 0:
		markers = l.Markers // the printer's own word, so no age to state
	case len(markers) == 0:
		pv.AsOf = "no ink levels reported yet"
	case !p.MarkersAt.IsZero():
		pv.AsOf = "ink as of " + when(p.MarkersAt, now) + ", the last time CUPS spoke to it"
	}
	for i, m := range markers {
		if i == maxInks {
			break
		}
		pv.Inks = append(pv.Inks, inkCell(m, i))
	}
	return pv
}

var driverless = regexp.MustCompile(`(?i)driverless|everywhere`)

func model(makeModel string) string {
	if driverless.MatchString(makeModel) {
		return "driverless"
	}
	return makeModel
}

// connection says how the printer is attached, from its device URI.
func connection(uri string) string {
	if uri == "" {
		return ""
	}
	scheme, _, _ := strings.Cut(uri, ":")
	plain, err := url.PathUnescape(uri)
	if err != nil {
		plain = uri
	}
	switch {
	case scheme == "usb":
		return "USB"
	case ippUSB(uri) || strings.Contains(plain, "(USB)"):
		return "USB through ipp-usb"
	}
	switch scheme {
	case "dnssd", "ipp", "ipps", "socket", "lpd", "http", "https":
		return "network"
	}
	return scheme
}

// ippUSB reports whether uri is one ipp-usb serves: this machine, on a port of
// 60000 or higher. Any other port on localhost is a printer of its own, such
// as the harness's on 8631.
func ippUSB(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || (u.Scheme != "ipp" && u.Scheme != "ipps") {
		return false
	}
	if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port >= 60000
}

var inkName = regexp.MustCompile(`(?i)black|cyan|magenta|yellow`)

// inkCell draws one supply. Every field can be missing: a nameless supply is
// "#n" and a level below 0 is "·" with an empty bar. The bar is always
// neutral, as in the prototype: the marker color is not drawn.
func inkCell(m cups.Marker, i int) inkView {
	iv := inkView{Label: fmt.Sprintf("#%d", i+1), Value: "·"}
	if m.Name != "" {
		if k := inkName.FindString(m.Name); k != "" {
			iv.Label = strings.ToUpper(k[:1])
		} else if utf8.RuneCountInString(m.Name) > 2 {
			iv.Label = string([]rune(m.Name)[:2])
		} else {
			iv.Label = m.Name
		}
	}
	if m.Level >= 0 {
		iv.Pct = min(m.Level, 100)
		iv.Value = fmt.Sprintf("%d %%", m.Level)
		low := m.Low
		if low < 0 {
			low = 10
		}
		iv.Low = m.Level <= low
	}
	return iv
}

var jobWords = map[int]string{3: "waiting", 4: "held", 5: "printing", 6: "stopped", 7: "canceled", 8: "failed", 9: "printed"}

func jobRow(j cups.Job, names map[string]string, now time.Time, active bool) jobView {
	jv := jobView{
		ID:      j.ID,
		Name:    j.Name,
		Printer: names[j.Printer],
		State:   "·",
	}
	if jv.Name == "" {
		jv.Name = fmt.Sprintf("job %d", j.ID)
	}
	if jv.Printer == "" {
		jv.Printer = j.Printer
	}
	if w, ok := jobWords[j.State]; ok {
		jv.State = w
	}
	switch j.State {
	case 3:
		jv.CanHold = true
	case 4:
		jv.State, jv.Class = glyphHeld+" held · waits for you", "held"
		jv.CanRelease = true
	case 5:
		jv.State = fmt.Sprintf("printing · sheet %d", j.Sheets+1)
		jv.ConfirmTitle = "Cancel " + quoted(jv.Name) + "?"
		jv.ConfirmText = fmt.Sprintf("It is printing on the %s. The sheet in the printer may still come out.", jv.Printer)
		jv.ConfirmLabel = fmt.Sprintf("Cancel job %d", j.ID)
	case 7:
		jv.Class = "off"
	case 6, 8:
		jv.Class = "bad"
		if j.Message != "" {
			jv.State += " · " + j.Message
		}
	}
	if active {
		jv.When = when(j.Created, now)
	} else {
		jv.When = when(j.Completed, now)
		jv.Pages = "·"
		if j.Sheets > 0 {
			jv.Pages = fmt.Sprint(j.Sheets)
		}
	}
	return jv
}

// when is the time of day for today and the weekday and date before that.
func when(t, now time.Time) string {
	if t.IsZero() {
		return "·"
	}
	t = t.In(now.Location())
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return t.Format("Mon 02 Jan 15:04")
}
