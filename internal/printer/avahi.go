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
	"slices"
	"strconv"
	"strings"
	"time"

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

// Avahi resolves dnssd:// device URIs and browses for services through the
// system's avahi-daemon.
type Avahi struct{}

const (
	avahiName    = "org.freedesktop.Avahi"
	avahiBrowser = avahiName + ".ServiceBrowser"
	avahiUnspec  = int32(-1) // AVAHI_IF_UNSPEC and AVAHI_PROTO_UNSPEC
)

// browseWait is how long Browse collects announcements when avahi does not
// say it has sent all it knows. A variable only so a test can lower it.
var browseWait = 2 * time.Second

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
	r, err := resolveService(ctx, conn, name, regtype, domain)
	if err != nil {
		return "", err
	}
	uri, err := resolvedURI(regtype, r)
	if err != nil {
		return "", fmt.Errorf("%q: %w", name, err)
	}
	return uri, nil
}

// resolvedURI is the ipp:// or ipps:// URI of what avahi found, or ErrOwnHost
// when that is this machine's CUPS.
func resolvedURI(regtype string, r avahiService) (string, error) {
	if ownResult(r.flags, r.port) {
		return "", ErrOwnHost
	}
	addr, zone, err := r.ip()
	if err != nil {
		return "", err
	}
	return serviceURI(regtype, addr, zone, r.port, r.txt), nil
}

// avahiService is what avahi's ResolveService returned for one service.
type avahiService struct {
	iface   int32
	address string
	port    uint16
	txt     [][]byte
	flags   uint32
}

// resolveService asks avahi for the address, port and TXT record of one
// service instance.
func resolveService(ctx context.Context, conn *dbus.Conn, name, regtype, domain string) (avahiService, error) {
	var (
		proto, aproto      int32
		rName, rType, rDom string
		host               string
		r                  avahiService
	)
	call := conn.Object(avahiName, "/").CallWithContext(ctx, avahiName+".Server.ResolveService", 0,
		avahiUnspec, avahiUnspec, name, regtype, domain, avahiUnspec, uint32(0))
	if call.Err != nil {
		return avahiService{}, fmt.Errorf("avahi could not resolve %q: %w", name, call.Err)
	}
	if err := call.Store(&r.iface, &proto, &rName, &rType, &rDom, &host, &aproto, &r.address, &r.port, &r.txt, &r.flags); err != nil {
		return avahiService{}, fmt.Errorf("avahi sent an unreadable answer: %w", err)
	}
	return r, nil
}

// ip parses the address avahi sent. A link-local IPv6 address needs the name
// of the interface it lives on, which comes back as the zone.
func (r avahiService) ip() (addr netip.Addr, zone string, err error) {
	addr, err = netip.ParseAddr(r.address)
	if err != nil {
		return netip.Addr{}, "", fmt.Errorf("avahi sent address %q: %w", r.address, err)
	}
	if addr.Is6() && addr.IsLinkLocalUnicast() && r.iface > 0 {
		if ifc, err := net.InterfaceByIndex(int(r.iface)); err == nil {
			zone = ifc.Name
		}
	}
	return addr, zone, nil
}

// Found is a service that Browse found and resolved. Host is an IP address,
// with the interface after a % for a link-local IPv6 one. TXT holds the TXT
// record with its keys in lower case.
type Found struct {
	Name string
	Host string
	Port uint16
	TXT  map[string]string
}

// Browse asks avahi for the services of type regtype, such as _uscan._tcp, in
// the local domain, and resolves each. It collects announcements until avahi
// says it has sent what it knows, or for two seconds. A service that does not
// resolve is left out. Services on this machine are kept: the caller decides
// what it may talk to. The caller's context bounds the whole call.
func (Avahi) Browse(ctx context.Context, regtype string) ([]Found, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("no system bus: %w", err)
	}
	// ServiceBrowserPrepare makes the browser without starting it, so the
	// match below is in place before the first ItemNew is sent.
	var path dbus.ObjectPath
	if err := conn.Object(avahiName, "/").CallWithContext(ctx, avahiName+".Server2.ServiceBrowserPrepare", 0,
		avahiUnspec, avahiUnspec, regtype, "", uint32(0)).Store(&path); err != nil {
		return nil, fmt.Errorf("avahi could not browse %s: %w", regtype, err)
	}
	browser := conn.Object(avahiName, path)
	defer browser.Call(avahiBrowser+".Free", 0)

	match := []dbus.MatchOption{dbus.WithMatchObjectPath(path), dbus.WithMatchInterface(avahiBrowser)}
	if err := conn.AddMatchSignalContext(ctx, match...); err != nil {
		return nil, fmt.Errorf("avahi could not browse %s: %w", regtype, err)
	}
	defer conn.RemoveMatchSignal(match...)
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)

	if call := browser.CallWithContext(ctx, avahiBrowser+".Start", 0); call.Err != nil {
		return nil, fmt.Errorf("avahi could not browse %s: %w", regtype, call.Err)
	}
	items, err := collect(ctx, signals, path)
	if err != nil {
		return nil, fmt.Errorf("avahi could not browse %s: %w", regtype, err)
	}
	var found []Found
	for _, it := range items {
		r, err := resolveService(ctx, conn, it.name, it.regtype, it.domain)
		if err != nil {
			continue
		}
		addr, zone, err := r.ip()
		if err != nil {
			continue
		}
		found = append(found, Found{Name: it.name, Host: addr.WithZone(zone).String(), Port: r.port, TXT: txtMap(r.txt)})
	}
	return found, nil
}

// item is one service instance a browser announced.
type item struct{ name, regtype, domain string }

// collect reads the browser's signals until AllForNow, browseWait or the end
// of ctx. A service is announced once per interface and protocol; it is
// listed once.
func collect(ctx context.Context, signals <-chan *dbus.Signal, path dbus.ObjectPath) ([]item, error) {
	timer := time.NewTimer(browseWait)
	defer timer.Stop()
	var items []item
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return items, nil
		case sig := <-signals:
			if sig.Path != path {
				continue
			}
			switch sig.Name {
			case avahiBrowser + ".ItemNew":
				// iface, protocol, name, type, domain, flags
				if len(sig.Body) < 5 {
					continue
				}
				name, _ := sig.Body[2].(string)
				regtype, _ := sig.Body[3].(string)
				domain, _ := sig.Body[4].(string)
				it := item{name, regtype, domain}
				if name != "" && !slices.Contains(items, it) {
					items = append(items, it)
				}
			case avahiBrowser + ".AllForNow":
				return items, nil
			case avahiBrowser + ".Failure":
				msg := "unknown error"
				if len(sig.Body) > 0 {
					msg, _ = sig.Body[0].(string)
				}
				return nil, errors.New(msg)
			}
		}
	}
}

// txtMap turns a TXT record into a map with lower-case keys. A key that comes
// twice keeps its first value, as DNS-SD has it.
func txtMap(txt [][]byte) map[string]string {
	m := make(map[string]string, len(txt))
	for _, kv := range txt {
		k, v, _ := strings.Cut(string(kv), "=")
		k = strings.ToLower(k)
		if _, seen := m[k]; k != "" && !seen {
			m[k] = v
		}
	}
	return m
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

// ownResult reports whether avahi found this machine's CUPS: a service on
// this machine, on port 631. The laptop shares its CUPS queues over DNS-SD,
// and skra must not speak TCP to its own CUPS. Another service on this
// machine is no loop: ipp-usb shares a USB printer as a queue on port 60000
// and up, and that printer may be asked. The dialer still refuses port 631 of
// every address of this machine, whatever avahi flagged.
func ownResult(flags uint32, port uint16) bool {
	return flags&(lookupResultLocal|lookupResultOurOwn) != 0 && port == 631
}
