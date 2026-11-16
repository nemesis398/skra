// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
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
	numericText    = "Write the address as a full IPv4 address, a host name, or an IPv6 address in brackets."
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
// which url.Parse refuses in a host, so for that scheme only the percent signs
// of the authority are escaped for the parse. Any other scheme is parsed as it
// is: CUPS decodes %XX in a host name before it connects, so a host such as
// local%68ost has to be refused, not read some other way.
func parseAddress(raw string) (*url.URL, error) {
	if i := strings.Index(raw, "://"); i >= 0 && strings.EqualFold(raw[:i], "dnssd") {
		start := i + len("://")
		auth := authority(raw)
		raw = raw[:start] + strings.ReplaceAll(auth, "%", "%25") + raw[start+len(auth):]
	}
	return url.Parse(raw)
}

// authority returns the authority of an address as typed, before url.Parse
// decodes any escape: what follows the first ://, up to the first /, ? or #.
// It is "" when there is no ://.
func authority(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return ""
	}
	auth := raw[i+len("://"):]
	if end := strings.IndexAny(auth, "/?#"); end >= 0 {
		auth = auth[:end]
	}
	return auth
}

// numberLabel is a part of a host that the system's address parser reads as a
// number: decimal, or hexadecimal with 0x.
var numberLabel = regexp.MustCompile(`^([0-9]+|0[xX][0-9a-fA-F]*)$`)

// oddNumber reports whether host is made of numbers only but is not a full
// dotted-quad IPv4 address in its canonical form. inet_aton reads "127.1",
// "2130706433" and "0x7f.0.0.1" as 127.0.0.1.
func oddNumber(host string) bool {
	for _, label := range strings.Split(host, ".") {
		if !numberLabel.MatchString(label) {
			return false
		}
	}
	ip, err := netip.ParseAddr(host)
	return err != nil || !ip.Is4()
}

// lookupAddrs and lookupTimeout are the name lookup of checkAddress, variables
// so that tests need not wait for a real resolver. lookupTimeout bounds the
// resolution of a dnssd address too.
var (
	lookupAddrs = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	lookupTimeout = 2 * time.Second
)

// rawHost returns the host part of the authority as typed, before url.Parse
// decodes any escape: without user[:password]@ and without a port.
func rawHost(raw string) string {
	auth := authority(raw)
	if j := strings.LastIndex(auth, "@"); j >= 0 {
		auth = auth[j+1:]
	}
	if strings.HasPrefix(auth, "[") {
		return auth // an IPv6 address in brackets; its port is not our business here
	}
	if j := strings.LastIndex(auth, ":"); j >= 0 {
		auth = auth[:j]
	}
	return auth
}

// pointsAtThisMachinesCUPS reports whether the host of u, once resolved, is an
// address of this machine and the port is 631. An address without a port gets
// the default of its scheme, which is 631 for ipp, ipps, http and https; a
// dnssd address is resolved by dnssdIsThisMachinesCUPS instead, which hands
// the address it resolved to here. A name that does not resolve is not: the
// printer may be away.
//
// This guards against setting up a queue that loops back into this CUPS. It is
// best effort and not a security boundary: CUPS connects to the printer, not
// skrá, and a name can resolve differently later. The connection of skrá's own
// status queries is refused where it is made (printer.Query).
func pointsAtThisMachinesCUPS(ctx context.Context, u *url.URL) bool {
	port := u.Port()
	if port == "" && slices.Contains(driverlessSchemes, u.Scheme) {
		port = "631"
	}
	if n, err := strconv.Atoi(port); err != nil || n != 631 {
		return false
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		return printer.IsOwnAddress(ip)
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	addrs, err := lookupAddrs(ctx, host)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(addrs, printer.IsOwnAddress)
}

// dnssdIsThisMachinesCUPS resolves a dnssd address through r and reports
// whether it is this machine's CUPS: avahi says so (the queues this CUPS
// shares), or the address it found is port 631 of this machine. Anything else
// avahi finds on this machine is a printer, such as the queue ipp-usb shares
// for a USB printer on port 60000. A service that does not resolve within
// lookupTimeout is not refused: the printer may be off, and the live status
// query refuses this machine's CUPS again when it connects. Without r nothing
// is resolved.
func dnssdIsThisMachinesCUPS(ctx context.Context, r printer.Resolver, addr string) bool {
	if r == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	resolved, err := r.Resolve(ctx, withoutUserinfo(addr))
	if errors.Is(err, printer.ErrOwnHost) {
		return true
	}
	if err != nil {
		return false
	}
	u, err := url.Parse(resolved)
	if err != nil {
		return true // avahi's answer is ours to read; one that does not parse is not let through
	}
	return printer.IsLocalCUPS(resolved) || pointsAtThisMachinesCUPS(ctx, u)
}

// checkAddress reads an address the page sends and returns it with its scheme
// in lower case, or the refusal. It sends nothing to the printer. r resolves a
// dnssd address to tell whether it is this machine's CUPS; it may be nil.
func checkAddress(ctx context.Context, r printer.Resolver, raw string) (string, *refusal) {
	bad := func(msg string) (string, *refusal) { return "", &refusal{http.StatusBadRequest, msg} }
	if raw == "" || len(raw) > maxText || !utf8.ValidString(raw) ||
		strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return bad(notAddressText)
	}
	// CUPS ends the user name at the first @ and ignores ? and # on the way;
	// Go ends the authority at them and the user name at the last @. The two
	// read "x:1?@127.0.0.1:631" and "a@b@host" as different hosts, so a ? or #
	// before the first /, or a second @, is not an address.
	if i := strings.Index(raw, "://"); i >= 0 {
		authority, _, _ := strings.Cut(raw[i+len("://"):], "/")
		if strings.ContainsAny(authority, "?#") || strings.Count(authority, "@") > 1 {
			return bad(notAddressText)
		}
	}
	u, err := parseAddress(raw)
	if err != nil || u.Scheme == "" {
		return bad(notAddressText)
	}
	if !slices.Contains(addressSchemes, u.Scheme) { // url.Parse has made the scheme lower case
		return bad(badSchemeText)
	}
	addr := u.Scheme + raw[len(u.Scheme):]
	switch {
	case u.Scheme == "usb":
		if u.Host == "" && u.Opaque == "" && u.Path == "" {
			return bad(notAddressText)
		}
		return addr, nil
	case u.Hostname() == "":
		return bad(notAddressText)
	case u.Scheme == "dnssd": // the instance name is percent-encoded and may be UTF-8
		if dnssdIsThisMachinesCUPS(ctx, r, addr) {
			return bad(ownCUPSText)
		}
		return addr, nil
	}
	// The host as typed, because url.Parse has decoded its escapes and CUPS,
	// the system resolver and IDNA fold a host name in ways nothing here can
	// follow: %XX, fullwidth or zero-width characters turn into "localhost".
	// Only an IPv6 zone, inside brackets, has a percent sign, and an
	// international name is written as punycode (xn--...).
	host := rawHost(raw)
	if !strings.HasPrefix(host, "[") && strings.Contains(host, "%") ||
		strings.ContainsFunc(host, func(r rune) bool { return r > unicode.MaxASCII }) {
		return bad(notAddressText)
	}
	if oddNumber(u.Hostname()) {
		return bad(numericText)
	}
	if printer.IsLocalCUPS(addr) || pointsAtThisMachinesCUPS(ctx, u) {
		return bad(ownCUPSText)
	}
	return addr, nil
}

// userinfoOf returns the user[:password] part of an address, or "".
func userinfoOf(addr string) string {
	auth := authority(addr)
	if j := strings.LastIndex(auth, "@"); j >= 0 {
		return auth[:j]
	}
	return ""
}

// withoutUserinfo is the address as it may be logged and shown. The address
// is parsed, and what it does not parse as is withheld; the user[:password]@
// is then cut out of the text as typed, so the rest of it stays byte for byte
// what was sent (a re-serialized URL would change escapes).
func withoutUserinfo(addr string) string {
	u, err := parseAddress(addr)
	if err != nil {
		return "(address withheld)"
	}
	if u.User == nil {
		return addr
	}
	// The user[:password]@ must stand right after the "://"; an address that
	// has it anywhere else is not cut at a guess.
	ui := userinfoOf(addr)
	i := strings.Index(addr, "://")
	if i < 0 || !strings.HasPrefix(addr[i+len("://"):], ui+"@") {
		return "(address withheld)"
	}
	i += len("://")
	return addr[:i] + addr[i+len(ui)+1:]
}

// scrub takes the credentials of addr out of a text that may quote the
// address, such as an error of CUPS: the user[:password]@ part as typed and as
// decoded. Only that, never the password alone, which may be one letter that
// the text holds elsewhere.
func scrub(text, addr string) string {
	ui := userinfoOf(addr)
	if ui == "" {
		return text
	}
	text = strings.ReplaceAll(text, ui+"@", "")
	if plain, err := url.PathUnescape(ui); err == nil && plain != ui {
		text = strings.ReplaceAll(text, plain+"@", "")
	}
	return text
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
	s.fragment(w, "devices", rows, renderFailedText)
}

// renderFailedText is the answer when a fragment of the add panel does not render.
const renderFailedText = "Could not render that."

// fragment renders a template of the page as HTML for skra.js to put in place.
// When the template fails it answers 500 with failed.
func (s *Server) fragment(w http.ResponseWriter, name string, data any, failed string) {
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		reply(w, http.StatusInternalServerError, failed)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// --- the add panel ---

type addView struct {
	URI, Shown, Title, Name, Info, ID, Make, DriverHint string
	Choose                                              bool // the driver list is open
	Driver, Shared, Default                             segView
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
	taken := func(n string) bool { return s.declared(n) || hasQueue(queues, n) }
	name := base
	for i := 2; taken(name); i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

// hasQueue reports whether one of the queues has the name. CUPS compares queue
// names without regard to case.
func hasQueue(queues []cups.Printer, name string) bool {
	return slices.ContainsFunc(queues, func(q cups.Printer) bool { return strings.EqualFold(q.Name, name) })
}

func (s *Server) addPanel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	q := r.URL.Query()
	addr, ref := checkAddress(ctx, s.deps.Resolver, strings.TrimSpace(q.Get("uri")))
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
		URI: addr, Shown: withoutUserinfo(addr), Title: info, Name: s.suggestName(info, model, queues), Info: info, ID: q.Get("id"),
		Driver:  segView{Group: "driver"},
		Shared:  segOf("shared", []string{"no", "yes"}, ident, "no"),
		Default: segOf("default", []string{"no", "yes"}, ident, pick(len(queues) == 0, "yes", "no")),
	}
	if v.Title == "" {
		v.Title = pick(model != "", model, v.Shown)
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
	s.fragment(w, "addpanel", v, renderFailedText)
}

// --- the driver list ---

// drivers lists the drivers CUPS has for a device, sorted by model and cut to
// maxDrivers. The device id finds the ones made for it, and a maker narrows
// them to those whose model starts with it, without regard to case. When none
// is left, the maker's drivers are listed instead, and when CUPS has none of
// those either, the device's own list stands whole: the maker is only a
// narrowing, never a reason to hide a driver that matched the device. Without
// an id or a maker, none are listed, because CUPS would answer with all of
// them. total is how many there were before the cut. The add check uses this
// same function, so a driver the list shows is never refused.
func (s *Server) drivers(ctx context.Context, id, maker string) (list []cups.PPD, total int, err error) {
	if id == "" && maker == "" {
		return nil, 0, nil
	}
	list, err = s.deps.CUPS.PPDs(ctx, id, maker)
	if err == nil && id != "" && maker != "" {
		list, err = s.narrowed(ctx, list, maker)
	}
	if err != nil {
		return nil, 0, err
	}
	key := func(p cups.PPD) string { return strings.ToLower(driverLabel(p)) }
	slices.SortStableFunc(list, func(a, b cups.PPD) int { return strings.Compare(key(a), key(b)) })
	total = len(list)
	return list[:min(total, maxDrivers)], total, nil
}

// narrowed is the drivers of a device (all, found by its id) that a maker
// narrows down; see drivers for the order of the fallbacks.
func (s *Server) narrowed(ctx context.Context, all []cups.PPD, maker string) ([]cups.PPD, error) {
	if kept := slices.DeleteFunc(slices.Clone(all), func(p cups.PPD) bool { return !hasPrefixFold(p.MakeModel, maker) }); len(kept) > 0 {
		return kept, nil
	}
	byMaker, err := s.deps.CUPS.PPDs(ctx, "", maker)
	if err != nil || len(byMaker) > 0 {
		return byMaker, err
	}
	return all, nil
}

// hasPrefixFold reports whether s starts with prefix, ignoring case.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// driverLabel is how the list shows a driver: its make and model, or its name.
func driverLabel(p cups.PPD) string { return pick(p.MakeModel != "", p.MakeModel, p.Name) }

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
		s.fragment(w, "drivers", driversView{Message: "Type the maker's name to list its drivers."}, renderFailedText)
		return
	}
	list, total, err := s.drivers(ctx, id, maker)
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	if len(list) == 0 {
		s.fragment(w, "drivers", driversView{Message: "CUPS has no driver for this model. Type another maker, or install the maker's driver package."}, renderFailedText)
		return
	}
	v := driversView{Hint: fmt.Sprintf("%d found", total)}
	if total > len(list) {
		v.Hint = fmt.Sprintf("first %d of %d; type the maker to narrow the list", len(list), total)
	}
	for _, p := range list {
		v.Choices = append(v.Choices, choice{Value: p.Name, Label: driverLabel(p)})
	}
	s.fragment(w, "drivers", v, renderFailedText)
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
	addr, ref := checkAddress(ctx, s.deps.Resolver, text("address"))
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
	id, maker := text("id"), text("make")
	if !okText(id) || !okText(maker) {
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

	// From here on skrá has asked CUPS something, so a refusal is an attempt
	// and goes into the history. The address is logged without its credentials.
	detail := withoutUserinfo(addr) + " with " + pick(ppd == "", "driverless", ppd)
	refuse := func(status int, err error) {
		s.record(r, "printer.add", name, detail, err)
		reply(w, status, err.Error())
	}
	queues, err := s.deps.CUPS.Printers(ctx)
	if err != nil {
		refuse(http.StatusBadGateway, err)
		return
	}
	if hasQueue(queues, name) {
		refuse(http.StatusConflict, errors.New("There is a queue named "+name+" already."))
		return
	}
	// Only a driver that CUPS lists for this device goes on to CUPS.
	if ppd != "" {
		list, _, err := s.drivers(ctx, id, maker)
		if err != nil {
			refuse(http.StatusBadGateway, err)
			return
		}
		if !slices.ContainsFunc(list, func(p cups.PPD) bool { return p.Name == ppd }) {
			refuse(http.StatusBadRequest, errors.New(wrongDriver))
			return
		}
	}

	if info == "" {
		info = name
	}
	err = s.deps.CUPS.Add(ctx, cups.AddSpec{Name: name, DeviceURI: addr, Info: info, Location: location, PPD: ppd, Shared: shared, Default: asDefault})
	// Credentials in the address stay out of the log and the answers.
	var late *cups.SetDefaultError
	isLate := errors.As(err, &late)
	said := ""
	if err != nil {
		said = scrub(err.Error(), addr)
		err = errors.New(said)
	}
	s.record(r, "printer.add", name, detail, err)
	s.hub.Refresh()
	switch {
	case isLate:
		// The queue exists; the page closes its panel and says so.
		w.Header().Set("X-Skra-Added", "true")
		reply(w, http.StatusBadGateway, "Added "+name+", but making it the default failed: "+scrub(late.Err.Error(), addr)+" Make it the default from the table.")
	case err != nil:
		reply(w, http.StatusBadGateway, said)
	default:
		reply(w, http.StatusOK, "Added "+name+". Send it a test page from the overview.")
	}
}
