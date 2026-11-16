// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/config"
)

// Only a service that its socket started exits when idle, after idle_exit;
// one started by hand stays, whatever idle_exit says.
func TestIdleFor(t *testing.T) {
	load := func(body string) config.Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	for _, tc := range []struct {
		name      string
		activated bool
		cfg       config.Config
		want      time.Duration
	}{
		{"socket-activated with the default", true, load(`{}`), 5 * time.Minute},
		{"socket-activated with 90s", true, load(`{"idle_exit":"90s"}`), 90 * time.Second},
		{"socket-activated with 0s", true, load(`{"idle_exit":"0s"}`), 0},
		{"started by hand with the default", false, load(`{}`), 0},
		{"started by hand with 90s", false, load(`{"idle_exit":"90s"}`), 0},
	} {
		if got := idleFor(tc.activated, tc.cfg); got != tc.want {
			t.Errorf("%s: idleFor = %v, want %v", tc.name, got, tc.want)
		}
	}
}
