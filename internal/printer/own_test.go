// SPDX-License-Identifier: AGPL-3.0-or-later

package printer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
)

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
		netip.MustParseAddr("127.0.0.1"):               true,
		netip.MustParseAddr("127.1.2.3"):               true,
		netip.MustParseAddr("::1"):                     true,
		netip.MustParseAddr("0.0.0.0"):                 true,
		netip.MustParseAddr("::"):                      true,
		netip.MustParseAddr("::ffff:127.0.0.1"):        true,
		netip.MustParseAddr("::ffff:0.0.0.0"):          true,
		mine:                                           true,
		netip.MustParseAddr("::ffff:" + mine.String()): mine.Is4(),
		netip.MustParseAddr("93.184.216.34"):           false,
		netip.MustParseAddr("2001:db8::5"):             false,
		netip.MustParseAddr("::ffff:93.184.216.34"):    false,
		{}: false,
	} {
		if got := IsOwnAddress(addr); got != want {
			t.Errorf("IsOwnAddress(%v) = %v, want %v", addr, got, want)
		}
	}
}

func TestDialerRefusesPort631OfThisMachine(t *testing.T) {
	mine := interfaceAddr(t)
	for _, addr := range []string{
		"127.0.0.1:631", "127.5.5.5:631", "[::1]:631", "0.0.0.0:631", "[::]:631", "[::ffff:127.0.0.1]:631",
		net.JoinHostPort(mine.String(), "631"),
	} {
		if err := refuseOwnCUPS("tcp", addr, nil); !errors.Is(err, errOwnCUPS) {
			t.Errorf("%s: err = %v, want errOwnCUPS", addr, err)
		}
	}
	for _, addr := range []string{
		"127.0.0.1:8631", "127.0.0.1:60631", "127.0.0.1:80", "[::1]:6310", net.JoinHostPort(mine.String(), "8631"),
		"93.184.216.34:631", "[2001:db8::5]:631",
	} {
		if err := refuseOwnCUPS("tcp", addr, nil); err != nil {
			t.Errorf("%s: err = %v, want none", addr, err)
		}
	}
}

// TestDialerNeverConnectsToThisMachinesCUPS goes through the real dialer, so a
// refusal here is a dial that did not happen.
func TestDialerNeverConnectsToThisMachinesCUPS(t *testing.T) {
	mine := interfaceAddr(t)
	for _, addr := range []string{"127.0.0.1:631", net.JoinHostPort(mine.String(), "631")} {
		conn, err := ownDialer.DialContext(context.Background(), "tcp", addr)
		if conn != nil {
			conn.Close()
		}
		if !errors.Is(err, errOwnCUPS) {
			t.Errorf("dial %s: err = %v, want errOwnCUPS", addr, err)
		}
	}
}

// connecting is a transport that dials with the real dialer and counts the
// connections that were made.
func connecting(n *atomic.Int32) *http.Transport {
	return &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := ownDialer.DialContext(ctx, network, addr)
		if err == nil {
			n.Add(1)
		}
		return conn, err
	}}
}

// TestQueryRefusesANameThatOnlyTheDialSeesIsLocal covers the names no string
// check can know: hosts-file aliases and this machine's own name. They reach
// 127.0.0.1 or ::1 at dial time, and the dial must not happen.
func TestQueryRefusesANameThatOnlyTheDialSeesIsLocal(t *testing.T) {
	var made atomic.Int32
	oldP, oldT := plainClient.Transport, tlsClient.Transport
	plainClient.Transport, tlsClient.Transport = connecting(&made), connecting(&made)
	defer func() { plainClient.Transport, tlsClient.Transport = oldP, oldT }()

	host, _ := os.Hostname()
	names := []string{"localhost.localdomain", "localhost4", "localhost6.localdomain6", host}
	tried := 0
	for _, name := range names {
		if name == "" {
			continue
		}
		ips, err := net.LookupIP(name)
		if err != nil || len(ips) == 0 {
			t.Logf("%s does not resolve here; skipped", name)
			continue
		}
		own := false
		for _, ip := range ips {
			if a, ok := netip.AddrFromSlice(ip); ok && IsOwnAddress(a.Unmap()) {
				own = true
			}
		}
		if !own {
			t.Logf("%s resolves to %v, which is not this machine; skipped", name, ips)
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
	if n := made.Load(); n != 0 {
		t.Errorf("%d connections were made to this machine's CUPS", n)
	}
}

func TestQueryStillReachesAPrinterOnLoopback(t *testing.T) {
	s := ippServer(t, answer(inkAnswer())) // 127.0.0.1 on a random port
	live := Query(context.Background(), "ipp://"+s.Listener.Addr().String()+"/ipp/print", nil)
	if !live.Reachable || live.Own {
		t.Errorf("live = %+v, want a reachable printer that is not Own", live)
	}
}
