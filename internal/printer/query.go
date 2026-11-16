// SPDX-License-Identifier: AGPL-3.0-or-later

// Package printer asks printers themselves for their ink levels and whether
// they are on the network, instead of trusting what CUPS last saw.
package printer

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/ipp"
)

// timeout bounds one whole query of one printer, a dnssd resolution included.
// A variable so a test can lower it.
var timeout = 2 * time.Second

// maxAnswer caps what a printer may send back. Six attributes are a few
// hundred bytes.
const maxAnswer = 1 << 20

// Live is what one printer said about itself. At is when the question was
// asked, whether or not the printer answered.
type Live struct {
	Reachable bool
	Markers   []cups.Marker
	Reasons   []string
	Message   string
	At        time.Time
	// Own is set when the address is this machine's own CUPS. Nothing was
	// asked, so the printer is not away either: the caller treats it as a
	// queue that is not queried.
	Own bool
}

// ErrOwnHost is what a Resolver returns for a service this machine offers
// itself, such as the CUPS queue it shares over DNS-SD.
var ErrOwnHost = errors.New("this machine's own service")

// IsLocalCUPS reports whether an ipp, ipps, http or https URI points at port
// 631 of this machine, which is where its CUPS listens (CUPS's ipp backend
// treats http like ipp). skra speaks to CUPS only over its unix socket, so
// Query never connects there: such a queue would be a loop queue, or a dnssd
// name that resolves to the CUPS this laptop shares itself. The port is
// compared as a number, and an empty one counts as 631. A URI that does not
// parse is not claimed here; a caller that takes addresses from a user refuses
// those itself.
func IsLocalCUPS(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "ipp", "ipps", "http", "https":
	default:
		return false
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n != 631 {
			return false
		}
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && (ip.IsLoopback() || ip.IsUnspecified() || ip.Unmap().IsLoopback() || ip.Unmap().IsUnspecified())
}

// Resolver turns a dnssd:// device URI into an ipp:// or ipps:// one.
type Resolver interface {
	Resolve(ctx context.Context, dnssdURI string) (string, error)
}

// Each client has its own transport: no proxy from the environment, and no
// kept-alive connections, because a laptop that changes network would
// otherwise hold dead ones. Printers carry self-signed certificates, so ipps
// skips verification. That is acceptable only because this client reads
// status and sends no document and no credentials.
//
// Neither follows a redirect: a printer on the LAN could otherwise bounce the
// POST to any host, 127.0.0.1:631 included. A redirect is an answer that is
// not a success, so the printer reads as unreachable.
var (
	plainClient = &http.Client{CheckRedirect: noRedirect, Transport: newTransport(refuseOwnCUPS, false)}
	tlsClient   = &http.Client{CheckRedirect: noRedirect, Transport: newTransport(refuseOwnCUPS, true)}
)

// newTransport builds the transport of a status client. control runs for every
// connection, after the name is resolved and before connect(); production
// passes refuseOwnCUPS, a test passes a wrapper that never lets a connect
// through.
func newTransport(control func(network, address string, c syscall.RawConn) error, skipVerify bool) *http.Transport {
	d := &net.Dialer{Control: control}
	t := &http.Transport{DisableKeepAlives: true, DialContext: d.DialContext}
	if skipVerify {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // see above
	}
	return t
}

// GuardedTransport is the transport of the status clients, for other clients
// that talk to devices on the LAN: no proxy, no kept-alive connections, and a
// dialer that never connects to this machine's CUPS. skipVerify skips the
// certificate check, which devices with self-signed certificates need. It does
// not stop redirects; a client sets CheckRedirect for that.
func GuardedTransport(skipVerify bool) *http.Transport {
	return newTransport(refuseOwnCUPS, skipVerify)
}

// interfaceAddrs lists the addresses of this machine's interfaces; a variable
// so that a test can make it fail.
var interfaceAddrs = net.InterfaceAddrs

// errOwnCUPS is what the dialer refuses a connection to this machine's CUPS with.
var errOwnCUPS = errors.New("this machine's own CUPS")

// IsOwnAddress reports whether ip is an address of this machine: loopback, the
// unspecified address (which connects to this machine), one of the addresses
// of its interfaces, read now, or an IPv4 address of those inside an IPv6 one
// (mapped ::ffff:a.b.c.d, compatible ::a.b.c.d, translated ::ffff:0:a.b.c.d).
// When the list of interface addresses cannot be read it says yes, because
// then nothing can be told apart from this machine.
func IsOwnAddress(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap().WithZone("")
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	if v4, ok := embeddedIPv4(ip); ok && IsOwnAddress(v4) {
		return true
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return true
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if mine, ok := netip.AddrFromSlice(ipn.IP); ok && mine.Unmap().WithZone("") == ip {
				return true
			}
		}
	}
	return false
}

// embeddedIPv4 returns the IPv4 address that an IPv6 address of the
// IPv4-compatible (::/96) or IPv4-translated (::ffff:0:0:0/96) kind carries.
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	if !ip.Is6() {
		return netip.Addr{}, false
	}
	b := ip.As16()
	switch [12]byte(b[:12]) {
	case [12]byte{}, // ::a.b.c.d
		[12]byte{8: 0xff, 9: 0xff}: // ::ffff:0:a.b.c.d
		return netip.AddrFrom4([4]byte(b[12:])), true
	}
	return netip.Addr{}, false
}

// refuseOwnCUPS is the Control function of the dialer. It runs after the host
// name is resolved and before the connection is made, so it sees the address
// that would be connected to, whatever the name was: a hosts-file alias, this
// machine's own name, a DNS name that points at 127.0.0.1. Port 631 of this
// machine is CUPS, and skrá speaks to CUPS only over its unix socket.
//
// It fails closed. On port 631 a connection goes through only when the host
// is an IP address that is none of this machine's; an empty host (Go passes
// ":631", which the kernel connects to ::1), a name, and anything unreadable
// is refused, and so is an address whose port cannot be read.
func refuseOwnCUPS(_, address string, _ syscall.RawConn) error {
	refuse := func() error { return fmt.Errorf("%w: %q", errOwnCUPS, address) }
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return refuse()
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return refuse()
	}
	if n != 631 {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || IsOwnAddress(ip) {
		return refuse()
	}
	return nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

var statusAttrs = []string{"marker-names", "marker-levels", "marker-colors", "marker-types",
	"printer-state-reasons", "printer-state-message"}

// resolved remembers the ipp:// address of each dnssd:// URI that resolved,
// so the hub does not send a multicast question every few seconds. A failed
// query drops the entry, and so does Forget.
var resolved sync.Map

// Forget drops the remembered address of every dnssd:// URI that keep does not
// want, such as those of queues that were removed. Without it the address of a
// removed queue stays for as long as skrá runs, and a queue added again with
// the same dnssd address is first asked where its printer was back then.
func Forget(keep func(deviceURI string) bool) {
	resolved.Range(func(k, _ any) bool {
		if uri := k.(string); !keep(uri) {
			resolved.Delete(uri)
		}
		return true
	})
}

// Query asks the printer at deviceURI (ipp, ipps or dnssd) for its markers and
// state. It never fails: anything that goes wrong, from a refused connection
// to an unreadable answer, is Reachable == false. r resolves dnssd URIs and may
// be nil, which makes every dnssd printer unreachable.
func Query(ctx context.Context, deviceURI string, r Resolver) Live {
	live := Live{At: time.Now()}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	target := deviceURI
	isDNSSD := strings.HasPrefix(deviceURI, "dnssd:")
	if isDNSSD {
		if v, ok := resolved.Load(deviceURI); ok {
			target = v.(string)
		} else {
			if r == nil {
				return live
			}
			uri, err := r.Resolve(ctx, deviceURI)
			if err != nil {
				live.Own = errors.Is(err, ErrOwnHost)
				return live
			}
			target = uri
		}
	}
	if IsLocalCUPS(target) { // never cached, never asked
		live.Own = true
		return live
	}
	if isDNSSD {
		resolved.Store(deviceURI, target)
	}
	res, err := fetch(ctx, target)
	if err != nil {
		if isDNSSD {
			resolved.Delete(deviceURI)
		}
		// The name led to this machine's CUPS and the dialer did not connect.
		live.Own = errors.Is(err, errOwnCUPS)
		return live
	}
	live.Reachable = true
	if gs := res.Of(ipp.GroupPrinter); len(gs) > 0 {
		g := gs[0]
		live.Markers = cups.MarkersFrom(g)
		live.Reasons = g.Strings("printer-state-reasons")
		live.Message = g.String("printer-state-message")
	}
	return live
}

// fetch sends Get-Printer-Attributes to an ipp:// or ipps:// URI and returns
// the decoded answer when its IPP status is a success.
func fetch(ctx context.Context, uri string) (*ipp.Response, error) {
	target, printerURI, err := httpURL(uri)
	if err != nil {
		return nil, err
	}
	body := ipp.NewRequest(ipp.OpGetPrinterAttributes).
		Op(ipp.URI("printer-uri", printerURI), ipp.Keyword("requested-attributes", statusAttrs...)).Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/ipp")
	c := plainClient
	if strings.HasPrefix(target, "https:") {
		c = tlsClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, err
	}
	res, err := ipp.Decode(raw)
	if err != nil {
		return nil, err
	}
	return res, res.Err()
}

// httpURL maps ipp://host[:port]/path to the http URL to post to and the
// printer-uri to put in the request: ipp becomes http, ipps becomes https, and
// the port defaults to 631. A link-local IPv6 address keeps its zone (the
// interface) in the URL to post to, which the connection needs, and loses it
// in the printer-uri: the name of an interface on this machine means nothing
// to the printer, and a strict one may reject a printer-uri that carries it.
func httpURL(uri string) (target, printerURI string, err error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", "", err
	}
	var scheme string
	switch u.Scheme {
	case "ipp":
		scheme = "http"
	case "ipps":
		scheme = "https"
	default:
		return "", "", fmt.Errorf("%q is not an ipp or ipps address", uri)
	}
	host := u.Hostname()
	if host == "" {
		return "", "", errors.New("no host in " + uri)
	}
	// The transport maps a host through IDNA, which turns a soft hyphen or a
	// zero-width space into nothing and so an address into ":631". A device URI
	// with a name outside ASCII is not queried.
	if strings.ContainsFunc(host, func(r rune) bool { return r > unicode.MaxASCII }) {
		return "", "", errors.New("the host in " + uri + " is not ASCII")
	}
	port := u.Port()
	if port == "" {
		port = "631"
	}
	hp := net.JoinHostPort(host, port)
	next := url.URL{Scheme: scheme, Host: hp, Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}
	if u.Path == "" {
		next.Path = "/"
	}
	pu := next
	pu.Scheme = u.Scheme
	if ip, err := netip.ParseAddr(host); err == nil && ip.Zone() != "" {
		bare, _, _ := strings.Cut(host, "%")
		pu.Host = net.JoinHostPort(bare, port)
	}
	return next.String(), pu.String(), nil
}
