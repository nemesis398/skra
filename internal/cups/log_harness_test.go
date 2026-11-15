// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package cups_test

import (
	"context"
	"errors"
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
	if err := c.SetLogLevel(ctx, want, ""); err != nil {
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
	if err := c.SetLogLevel(ctx, want, ""); err != nil {
		t.Errorf("setting %s again: %v", want, err)
	}
}

// TestLogLevelNeedsThePassword changes the log level as a user other than
// root, against the harness's <Location /admin/conf>, which asks for Basic
// and the password of a SystemGroup member for a PUT like Fedora's. CUPS
// takes PeerCred for a PUT from root alone, so without the password the
// change is refused; with it, it goes through.
func TestLogLevelNeedsThePassword(t *testing.T) {
	root := testenv.ExclusiveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	before, err := root.ReadConf(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rcancel()
		if err := root.WriteConf(rctx, before); err != nil {
			t.Errorf("restoring cupsd.conf: %v", err)
		}
	})
	orig, err := root.LogLevel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := "debug"
	if orig == "debug" {
		want = "info"
	}
	admin := testenv.ClientAs(t, testenv.Admin)
	for _, tc := range []struct {
		name     string
		c        *cups.Client
		password string
		err      error
	}{
		{"no password", admin, "", cups.ErrNeedsPassword},
		{"a wrong password", admin, "not-" + testenv.Password, cups.ErrWrongPassword},
		{"outside the system group", testenv.ClientAs(t, testenv.NonAdmin), testenv.Password, cups.ErrNotSystemGroup},
	} {
		if err := tc.c.SetLogLevel(ctx, want, tc.password); !errors.Is(err, tc.err) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.err)
		}
		if now, err := root.ReadConf(ctx); err != nil || now != before {
			t.Fatalf("%s: cupsd.conf changed (%v):\n%s", tc.name, err, now)
		}
	}
	if err := admin.SetLogLevel(ctx, want, testenv.Password); err != nil {
		t.Fatalf("with the password: %v", err)
	}
	if got, err := admin.LogLevel(ctx); err != nil || got != want {
		t.Errorf("LogLevel = %q, %v; want %q", got, err, want)
	}
	if after, err := root.ReadConf(ctx); err != nil || after != cups.ReplaceLogLevel(before, want) {
		t.Errorf("cupsd.conf differs beyond the LogLevel line (%v):\n%s", err, after)
	}
}
