// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/printer"
)

// Snapshot is what one poll of CUPS saw. Err is set when CUPS did not answer
// all of it; the other fields then hold the last good data.
type Snapshot struct {
	Printers     []cups.Printer
	Default      string
	Active, Done []cups.Job
	Err          string
	At           time.Time
	// Live is the last answer of each printer that skra asks itself, keyed by
	// queue name. A queue it does not ask has no entry.
	Live map[string]printer.Live
}

// Hub runs one poller for any number of subscribers. The poller, and the
// optional live loop, run only while somebody subscribes, so an idle service
// does no work at all.
type Hub struct {
	poll  func(context.Context) Snapshot
	every time.Duration

	refresh  chan struct{}
	live     func(context.Context) bool
	liveTick time.Duration

	mu     sync.Mutex
	subs   map[chan Snapshot]struct{}
	last   Snapshot
	have   bool // last is the result of a poll of the running poller
	cancel context.CancelFunc
}

// NewHub returns a hub that calls poll every interval while it has subscribers.
func NewHub(poll func(context.Context) Snapshot, every time.Duration) *Hub {
	return &Hub{
		poll:    poll,
		every:   every,
		refresh: make(chan struct{}, 1),
		subs:    map[chan Snapshot]struct{}{},
	}
}

// WithLive adds a second loop that calls fn every interval while somebody
// subscribes, beside the CUPS poll and on its own goroutine, so a printer
// that is slow to answer never holds the poll up. fn reports whether it
// learned something new; the hub then polls at once so the snapshot carries
// it. Call it before the first Subscribe.
func (h *Hub) WithLive(fn func(context.Context) bool, every time.Duration) *Hub {
	h.live, h.liveTick = fn, every
	return h
}

// Subscribe returns a channel of snapshots that closes when ctx ends. The
// first subscriber starts the poller, which polls at once. A later one gets
// the last snapshot at once, without waiting for the next tick. A reader that
// falls behind skips to the newest snapshot instead of blocking the poller.
func (h *Hub) Subscribe(ctx context.Context) <-chan Snapshot {
	ch := make(chan Snapshot, 1)
	h.mu.Lock()
	if len(h.subs) == 0 {
		pctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		h.have = false
		select { // a Refresh from before this run is moot: the run polls at once
		case <-h.refresh:
		default:
		}
		go h.run(pctx)
		if h.live != nil {
			go h.runLive(pctx)
		}
	}
	h.subs[ch] = struct{}{}
	if h.have {
		ch <- h.last
	}
	h.mu.Unlock()

	go func() {
		<-ctx.Done()
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, ch)
		close(ch)
		if len(h.subs) == 0 && h.cancel != nil {
			h.cancel()
			h.cancel = nil
		}
	}()
	return ch
}

// Subscribers is the number of open subscriptions.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Refresh polls now instead of at the next tick, for use after an action. It
// does nothing while nobody subscribes.
func (h *Hub) Refresh() {
	select {
	case h.refresh <- struct{}{}:
	default:
	}
}

func (h *Hub) run(ctx context.Context) {
	t := time.NewTicker(h.every)
	defer t.Stop()
	h.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.refresh:
		}
		h.pollOnce(ctx)
	}
}

func (h *Hub) runLive(ctx context.Context) {
	t := time.NewTicker(h.liveTick)
	defer t.Stop()
	for {
		if h.live(ctx) && ctx.Err() == nil {
			h.Refresh()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *Hub) pollOnce(ctx context.Context) {
	snap := h.poll(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if ctx.Err() != nil { // the last subscriber left during the poll
		return
	}
	h.last, h.have = snap, true
	for ch := range h.subs {
		select { // drop an unread snapshot: the newest one wins
		case <-ch:
		default:
		}
		ch <- snap
	}
}

// events streams the overview fragments as Server-Sent Events: the whole set
// on connect, then every fragment that changed after a poll.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	sent := map[string]string{} // per connection: what this tab shows now
	for snap := range s.hub.Subscribe(r.Context()) {
		for _, f := range s.fragments(snap) {
			if sent[f.event] == f.html {
				continue
			}
			sent[f.event] = f.html
			if err := writeEvent(w, f.event, f.html); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// lineBreaks folds CRLF and a bare CR into LF: EventSource ends a line at any
// of the three, so a CR inside a job name would otherwise cut the frame short.
var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// writeEvent frames html as one event; every line of a multi-line payload
// needs its own "data:" prefix.
func writeEvent(w http.ResponseWriter, event, html string) error {
	html = lineBreaks.Replace(html)
	var b strings.Builder
	fmt.Fprintf(&b, "event: %s\n", event)
	for _, line := range strings.Split(html, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	_, err := w.Write([]byte(b.String()))
	return err
}
