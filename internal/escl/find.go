// SPDX-License-Identifier: AGPL-3.0-or-later

package escl

import (
	"cmp"
	"context"
	"log"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/printer"
)

// Found is a service the network announced, as the browser resolved it.
type Found = printer.Found

// Browser finds the services of one DNS-SD type, such as _uscan._tcp.
// printer.Avahi is the one skra uses.
type Browser interface {
	Browse(ctx context.Context, regtype string) ([]Found, error)
}

// Scanner is a scanner skra can offer: a name to show and the URL of its eSCL
// root. UUID is the one the scanner announces, empty for a configured
// scanner. Declared marks a scanner from the config.
type Scanner struct {
	Name     string
	URL      string
	UUID     string
	Declared bool
}

// The DNS-SD types of eSCL over http and over https.
const (
	regtypeHTTP  = "_uscan._tcp"
	regtypeHTTPS = "_uscans._tcp"
)

// ippPort is the port of IPP. A scanner announced on it would be a print
// server, possibly this machine's CUPS, which skra never speaks TCP to.
const ippPort = 631

// Find lists the configured scanners first, in their order, and then those
// the network announces, by name. A scanner announced over both http and
// https is listed once, with the https URL; one announced at a configured URL
// is listed only as configured. Scanners on this machine, such as a USB
// scanner shared by ipp-usb, are kept. Find never fails: a browser that
// fails, or no browser at all, leaves the configured scanners.
func Find(ctx context.Context, cfg []config.Scanner, b Browser) []Scanner {
	out := make([]Scanner, 0, len(cfg))
	for _, c := range cfg {
		out = append(out, Scanner{Name: c.Name, URL: c.URL, Declared: true})
	}
	if b == nil {
		return out
	}

	// A failed browse is logged, so a scanner missing from the list can be
	// traced to avahi.
	browse := func(regtype string) []Found {
		found, err := b.Browse(ctx, regtype)
		if err != nil {
			log.Printf("skra: browsing %s: %v", regtype, err)
		}
		return found
	}
	var wg sync.WaitGroup
	var plain, secure []Found
	wg.Go(func() { plain = browse(regtypeHTTP) })
	wg.Go(func() { secure = browse(regtypeHTTPS) })
	wg.Wait()

	// The https announcements go first, so they win for the same scanner.
	type candidate struct {
		key string
		s   Scanner
	}
	var cands []candidate
	// seen holds the keys already listed, first those of scanners announced
	// at a configured URL.
	seen := map[string]bool{}
	for _, r := range []struct {
		found  []Found
		scheme string
	}{{secure, "https"}, {plain, "http"}} {
		for _, f := range r.found {
			s, ok := announcedScanner(f, r.scheme)
			if !ok {
				continue
			}
			key := "name:" + f.Name
			if s.UUID != "" {
				key = "uuid:" + s.UUID
			}
			if slices.ContainsFunc(out, func(c Scanner) bool { return c.URL == s.URL }) {
				seen[key] = true
			}
			cands = append(cands, candidate{key, s})
		}
	}
	var announced []Scanner
	for _, c := range cands {
		if seen[c.key] {
			continue
		}
		seen[c.key] = true
		announced = append(announced, c.s)
	}
	slices.SortStableFunc(announced, func(a, b Scanner) int { return cmp.Compare(a.Name, b.Name) })
	return append(out, announced...)
}

// announcedScanner builds the scanner a browse result describes. The TXT key
// rs holds the path of the eSCL root, eSCL when it is missing; ty holds the
// model, which names the scanner when it is there. A result without a host
// or port, or on the IPP port, is no scanner.
func announcedScanner(f Found, scheme string) (Scanner, bool) {
	if f.Host == "" || f.Port == 0 || f.Port == ippPort {
		return Scanner{}, false
	}
	rs := strings.Trim(txt(f.TXT, "rs"), "/")
	if rs == "" {
		rs = "eSCL"
	}
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(f.Host, strconv.Itoa(int(f.Port))), Path: "/" + rs}
	name := strings.TrimSpace(txt(f.TXT, "ty"))
	if name == "" {
		name = f.Name
	}
	return Scanner{Name: name, URL: u.String(), UUID: strings.ToLower(txt(f.TXT, "uuid"))}, true
}

// txt looks key up in a TXT record. DNS-SD keys ignore case.
func txt(m map[string]string, key string) string {
	if v, ok := m[key]; ok {
		return v
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}
