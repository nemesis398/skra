// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nemesis398/skra/internal/cups"
)

// maxText is the longest description, location or address CUPS takes, in bytes.
const maxText = 1023

// A queue action is the CUPS call, the event it logs and the footer message.
var queueActions = map[string]struct {
	event string
	do    func(CUPS, context.Context, string) error
	said  func(name, short string) string
}{
	"default": {"printer.default", CUPS.SetDefault, func(_, short string) string { return short + " is the default printer now." }},
	"delete":  {"printer.delete", CUPS.Delete, func(name, _ string) string { return "Removed the queue " + name + "." }},
	"accept": {"printer.accept", func(c CUPS, ctx context.Context, n string) error { return c.Accept(ctx, n, true) },
		func(_, short string) string { return short + " accepts new jobs again." }},
	"reject": {"printer.reject", func(c CUPS, ctx context.Context, n string) error { return c.Accept(ctx, n, false) },
		func(_, short string) string { return short + " turns new jobs away now." }},
	"pause": {"printer.pause", func(c CUPS, ctx context.Context, n string) error { return c.Pause(ctx, n, true) },
		func(_, short string) string { return "Paused the queue of " + short + "." }},
	"resume": {"printer.resume", func(c CUPS, ctx context.Context, n string) error { return c.Pause(ctx, n, false) },
		func(_, short string) string { return "Resumed the queue of " + short + "." }},
}

func (s *Server) printerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /printers", s.printersPage)
	mux.HandleFunc("GET /printers/{name}/settings", s.settingsPanel)
	mux.HandleFunc("POST /printers/{name}/settings", s.saveSettings)
	for act := range queueActions {
		mux.HandleFunc("POST /printers/{name}/"+act, s.queueAction(act))
	}
	mux.HandleFunc("POST /printers/{name}/cancel-all", s.cancelAll)
	mux.HandleFunc("POST /printers/{name}/move-all", s.moveAll)
	mux.HandleFunc("POST /printers/{name}/command", s.command)
}

// declared reports whether ravnix declares the queue, which then can be
// neither removed nor given another address here.
func (s *Server) declared(name string) bool { return slices.Contains(s.cfg.DeclaredQueues, name) }

func declaredText(name string) string { return name + " is declared in ravnix; remove it there." }

var errNoQueue = errors.New("no such queue")

// queue asks CUPS afresh for the queue and, for the move dialog and the short
// names, for all queues.
func (s *Server) queue(ctx context.Context, name string) (cups.Printer, []cups.Printer, error) {
	all, err := s.deps.CUPS.Printers(ctx)
	if err != nil {
		return cups.Printer{}, nil, err
	}
	for _, p := range all {
		if p.Name == name {
			return p, all, nil
		}
	}
	return cups.Printer{}, all, errNoQueue
}

// queueFailed answers for a queue that could not be read.
func queueFailed(w http.ResponseWriter, name string, err error) {
	if errors.Is(err, errNoQueue) {
		reply(w, http.StatusNotFound, "There is no queue named "+name+".")
		return
	}
	reply(w, http.StatusBadGateway, err.Error())
}

// jobsOn counts the jobs that are not over on the queue.
func (s *Server) jobsOn(ctx context.Context, name string) (int, error) {
	jobs, err := s.deps.CUPS.Jobs(ctx, "not-completed", 0)
	if err != nil {
		return 0, err
	}
	return countOn(jobs, name), nil
}

func countOn(jobs []cups.Job, name string) int {
	n := 0
	for _, j := range jobs {
		if j.Printer == name {
			n++
		}
	}
	return n
}

func (s *Server) queueAction(act string) http.HandlerFunc {
	a := queueActions[act]
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if act == "delete" && s.declared(name) {
			reply(w, http.StatusForbidden, declaredText(name))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
		defer cancel()
		err := a.do(s.deps.CUPS, ctx, name)
		s.record(r, a.event, name, "", err)
		s.hub.Refresh()
		if err != nil {
			reply(w, http.StatusBadGateway, err.Error())
			return
		}
		reply(w, http.StatusOK, a.said(name, s.shortPrinter(name)))
	}
}

func (s *Server) cancelAll(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	n, err := s.jobsOn(ctx, name)
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	err = s.deps.CUPS.CancelAll(ctx, name)
	s.record(r, "printer.cancelall", name, plural(n, "job", "jobs"), err)
	s.hub.Refresh()
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	reply(w, http.StatusOK, fmt.Sprintf("Canceled %s on %s.", plural(n, "job", "jobs"), s.shortPrinter(name)))
}

func (s *Server) moveAll(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	to := strings.TrimSpace(r.PostFormValue("to"))
	switch to {
	case "":
		reply(w, http.StatusBadRequest, "Choose the queue to move the jobs to.")
		return
	case name:
		reply(w, http.StatusBadRequest, "The jobs are on that queue already. Choose another one.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	n, err := s.jobsOn(ctx, name)
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	err = s.deps.CUPS.MoveAll(ctx, name, to)
	s.record(r, "printer.moveall", name, "to "+to, err)
	s.hub.Refresh()
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	reply(w, http.StatusOK, fmt.Sprintf("Moved %s from %s to %s.", plural(n, "job", "jobs"), s.shortPrinter(name), s.shortPrinter(to)))
}

// commandLabels are the words for the maintenance commands CUPS drivers
// commonly list; any other command is shown by its own name.
var commandLabels = map[string]string{
	"Clean":             "clean print heads",
	"PrintSelfTestPage": "print a self-test page",
	"ReportLevels":      "ask for ink levels",
}

func commandLabel(c string) string {
	if l, ok := commandLabels[c]; ok {
		return l
	}
	return c
}

func (s *Server) command(w http.ResponseWriter, r *http.Request) {
	name, cmd := r.PathValue("name"), r.PostFormValue("command")
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	p, all, err := s.queue(ctx, name)
	if err != nil {
		queueFailed(w, name, err)
		return
	}
	short := shortNames(all)[name]
	if !slices.Contains(p.Commands, cmd) {
		reply(w, http.StatusBadRequest, short+" does not offer that command.")
		return
	}
	id, err := s.deps.CUPS.Command(ctx, name, cmd)
	detail := cmd
	if err == nil {
		detail = fmt.Sprintf("%s as job %d", cmd, id)
	}
	s.record(r, "printer.command", name, detail, err)
	s.hub.Refresh()
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	reply(w, http.StatusOK, fmt.Sprintf("Sent %s to %s as job %d.", quoted(commandLabel(cmd)), short, id))
}

// --- the page and the queue table ---

// queueView is one row of the queue table.
type queueView struct {
	Name, Info, Conn, State, Class, Notes string
	Default, CanRemove                    bool
	// The remove button always asks first, and names the waiting jobs.
	RemoveTitle, RemoveText, RemoveLabel string
}

func (s *Server) queueRows(snap Snapshot, now time.Time) []queueView {
	var out []queueView
	for _, p := range snap.Printers {
		pv := printerCell(p, snap.Default, now, snap.Live)
		q := queueView{
			Name: p.Name, Info: p.Info, Conn: connection(p.DeviceURI), State: pv.State,
			Default: pv.Default, CanRemove: !s.declared(p.Name),
		}
		switch pv.Class {
		case "need":
			q.Class = "held"
		case "bad":
			q.Class = "bad"
		case "away":
			q.Class = "off"
		}
		var notes []string
		if q.Default {
			notes = append(notes, "default")
		}
		if !p.Accepting {
			notes = append(notes, "rejects jobs")
		}
		if !q.CanRemove {
			notes = append(notes, "declared in ravnix")
		}
		q.Notes = "·"
		if len(notes) > 0 {
			q.Notes = strings.Join(notes, " · ")
		}
		q.RemoveTitle = "Remove the queue " + p.Name + "?"
		q.RemoveLabel = "Remove " + p.Name
		q.RemoveText = removeText(countOn(snap.Active, p.Name))
		out = append(out, q)
	}
	return out
}

func removeText(waiting int) string {
	const rest = "The printer itself is not touched, and you can add it again below."
	switch waiting {
	case 0:
		return rest
	case 1:
		return "Its 1 waiting job is canceled with it. " + rest
	}
	return fmt.Sprintf("Its %d waiting jobs are canceled with it. %s", waiting, rest)
}

func (s *Server) printersPage(w http.ResponseWriter, r *http.Request) { s.render(w, r, "printers") }

// --- the settings panel ---

type choice struct {
	Value, Label string
	On           bool
}

// segView is a row of buttons of which one is on; selView a drop-down.
type segView struct {
	Group   string
	Choices []choice
}

type selView struct {
	ID, Name string
	Choices  []choice
}

type commandView struct{ Name, Label string }

type settingsView struct {
	Name, Title, Info, Location, Address, AddressHint string
	Declared                                          bool
	State, Accept, Shared, Sides, Color, Quality      segView
	Media, Policy                                     selView

	// The buttons that act at once: the opposite of what the queue does now.
	PauseAct, PauseLabel, AcceptAct, AcceptLabel string

	Jobs                                   int
	CancelTitle, CancelText, CancelLabel   string
	MoveTo, MoveTitle, MoveText, MoveLabel string
	Commands                               []commandView
}

var (
	qualityLabels = map[int]string{3: "draft", 4: "normal", 5: "high"}
	policyLabels  = map[string]string{
		"retry-job": "try the job again later", "retry-current-job": "try the job again now",
		"abort-job": "drop the job, carry on", "stop-printer": "pause the queue",
	}
	sidesWords = strings.NewReplacer("two-sided-", "2-sided, ", "one-sided", "1-sided", "-edge", " edge")
	paperName  = regexp.MustCompile(`^[a-c]\d+$`)
)

// mediaLabel turns a PWG media name such as iso_a4_210x297mm into "A4 · 210 × 297mm".
func mediaLabel(m string) string {
	parts := strings.Split(m, "_")
	if len(parts) < 3 {
		return m
	}
	name := strings.Join(parts[1:len(parts)-1], " ")
	if paperName.MatchString(name) {
		name = strings.ToUpper(name)
	} else if r, n := utf8.DecodeRuneInString(name); n > 0 {
		name = string(unicode.ToUpper(r)) + name[n:]
	}
	return name + " · " + strings.Replace(parts[len(parts)-1], "x", " × ", 1)
}

// pick is a if cond holds and b otherwise.
func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func ident(s string) string { return s }

func colorLabel(c string) string {
	if c == "monochrome" {
		return "gray"
	}
	return c
}

// segOf offers the values, with the current one on. It is empty when there is
// no choice to make.
func segOf(group string, values []string, label func(string) string, cur string) segView {
	if len(values) < 2 {
		return segView{Group: group}
	}
	sv := segView{Group: group}
	for _, v := range values {
		sv.Choices = append(sv.Choices, choice{Value: v, Label: label(v), On: v == cur})
	}
	return sv
}

// selectOf offers the values as a drop-down. A current value that the list
// lacks is added, so a save does not change it to the first one.
func selectOf(id, name string, values []string, label func(string) string, cur string) selView {
	sv := selView{ID: id, Name: name}
	if len(values) == 0 {
		return sv
	}
	if cur != "" && !slices.Contains(values, cur) {
		values = append(slices.Clone(values), cur)
	}
	for _, v := range values {
		sv.Choices = append(sv.Choices, choice{Value: v, Label: label(v), On: v == cur})
	}
	return sv
}

func (s *Server) settingsPanel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	p, all, err := s.queue(ctx, name)
	if err != nil {
		queueFailed(w, name, err)
		return
	}
	jobs, err := s.jobsOn(ctx, name)
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, "settings", settingsFor(p, all, jobs, s.declared(name))); err != nil {
		reply(w, http.StatusInternalServerError, "Could not render the settings.")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

func settingsFor(p cups.Printer, all []cups.Printer, jobs int, declared bool) settingsView {
	names := shortNames(all)
	short := names[p.Name]
	paused := p.State == 5
	sv := settingsView{
		Name: p.Name, Title: p.Info, Info: p.Info, Location: p.Location, Address: p.DeviceURI,
		AddressHint: connection(p.DeviceURI), Declared: declared,
		State:    segOf("state", []string{"running", "paused"}, ident, pick(paused, "paused", "running")),
		Accept:   segOf("accept", []string{"accepted", "rejected"}, ident, pick(p.Accepting, "accepted", "rejected")),
		Shared:   segOf("shared", []string{"no", "yes"}, ident, pick(p.Shared, "yes", "no")),
		Sides:    segOf("sides", p.SidesSupported, sidesWords.Replace, p.SidesDefault),
		Color:    segOf("color", p.ColorSupported, colorLabel, p.ColorDefault),
		Media:    selectOf("c-media", "media", slices.DeleteFunc(slices.Clone(p.MediaSupported), func(m string) bool { return strings.HasPrefix(m, "custom_") }), mediaLabel, p.MediaDefault),
		Policy:   selectOf("c-policy", "policy", p.ErrorPolicies, policyLabel, p.ErrorPolicy),
		Jobs:     jobs,
		PauseAct: "pause", PauseLabel: "pause queue", AcceptAct: "reject", AcceptLabel: "reject new jobs",
	}
	if sv.Title == "" {
		sv.Title = p.Name
	}
	if paused {
		sv.PauseAct, sv.PauseLabel = "resume", "resume queue"
	}
	if !p.Accepting {
		sv.AcceptAct, sv.AcceptLabel = "accept", "accept new jobs"
	}
	var q []string
	for _, v := range p.QualitySupported {
		q = append(q, strconv.Itoa(v))
	}
	sv.Quality = segOf("quality", q, func(v string) string {
		if n, err := strconv.Atoi(v); err == nil && qualityLabels[n] != "" {
			return qualityLabels[n]
		}
		return v
	}, strconv.Itoa(p.QualityDefault))

	if jobs > 0 {
		count := plural(jobs, "job", "jobs")
		sv.CancelTitle = "Cancel " + count + " on " + short + "?"
		sv.CancelText = "Everything waiting or printing on this queue is canceled. A sheet already in the printer may still come out."
		sv.CancelLabel = "Cancel " + count
		var targets []moveTarget
		for _, o := range all {
			if o.Name != p.Name {
				targets = append(targets, moveTarget{Name: o.Name, Label: names[o.Name]})
			}
		}
		if raw, err := json.Marshal(targets); err == nil && len(targets) > 0 {
			sv.MoveTo = string(raw)
			sv.MoveTitle = "Move " + count + " from " + short
			sv.MoveText = "Choose the printer to move the jobs to."
			sv.MoveLabel = "Move " + count
		}
	}
	for _, c := range p.Commands {
		sv.Commands = append(sv.Commands, commandView{Name: c, Label: commandLabel(c)})
	}
	return sv
}

func policyLabel(p string) string {
	if l, ok := policyLabels[p]; ok {
		return l
	}
	return p
}

// --- saving the settings ---

// refusal is a request the server will not send on, with the status to answer.
type refusal struct {
	status int
	msg    string
}

// plan is what a settings post asks for, compared with the queue as it is.
type plan struct {
	ch     cups.Changes
	fields []string // the settings Modify sends, in the order of the form
	accept *bool
	pause  *bool
}

var addressForm = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://\S+$`)

// okText reports whether v can be a description or a location: one line of
// text that fits an IPP text value.
func okText(v string) bool {
	return len(v) <= maxText && utf8.ValidString(v) && !strings.ContainsFunc(v, unicode.IsControl)
}

// planSettings reads the form against the queue as CUPS has it. A field the
// form leaves out stays as it is, and so does a field that has not changed.
// Anything changed that the queue does not offer is refused, so it never
// reaches CUPS.
func (s *Server) planSettings(p cups.Printer, f url.Values) (plan, *refusal) {
	var pl plan
	bad := func(msg string) (plan, *refusal) { return plan{}, &refusal{http.StatusBadRequest, msg} }
	// field returns the trimmed value of a form field when it differs from cur.
	field := func(name, cur string) (string, bool) {
		if !f.Has(name) {
			return "", false
		}
		v := strings.TrimSpace(f.Get(name))
		return v, v != cur
	}
	note := func(label string) { pl.fields = append(pl.fields, label) }

	if v, ok := field("info", p.Info); ok {
		if !okText(v) {
			return bad("The description takes one line of up to 1023 characters.")
		}
		pl.ch.Info = &v
		note("description")
	}
	if v, ok := field("location", p.Location); ok {
		if !okText(v) {
			return bad("The location takes one line of up to 1023 characters.")
		}
		pl.ch.Location = &v
		note("location")
	}
	if v, ok := field("address", p.DeviceURI); ok {
		switch {
		case s.declared(p.Name):
			return plan{}, &refusal{http.StatusForbidden, declaredText(p.Name)}
		case len(v) > maxText || !addressForm.MatchString(v):
			return bad("That is not a printer address. It looks like ipp://printer.local/ipp/print.")
		}
		pl.ch.DeviceURI = &v
		note("address")
	}
	if f.Has("shared") {
		v := f.Get("shared")
		if v != "yes" && v != "no" {
			return bad("Shared on the network is yes or no.")
		}
		if on := v == "yes"; on != p.Shared {
			pl.ch.Shared = &on
			note("shared")
		}
	}
	for _, o := range []struct {
		name, label string
		cur         string
		offered     []string
		dst         **string
	}{
		{"sides", "sides", p.SidesDefault, p.SidesSupported, &pl.ch.Sides},
		{"color", "color", p.ColorDefault, p.ColorSupported, &pl.ch.Color},
	} {
		if v, ok := field(o.name, o.cur); ok {
			if !slices.Contains(o.offered, v) {
				return bad("This printer does not offer that choice for " + o.label + ".")
			}
			*o.dst = &v
			note(o.label)
		}
	}
	if v, ok := field("quality", strconv.Itoa(p.QualityDefault)); ok {
		n, err := strconv.Atoi(v)
		if err != nil || !slices.Contains(p.QualitySupported, n) {
			return bad("This printer does not offer that choice for quality.")
		}
		pl.ch.Quality = &n
		note("quality")
	}
	if v, ok := field("media", p.MediaDefault); ok {
		if !slices.Contains(p.MediaSupported, v) {
			return bad("This printer does not offer that paper.")
		}
		pl.ch.Media = &v
		note("paper")
	}
	if v, ok := field("policy", p.ErrorPolicy); ok {
		if !slices.Contains(p.ErrorPolicies, v) {
			return bad("This printer does not offer that choice for what to do when printing fails.")
		}
		pl.ch.ErrorPolicy = &v
		note("when printing fails")
	}
	if f.Has("accept") {
		v := f.Get("accept")
		if v != "accepted" && v != "rejected" {
			return bad("New jobs are accepted or rejected.")
		}
		if on := v == "accepted"; on != p.Accepting {
			pl.accept = &on
		}
	}
	if f.Has("state") {
		v := f.Get("state")
		if v != "running" && v != "paused" {
			return bad("The queue is running or paused.")
		}
		if on := v == "paused"; on != (p.State == 5) {
			pl.pause = &on
		}
	}
	return pl, nil
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		reply(w, http.StatusBadRequest, "The form could not be read. Reload the page and try again.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	p, _, err := s.queue(ctx, name)
	if err != nil {
		queueFailed(w, name, err)
		return
	}
	pl, ref := s.planSettings(p, r.PostForm)
	if ref != nil {
		reply(w, ref.status, ref.msg)
		return
	}

	var done []string
	fail := func(err error) {
		s.hub.Refresh()
		reply(w, http.StatusBadGateway, err.Error())
	}
	if len(pl.fields) > 0 {
		err := s.deps.CUPS.Modify(ctx, name, pl.ch)
		s.record(r, "printer.modify", name, strings.Join(pl.fields, ", "), err)
		if err != nil {
			fail(err)
			return
		}
		done = append(done, plural(len(pl.fields), "setting", "settings"))
	}
	if pl.accept != nil {
		event, said := "printer.reject", "rejects new jobs"
		if *pl.accept {
			event, said = "printer.accept", "accepts new jobs"
		}
		err := s.deps.CUPS.Accept(ctx, name, *pl.accept)
		s.record(r, event, name, "", err)
		if err != nil {
			fail(err)
			return
		}
		done = append(done, said)
	}
	if pl.pause != nil {
		event, said := "printer.resume", "running"
		if *pl.pause {
			event, said = "printer.pause", "paused"
		}
		err := s.deps.CUPS.Pause(ctx, name, *pl.pause)
		s.record(r, event, name, "", err)
		if err != nil {
			fail(err)
			return
		}
		done = append(done, said)
	}
	if len(done) == 0 {
		reply(w, http.StatusOK, "Nothing changed.")
		return
	}
	s.hub.Refresh()
	reply(w, http.StatusOK, "Saved "+name+": "+strings.Join(done, ", ")+".")
}
