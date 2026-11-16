// SPDX-License-Identifier: AGPL-3.0-or-later

package printer

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestParseDNSSD(t *testing.T) {
	name, regtype, domain, err := ParseDNSSD("dnssd://Brother%20MFC-J6975DW._ipp._tcp.local/?uuid=e3248000-80ce-11db-8000-94ddf8f11cb0")
	if err != nil {
		t.Fatal(err)
	}
	if name != "Brother MFC-J6975DW" || regtype != "_ipp._tcp" || domain != "local" {
		t.Errorf("got %q, %q, %q", name, regtype, domain)
	}
}

func TestParseDNSSDShapes(t *testing.T) {
	for _, tc := range []struct{ uri, name, regtype, domain string }{
		{"dnssd://Plain._ipps._tcp.local", "Plain", "_ipps._tcp", "local"},
		{"dnssd://HP%20Officejet%205.0._ipp._tcp.local./cups", "HP Officejet 5.0", "_ipp._tcp", "local."},
		{"dnssd://M%C3%BCller%20Drucker._ipp._tcp.example.org/", "Müller Drucker", "_ipp._tcp", "example.org"},
		{"dnssd://A._printer._tcp.local/?uuid=x", "A", "_printer._tcp", "local"},
	} {
		name, regtype, domain, err := ParseDNSSD(tc.uri)
		if err != nil || name != tc.name || regtype != tc.regtype || domain != tc.domain {
			t.Errorf("ParseDNSSD(%q) = %q, %q, %q, %v", tc.uri, name, regtype, domain, err)
		}
	}
}

func TestParseDNSSDRejects(t *testing.T) {
	for _, uri := range []string{"", "ipp://x._ipp._tcp.local/", "dnssd://", "dnssd://nothing", "dnssd://x._ipp.local/", "dnssd://%zz._ipp._tcp.local/", "dnssd://._ipp._tcp.local/"} {
		if _, _, _, err := ParseDNSSD(uri); err == nil {
			t.Errorf("ParseDNSSD(%q) accepted", uri)
		}
	}
}

func TestServiceURI(t *testing.T) {
	for _, tc := range []struct {
		regtype, addr string
		port          uint16
		txt           [][]byte
		want          string
	}{
		{"_ipp._tcp", "192.168.1.5", 631, [][]byte{[]byte("txtvers=1"), []byte("rp=ipp/print")}, "ipp://192.168.1.5:631/ipp/print"},
		{"_ipps._tcp", "192.168.1.5", 443, [][]byte{[]byte("rp=ipp/print")}, "ipps://192.168.1.5:443/ipp/print"},
		{"_ipp._tcp", "2001:db8::5", 631, [][]byte{[]byte("rp=ipp/print")}, "ipp://[2001:db8::5]:631/ipp/print"},
		{"_ipp._tcp", "10.0.0.2", 631, [][]byte{[]byte("rp=/printers/x")}, "ipp://10.0.0.2:631/printers/x"},
		{"_ipp._tcp", "10.0.0.2", 631, nil, "ipp://10.0.0.2:631/ipp/print"},
		{"_ipp._tcp", "10.0.0.2", 631, [][]byte{[]byte("rp=")}, "ipp://10.0.0.2:631/ipp/print"},
		{"_ipp._tcp", "10.0.0.2", 631, [][]byte{[]byte("RP=ipp/x")}, "ipp://10.0.0.2:631/ipp/x"},
		{"_ipp._tcp", "10.0.0.2", 631, [][]byte{[]byte("rp=my printer")}, "ipp://10.0.0.2:631/my%20printer"},
	} {
		if got := serviceURI(tc.regtype, netip.MustParseAddr(tc.addr), "", tc.port, tc.txt); got != tc.want {
			t.Errorf("serviceURI(%s, %s) = %q, want %q", tc.regtype, tc.addr, got, tc.want)
		}
	}
}

func TestServiceURIScopesALinkLocalAddress(t *testing.T) {
	got := serviceURI("_ipp._tcp", netip.MustParseAddr("fe80::1"), "wlp3s0", 631, [][]byte{[]byte("rp=ipp/print")})
	if want := "ipp://[fe80::1%25wlp3s0]:631/ipp/print"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Only port 631 of this machine is its CUPS. Another service that avahi
// finds here, such as the queue ipp-usb shares for a USB printer on port
// 60000, is a printer that may be asked.
func TestOwnResult(t *testing.T) {
	const (
		cached, wideArea, multicast = 1, 2, 4
		local, ourOwn, static       = 8, 16, 32
	)
	for _, tc := range []struct {
		flags uint32
		port  uint16
		want  bool
	}{
		{0, 631, false}, {cached, 631, false}, {wideArea, 631, false}, {multicast, 631, false}, {static, 631, false},
		{cached | multicast, 631, false}, {multicast | static, 631, false},
		{local, 631, true}, {ourOwn, 631, true}, {local | ourOwn, 631, true}, {local | multicast, 631, true}, {ourOwn | cached | static, 631, true},
		{local, 60000, false}, {ourOwn, 60000, false}, {local | ourOwn, 8631, false}, {local, 0, false}, {local, 6310, false},
		{0, 60000, false},
	} {
		if got := ownResult(tc.flags, tc.port); got != tc.want {
			t.Errorf("ownResult(%d, %d) = %v, want %v", tc.flags, tc.port, got, tc.want)
		}
	}
}

// What Resolve makes of avahi's answer.
func TestResolvedURI(t *testing.T) {
	const local = 8
	usb := avahiService{address: "127.0.0.1", port: 60000, flags: local, txt: [][]byte{[]byte("rp=ipp/print")}}
	if got, err := resolvedURI("_ipp._tcp", usb); err != nil || got != "ipp://127.0.0.1:60000/ipp/print" {
		t.Errorf("the ipp-usb queue: %q, %v; want it queried at 127.0.0.1:60000", got, err)
	}
	for _, own := range []avahiService{
		{address: "127.0.0.1", port: 631, flags: local},
		{address: "192.0.2.10", port: 631, flags: local},
		{address: "::1", port: 631, flags: 16},
	} {
		if got, err := resolvedURI("_ipp._tcp", own); !errors.Is(err, ErrOwnHost) {
			t.Errorf("%+v: %q, %v; want ErrOwnHost", own, got, err)
		}
	}
	if got, err := resolvedURI("_ipps._tcp", avahiService{address: "192.0.2.7", port: 443}); err != nil || got != "ipps://192.0.2.7:443/ipp/print" {
		t.Errorf("a printer on the LAN: %q, %v", got, err)
	}
	if _, err := resolvedURI("_ipp._tcp", avahiService{address: "not an address", port: 631}); err == nil {
		t.Error("an unreadable address was accepted")
	}
}

func browserSignal(path dbus.ObjectPath, member string, body ...any) *dbus.Signal {
	return &dbus.Signal{Path: path, Name: "org.freedesktop.Avahi.ServiceBrowser." + member, Body: body}
}

func TestCollect(t *testing.T) {
	const path = dbus.ObjectPath("/Client1/ServiceBrowser1")
	signals := make(chan *dbus.Signal, 8)
	signals <- browserSignal(path, "ItemNew", int32(3), int32(0), "Brother MFC-J6975DW", "_uscan._tcp", "local", uint32(0))
	// The same service over IPv6 and on another interface.
	signals <- browserSignal(path, "ItemNew", int32(3), int32(1), "Brother MFC-J6975DW", "_uscan._tcp", "local", uint32(0))
	signals <- browserSignal(path, "ItemNew", int32(1), int32(0), "Brother MFC-J6975DW", "_uscan._tcp", "local", uint32(4))
	// Another browser's signal on the shared connection.
	signals <- browserSignal("/Client1/ServiceBrowser2", "ItemNew", int32(3), int32(0), "Other", "_uscan._tcp", "local", uint32(0))
	signals <- browserSignal(path, "ItemNew", int32(1), int32(0), "EPSON ET-4850 Series", "_uscan._tcp", "local", uint32(12))
	signals <- browserSignal(path, "CacheExhausted")
	signals <- browserSignal(path, "AllForNow")
	signals <- browserSignal(path, "ItemNew", int32(3), int32(0), "Late", "_uscan._tcp", "local", uint32(0))
	items, err := collect(context.Background(), signals, path)
	if err != nil {
		t.Fatal(err)
	}
	want := []item{{"Brother MFC-J6975DW", "_uscan._tcp", "local"}, {"EPSON ET-4850 Series", "_uscan._tcp", "local"}}
	if !slices.Equal(items, want) {
		t.Errorf("collect = %v, want %v", items, want)
	}
}

func TestCollectFailureAndWait(t *testing.T) {
	const path = dbus.ObjectPath("/Client1/ServiceBrowser1")
	signals := make(chan *dbus.Signal, 1)
	signals <- browserSignal(path, "Failure", "Too many objects")
	if _, err := collect(context.Background(), signals, path); err == nil || err.Error() != "Too many objects" {
		t.Errorf("Failure: err = %v", err)
	}

	defer func(d time.Duration) { browseWait = d }(browseWait)
	browseWait = 10 * time.Millisecond
	signals <- browserSignal(path, "ItemNew", int32(3), int32(0), "Brother", "_uscan._tcp", "local", uint32(0))
	items, err := collect(context.Background(), signals, path)
	if err != nil || len(items) != 1 {
		t.Errorf("no AllForNow: %v, %v", items, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	browseWait = time.Minute
	if _, err := collect(ctx, signals, path); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v", err)
	}
}

func TestTXTMap(t *testing.T) {
	got := txtMap([][]byte{[]byte("txtvers=1"), []byte("UUID=e3248000"), []byte("uuid=second"), []byte("rs=eSCL"), []byte("duplex"), []byte("=nokey"), []byte("ty=Brother MFC-J6975DW")})
	want := map[string]string{"txtvers": "1", "uuid": "e3248000", "rs": "eSCL", "duplex": "", "ty": "Brother MFC-J6975DW"}
	if !maps.Equal(got, want) {
		t.Errorf("txtMap = %v, want %v", got, want)
	}
}
