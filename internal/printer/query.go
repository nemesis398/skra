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
	"net/url"
	"strings"
	"sync"
	"time"

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
	plainClient = &http.Client{CheckRedirect: noRedirect, Transport: &http.Transport{DisableKeepAlives: true}}
	tlsClient   = &http.Client{CheckRedirect: noRedirect, Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // see above
	}}
)

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

var statusAttrs = []string{"marker-names", "marker-levels", "marker-colors", "marker-types",
	"printer-state-reasons", "printer-state-message"}

// resolved remembers the ipp:// address of each dnssd:// URI that resolved,
// so the hub does not send a multicast question every few seconds. A failed
// query drops the entry.
var resolved sync.Map

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
			ip, err := r.Resolve(ctx, deviceURI)
			if err != nil {
				return live
			}
			target = ip
			resolved.Store(deviceURI, ip)
		}
	}
	res, err := fetch(ctx, target)
	if err != nil {
		if isDNSSD {
			resolved.Delete(deviceURI)
		}
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
// the port defaults to 631.
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
	return next.String(), pu.String(), nil
}
