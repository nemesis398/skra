// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func countedPoll(n *atomic.Int32) func(context.Context) Snapshot {
	return func(context.Context) Snapshot {
		n.Add(1)
		return Snapshot{Default: "poll", At: time.Now()}
	}
}

// next waits up to a second for one snapshot.
func next(t *testing.T, ch <-chan Snapshot) Snapshot {
	t.Helper()
	select {
	case s, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed")
		}
		return s
	case <-time.After(time.Second):
		t.Fatal("no snapshot within 1 s")
	}
	return Snapshot{}
}

func TestHubPollsOnlyWithSubscribers(t *testing.T) {
	var n atomic.Int32
	h := NewHub(countedPoll(&n), 10*time.Millisecond)

	time.Sleep(50 * time.Millisecond)
	if got := n.Load(); got != 0 {
		t.Fatalf("%d polls with no subscriber, want 0", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch := h.Subscribe(ctx)
	next(t, ch)
	time.Sleep(50 * time.Millisecond)
	if got := n.Load(); got < 2 {
		t.Fatalf("%d polls in 50 ms with a subscriber, want at least 2", got)
	}
	if got := h.Subscribers(); got != 1 {
		t.Fatalf("Subscribers() = %d, want 1", got)
	}

	cancel()
	time.Sleep(20 * time.Millisecond) // a poll in flight may still finish
	if got := h.Subscribers(); got != 0 {
		t.Fatalf("Subscribers() = %d after cancel, want 0", got)
	}
	settled := n.Load()
	time.Sleep(50 * time.Millisecond)
	if got := n.Load(); got != settled {
		t.Fatalf("%d polls after the last subscriber left, want %d", got, settled)
	}

	// The poller starts again for the next subscriber.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	next(t, h.Subscribe(ctx2))
	if got := n.Load(); got <= settled {
		t.Errorf("no poll after resubscribing (%d)", got)
	}
}

func TestHubOnePollerForTwoClients(t *testing.T) {
	var n atomic.Int32
	h := NewHub(countedPoll(&n), 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := h.Subscribe(ctx), h.Subscribe(ctx)

	var gotA, gotB atomic.Int32
	go func() {
		for range a {
			gotA.Add(1)
		}
	}()
	go func() {
		for range b {
			gotB.Add(1)
		}
	}()
	time.Sleep(200 * time.Millisecond)

	// One poller: an immediate poll plus about ten ticks. Two pollers would
	// make about twenty.
	if got := n.Load(); got < 2 || got > 14 {
		t.Errorf("%d polls in 200 ms at 20 ms for two clients, want 2 to 14", got)
	}
	if gotA.Load() < 2 || gotB.Load() < 2 {
		t.Errorf("clients got %d and %d snapshots, want at least 2 each", gotA.Load(), gotB.Load())
	}
	if got := h.Subscribers(); got != 2 {
		t.Errorf("Subscribers() = %d, want 2", got)
	}
}

func TestHubNewSubscriberGetsLastAtOnce(t *testing.T) {
	var n atomic.Int32
	h := NewHub(countedPoll(&n), time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	next(t, h.Subscribe(ctx))

	// A reconnecting tab must not wait a whole interval for its first state.
	start := time.Now()
	next(t, h.Subscribe(ctx))
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("second subscriber waited %v", d)
	}
	if got := n.Load(); got != 1 {
		t.Errorf("%d polls, want 1: the second subscriber should reuse the last snapshot", got)
	}
}

func TestHubRefreshPollsNow(t *testing.T) {
	var n atomic.Int32
	h := NewHub(countedPoll(&n), time.Hour)

	h.Refresh() // nobody watches: nothing to poll for
	time.Sleep(20 * time.Millisecond)
	if got := n.Load(); got != 0 {
		t.Fatalf("Refresh polled %d times with no subscriber", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.Subscribe(ctx)
	next(t, ch)
	h.Refresh()
	next(t, ch)
	if got := n.Load(); got != 2 {
		t.Errorf("%d polls, want 2", got)
	}
}

func TestHubKeepsOnlyTheLatestForASlowClient(t *testing.T) {
	var n atomic.Int32
	h := NewHub(func(context.Context) Snapshot {
		return Snapshot{Default: string(rune('a' + n.Add(1) - 1))}
	}, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.Subscribe(ctx)
	time.Sleep(100 * time.Millisecond) // never read: the poller must not block
	if got := n.Load(); got < 5 {
		t.Fatalf("poller blocked on a slow client after %d polls", got)
	}
	s := next(t, ch)
	if s.Default == "a" {
		t.Errorf("slow client got the oldest snapshot %q, want a newer one", s.Default)
	}
}
