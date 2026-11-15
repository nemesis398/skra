// SPDX-License-Identifier: AGPL-3.0-or-later

package printer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"testing"
)

// No test in this file ever connects to port 631. The tests of the dialer
// guard wrap its Control step: the wrapper asks the real refuseOwnCUPS, records
// its verdict, and then never lets a connect() happen. If the guard breaks,
// a test fails on the recorded verdict instead of reaching the real CUPS.

var errStopped = errors.New("test: stopped before connect()")

type verdict struct {
	address string
	err     error // nil: the guard would have let the connection through
}

type recorder struct {
	mu  sync.Mutex
	got []verdict
}

// control is the wrapper: it records the guard's verdict and returns it when it
// is a refusal, and a stop otherwise. It never returns nil.
func (r *recorder) control(network, address string, c syscall.RawConn) error {
	err := refuseOwnCUPS(network, address, c)
	r.mu.Lock()
	r.got = append(r.got, verdict{address, err})
	r.mu.Unlock()
	if err != nil {
		return err
	}
	return errStopped
}

func (r *recorder) verdicts() []verdict {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]verdict(nil), r.got...)
}

// wrapped swaps the status clients for ones whose dialer is wrapped, for the test.
func wrapped(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{}
	oldP, oldT := plainClient.Transport, tlsClient.Transport
	plainClient.Transport, tlsClient.Transport = newTransport(r.control, false), newTransport(r.control, true)
	t.Cleanup(func() { plainClient.Transport, tlsClient.Transport = oldP, oldT })
	return r
}

// interfaceAddr returns an address of one of this machine's interfaces that is
// not loopback, or skips the test.
func interfaceAddr(t *testing.T) netip.Addr {
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

func TestIsOwnAddress(t *testing.T) {
	mine := interfaceAddr(t)
	for addr, want := range map[netip.Addr]bool{
		netip.MustParseAddr("127.0.0.1"):                 true,
		netip.MustParseAddr("127.1.2.3"):                 true,
		netip.MustParseAddr("::1"):                       true,
		netip.MustParseAddr("0.0.0.0"):                   true,
		netip.MustParseAddr("::"):                        true,
		netip.MustParseAddr("::ffff:127.0.0.1"):          true,
		netip.MustParseAddr("::ffff:0.0.0.0"):            true,
		netip.MustParseAddr("::127.0.0.1"):               true, // IPv4-compatible
		netip.MustParseAddr("::7f00:1"):                  true,
		netip.MustParseAddr("::ffff:0:127.0.0.1"):        true, // IPv4-translated
		netip.MustParseAddr("::ffff:0:7f00:1"):           true,
		netip.MustParseAddr("::" + mine.String()):        mine.Is4(),
		netip.MustParseAddr("::ffff:0:" + mine.String()): mine.Is4(),
		mine: true,
		netip.MustParseAddr("::ffff:" + mine.String()): mine.Is4(),
		netip.MustParseAddr("93.184.216.34"):           false,
		netip.MustParseAddr("2001:db8::5"):             false,
		netip.MustParseAddr("::ffff:93.184.216.34"):    false,
		netip.MustParseAddr("::10.0.0.5"):              false,
		netip.MustParseAddr("::ffff:0:93.184.216.34"):  false,
		{}: false,
	} {
		if got := IsOwnAddress(addr); got != want {
			t.Errorf("IsOwnAddress(%v) = %v, want %v", addr, got, want)
		}
	}
}

// The verdict of the guard for an address Go hands to Control. It is a plain
// function, so these cases dial nothing.
func TestGuardVerdicts(t *testing.T) {
	mine := interfaceAddr(t)
	for _, addr := range []string{
		"127.0.0.1:631", "127.5.5.5:631", "[::1]:631", "0.0.0.0:631", "[::]:631", "[::ffff:127.0.0.1]:631",
		net.JoinHostPort(mine.String(), "631"),
		":631", // what Go passes for an empty host; the kernel connects it to [::]:631, which is ::1
		"[::127.0.0.1]:631", "[::ffff:0:127.0.0.1]:631", "[::7f00:1]:631", "[::ffff:0:7f00:1]:631",
		"[::" + mine.String() + "]:631",
		// What cannot be read is refused, never let through.
		"localhost:631", "printer.local:631", "127.0.0.1", "", "631", "127.0.0.1:x:631",
		"127.0.0.1:", "127.0.0.1:631x", "127.0.0.1:-631",
	} {
		if err := refuseOwnCUPS("tcp", addr, nil); !errors.Is(err, errOwnCUPS) {
			t.Errorf("%q: verdict %v, want a refusal", addr, err)
		}
	}
	for _, addr := range []string{
		"127.0.0.1:8631", "127.0.0.1:60631", "127.0.0.1:80", "[::1]:6310", net.JoinHostPort(mine.String(), "8631"),
		"93.184.216.34:631", "[2001:db8::5]:631", "[::ffff:93.184.216.34]:631", "[::10.0.0.5]:631", "[::ffff:0:93.184.216.34]:631",
		":8631", "[fe80::1%eth0]:631",
	} {
		if err := refuseOwnCUPS("tcp", addr, nil); err != nil {
			t.Errorf("%q: verdict %v, want it let through", addr, err)
		}
	}
}

// Without the list of the machine's own addresses nothing can be told apart
// from it, so port 631 is refused for every address.
func TestGuardFailsClosedWithoutTheInterfaceList(t *testing.T) {
	old := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) { return nil, errors.New("netlink failed") }
	defer func() { interfaceAddrs = old }()
	for _, addr := range []string{"93.184.216.34:631", "[2001:db8::5]:631", "10.0.0.5:631"} {
		if err := refuseOwnCUPS("tcp", addr, nil); !errors.Is(err, errOwnCUPS) {
			t.Errorf("%q: verdict %v, want a refusal", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:8631", "93.184.216.34:8631"} {
		if err := refuseOwnCUPS("tcp", addr, nil); err != nil {
			t.Errorf("%q: other ports are not CUPS, verdict %v", addr, err)
		}
	}
	if !IsOwnAddress(netip.MustParseAddr("93.184.216.34")) {
		t.Error("IsOwnAddress says an address is not this machine's when the list is unavailable")
	}
}

// TestQueryRefusesANameThatOnlyTheDialSeesIsLocal covers the names no string
// check can know: hosts-file aliases and this machine's own name. Each reaches
// 127.0.0.1 or ::1 at dial time; the wrapper records the guard's verdict and
// stops before any connect().
func TestQueryRefusesANameThatOnlyTheDialSeesIsLocal(t *testing.T) {
	rec := wrapped(t)
	host, _ := os.Hostname()
	tried := 0
	for _, name := range []string{"localhost.localdomain", "localhost4", "localhost6.localdomain6", host} {
		if name == "" {
			continue
		}
		ips, err := net.LookupIP(name)
		own := false
		for _, ip := range ips {
			if a, ok := netip.AddrFromSlice(ip); ok && IsOwnAddress(a.Unmap()) {
				own = true
			}
		}
		if err != nil || !own {
			t.Logf("%s does not resolve to this machine here; skipped", name)
			continue
		}
		tried++
		uri := "ipp://" + name + ":631/printers/x"
		r := &fakeResolver{uri: uri}
		for _, via := range []string{uri, unique("dnssd://Alias-%s._ipp._tcp.local/")} {
			live := Query(context.Background(), via, r)
			if live.Reachable || !live.Own {
				t.Errorf("%s: %+v, want Own and not reachable", via, live)
			}
		}
	}
	if tried == 0 {
		t.Skip("no local alias resolves on this machine")
	}
	vs := rec.verdicts()
	if len(vs) == 0 {
		t.Fatal("the guard was never asked")
	}
	for _, v := range vs {
		if !errors.Is(v.err, errOwnCUPS) {
			t.Errorf("the guard let %s through (verdict %v)", v.address, v.err)
		}
	}
}

// Addresses that IsLocalCUPS does not recognise reach the dialer, whose guard
// has to refuse them.
func TestQueryRefusesOddFormsOfThisMachineAtTheDialer(t *testing.T) {
	rec := wrapped(t)
	for _, uri := range []string{
		"ipp://[::127.0.0.1]:631/x", "ipp://[::7f00:1]/x", "ipp://[::ffff:0:127.0.0.1]:631/x", "ipps://[::ffff:0:7f00:1]/x",
	} {
		before := len(rec.verdicts())
		live := Query(context.Background(), uri, nil)
		vs := rec.verdicts()[before:]
		if live.Reachable || !live.Own || len(vs) == 0 {
			t.Errorf("%s: %+v with %d verdicts, want Own, not reachable and a verdict", uri, live, len(vs))
		}
		for _, v := range vs {
			if !errors.Is(v.err, errOwnCUPS) {
				t.Errorf("%s: the guard let %s through (verdict %v)", uri, v.address, v.err)
			}
		}
	}
}

// A host that a plain parse calls non-empty but the transport's IDNA mapping
// turns into "" (a soft hyphen, a zero-width space) used to be dialed as
// ":631", which the kernel connects to ::1. Such a URI never reaches the
// dialer now; and if a host like it did, the guard refuses.
func TestQueryRefusesAHostThatMapsToNothing(t *testing.T) {
	rec := wrapped(t)
	for _, host := range []string{"%C2%AD", "%E2%80%8B", "%CD%8F", "%EF%B8%80", "%C2%AD%C2%AD"} {
		for _, uri := range []string{"ipp://" + host + ":631/", "ipp://" + host + "/", "ipps://" + host + ":631/x"} {
			live := Query(context.Background(), uri, nil)
			if live.Reachable {
				t.Errorf("%s: reachable", uri)
			}
			if _, _, err := httpURL(uri); err == nil {
				t.Errorf("httpURL(%q) accepted", uri)
			}
		}
	}
	if vs := rec.verdicts(); len(vs) != 0 {
		t.Errorf("%d dials for hosts that are nothing: %+v", len(vs), vs)
	}

	// The transport itself, given such a host: whatever it dials on 631 is refused.
	rec = wrapped(t)
	for _, host := range []string{"­", "​", "͏", "︀"} {
		req, _ := http.NewRequest(http.MethodPost, "http://"+host+":631/", nil)
		plainClient.Do(req) //nolint:errcheck // stopped by the wrapper
	}
	vs := rec.verdicts()
	t.Logf("the transport asked the guard about %d addresses: %+v", len(vs), vs)
	for _, v := range vs {
		if !errors.Is(v.err, errOwnCUPS) {
			t.Errorf("the guard let %q through (verdict %v)", v.address, v.err)
		}
	}
}

// A port other than 631 is another service: the guard lets it through to the
// wrapper's stop, and Query reads the printer as away, not as this machine.
func TestGuardLetsOtherPortsOfThisMachinePass(t *testing.T) {
	rec := wrapped(t)
	live := Query(context.Background(), "ipp://127.0.0.1:60631/ipp/print", nil)
	if live.Reachable || live.Own {
		t.Errorf("live = %+v", live)
	}
	vs := rec.verdicts()
	if len(vs) != 1 || vs[0].err != nil {
		t.Errorf("verdicts = %+v, want one that let the connection through", vs)
	}
}

// The real clients, to a printer on 127.0.0.1 on a random port, which is not 631.
func TestQueryStillReachesAPrinterOnLoopback(t *testing.T) {
	s := ippServer(t, answer(inkAnswer()))
	live := Query(context.Background(), "ipp://"+s.Listener.Addr().String()+"/ipp/print", nil)
	if !live.Reachable || live.Own {
		t.Errorf("live = %+v, want a reachable printer that is not Own", live)
	}
}
