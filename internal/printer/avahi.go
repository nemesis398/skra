// SPDX-License-Identifier: AGPL-3.0-or-later

package printer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
)

// dnssdHost splits the host part of a dnssd URI, "Name._ipp._tcp.local". The
// instance name may hold dots, so the name runs up to the last service label.
var dnssdHost = regexp.MustCompile(`^(.+)\.(_[^.]+\._(?:tcp|udp))\.(.+)$`)

// ParseDNSSD splits a dnssd:// device URI such as
// dnssd://Brother%20MFC-J6975DW._ipp._tcp.local/?uuid=... into the service
// instance name, the registration type and the domain.
func ParseDNSSD(uri string) (name, regtype, domain string, err error) {
	rest, ok := strings.CutPrefix(uri, "dnssd://")
	if !ok {
		return "", "", "", fmt.Errorf("%q is not a dnssd address", uri)
	}
	host, _, _ := strings.Cut(rest, "/")
	host, _, _ = strings.Cut(host, "?")
	plain, err := url.PathUnescape(host)
	if err != nil {
		return "", "", "", fmt.Errorf("%q: %w", uri, err)
	}
	m := dnssdHost.FindStringSubmatch(plain)
	if m == nil {
		return "", "", "", fmt.Errorf("%q has no service type", uri)
	}
	return m[1], m[2], m[3], nil
}

// Avahi resolves dnssd:// device URIs through the system's avahi-daemon.
type Avahi struct{}

const (
	avahiName   = "org.freedesktop.Avahi"
	avahiUnspec = int32(-1) // AVAHI_IF_UNSPEC and AVAHI_PROTO_UNSPEC
)

var _ Resolver = Avahi{}

// Resolve asks avahi for the address, port and TXT record of the service and
// returns the ipp:// or ipps:// URI that reaches it. The caller's context
// bounds the wait: avahi itself waits several seconds for a service that has
// gone.
func (Avahi) Resolve(ctx context.Context, dnssdURI string) (string, error) {
	name, regtype, domain, err := ParseDNSSD(dnssdURI)
	if err != nil {
		return "", err
	}
	conn, err := dbus.SystemBus()
	if err != nil {
		return "", fmt.Errorf("no system bus: %w", err)
	}
	var (
		iface, proto, aproto int32
		rName, rType, rDom   string
		host, address        string
		port                 uint16
		txt                  [][]byte
		flags                uint32
	)
	call := conn.Object(avahiName, "/").CallWithContext(ctx, avahiName+".Server.ResolveService", 0,
		avahiUnspec, avahiUnspec, name, regtype, domain, avahiUnspec, uint32(0))
	if call.Err != nil {
		return "", fmt.Errorf("avahi could not resolve %q: %w", name, call.Err)
	}
	if err := call.Store(&iface, &proto, &rName, &rType, &rDom, &host, &aproto, &address, &port, &txt, &flags); err != nil {
		return "", fmt.Errorf("avahi sent an unreadable answer: %w", err)
	}
	if ownResult(flags) {
		return "", fmt.Errorf("%q: %w", name, ErrOwnHost)
	}
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return "", fmt.Errorf("avahi sent address %q: %w", address, err)
	}
	zone := ""
	if addr.Is6() && addr.IsLinkLocalUnicast() && iface > 0 {
		if ifc, err := net.InterfaceByIndex(int(iface)); err == nil {
			zone = ifc.Name
		}
	}
	uri := serviceURI(regtype, addr, zone, port, txt)
	if uri == "" {
		return "", errors.New("avahi found no usable address for " + name)
	}
	return uri, nil
}

// serviceURI builds the printer's address from what avahi returned. The TXT
// record key rp holds the resource path; a printer that sends none gets the
// path nearly every driverless printer serves. A link-local IPv6 address needs
// the name of the interface it lives on.
func serviceURI(regtype string, addr netip.Addr, zone string, port uint16, txt [][]byte) string {
	scheme := "ipp"
	if strings.HasPrefix(regtype, "_ipps.") {
		scheme = "ipps"
	}
	rp := "ipp/print"
	for _, kv := range txt {
		if k, v, ok := strings.Cut(string(kv), "="); ok && strings.EqualFold(k, "rp") && v != "" {
			rp = v
			break
		}
	}
	rp = strings.TrimLeft(rp, "/")
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(addr.WithZone(zone).String(), strconv.Itoa(int(port))), Path: "/" + rp}
	return u.String()
}

// Result flags of ResolveService (AvahiLookupResultFlags).
const (
	lookupResultLocal  = 8  // the service is on this machine
	lookupResultOurOwn = 16 // this client registered the service itself
)

// ownResult reports whether avahi found the service on this machine. The
// laptop shares its CUPS queues over DNS-SD, and skra must not speak TCP to
// its own CUPS.
func ownResult(flags uint32) bool { return flags&(lookupResultLocal|lookupResultOurOwn) != 0 }
