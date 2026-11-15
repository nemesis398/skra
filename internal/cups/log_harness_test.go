// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package cups_test

import (
	"context"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/testenv"
)

// TestLogLevelRoundTrip reads the harness's log level, sets debug through CUPS
// itself, reads it back, and puts cupsd.conf back byte for byte. A change of
// cupsd.conf restarts the scheduler, so the test holds the harness alone.
func TestLogLevelRoundTrip(t *testing.T) {
	c := testenv.ExclusiveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	before, err := c.ReadConf(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The original text, not a rewrite of it, and wait until CUPS is back
		// so the next test does not meet the restart.
		rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rcancel()
		if err := c.WriteConf(rctx, before); err != nil {
			t.Errorf("restoring cupsd.conf: %v", err)
		}
		if _, err := c.Printers(rctx); err != nil {
			t.Errorf("CUPS does not list printers after the restore: %v", err)
		}
	})
	orig, err := c.LogLevel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("log level before: %s", orig)

	want := "debug"
	if orig == "debug" {
		want = "info"
	}
	if err := c.SetLogLevel(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := c.LogLevel(ctx)
	if err != nil || got != want {
		t.Fatalf("LogLevel = %q, %v; want %q", got, err, want)
	}
	// Only the LogLevel line changed, and CUPS is back.
	after, err := c.ReadConf(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != cups.ReplaceLogLevel(before, want) {
		t.Errorf("cupsd.conf differs beyond the LogLevel line:\n%s", after)
	}
	if _, err := c.Printers(ctx); err != nil {
		t.Errorf("CUPS does not list printers after the change: %v", err)
	}
	// Setting the same level again writes nothing and does not restart.
	if err := c.SetLogLevel(ctx, want); err != nil {
		t.Errorf("setting %s again: %v", want, err)
	}
}
