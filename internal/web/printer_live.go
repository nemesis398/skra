// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/printer"
)

const (
	// liveStale is how old an answer may be before the page stops showing
	// it. The live loop asks every 5 s while a page is open, so an answer much
	// older than that comes from before the last time the hub slept.
	liveStale = 15 * time.Second
	// printersTimeout bounds the CUPS listing the first live round makes when no
	// CUPS poll has finished yet.
	printersTimeout = 3 * time.Second
	// liveEvery is how often the hub asks the printers themselves. New
	// copies it into the server, where a test can lower it.
	liveEvery = 5 * time.Second
)

// queryable reports whether skra may ask the printer behind a device URI
// itself: only those that speak IPP (ipp, ipps) or are found by name (dnssd).
// ipp://localhost:60000 is how ipp-usb shows up, so it counts. A USB, socket or
// lpd queue cannot be asked, and CUPS's word is all there is for it. Neither
// can a loop queue to a CUPS on this machine (localhost, port 631): skra
// speaks to CUPS only over its unix socket, never over TCP. This is the cheap
// early skip; printer.Query refuses the same address again after resolving.
func queryable(uri string) bool {
	scheme, _, ok := strings.Cut(uri, ":")
	switch {
	case !ok:
		return false
	case scheme == "dnssd":
		return true
	case scheme != "ipp" && scheme != "ipps":
		return false
	}
	_, err := url.Parse(uri)
	return err == nil && !printer.IsLocalCUPS(uri)
}

// livePoll asks every queryable printer for its status, in parallel, and keeps
// the answers for the CUPS polls to merge. It reports whether anything other
// than the time of the answer changed.
func (s *Server) livePoll(ctx context.Context) bool {
	if s.deps.Query == nil {
		return false
	}
	var targets []cups.Printer
	for _, p := range s.knownPrinters(ctx) {
		if queryable(p.DeviceURI) {
			targets = append(targets, p)
		}
	}
	answers := make([]printer.Live, len(targets))
	var wg sync.WaitGroup
	for i, p := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = s.deps.Query(ctx, p.DeviceURI)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil { // the last page closed: the answers say nothing true now
		return false
	}

	next := make(map[string]printer.Live, len(targets))
	for i, p := range targets {
		if answers[i].Own { // refused: this machine's own CUPS. Treat it as not queried.
			continue
		}
		if answers[i].At.IsZero() {
			answers[i].At = time.Now()
		}
		next[p.Name] = answers[i]
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	changed := len(next) != len(s.live)
	for name, l := range next {
		if old, ok := s.live[name]; !ok || !sameLive(old, l) {
			changed = true
		}
	}
	s.live = next
	return changed
}

// knownPrinters is the queues of the last good CUPS poll. The first live round
// of a bare event stream can start before any poll has finished, so then it
// asks CUPS itself.
func (s *Server) knownPrinters(ctx context.Context) []cups.Printer {
	s.goodMu.RLock()
	ps := slices.Clone(s.good.Printers)
	s.goodMu.RUnlock()
	if len(ps) > 0 || s.deps.CUPS == nil {
		return ps
	}
	ctx, cancel := context.WithTimeout(ctx, printersTimeout)
	defer cancel()
	ps, _ = s.deps.CUPS.Printers(ctx)
	return ps
}

// liveNow is a copy of the answers that are still fresh, for a snapshot.
func (s *Server) liveNow() map[string]printer.Live {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	var out map[string]printer.Live
	for name, l := range s.live {
		if time.Since(l.At) > liveStale {
			continue
		}
		if out == nil {
			out = map[string]printer.Live{}
		}
		out[name] = l
	}
	return out
}

func sameLive(a, b printer.Live) bool {
	return a.Reachable == b.Reachable && a.Message == b.Message &&
		slices.Equal(a.Reasons, b.Reasons) && slices.Equal(a.Markers, b.Markers)
}
