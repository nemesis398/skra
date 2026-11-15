// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/printer"
)

func (f *fakeCUPS) Devices(ctx context.Context, timeout time.Duration) ([]cups.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devTimeout = timeout
	if d, ok := ctx.Deadline(); ok {
		f.devLeft = time.Until(d)
	}
	return f.devices, f.devErr
}

func (f *fakeCUPS) PPDs(_ context.Context, id, make string) ([]cups.PPD, error) {
	f.mu.Lock()
	f.ppdAsked = append(f.ppdAsked, [2]string{id, make})
	fn := f.ppdsFn
	f.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(id, make)
}

func (f *fakeCUPS) Add(_ context.Context, spec cups.AddSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adds = append(f.adds, spec)
	return f.addErr
}

func (f *fakeCUPS) added() []cups.AddSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cups.AddSpec(nil), f.adds...)
}

func (f *fakeCUPS) asked() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.ppdAsked...)
}

// The address check looks a host name up. Tests must not wait for the real
// resolver, so by default no name resolves; a test that wants answers sets
// names, and the tests of the real resolver put realLookup back.
var (
	realLookup = lookupAddrs
	names      = map[string][]netip.Addr{}
	lookups    atomic.Int32
)

func init() { lookupAddrs = fakeLookup }

func fakeLookup(_ context.Context, host string) ([]netip.Addr, error) {
	lookups.Add(1)
	if a, ok := names[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// withNames makes the given names resolve for the test.
func withNames(t *testing.T, m map[string][]netip.Addr) {
	t.Helper()
	old := names
	names = m
	t.Cleanup(func() { names = old })
}

// addForm is a good form for an IPP printer; a test changes what it tests.
func addForm(kv ...string) url.Values {
	v := url.Values{
		"name": {"Neu"}, "info": {"Neues Gerät"}, "location": {"Flur"}, "address": {"ipp://neu.local/ipp/print"},
		"driver": {"everywhere"}, "shared": {"no"}, "default": {"no"},
	}
	for i := 0; i < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func TestAddValidatesName(t *testing.T) {
	f := queuesFake() // Hall and Virtual-MFC
	s, h := actionServer(t, f, "Ravnix-Q")
	for _, tc := range []struct {
		name   string
		status int
		text   string
	}{
		{"My printer", 400, "A queue name takes letters, digits, dot, dash and underscore, without spaces."},
		{"", 400, "A queue name takes letters, digits, dot, dash and underscore, without spaces."},
		{"Büro", 400, "A queue name takes letters, digits, dot, dash and underscore, without spaces."},
		{"a/b", 400, "A queue name takes letters, digits, dot, dash and underscore, without spaces."},
		{strings.Repeat("x", 128), 400, "A queue name takes letters, digits, dot, dash and underscore, without spaces."},
		{"HALL", 409, "There is a queue named HALL already."},
		{"virtual-mfc", 409, "There is a queue named virtual-mfc already."},
		{"ravnix-q", 403, "ravnix-q is declared in ravnix; add it there."},
	} {
		rec := postTo(s, h, "/printers/add", addForm("name", tc.name))
		if rec.Code != tc.status || rec.Body.String() != tc.text {
			t.Errorf("name %q: %d %q, want %d %q", tc.name, rec.Code, rec.Body.String(), tc.status, tc.text)
		}
	}
	if got := f.added(); len(got) != 0 {
		t.Errorf("CUPS was asked to add %+v", got)
	}
	if n := len(events(t, s, 5)); n != 0 {
		t.Errorf("%d events logged for refusals that never reached CUPS", n)
	}
}

func TestAddAcceptsTheNamesCUPSTakes(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	for _, name := range []string{"A_b.c-1", "x", strings.Repeat("y", 127)} {
		if rec := postTo(s, h, "/printers/add", addForm("name", name)); rec.Code != 200 {
			t.Errorf("name %q: %d %q", name, rec.Code, rec.Body.String())
		}
	}
	if got := f.added(); len(got) != 3 {
		t.Errorf("added %d queues, want 3", len(got))
	}
}

func TestAddChecksTheAddress(t *testing.T) {
	const notAddress = "That is not a printer address. It looks like ipp://printer.local/ipp/print."
	const scheme = "skrá adds printers by network or USB address."
	const own = "That address is this machine's own CUPS."
	const numeric = "Write the address as a full IPv4 address, a host name, or an IPv6 address in brackets."
	f := queuesFake()
	s, h := actionServer(t, f)
	bad := []struct{ addr, text string }{
		{"file:///etc/passwd", scheme},
		{"file:/etc/passwd", scheme},
		{"javascript:alert(1)", scheme},
		{"/var/spool/cups/x", notAddress},
		{"hp", notAddress},
		{"printer.local", notAddress},
		{"", notAddress},
		{"ipp://", notAddress},
		{"ipp://:631/x", notAddress},
		{"ipp://host/a b", notAddress},
		{"ipp://host/\x01", notAddress},
		{"ipp://host\n/x", notAddress},
		{"ipp://" + strings.Repeat("h", 1100), notAddress},
		{"ipp://localhost/printers/Hall", own},
		{"ipp://localhost:631/printers/Hall", own},
		{"IPPS://127.0.0.1:631/printers/Hall", own},
		{"IPP://localhost", own},
		{"ipp://localhost./printers/Hall", own},
		{"ipp://foo.localhost/printers/Hall", own},
		{"ipp://user:pw@localhost/printers/Hall", own},
		{"ipp://[::1]/printers/Hall", own},
		{"ipp://[::ffff:127.0.0.1]/printers/Hall", own},
		{"ipp://[::1%25lo]/printers/Hall", own},
		{"ipp://[::]:631/", own},
		{"ipp://0.0.0.0/", own},
		{"ipp://localhost:0631/printers/Hall", own},
		{"http://localhost:631/printers/Hall", own},
		{"https://127.0.0.1:631/printers/Hall", own},
		{"http://127.0.0.1/", own},
		// A percent-encoded host is what CUPS decodes before it connects.
		{"ipp://local%68ost/printers/x", notAddress},
		{"ipp://%31%32%37.0.0.1:631/printers/x", notAddress},
		{"ipp://127%2e0.0.1/printers/x", notAddress},
		{"ipp://local%25host/", notAddress},
		{"http://local%68ost:631/", notAddress},
		{"socket://%31%32%37.0.0.1:631", notAddress},
		// Escapes that decode to non-ASCII, and non-ASCII written out: the system
		// resolver may fold these to localhost. International names are punycode.
		{"ipp://%EF%BD%8C%EF%BD%8F%EF%BD%83%EF%BD%81%EF%BD%8C%EF%BD%88%EF%BD%8F%EF%BD%93%EF%BD%94/printers/x", notAddress},
		{"ipp://local%E2%80%8Bhost/", notAddress},
		{"ipp://ｌｏｃａｌｈｏｓｔ/", notAddress},
		{"ipp://Drucker-für-alle.local/ipp/print", notAddress},
		{"socket://Fu\u00dfball.local:9100", notAddress},
		{"ipp://user:pw@local%68ost/", notAddress},
		// CUPS cuts the user name at the first @ whatever comes before it; Go stops
		// at ? and #. A ? or # before the first / is not an address.
		{"ipp://x:1?@127.0.0.1:631/printers/q", notAddress},
		{"ipp://x:1#@localhost/printers/q", notAddress},
		{"socket://x:9100?@127.0.0.1:631", notAddress},
		{"ipp://admin:1234#x@printer.local/ipp/print", notAddress},
		{"ipp://neu.local?x=1", notAddress},
		{"ipp://neu.local:631#frag", notAddress},
		{"lpd://x?@127.0.0.1:631/q", notAddress},
		// CUPS cuts the user name at the first @, Go at the last: with more than
		// one @ in the authority they read different hosts.
		{"ipp://a@b@localhost/", notAddress},
		{"ipp://u:p@127.0.0.1:631@printer.local/", notAddress},
		{"socket://a@b@host:9100", notAddress},
		{"ipp://a@@neu.local/ipp/print", notAddress},
		// A number that is not a full dotted-quad address is read as one by the system.
		{"ipp://127.1/", numeric},
		{"ipp://0/", numeric},
		{"ipp://2130706433/", numeric},
		{"ipp://0x7f.0.0.1/", numeric},
		{"ipp://127.0.1/", numeric},
		{"ipp://010.0.0.1/", numeric},
		{"socket://127.1:9100", numeric},
		{"ipp://0x7f000001:631/", numeric},
	}
	for _, tc := range bad {
		rec := postTo(s, h, "/printers/add", addForm("address", tc.addr))
		if rec.Code != 400 || rec.Body.String() != tc.text {
			t.Errorf("address %q: %d %q, want 400 %q", tc.addr, rec.Code, rec.Body.String(), tc.text)
		}
		rec = get(h, "/printers/new?uri="+url.QueryEscape(tc.addr))
		if rec.Code != 400 || rec.Body.String() != tc.text {
			t.Errorf("panel for %q: %d %q, want 400 %q", tc.addr, rec.Code, rec.Body.String(), tc.text)
		}
	}
	if got := f.added(); len(got) != 0 {
		t.Errorf("CUPS was asked to add %+v", got)
	}

	f.set(func(f *fakeCUPS) {
		f.ppdsFn = func(string, string) ([]cups.PPD, error) {
			return []cups.PPD{{Name: "gen.ppd", MakeModel: "Generic"}}, nil
		}
	})
	for _, addr := range []string{
		"ipp://neu.local:631/ipp/print", "ipps://neu.local/ipp/print", "http://10.0.0.7:631/ipp/print", "https://neu.local/ipp/print",
		"dnssd://Brother%20DCP-L2530DW._ipp._tcp.local./?uuid=1", "dnssd://Fu%C3%9Fball._ipp._tcp.local./",
		"IPP://neu.local/ipp/print", "ipp://localhost:8631/ipp/print", "ipp://[fe80::1]:631/ipp/print",
		"ipp://[fe80::1%25eth0]:631/ipp/print", "ipp://3com.local/ipp/print", "ipp://192.168.1.5/ipp/print", "http://localhost:8631/ipp/print",
		"ipps://localhost:443/ipp/print", "ipp://user:p%40ss@neu.local/ipp/print", "ipp://neu.local/ipp%2Fprint?a=%C3%BC",
		"ipp://xn--drucker-fr-alle-4ob.local/ipp/print",
		"ipp://neu.local/ipp/print?a=b#c", "ipp://u:pass@neu.local/ipp/print", "ipp://neu.local/a@b@c", "ipp://neu.local:631/x?y=@z", "ipp://neu.local/ipp#@127.0.0.1:631",
	} {
		rec := postTo(s, h, "/printers/add", addForm("address", addr, "name", fmt.Sprintf("Q%d", len(f.added()))))
		if rec.Code != 200 {
			t.Errorf("address %q: %d %q", addr, rec.Code, rec.Body.String())
		}
	}
	for _, addr := range []string{"socket://10.0.0.7:9100", "lpd://10.0.0.7/queue", "usb://Brother/HL-L2350DW?serial=1", "usb:/dev/usb/lp0"} {
		rec := postTo(s, h, "/printers/add", addForm("address", addr, "driver", "choose", "ppd", "gen.ppd", "make", "Generic", "name", fmt.Sprintf("Q%d", len(f.added()))))
		if rec.Code != 200 {
			t.Errorf("address %q: %d %q", addr, rec.Code, rec.Body.String())
		}
	}
}

func TestAddDriverChoice(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	panel := func(uri string) string {
		t.Helper()
		rec := get(h, "/printers/new?uri="+url.QueryEscape(uri))
		if rec.Code != 200 {
			t.Fatalf("panel for %s: %d %q", uri, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	for _, uri := range []string{"socket://10.0.0.7:9100", "lpd://10.0.0.7/q", "usb://Brother/HL-L2350DW?serial=1"} {
		p := panel(uri)
		if !strings.Contains(p, `class="on" data-pick="choose"`) || strings.Contains(p, `data-pick="everywhere"`) || strings.Contains(p, "driverless") {
			t.Errorf("%s offers a driverless queue, or does not default to choosing a driver:\n%s", uri, p)
		}
		if strings.Contains(p, `id="ppd-row" hidden`) {
			t.Errorf("%s hides the driver list", uri)
		}
	}
	for _, uri := range []string{"ipp://neu.local/ipp/print", "ipps://neu.local/ipp/print", "dnssd://Neu._ipp._tcp.local./", "http://neu.local:631/ipp/print"} {
		p := panel(uri)
		if !regexp.MustCompile(`class="on" data-pick="everywhere"[^>]*>driverless<`).MatchString(p) || !strings.Contains(p, `data-pick="choose"`) {
			t.Errorf("%s does not default to driverless with a driver as the other choice:\n%s", uri, p)
		}
		if !strings.Contains(p, `id="ppd-row" hidden`) {
			t.Errorf("%s shows the driver list though driverless is on", uri)
		}
	}

	// The server holds to what the panel offers.
	for _, uri := range []string{"socket://10.0.0.7:9100", "lpd://10.0.0.7/q", "usb://Brother/HL-L2350DW?serial=1"} {
		rec := postTo(s, h, "/printers/add", addForm("address", uri))
		if rec.Code != 400 || rec.Body.String() != "Driverless works with IPP printers only. Choose a driver for this address." {
			t.Errorf("driverless for %s: %d %q", uri, rec.Code, rec.Body.String())
		}
	}
	if got := f.added(); len(got) != 0 {
		t.Errorf("CUPS was asked to add %+v", got)
	}
}

func TestAddPanelSuggestsAName(t *testing.T) {
	f := queuesFake()
	_, h := actionServer(t, f)
	for _, tc := range []struct{ query, name string }{
		{"uri=ipp://neu.local/ipp/print&info=Hall+Laser", `value="Hall-Laser"`},
		{"uri=ipp://neu.local/ipp/print&info=Hall", `value="Hall-2"`},
		{"uri=ipp://neu.local/ipp/print&info=Fu%C3%9Fball+%28Flur%29", `value="Fu-ball-Flur"`},
		{"uri=ipp://neu.local/ipp/print", `value="Printer"`},
		{"uri=ipp://neu.local/ipp/print&info=%3C%3E&model=Brother+HL", `value="Brother-HL"`},
	} {
		body := get(h, "/printers/new?"+tc.query).Body.String()
		if !regexp.MustCompile(`name="name"[^>]*` + regexp.QuoteMeta(tc.name)).MatchString(body) {
			t.Errorf("%s: the name field is not %s:\n%s", tc.query, tc.name, body)
		}
	}
}

func TestAddPanelEscapesWhatTheDeviceSays(t *testing.T) {
	f := queuesFake()
	_, h := actionServer(t, f)
	q := url.Values{"uri": {"ipp://neu.local/ipp/print?a=\"><script>x</script>"}, "info": {"<b>x</b>\" onfocus=\"y"}, "id": {"MFG:A;<i>;"}}
	body := get(h, "/printers/new?"+q.Encode()).Body.String()
	for _, raw := range []string{"<b>x</b>", "<script>x</script>", "<i>;", `onfocus="y`} {
		if strings.Contains(body, raw) {
			t.Errorf("the panel carries %q unescaped", raw)
		}
	}
	if !strings.Contains(body, "&lt;b&gt;x&lt;/b&gt;") {
		t.Errorf("the description is not shown escaped:\n%s", body)
	}
}

func TestAddPanelDefaultsToTheFirstQueueBeingTheDefault(t *testing.T) {
	_, h := actionServer(t, &fakeCUPS{})
	body := get(h, "/printers/new?uri=ipp://neu.local/ipp/print").Body.String()
	if !regexp.MustCompile(`data-group="default"[^>]*>.*class="on" data-pick="yes"`).MatchString(body) {
		t.Errorf("the first queue is not offered as the default:\n%s", body)
	}
	_, h = actionServer(t, queuesFake())
	body = get(h, "/printers/new?uri=ipp://neu.local/ipp/print").Body.String()
	if !regexp.MustCompile(`data-group="default"[^>]*>.*class="on" data-pick="no"`).MatchString(body) {
		t.Errorf("a later queue is offered as the default:\n%s", body)
	}
}

func TestAddDriverless(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/printers/add", addForm("name", " Neu ", "info", "", "shared", "yes", "default", "yes", "ppd", "ignored.ppd"))
	if rec.Code != 200 || rec.Body.String() != "Added Neu. Send it a test page from the overview." {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	got := f.added()
	want := cups.AddSpec{Name: "Neu", DeviceURI: "ipp://neu.local/ipp/print", Info: "Neu", Location: "Flur", Shared: true, Default: true}
	if len(got) != 1 || got[0] != want {
		t.Errorf("added %+v, want %+v", got, want)
	}
	ev := lastEvent(t, s)
	if ev.Action != "printer.add" || ev.Target != "Neu" || !ev.OK || ev.Actor != "mika" ||
		!strings.Contains(ev.Detail, "ipp://neu.local/ipp/print") || !strings.Contains(ev.Detail, "driverless") {
		t.Errorf("event = %+v", ev)
	}
}

func TestAddTakesTheDescriptionAndLocationAsTyped(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	if rec := postTo(s, h, "/printers/add", addForm("info", "  Brother im Büro  ", "location", " ")); rec.Code != 200 {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if got := f.added(); len(got) != 1 || got[0].Info != "Brother im Büro" || got[0].Location != "" {
		t.Errorf("added %+v", got)
	}
	for _, field := range []string{"info", "location"} {
		rec := postTo(s, h, "/printers/add", addForm("name", "Other", field, "two\nlines"))
		if rec.Code != 400 {
			t.Errorf("%s with a line break: %d %q", field, rec.Code, rec.Body.String())
		}
	}
	if got := f.added(); len(got) != 1 {
		t.Errorf("a refused form reached CUPS: %+v", got)
	}
}

func TestAddWithADriver(t *testing.T) {
	f := queuesFake()
	f.ppdsFn = func(id, make string) ([]cups.PPD, error) {
		if id == "MFG:Brother;MDL:HL;" {
			return []cups.PPD{{Name: "drv:///brother.drv/hl.ppd", MakeModel: "Brother HL"}, {Name: "drv:///brother.drv/hl2.ppd", MakeModel: "Brother HL2"}}, nil
		}
		return nil, nil
	}
	s, h := actionServer(t, f)
	form := addForm("address", "socket://10.0.0.7:9100", "driver", "choose", "id", "MFG:Brother;MDL:HL;", "make", "Brother", "ppd", "drv:///brother.drv/hl2.ppd")
	rec := postTo(s, h, "/printers/add", form)
	if rec.Code != 200 {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	got := f.added()
	if len(got) != 1 || got[0].PPD != "drv:///brother.drv/hl2.ppd" || got[0].DeviceURI != "socket://10.0.0.7:9100" {
		t.Errorf("added %+v", got)
	}
	if ev := lastEvent(t, s); !strings.Contains(ev.Detail, "socket://10.0.0.7:9100") || !strings.Contains(ev.Detail, "drv:///brother.drv/hl2.ppd") || strings.Contains(ev.Detail, "driverless") {
		t.Errorf("event = %+v", ev)
	}
	if asked := f.asked(); len(asked) == 0 || asked[0] != [2]string{"MFG:Brother;MDL:HL;", "Brother"} {
		t.Errorf("the drivers were asked for as %v", asked)
	}

	// A driver the list for this device lacks is never sent on, and neither is none.
	for _, tc := range []struct{ ppd, text string }{
		{"drv:///other.drv/x.ppd", "That driver is not one CUPS offers for this printer. Choose one from the list."},
		{"everywhere", "That driver is not one CUPS offers for this printer. Choose one from the list."},
		{"", "Choose a driver first."},
	} {
		rec := postTo(s, h, "/printers/add", addForm("name", "Other", "address", "socket://10.0.0.7:9100", "driver", "choose",
			"id", "MFG:Brother;MDL:HL;", "ppd", tc.ppd))
		if rec.Code != 400 || rec.Body.String() != tc.text {
			t.Errorf("ppd %q: %d %q, want 400 %q", tc.ppd, rec.Code, rec.Body.String(), tc.text)
		}
	}
	if len(f.added()) != 1 {
		t.Errorf("a refused driver reached CUPS: %+v", f.added())
	}

	// The same driver is no good for a form that names no device id and no maker.
	rec = postTo(s, h, "/printers/add", addForm("name", "Third", "address", "socket://10.0.0.7:9100", "driver", "choose", "ppd", "drv:///brother.drv/hl.ppd"))
	if rec.Code != 400 {
		t.Errorf("a driver without the device it was listed for: %d %q", rec.Code, rec.Body.String())
	}
	rec = postTo(s, h, "/printers/add", addForm("name", "Fourth", "driver", "sideways"))
	if rec.Code != 400 {
		t.Errorf("an unknown driver choice: %d %q", rec.Code, rec.Body.String())
	}
}

func TestAddRefusesWhatTheFormDoesNotSay(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	for _, kv := range [][]string{{"shared", "maybe"}, {"default", "1"}} {
		if rec := postTo(s, h, "/printers/add", addForm(kv...)); rec.Code != 400 {
			t.Errorf("%v: %d %q", kv, rec.Code, rec.Body.String())
		}
	}
	if len(f.added()) != 0 {
		t.Error("a bad form reached CUPS")
	}
}

func TestAddWhenCUPSRefuses(t *testing.T) {
	f := queuesFake()
	f.addErr = errors.New("The printer did not answer.")
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/printers/add", addForm())
	if rec.Code != 502 || rec.Body.String() != "The printer did not answer." {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	ev := lastEvent(t, s)
	if ev.Action != "printer.add" || ev.Target != "Neu" || ev.OK || !strings.Contains(ev.Detail, "ipp://neu.local/ipp/print") || !strings.Contains(ev.Detail, "The printer did not answer.") {
		t.Errorf("event = %+v", ev)
	}
}

func TestAddWhenTheListOfQueuesCannotBeRead(t *testing.T) {
	f := queuesFake()
	f.err = errors.New("CUPS does not answer: boom")
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/printers/add", addForm())
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("%d %q", rec.Code, rec.Body.String())
	}
	if len(f.added()) != 0 {
		t.Error("a name was not checked against a list that could not be read, yet the queue was added")
	}
}

func TestAddSaysTheQueueExistsWhenOnlyTheDefaultFailed(t *testing.T) {
	f := queuesFake()
	f.addErr = &cups.SetDefaultError{Err: errors.New("Not allowed.")}
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/printers/add", addForm("default", "yes"))
	if rec.Code != 502 || rec.Body.String() != "Added Neu, but making it the default failed: Not allowed. Make it the default from the table." {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Skra-Added") != "true" {
		t.Error("the answer does not tell the page that the queue exists")
	}
	if ev := lastEvent(t, s); ev.OK || !strings.Contains(ev.Detail, "Not allowed.") {
		t.Errorf("event = %+v", ev)
	}
	// A plain failure does not say so.
	f.addErr = errors.New("nope")
	if rec := postTo(s, h, "/printers/add", addForm("name", "Other")); rec.Header().Get("X-Skra-Added") != "" {
		t.Error("a failed add says the queue exists")
	}
}

func TestAddRefreshesOpenPages(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	old := pollEvery
	pollEvery = time.Hour // only a Refresh can produce the second snapshot
	defer func() { pollEvery = old }()
	s.hub = NewHub(s.tickPoll, pollEvery)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.hub.Subscribe(ctx)
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no first snapshot")
	}
	f.set(func(f *fakeCUPS) { f.printers = append(f.printers, cups.Printer{Name: "Neu", Info: "Neues Gerät"}) })
	postTo(s, h, "/printers/add", addForm("name", "Neu2"))
	select {
	case snap := <-ch:
		if len(snap.Printers) != 3 {
			t.Errorf("snapshot after the add has %d queues", len(snap.Printers))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the add did not refresh the open page")
	}
}

func TestSearchShowsCountdown(t *testing.T) {
	f := queuesFake() // Hall is at ipp://hall.local/ipp/print
	f.devices = []cups.Device{
		{URI: "ipp://hall.local/ipp/print", Info: "Hall Laser", MakeModel: "Hall Laser 3000", Class: "network"},
		{URI: "dnssd://Neue%20Karte._ipp._tcp.local./?uuid=7", Info: "<b>Neue</b> Karte", MakeModel: "Brother HL-L2350DW", ID: "MFG:Brother;MDL:HL-L2350DW;", Class: "network"},
		{URI: "usb://Brother/HL-L2350DW?serial=1", Class: "direct"},
		{URI: "hp", Info: "HP Printer (HPLIP)"},
	}
	s, h := actionServer(t, f)

	// The page counts down the window the server asks CUPS for.
	page := get(h, "/printers").Body.String()
	for _, want := range []string{`data-act="search"`, `data-seconds="10"`, `id="devices"`, `id="manual-uri"`, `data-act="add-manual"`, `id="add-panel"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the printers page lacks %q", want)
		}
	}

	start := time.Now()
	rec := postTo(s, h, "/printers/search", nil)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	if time.Since(start) >= 10*time.Second {
		t.Error("the search outlasted its window")
	}
	f.mu.Lock()
	timeout, left := f.devTimeout, f.devLeft
	f.mu.Unlock()
	if timeout != 10*time.Second {
		t.Errorf("CUPS was given a timeout of %v, want 10s", timeout)
	}
	if left <= 10*time.Second || left > 15*time.Second {
		t.Errorf("the request context had %v left, want between 10s and 15s", left)
	}
	body := rec.Body.String()
	if strings.Count(body, "has a queue") != 1 {
		t.Errorf("%d devices marked as having a queue, want 1:\n%s", strings.Count(body, "has a queue"), body)
	}
	hall := rowWith(t, body, "Hall Laser")
	if !strings.Contains(hall, "has a queue") || !strings.Contains(hall, "Hall") || !strings.Contains(hall, ">add again<") {
		t.Errorf("Hall's row = %s", hall)
	}
	neue := rowWith(t, body, "Karte")
	if strings.Contains(neue, "has a queue") || !strings.Contains(neue, "none yet") || !strings.Contains(neue, ">add<") ||
		!strings.Contains(neue, `data-device="dnssd://Neue%20Karte._ipp._tcp.local./?uuid=7"`) || !strings.Contains(neue, `data-id="MFG:Brother;MDL:HL-L2350DW;"`) ||
		!strings.Contains(neue, `data-model="Brother HL-L2350DW"`) || !strings.Contains(neue, "network") {
		t.Errorf("the row of the new device = %s", neue)
	}
	if strings.Contains(body, "<b>Neue</b>") || !strings.Contains(body, "&lt;b&gt;Neue&lt;/b&gt;") {
		t.Errorf("a device name is not escaped:\n%s", body)
	}
	if !strings.Contains(rowWith(t, body, "usb://Brother/HL-L2350DW?serial=1"), "USB") {
		t.Error("a device without a description is not shown by its address, as USB")
	}
	if strings.Contains(body, "HPLIP") || strings.Contains(body, ">hp<") {
		t.Error("a backend placeholder without a scheme is listed")
	}
}

// rowWith returns the table row that holds text.
func rowWith(t *testing.T, body, text string) string {
	t.Helper()
	for _, row := range strings.SplitAfter(body, "</tr>") {
		if strings.Contains(row, text) {
			return row
		}
	}
	t.Fatalf("no row with %q in\n%s", text, body)
	return ""
}

func TestSearchWithNothingFoundOrCUPSDown(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/printers/search", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "No printer answered. Check that it is on and on this network, or enter its address below.") ||
		strings.Contains(rec.Body.String(), "<table") {
		t.Errorf("%d %q", rec.Code, rec.Body.String())
	}
	f.devErr = errors.New("CUPS does not answer: boom")
	rec = postTo(s, h, "/printers/search", nil)
	if rec.Code != 502 || rec.Body.String() != "CUPS does not answer: boom" {
		t.Errorf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestDriversListSortedAndCapped(t *testing.T) {
	f := queuesFake()
	f.ppdsFn = func(id, make string) ([]cups.PPD, error) {
		var out []cups.PPD
		for i := 350; i > 0; i-- { // CUPS's order is not the display order
			out = append(out, cups.PPD{Name: fmt.Sprintf("drv:///m.drv/%03d.ppd", i), MakeModel: fmt.Sprintf("Maker Model %03d", i)})
		}
		return out, nil
	}
	_, h := actionServer(t, f)
	rec := get(h, "/printers/drivers?"+url.Values{"id": {"MFG:Maker;"}, "make": {"Maker"}}.Encode())
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	body := rec.Body.String()
	if n := strings.Count(body, "<option"); n != 300 {
		t.Errorf("%d options, want 300", n)
	}
	if !strings.Contains(body, "first 300 of 350") || !strings.Contains(body, `name="ppd"`) {
		t.Errorf("the list does not say it is cut:\n%.400s", body)
	}
	first, last := strings.Index(body, "Maker Model 001"), strings.Index(body, "Maker Model 300")
	if first < 0 || last < first || strings.Contains(body, "Maker Model 301") {
		t.Errorf("not sorted by model, or not cut at 300 after sorting (%d, %d)", first, last)
	}
	if asked := f.asked(); len(asked) != 1 || asked[0] != [2]string{"MFG:Maker;", "Maker"} {
		t.Errorf("CUPS was asked %v", asked)
	}
}

func TestDriversFallBackToTheMakerAndExplainAnEmptyList(t *testing.T) {
	f := queuesFake()
	f.ppdsFn = func(id, make string) ([]cups.PPD, error) {
		if id == "" && make == "Brother" {
			return []cups.PPD{{Name: "b.ppd", MakeModel: "Brother <HL>"}}, nil
		}
		return nil, nil
	}
	_, h := actionServer(t, f)
	body := get(h, "/printers/drivers?"+url.Values{"id": {"MFG:Brother;MDL:New;"}, "make": {"Brother"}}.Encode()).Body.String()
	if !strings.Contains(body, `value="b.ppd"`) || !strings.Contains(body, "Brother &lt;HL&gt;") || !strings.Contains(body, "1 found") {
		t.Errorf("no fall back from the device id to the maker:\n%s", body)
	}
	if asked := f.asked(); len(asked) != 2 || asked[1] != [2]string{"", "Brother"} {
		t.Errorf("CUPS was asked %v", asked)
	}

	body = get(h, "/printers/drivers?make=Nobody").Body.String()
	if strings.Contains(body, "<option") || !strings.Contains(body, "CUPS has no driver for this model.") {
		t.Errorf("an empty list is not explained:\n%s", body)
	}
}

func TestDriversNeedAMakerOrADeviceID(t *testing.T) {
	f := queuesFake()
	_, h := actionServer(t, f)
	rec := get(h, "/printers/drivers")
	if rec.Code != 200 || !strings.Contains(html.UnescapeString(rec.Body.String()), "Type the maker's name to list its drivers.") {
		t.Errorf("%d %q", rec.Code, rec.Body.String())
	}
	if asked := f.asked(); len(asked) != 0 {
		t.Errorf("CUPS was asked for all its drivers: %v", asked)
	}
}

func TestDriversWhenCUPSFails(t *testing.T) {
	f := queuesFake()
	f.ppdsFn = func(string, string) ([]cups.PPD, error) { return nil, errors.New("CUPS does not answer: boom") }
	_, h := actionServer(t, f)
	if rec := get(h, "/printers/drivers?make=Brother"); rec.Code != 502 || rec.Body.String() != "CUPS does not answer: boom" {
		t.Errorf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestAddRoutesNeedTheToken(t *testing.T) {
	f := queuesFake()
	_, h := actionServer(t, f)
	for _, path := range []string{"/printers/search", "/printers/add"} {
		req := httptest.NewRequest("POST", path, nil)
		req.Host = "127.0.0.1:7631"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("POST %s without a token: status %d, want 403", path, rec.Code)
		}
	}
	if len(f.added()) != 0 || f.devTimeout != 0 {
		t.Error("CUPS was asked")
	}
}

func TestAddKeepsCredentialsOutOfTheLogAndTheAnswers(t *testing.T) {
	const addr = "ipp://mika:s3cret@neu.local/ipp/print"
	f := queuesFake()
	s, h := actionServer(t, f)

	// The address CUPS gets is the one typed.
	if rec := postTo(s, h, "/printers/add", addForm("address", addr)); rec.Code != 200 || strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if got := f.added(); len(got) != 1 || got[0].DeviceURI != addr {
		t.Errorf("added %+v, want the address as typed", got)
	}
	ev := lastEvent(t, s)
	if strings.Contains(ev.Detail, "s3cret") || strings.Contains(ev.Detail, "mika:") || !strings.Contains(ev.Detail, "ipp://neu.local/ipp/print") {
		t.Errorf("event detail = %q", ev.Detail)
	}

	// A CUPS error that quotes the address, plain or with the part before the @.
	for _, msg := range []string{"Bad device-uri \"" + addr + "\".", "Cannot reach mika:s3cret@neu.local."} {
		f.addErr = errors.New(msg)
		rec := postTo(s, h, "/printers/add", addForm("name", "Other", "address", addr))
		if rec.Code != 502 || strings.Contains(rec.Body.String(), "s3cret") || !strings.Contains(rec.Body.String(), "neu.local") {
			t.Errorf("%q: answer %d %q", msg, rec.Code, rec.Body.String())
		}
		if ev := lastEvent(t, s); ev.OK || strings.Contains(ev.Detail, "s3cret") {
			t.Errorf("%q: event %+v", msg, ev)
		}
	}
	f.addErr = &cups.SetDefaultError{Err: errors.New("no default for " + addr)}
	rec := postTo(s, h, "/printers/add", addForm("name", "Third", "address", addr, "default", "yes"))
	if rec.Code != 502 || strings.Contains(rec.Body.String(), "s3cret") {
		t.Errorf("default failure: %d %q", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, s); strings.Contains(ev.Detail, "s3cret") {
		t.Errorf("default failure event %+v", ev)
	}

	// The panel shows the address without them.
	body := get(h, "/printers/new?"+url.Values{"uri": {addr}}.Encode()).Body.String()
	head := regexp.MustCompile(`<h3>.*</h3>`).FindString(body)
	if head == "" || strings.Contains(head, "s3cret") || !strings.Contains(head, "ipp://neu.local/ipp/print") {
		t.Errorf("panel heading = %q", head)
	}
}

// ownInterfaceAddr is an address of one of this machine's interfaces, or a skip.
func ownInterfaceAddr(t *testing.T) netip.Addr {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip(err)
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			if ip, ok := netip.AddrFromSlice(ipn.IP); ok {
				return ip.Unmap()
			}
		}
	}
	t.Skip("no interface address besides loopback")
	return netip.Addr{}
}

func TestAddRefusesANameThatPointsAtThisMachinesCUPS(t *testing.T) {
	const own = "That address is this machine's own CUPS."
	mine := ownInterfaceAddr(t)
	public := netip.MustParseAddr("93.184.216.34")
	withNames(t, map[string][]netip.Addr{
		"alias.example":  {netip.MustParseAddr("127.0.0.1")},
		"mapped.example": {netip.MustParseAddr("::ffff:127.0.0.1")},
		"six.example":    {netip.MustParseAddr("::1")},
		"zero.example":   {netip.MustParseAddr("0.0.0.0")},
		"me.example":     {mine},
		"mixed.example":  {public, netip.MustParseAddr("127.0.0.1")},
		"far.example":    {public},
	})
	s, h := actionServer(t, queuesFake())
	for _, tc := range []struct {
		addr string
		code int
	}{
		{"ipp://alias.example/printers/x", 400}, // no port: 631
		{"ipps://alias.example/printers/x", 400},
		{"http://alias.example/printers/x", 400},
		{"https://alias.example/printers/x", 400},
		{"ipp://alias.example:631/printers/x", 400},
		{"ipp://alias.example:0631/printers/x", 400},
		{"ipp://mapped.example/x", 400},
		{"ipp://six.example/x", 400},
		{"ipp://zero.example/x", 400},
		{"ipp://me.example/x", 400},
		{"ipp://mixed.example/x", 400}, // any address of the name
		{"socket://alias.example:631", 400},
		{"lpd://alias.example:631/q", 400},
		{"ipp://alias.example:8631/ipp/print", 200}, // another port is another service
		{"ipp://alias.example:60000/ipp/print", 200},
		{"socket://alias.example", 200}, // 9100
		{"lpd://alias.example/q", 200},  // 515
		{"ipp://far.example/ipp/print", 200},
		{"ipp://unknown.example/ipp/print", 200}, // does not resolve: the printer may be away
	} {
		form := addForm("address", tc.addr, "name", "N"+strings.NewReplacer("/", "", ":", "", ".", "").Replace(tc.addr))
		if !strings.HasPrefix(tc.addr, "ip") && !strings.HasPrefix(tc.addr, "http") {
			form.Set("driver", "choose")
			form.Set("make", "Generic")
			form.Set("ppd", "gen.ppd")
		}
		f := s.deps.CUPS.(*fakeCUPS)
		f.set(func(f *fakeCUPS) {
			f.ppdsFn = func(string, string) ([]cups.PPD, error) {
				return []cups.PPD{{Name: "gen.ppd", MakeModel: "Generic"}}, nil
			}
		})
		rec := postTo(s, h, "/printers/add", form)
		if rec.Code != tc.code || (tc.code == 400 && rec.Body.String() != own) {
			t.Errorf("%s: %d %q, want %d", tc.addr, rec.Code, rec.Body.String(), tc.code)
		}
		if panel := get(h, "/printers/new?uri="+url.QueryEscape(tc.addr)); panel.Code != tc.code {
			t.Errorf("panel for %s: %d %q, want %d", tc.addr, panel.Code, panel.Body.String(), tc.code)
		}
	}
}

func TestAddLooksNoNameUpWhereItCannotMatter(t *testing.T) {
	s, h := actionServer(t, queuesFake())
	lookups.Store(0)
	for _, addr := range []string{"dnssd://Neu._ipp._tcp.local./", "usb://Brother/HL?serial=1", "ipp://10.0.0.7/ipp/print", "ipp://[fe80::1]/ipp/print"} {
		get(h, "/printers/new?uri="+url.QueryEscape(addr))
	}
	if n := lookups.Load(); n != 0 {
		t.Errorf("%d lookups for a dnssd or usb address or an address literal", n)
	}
	_ = s
}

func TestAddDoesNotWaitLongForASlowResolver(t *testing.T) {
	oldLookup, oldTimeout := lookupAddrs, lookupTimeout
	lookupAddrs = func(ctx context.Context, host string) ([]netip.Addr, error) {
		<-ctx.Done() // a resolver that never answers
		return nil, ctx.Err()
	}
	lookupTimeout = 50 * time.Millisecond
	defer func() { lookupAddrs, lookupTimeout = oldLookup, oldTimeout }()
	_, h := actionServer(t, queuesFake())
	start := time.Now()
	rec := get(h, "/printers/new?uri="+url.QueryEscape("ipp://slow.example/ipp/print"))
	if rec.Code != 200 {
		t.Errorf("a name that did not resolve in time: %d %q", rec.Code, rec.Body.String())
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v", took)
	}
}

// TestAddRefusesThisMachinesOwnNames uses the machine's real resolver: the
// hosts-file aliases and the machine's name. Each is skipped when it does not
// resolve to this machine here.
func TestAddRefusesThisMachinesOwnNames(t *testing.T) {
	lookupAddrs = realLookup
	defer func() { lookupAddrs = fakeLookup }()
	host, _ := os.Hostname()
	_, h := actionServer(t, queuesFake())
	tried := 0
	for _, name := range []string{"localhost.localdomain", "localhost4", "localhost6.localdomain6", host} {
		if name == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", name)
		cancel()
		own := false
		for _, ip := range ips {
			own = own || printer.IsOwnAddress(ip.Unmap())
		}
		if err != nil || !own {
			t.Logf("%s does not resolve to this machine here; skipped", name)
			continue
		}
		tried++
		for _, addr := range []string{"ipp://" + name + "/printers/x", "ipp://" + name + ":631/printers/x", "http://" + name + ":631/"} {
			rec := get(h, "/printers/new?uri="+url.QueryEscape(addr))
			if rec.Code != 400 || rec.Body.String() != "That address is this machine's own CUPS." {
				t.Errorf("%s: %d %q", addr, rec.Code, rec.Body.String())
			}
		}
		if rec := get(h, "/printers/new?uri="+url.QueryEscape("ipp://"+name+":8631/ipp/print")); rec.Code != 200 {
			t.Errorf("%s on another port: %d %q", name, rec.Code, rec.Body.String())
		}
	}
	if tried == 0 {
		t.Skip("none of the local names resolves here")
	}
}

func TestWithoutUserinfoKeepsTheRestOfTheAddress(t *testing.T) {
	for in, want := range map[string]string{
		"ipp://u:p@h/":                                     "ipp://h/",
		"ipp://u:p@host-p.local/p/print":                   "ipp://host-p.local/p/print",
		"ipp://u:neu@neu.local/neu/print":                  "ipp://neu.local/neu/print",
		"ipp://u@h/x@y":                                    "ipp://h/x@y",
		"ipp://a@b@localhost/":                             "ipp://localhost/",
		"ipp://u:p%40q@h:631/ipp?x=@":                      "ipp://h:631/ipp?x=@",
		"dnssd://u:p@Neu%20Karte._ipp._tcp.local./?uuid=1": "dnssd://Neu%20Karte._ipp._tcp.local./?uuid=1",
		"ipp://h/no/userinfo":                              "ipp://h/no/userinfo",
		"dnssd://Neu%20Karte._ipp._tcp.local./":            "dnssd://Neu%20Karte._ipp._tcp.local./",
		"ipp://[bad":                                       "(address withheld)",
		"ipp://h:xx/":                                      "(address withheld)",
	} {
		if got := withoutUserinfo(in); got != want {
			t.Errorf("withoutUserinfo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScrubHidesTheCredentialsWithoutGarblingTheText(t *testing.T) {
	// A one-letter password: the text may still hold the letter elsewhere.
	const addr = "ipp://u:p@host-p.local/p/print"
	got := scrub("Cannot add \""+addr+"\": printer p says no (u:p@ is wrong)", addr)
	if want := "Cannot add \"ipp://host-p.local/p/print\": printer p says no ( is wrong)"; got != want {
		t.Errorf("scrub = %q, want %q", got, want)
	}
}

func TestAddLogsAnAddressWithAShortPasswordIntact(t *testing.T) {
	for _, tc := range []struct{ addr, shown string }{
		{"ipp://u:p@host-p.local/p/print", "ipp://host-p.local/p/print"},
		{"ipp://u:neu@neu.local/neu/print", "ipp://neu.local/neu/print"},
		{"ipp://u:i@ipp.local/i/ipp", "ipp://ipp.local/i/ipp"},
	} {
		f := queuesFake()
		s, h := actionServer(t, f)
		rec := postTo(s, h, "/printers/add", addForm("address", tc.addr))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %q", tc.addr, rec.Code, rec.Body.String())
		}
		if got := f.added(); len(got) != 1 || got[0].DeviceURI != tc.addr {
			t.Errorf("%s: CUPS got %+v", tc.addr, got)
		}
		if ev := lastEvent(t, s); ev.Detail != tc.shown+" with driverless" {
			t.Errorf("%s: detail = %q, want %q", tc.addr, ev.Detail, tc.shown+" with driverless")
		}
		// And an error that quotes the address, once the address has gone through scrub.
		f.addErr = errors.New("Bad device-uri \"" + tc.addr + "\".")
		rec = postTo(s, h, "/printers/add", addForm("name", "Other", "address", tc.addr))
		if rec.Body.String() != "Bad device-uri \""+tc.shown+"\"." {
			t.Errorf("%s: answer %q", tc.addr, rec.Body.String())
		}
		if ev := lastEvent(t, s); ev.Detail != tc.shown+" with driverless: Bad device-uri \""+tc.shown+"\"." {
			t.Errorf("%s: failed detail = %q", tc.addr, ev.Detail)
		}
	}
}
