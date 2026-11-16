// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The hub tests run in a synctest bubble: the tickers run on its fake clock,
// and synctest.Wait returns once every goroutine of the hub has done what it
// can, so the counts are exact and no test sleeps on the wall clock.

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

// settle lets the bubble's clock run for d, then waits until every goroutine
// in it is blocked again.
func settle(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func TestHubPollsOnlyWithSubscribers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var n atomic.Int32
		h := NewHub(countedPoll(&n), 10*time.Millisecond)

		settle(50 * time.Millisecond)
		if got := n.Load(); got != 0 {
			t.Fatalf("%d polls with no subscriber, want 0", got)
		}

		ctx, cancel := context.WithCancel(context.Background())
		ch := h.Subscribe(ctx)
		next(t, ch)
		settle(50 * time.Millisecond)
		if got := n.Load(); got != 6 {
			t.Fatalf("%d polls in 50 ms at 10 ms, want 6: one at once and five ticks", got)
		}
		if got := h.Subscribers(); got != 1 {
			t.Fatalf("Subscribers() = %d, want 1", got)
		}

		cancel()
		synctest.Wait() // the run has ended: no grace period to guess
		if got := h.Subscribers(); got != 0 {
			t.Fatalf("Subscribers() = %d after cancel, want 0", got)
		}
		settled := n.Load()
		settle(50 * time.Millisecond)
		if got := n.Load(); got != settled {
			t.Fatalf("%d polls after the last subscriber left, want %d", got, settled)
		}

		// The poller starts again for the next subscriber.
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		next(t, h.Subscribe(ctx2))
		if got := n.Load(); got != settled+1 {
			t.Errorf("%d polls after resubscribing, want %d", got, settled+1)
		}
	})
}

func TestHubOnePollerForTwoClients(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		settle(200 * time.Millisecond)

		// One poller: a poll at once plus ten ticks. Two pollers would make
		// twenty-two.
		if got := n.Load(); got != 11 {
			t.Errorf("%d polls in 200 ms at 20 ms for two clients, want 11", got)
		}
		if gotA.Load() != n.Load() || gotB.Load() != n.Load() {
			t.Errorf("clients got %d and %d snapshots, want %d each", gotA.Load(), gotB.Load(), n.Load())
		}
		if got := h.Subscribers(); got != 2 {
			t.Errorf("Subscribers() = %d, want 2", got)
		}
	})
}

func TestHubNewSubscriberGetsLastAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var n atomic.Int32
		h := NewHub(countedPoll(&n), time.Hour)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		next(t, h.Subscribe(ctx))

		// A reconnecting tab must not wait a whole interval for its first state.
		start := time.Now()
		next(t, h.Subscribe(ctx))
		if d := time.Since(start); d != 0 {
			t.Errorf("second subscriber waited %v", d)
		}
		if got := n.Load(); got != 1 {
			t.Errorf("%d polls, want 1: the second subscriber should reuse the last snapshot", got)
		}
	})
}

func TestHubRefreshPollsNow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var n atomic.Int32
		h := NewHub(countedPoll(&n), time.Hour)

		h.Refresh() // nobody watches: nothing to poll for
		settle(20 * time.Millisecond)
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
	})
}

func TestHubKeepsOnlyTheLatestForASlowClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var n atomic.Int32
		h := NewHub(func(context.Context) Snapshot {
			return Snapshot{Default: string(rune('a' + n.Add(1) - 1))}
		}, 5*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := h.Subscribe(ctx)
		settle(100 * time.Millisecond) // never read: the poller must not block
		if got := n.Load(); got != 21 {
			t.Fatalf("%d polls in 100 ms at 5 ms for a slow client, want 21", got)
		}
		if s := next(t, ch); s.Default != "u" {
			t.Errorf("slow client got snapshot %q, want the newest, u", s.Default)
		}
	})
}

func TestHubRunsTheLiveLoopOnlyWithSubscribers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var polls, lives atomic.Int32
		h := NewHub(countedPoll(&polls), time.Hour).WithLive(func(context.Context) bool {
			lives.Add(1)
			return false
		}, 10*time.Millisecond)

		settle(50 * time.Millisecond)
		if got := lives.Load(); got != 0 {
			t.Fatalf("%d live rounds with no subscriber, want 0", got)
		}

		ctx, cancel := context.WithCancel(context.Background())
		next(t, h.Subscribe(ctx))
		settle(60 * time.Millisecond)
		if got := lives.Load(); got != 7 {
			t.Fatalf("%d live rounds in 60 ms at 10 ms, want 7: one at once and six ticks", got)
		}

		cancel()
		synctest.Wait()
		settled := lives.Load()
		settle(60 * time.Millisecond)
		if got := lives.Load(); got != settled {
			t.Fatalf("%d live rounds after the last subscriber left, want %d", got, settled)
		}

		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		next(t, h.Subscribe(ctx2))
		synctest.Wait()
		if got := lives.Load(); got != settled+1 {
			t.Errorf("%d live rounds after resubscribing, want %d", got, settled+1)
		}
	})
}

func TestHubLiveChangePollsAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var polls atomic.Int32
		var changed atomic.Bool
		h := NewHub(countedPoll(&polls), time.Hour).WithLive(func(context.Context) bool {
			return changed.Swap(false)
		}, 10*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := h.Subscribe(ctx)
		next(t, ch)
		settle(30 * time.Millisecond)
		if got := polls.Load(); got != 1 {
			t.Fatalf("%d polls while nothing changed, want 1", got)
		}
		changed.Store(true)
		start := time.Now()
		next(t, ch) // the live change pushes a snapshot without waiting for the CUPS tick
		if d := time.Since(start); d > 10*time.Millisecond {
			t.Errorf("the change took %v to arrive, want at most one live tick", d)
		}
		if got := polls.Load(); got != 2 {
			t.Errorf("%d polls, want 2", got)
		}
	})
}

func TestHubLiveRoundNeverBlocksTheCUPSPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var polls atomic.Int32
		release := make(chan struct{})
		defer close(release)
		h := NewHub(countedPoll(&polls), 10*time.Millisecond).WithLive(func(ctx context.Context) bool {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return false
		}, 10*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h.Subscribe(ctx)
		settle(100 * time.Millisecond)
		if got := polls.Load(); got != 11 {
			t.Errorf("%d CUPS polls in 100 ms at 10 ms while a live round hangs, want 11", got)
		}
	})
}
