// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
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
	"github.com/nemesis398/skra/internal/printer"
)

const (
	// searchSeconds is how long CUPS looks for printers. The browser counts
	// the same number down, from the data-seconds of the search button.
	searchSeconds = 10
	// searchTimeout bounds the whole search: CUPS answers when its own timeout
	// is over, and a little later when the machine is slow.
	searchTimeout = 15 * time.Second
	// addTimeout bounds an add. A driverless queue makes cupsd query the
	// printer, and the CUPS client waits 45 s for it.
	addTimeout = 60 * time.Second
	// maxDrivers is how many drivers the list offers; CUPS has thousands.
	maxDrivers = 300
	// maxSuggest is the longest queue name the panel suggests.
	maxSuggest = 60
)

const (
	notAddressText = "That is not a printer address. It looks like ipp://printer.local/ipp/print."
	badSchemeText  = "skrá adds printers by network or USB address."
	ownCUPSText    = "That address is this machine's own CUPS."
	badNameText    = "A queue name takes letters, digits, dot, dash and underscore, without spaces."
	noDriverText   = "Choose a driver first."
	wrongDriver    = "That driver is not one CUPS offers for this printer. Choose one from the list."
	needsDriver    = "Driverless works with IPP printers only. Choose a driver for this address."
)

func (s *Server) addRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /printers/search", s.search)
	mux.HandleFunc("GET /printers/new", s.addPanel)
	mux.HandleFunc("GET /printers/drivers", s.driverList)
	mux.HandleFunc("POST /printers/add", s.addPrinter)
}

// addNameForm is what a new queue may be called. It is narrower than what an
// existing queue can be named (validQueueName), because skrá makes only
// names that every tool takes.
var addNameForm = regexp.MustCompile(`^[A-Za-z0-9._-]{1,127}$`)

var (
	// addressSchemes are the ones skrá makes a queue for. Not file:, which
	// would let CUPS write to a path, and not CUPS's own backends' internals.
	addressSchemes = []string{"ipp", "ipps", "dnssd", "socket", "lpd", "usb", "http", "https"}
	// driverlessSchemes are the ones whose printer can say what it does.
	driverlessSchemes = []string{"ipp", "ipps", "dnssd", "http", "https"}
)

// parseAddress parses a device address with url.Parse. A dnssd address holds
// the service name percent-encoded in its host ("Brother%20DCP._ipp._tcp.local."),
// which url.Parse refuses in a host, so the percent signs of the authority are
// escaped for the parse; only the parse sees that.
func parseAddress(raw string) (*url.URL, error) {
	if i := strings.Index(raw, "://"); i >= 0 {
		start := i + len("://")
		end := start + strings.IndexAny(raw[start:]+"/", "/?#")
		raw = raw[:start] + strings.ReplaceAll(raw[start:end], "%", "%25") + raw[end:]
	}
	return url.Parse(raw)
}

// checkAddress reads an address the page sends and returns it with its scheme
// in lower case, or the refusal. It sends nothing anywhere.
func checkAddress(raw string) (string, *refusal) {
	bad := func(msg string) (string, *refusal) { return "", &refusal{http.StatusBadRequest, msg} }
	if raw == "" || len(raw) > maxText || !utf8.ValidString(raw) ||
		strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return bad(notAddressText)
	}
	u, err := parseAddress(raw)
	if err != nil || u.Scheme == "" {
		return bad(notAddressText)
	}
	if !slices.Contains(addressSchemes, u.Scheme) { // url.Parse has made the scheme lower case
		return bad(badSchemeText)
	}
	if u.Scheme == "usb" {
		if u.Host == "" && u.Opaque == "" && u.Path == "" {
			return bad(notAddressText)
		}
	} else if u.Hostname() == "" {
		return bad(notAddressText)
	}
	addr := u.Scheme + raw[len(u.Scheme):]
	if printer.IsLocalCUPS(addr) {
		return bad(ownCUPSText)
	}
	return addr, nil
}

func schemeOf(addr string) string {
	scheme, _, _ := strings.Cut(addr, ":")
	return scheme
}

// --- the search ---

type deviceView struct {
	Name, Model, Conn, ID, URI, Info string
	Queue                            string // the queues that have this address
	Has                              bool
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	devs, err := s.deps.CUPS.Devices(ctx, searchSeconds*time.Second)
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	queues, err := s.deps.CUPS.Printers(ctx)
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	var rows []deviceView
	for _, d := range devs {
		if !strings.Contains(d.URI, "://") {
			continue
		}
		v := deviceView{Name: d.Info, Model: d.MakeModel, Conn: connection(d.URI), ID: d.ID, URI: d.URI, Info: d.Info}
		if v.Name == "" {
			v.Name = d.URI
		}
		var names []string
		for _, q := range queues {
			if q.DeviceURI == d.URI {
				names = append(names, q.Name)
			}
		}
		v.Has, v.Queue = len(names) > 0, strings.Join(names, ", ")
		rows = append(rows, v)
	}
	s.fragment(w, "devices", rows)
}

// fragment renders a template of the page as HTML for skra.js to put in place.
func (s *Server) fragment(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		reply(w, http.StatusInternalServerError, "Could not render that.")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// --- the add panel ---

type addView struct {
	URI, Title, Name, Info, ID, Make, DriverHint string
	Choose                                       bool // the driver list is open
	Driver, Shared, Default                      segView
}

var notNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// suggestName makes a queue name out of the device's description or, failing
// that, its model, and numbers it when a queue (or ravnix) has the name.
func (s *Server) suggestName(info, model string, queues []cups.Printer) string {
	clean := func(v string) string {
		v = strings.Trim(notNameChars.ReplaceAllString(v, "-"), "-")
		if len(v) > maxSuggest {
			v = strings.Trim(v[:maxSuggest], "-")
		}
		return v
	}
	base := clean(info)
	if base == "" {
		base = clean(model)
	}
	if base == "" {
		base = "Printer"
	}
	taken := func(n string) bool {
		return s.declared(n) || slices.ContainsFunc(queues, func(q cups.Printer) bool { return strings.EqualFold(q.Name, n) })
	}
	name := base
	for i := 2; taken(name); i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

func (s *Server) addPanel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	q := r.URL.Query()
	addr, ref := checkAddress(strings.TrimSpace(q.Get("uri")))
	if ref != nil {
		reply(w, ref.status, ref.msg)
		return
	}
	queues, err := s.deps.CUPS.Printers(ctx)
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	info, model := strings.TrimSpace(q.Get("info")), strings.TrimSpace(q.Get("model"))
	if !okText(info) || !okText(model) || !okText(q.Get("id")) {
		reply(w, http.StatusBadRequest, "That device sent a description the page cannot show.")
		return
	}
	v := addView{
		URI: addr, Title: info, Name: s.suggestName(info, model, queues), Info: info, ID: q.Get("id"),
		Driver:  segView{Group: "driver"},
		Shared:  segOf("shared", []string{"no", "yes"}, ident, "no"),
		Default: segOf("default", []string{"no", "yes"}, ident, pick(len(queues) == 0, "yes", "no")),
	}
	if v.Title == "" {
		v.Title = pick(model != "", model, addr)
	}
	v.Make, _, _ = strings.Cut(model, " ")
	if slices.Contains(driverlessSchemes, schemeOf(addr)) {
		v.Driver.Choices = []choice{{"everywhere", "driverless", true}, {"choose", "choose a driver", false}}
		v.DriverHint = "driverless asks the printer itself; right for almost every printer made since 2015"
	} else {
		v.Choose = true
		v.Driver.Choices = []choice{{"choose", "choose a driver", true}}
		v.DriverHint = "this address does not speak IPP"
	}
	s.fragment(w, "addpanel", v)
}

// --- the driver list ---

// drivers lists the drivers CUPS has for a device, sorted by model and cut to
// maxDrivers. The device id finds the ones made for it; when none is, the
// maker's drivers are listed instead. Without either, none are listed, because
// CUPS would answer with all of them. total is how many CUPS had.
func (s *Server) drivers(ctx context.Context, id, maker string) (list []cups.PPD, total int, err error) {
	if id == "" && maker == "" {
		return nil, 0, nil
	}
	list, err = s.deps.CUPS.PPDs(ctx, id, maker)
	if err == nil && len(list) == 0 && id != "" && maker != "" {
		list, err = s.deps.CUPS.PPDs(ctx, "", maker)
	}
	if err != nil {
		return nil, 0, err
	}
	label := func(p cups.PPD) string { return strings.ToLower(pick(p.MakeModel != "", p.MakeModel, p.Name)) }
	slices.SortStableFunc(list, func(a, b cups.PPD) int { return strings.Compare(label(a), label(b)) })
	total = len(list)
	return list[:min(total, maxDrivers)], total, nil
}

type driversView struct {
	Message, Hint string
	Choices       []choice
}

func (s *Server) driverList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	id, maker := strings.TrimSpace(r.URL.Query().Get("id")), strings.TrimSpace(r.URL.Query().Get("make"))
	if !okText(id) || !okText(maker) {
		reply(w, http.StatusBadRequest, "That is not a device id or a maker.")
		return
	}
	if id == "" && maker == "" {
		s.fragment(w, "drivers", driversView{Message: "Type the maker's name to list its drivers."})
		return
	}
	list, total, err := s.drivers(ctx, id, maker)
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	if len(list) == 0 {
		s.fragment(w, "drivers", driversView{Message: "CUPS has no driver for this model. Type another maker, or install the maker's driver package."})
		return
	}
	v := driversView{Hint: fmt.Sprintf("%d found", total)}
	if total > len(list) {
		v.Hint = fmt.Sprintf("first %d of %d; type the maker to narrow the list", len(list), total)
	}
	for _, p := range list {
		v.Choices = append(v.Choices, choice{Value: p.Name, Label: pick(p.MakeModel != "", p.MakeModel, p.Name)})
	}
	s.fragment(w, "drivers", v)
}

// --- adding the queue ---

// yesNo reads a yes/no field of the form.
func yesNo(f url.Values, name string) (value, ok bool) {
	switch f.Get(name) {
	case "yes":
		return true, true
	case "no", "":
		return false, true
	}
	return false, false
}

func (s *Server) addPrinter(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), addTimeout)
	defer cancel()
	if err := r.ParseForm(); err != nil {
		reply(w, http.StatusBadRequest, "The form could not be read. Reload the page and try again.")
		return
	}
	f := r.PostForm
	text := func(name string) string { return strings.TrimSpace(f.Get(name)) }

	name := text("name")
	if !addNameForm.MatchString(name) {
		reply(w, http.StatusBadRequest, badNameText)
		return
	}
	if s.declared(name) {
		reply(w, http.StatusForbidden, name+" is declared in ravnix; add it there.")
		return
	}
	addr, ref := checkAddress(text("address"))
	if ref != nil {
		reply(w, ref.status, ref.msg)
		return
	}
	info, location := text("info"), text("location")
	if !okText(info) {
		reply(w, http.StatusBadRequest, "The description takes one line of up to 1023 characters.")
		return
	}
	if !okText(location) {
		reply(w, http.StatusBadRequest, "The location takes one line of up to 1023 characters.")
		return
	}
	shared, ok1 := yesNo(f, "shared")
	asDefault, ok2 := yesNo(f, "default")
	if !ok1 || !ok2 {
		reply(w, http.StatusBadRequest, "Shared on the network and default printer are yes or no.")
		return
	}
	if !okText(text("id")) || !okText(text("make")) {
		reply(w, http.StatusBadRequest, "That is not a device id or a maker.")
		return
	}
	var ppd string // empty: driverless
	switch f.Get("driver") {
	case "everywhere":
		if !slices.Contains(driverlessSchemes, schemeOf(addr)) {
			reply(w, http.StatusBadRequest, needsDriver)
			return
		}
	case "choose":
		if ppd = text("ppd"); ppd == "" {
			reply(w, http.StatusBadRequest, noDriverText)
			return
		}
	default:
		reply(w, http.StatusBadRequest, "Choose driverless or a driver.")
		return
	}

	queues, err := s.deps.CUPS.Printers(ctx)
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	if slices.ContainsFunc(queues, func(q cups.Printer) bool { return strings.EqualFold(q.Name, name) }) {
		reply(w, http.StatusConflict, "There is a queue named "+name+" already.")
		return
	}
	// Only a driver that CUPS lists for this device goes on to CUPS.
	if ppd != "" {
		list, _, err := s.drivers(ctx, text("id"), text("make"))
		if err != nil {
			reply(w, http.StatusBadGateway, err.Error())
			return
		}
		if !slices.ContainsFunc(list, func(p cups.PPD) bool { return p.Name == ppd }) {
			reply(w, http.StatusBadRequest, wrongDriver)
			return
		}
	}

	if info == "" {
		info = name
	}
	err = s.deps.CUPS.Add(ctx, cups.AddSpec{Name: name, DeviceURI: addr, Info: info, Location: location, PPD: ppd, Shared: shared, Default: asDefault})
	s.record(r, "printer.add", name, addr+" with "+pick(ppd == "", "driverless", ppd), err)
	s.hub.Refresh()
	var late *cups.SetDefaultError
	switch {
	case errors.As(err, &late):
		// The queue exists; the page closes its panel and says so.
		w.Header().Set("X-Skra-Added", "true")
		reply(w, http.StatusBadGateway, "Added "+name+", but making it the default failed: "+late.Err.Error()+" Make it the default from the table.")
	case err != nil:
		reply(w, http.StatusBadGateway, err.Error())
	default:
		reply(w, http.StatusOK, "Added "+name+". Send it a test page from the overview.")
	}
}
