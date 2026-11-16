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
	"time"

	"github.com/nemesis398/skra/internal/testenv"
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

// fakeInterfaces makes this machine's interfaces, for the test, loopback and
// two documentation addresses, so that the cases of an own address run on a
// machine that has nothing but loopback. It returns the two.
func fakeInterfaces(t *testing.T) (v4, v6 netip.Addr) {
	t.Helper()
	v4, v6 = netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")
	old := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.ParseIP("::1"), Mask: net.CIDRMask(128, 128)},
			&net.IPNet{IP: v4.AsSlice(), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: v6.AsSlice(), Mask: net.CIDRMask(64, 128)},
		}, nil
	}
	t.Cleanup(func() { interfaceAddrs = old })
	return v4, v6
}

func TestIsOwnAddress(t *testing.T) {
	// The machine's real list first, which only a machine with an address
	// besides loopback has.
	t.Run("real interface", func(t *testing.T) { checkIsOwnAddress(t, testenv.InterfaceAddr(t)) })
	v4, v6 := fakeInterfaces(t)
	checkIsOwnAddress(t, v4)
	checkIsOwnAddress(t, v6)
	for _, addr := range []string{"192.0.2.11", "2001:db8::11", "::ffff:192.0.2.11"} {
		if IsOwnAddress(netip.MustParseAddr(addr)) {
			t.Errorf("IsOwnAddress(%s) = true for a neighbour of this machine's address", addr)
		}
	}
}

// checkIsOwnAddress checks IsOwnAddress for the fixed cases and for mine, an
// address of this machine's interfaces.
func checkIsOwnAddress(t *testing.T, mine netip.Addr) {
	t.Helper()
	for addr, want := range map[netip.Addr]bool{
		netip.MustParseAddr("127.0.0.1"):              true,
		netip.MustParseAddr("127.1.2.3"):              true,
		netip.MustParseAddr("::1"):                    true,
		netip.MustParseAddr("0.0.0.0"):                true,
		netip.MustParseAddr("::"):                     true,
		netip.MustParseAddr("::ffff:127.0.0.1"):       true,
		netip.MustParseAddr("::ffff:0.0.0.0"):         true,
		netip.MustParseAddr("::127.0.0.1"):            true, // IPv4-compatible
		netip.MustParseAddr("::7f00:1"):               true,
		netip.MustParseAddr("::ffff:0:127.0.0.1"):     true, // IPv4-translated
		netip.MustParseAddr("::ffff:0:7f00:1"):        true,
		mine:                                          true,
		netip.MustParseAddr("93.184.216.34"):          false,
		netip.MustParseAddr("2001:db8::5"):            false,
		netip.MustParseAddr("::ffff:93.184.216.34"):   false,
		netip.MustParseAddr("::10.0.0.5"):             false,
		netip.MustParseAddr("::ffff:0:93.184.216.34"): false,
		{}: false,
	} {
		if got := IsOwnAddress(addr); got != want {
			t.Errorf("IsOwnAddress(%v) = %v, want %v", addr, got, want)
		}
	}
	if mine.Is4() {
		for _, embedded := range []string{"::", "::ffff:0:", "::ffff:"} {
			if addr := netip.MustParseAddr(embedded + mine.String()); !IsOwnAddress(addr) {
				t.Errorf("IsOwnAddress(%v) = false, want true", addr)
			}
		}
	}
}

// The verdict of the guard for an address Go hands to Control. It is a plain
// function, so these cases dial nothing.
func TestGuardVerdicts(t *testing.T) {
	t.Run("real interface", func(t *testing.T) { checkGuardVerdicts(t, testenv.InterfaceAddr(t)) })
	v4, v6 := fakeInterfaces(t)
	checkGuardVerdicts(t, v4)
	checkGuardVerdicts(t, v6)
}

// checkGuardVerdicts checks the guard for the fixed cases and for mine, an
// address of this machine's interfaces.
func checkGuardVerdicts(t *testing.T, mine netip.Addr) {
	t.Helper()
	refused := []string{
		"127.0.0.1:631", "127.5.5.5:631", "[::1]:631", "0.0.0.0:631", "[::]:631", "[::ffff:127.0.0.1]:631",
		net.JoinHostPort(mine.String(), "631"),
		":631", // what Go passes for an empty host; the kernel connects it to [::]:631, which is ::1
		"[::127.0.0.1]:631", "[::ffff:0:127.0.0.1]:631", "[::7f00:1]:631", "[::ffff:0:7f00:1]:631",
		// What cannot be read is refused, never let through.
		"localhost:631", "printer.local:631", "127.0.0.1", "", "631", "127.0.0.1:x:631",
		"127.0.0.1:", "127.0.0.1:631x", "127.0.0.1:-631",
	}
	if mine.Is4() {
		refused = append(refused, "[::"+mine.String()+"]:631", "[::ffff:"+mine.String()+"]:631")
	}
	for _, addr := range refused {
		if err := refuseOwnCUPS("tcp", addr, nil); !errors.Is(err, errOwnCUPS) {
			t.Errorf("%q: verdict %v, want a refusal", addr, err)
		}
	}
	for _, addr := range []string{
		"127.0.0.1:8631", "127.0.0.1:60631", "127.0.0.1:80", "[::1]:6310", net.JoinHostPort(mine.String(), "8631"),
		net.JoinHostPort(mine.String(), "60000"),
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

	// The transport itself, given such a host: it dials ":631", and the guard
	// has to be asked and has to refuse. A Go that one day refuses such a host
	// before it dials fails here too, and the case then belongs to the half
	// above.
	rec = wrapped(t)
	for _, host := range []string{"\u00ad", "\u200b", "\u034f", "\ufe00"} {
		before := len(rec.verdicts())
		req, _ := http.NewRequest(http.MethodPost, "http://"+host+":631/", nil)
		_, err := plainClient.Do(req)
		vs := rec.verdicts()[before:]
		if len(vs) == 0 || !errors.Is(err, errOwnCUPS) {
			t.Errorf("%q: %d verdicts and error %v, want the guard asked and the request refused by it", host, len(vs), err)
		}
		for _, v := range vs {
			if !errors.Is(v.err, errOwnCUPS) {
				t.Errorf("%q: the guard let %q through (verdict %v)", host, v.address, v.err)
			}
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

// The clients skrá really uses, not a test's wrapper, and the transport other
// packages build theirs from, all pass the guard. With the list of this
// machine's addresses unreadable, every address on port 631 counts as this
// machine's, so the guard refuses 192.0.2.1:631 before connect(). That is a
// documentation address nothing answers on: a client without the guard
// reaches no CUPS either, and fails here on the timeout instead.
func TestProductionClientsPassTheGuard(t *testing.T) {
	old := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) { return nil, errors.New("test: no interface list") }
	defer func() { interfaceAddrs = old }()

	for _, uri := range []string{"ipp://192.0.2.1:631/", "ipps://192.0.2.1:631/ipp/print"} {
		if live := Query(context.Background(), uri, nil); !live.Own || live.Reachable {
			t.Errorf("%s: %+v, want Own: the status client did not ask the guard", uri, live)
		}
	}
	for _, skipVerify := range []bool{false, true} {
		c := &http.Client{Transport: GuardedTransport(skipVerify)}
		for _, u := range []string{"http://192.0.2.1:631/", "https://192.0.2.1:631/eSCL/ScannerStatus"} {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			_, err := c.Do(req)
			cancel()
			if !errors.Is(err, errOwnCUPS) {
				t.Errorf("GuardedTransport(%t) to %s: %v, want the guard's refusal", skipVerify, u, err)
			}
		}
	}
}
