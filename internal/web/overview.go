// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nemesis398/skra/internal/cups"
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
	s.good = snap
	return snap
}

func sameJobs(a, b []cups.Job) bool {
	return slices.EqualFunc(a, b, func(x, y cups.Job) bool { return x.ID == y.ID && x.State == y.State })
}

// tickPoll is the hub's poll function.
func (s *Server) tickPoll(ctx context.Context) Snapshot { return s.poll(ctx, false) }

// page renders the whole overview from a fresh poll, so it works before the
// event stream connects.
func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	snap := s.poll(r.Context(), true)
	var buf bytes.Buffer
	data := struct {
		Token string
		V     view
	}{s.token, s.view(snap)}
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
	for _, name := range []string{"printers", "queue", "recent", "need", "foot"} {
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
	Recent       []jobView
	Need         string
	Msg          string
	MsgBad       bool
	Fresh        string
}

type printerView struct {
	Display, State, Class, Facts, AsOf, Link string
	Default, Away                            bool
	Inks                                     []inkView
}

type inkView struct {
	Label, Value string
	Pct          int
	Low          bool
}

type jobView struct {
	ID                                       int
	Name, Printer, State, Class, When, Pages string
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
		pv := printerCell(p, snap.Default, now)
		if pv.Class == "need" {
			stuck++
		}
		v.Printers = append(v.Printers, pv)
	}
	names := map[string]string{}
	for _, p := range snap.Printers {
		names[p.Name] = p.Name
		if f := strings.Fields(p.Info); len(f) > 0 {
			names[p.Name] = f[0]
		}
	}
	for _, j := range snap.Active {
		if j.State == 4 {
			held++
		}
		v.Queue = append(v.Queue, jobRow(j, names, now, true))
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

func printerCell(p cups.Printer, def string, now time.Time) printerView {
	word, class := printerState(p)
	pv := printerView{
		Display: p.Info,
		State:   word,
		Class:   class,
		Away:    class == "away",
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

	for i, m := range p.Markers {
		if i == maxInks {
			break
		}
		pv.Inks = append(pv.Inks, inkCell(m, i))
	}
	if p.MarkersAt.IsZero() {
		pv.AsOf = "no ink levels reported yet"
	} else {
		pv.AsOf = "ink as of " + when(p.MarkersAt, now) + ", the last time CUPS spoke to it"
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
	case strings.HasPrefix(uri, "ipp://localhost") || strings.HasPrefix(uri, "ipps://localhost") ||
		strings.HasPrefix(uri, "ipp://127.0.0.1") || strings.HasPrefix(uri, "ipps://127.0.0.1") ||
		strings.Contains(plain, "(USB)"):
		return "USB through ipp-usb"
	}
	switch scheme {
	case "dnssd", "ipp", "ipps", "socket", "lpd", "http", "https":
		return "network"
	}
	return scheme
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
	case 4:
		jv.State, jv.Class = glyphHeld+" held · waits for you", "held"
	case 5:
		jv.State = fmt.Sprintf("printing · sheet %d", j.Sheets+1)
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
