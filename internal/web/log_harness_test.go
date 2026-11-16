// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package web

import (
	"context"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/store"
	"github.com/nemesis398/skra/internal/testenv"
)

// TestLogLevelWhileAJobPrints changes the log level while a job is printing,
// on a queue whose device at 192.0.2.1 (a documentation address) never
// answers, so the job stays processing. CUPS 2.4 then puts its reload off
// until the job is done or ReloadTimeout has passed. It pins what the
// harness's cupsd does: it keeps answering GETs while the reload waits, with
// the new level in cupsd.conf, so skrá waits out its ten seconds and answers
// ErrReloadWaits, and the history counts the change as made.
func TestLogLevelWhileAJobPrints(t *testing.T) {
	c := testenv.ExclusiveClient(t)
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(config.Config{Listen: "127.0.0.1:7631", User: "root"}, Deps{CUPS: c, Store: db}, NewToken())
	h := s.Handler()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	orig, err := c.LogLevel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := "debug"
	if orig == "debug" {
		want = "info"
	}
	// Cleanups run last first: the queue and its job go before the level is
	// put back, so the reload for the restore does not wait for the job.
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rcancel()
		if err := c.SetLogLevel(rctx, orig, ""); err != nil {
			t.Errorf("putting the level back to %s: %v", orig, err)
		}
		if _, err := c.Printers(rctx); err != nil {
			t.Errorf("CUPS does not list printers after the restore: %v", err)
		}
	})
	const q = "skra-web-stuck"
	testenv.Exec(t, "lpadmin", "-p", q, "-E", "-v", "socket://192.0.2.1:9100")
	t.Cleanup(func() { testenv.Command("lpadmin", "-x", q).Run() }) // takes its job along
	testenv.Exec(t, "sh", "-c", "echo stuck | lp -d "+q+" -o raw -t skra-stuck")
	for !printingOn(t, ctx, c, q) {
		time.Sleep(200 * time.Millisecond)
	}

	// A second client looks at cupsd.conf while skrá waits for the reload.
	var looked, answered atomic.Int32
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		probe := cups.New(testenv.Socket(t), "root")
		for {
			select {
			case <-stop:
				return
			case <-time.After(500 * time.Millisecond):
			}
			pctx, pcancel := context.WithTimeout(context.Background(), time.Second)
			got, err := probe.LogLevel(pctx)
			pcancel()
			looked.Add(1)
			if err == nil && got == want {
				answered.Add(1)
			}
		}
	}()
	start := time.Now()
	rec := postTo(s, h, "/printers/log/level", url.Values{"level": {want}})
	took := time.Since(start)
	close(stop)
	<-done
	t.Logf("%d after %v: %q; %d of %d looks during the wait answered %s",
		rec.Code, took.Round(10*time.Millisecond), rec.Body.String(), answered.Load(), looked.Load(), want)

	if rec.Code != 200 || rec.Body.String() != cups.ErrReloadWaits.Error() {
		t.Errorf("status %d, body %q; want 200 and ErrReloadWaits", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, s); ev.Action != "cups.loglevel" || !ev.OK || ev.Detail != orig+" → "+want {
		t.Errorf("history %+v, want the change counted as made", ev)
	}
	if answered.Load() == 0 {
		t.Errorf("no look during the wait answered: %d looks", looked.Load())
	}
	if !printingOn(t, ctx, c, q) {
		t.Error("the job no longer prints after the wait")
	}
}

// printingOn reports whether CUPS has a job printing on queue q.
func printingOn(t *testing.T, ctx context.Context, c *cups.Client, q string) bool {
	t.Helper()
	jobs, err := c.Jobs(ctx, "not-completed", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Printer == q && j.State == 5 {
			return true
		}
	}
	return false
}
