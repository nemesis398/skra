// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load("/nonexistent")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:7631" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.CUPSSocket != "/run/cups/cups.sock" {
		t.Errorf("CUPSSocket = %q", cfg.CUPSSocket)
	}
	if cfg.User != os.Getenv("USER") {
		t.Errorf("User = %q, want %q", cfg.User, os.Getenv("USER"))
	}
	wantState := filepath.Join(os.Getenv("HOME"), ".local/state/skra")
	if cfg.StateDir != wantState {
		t.Errorf("StateDir = %q, want %q", cfg.StateDir, wantState)
	}
	if cfg.IdleExit != "5m" {
		t.Errorf("IdleExit = %q", cfg.IdleExit)
	}
	if len(cfg.DeclaredQueues) != 0 || cfg.CupsdConfManaged {
		t.Errorf("declared queues or cupsd_conf_managed not empty by default: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	path := writeConfig(t, `{"listen":"127.0.0.1:9999","declared_queues":["Brother-MFC-J6975DW"],"cupsd_conf_managed":true}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if len(cfg.DeclaredQueues) != 1 || cfg.DeclaredQueues[0] != "Brother-MFC-J6975DW" {
		t.Errorf("DeclaredQueues = %v", cfg.DeclaredQueues)
	}
	if !cfg.CupsdConfManaged {
		t.Error("CupsdConfManaged = false")
	}
	if cfg.CUPSSocket != "/run/cups/cups.sock" || cfg.IdleExit != "5m" {
		t.Errorf("defaults lost: %+v", cfg)
	}
}

func TestLoadRejectsNonLoopback(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:7631", ":7631", "192.168.1.5:7631", "example.org:7631", "[::]:7631", "nonsense"} {
		path := writeConfig(t, `{"listen":"`+listen+`"}`)
		_, err := Load(path)
		if err == nil {
			t.Errorf("listen %q accepted", listen)
			continue
		}
		if !strings.Contains(err.Error(), "listen") {
			t.Errorf("listen %q: error %q does not name listen", listen, err)
		}
	}
}

func TestLoadAcceptsLoopbackForms(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:0", "localhost:7631", "[::1]:7631"} {
		if _, err := Load(writeConfig(t, `{"listen":"`+listen+`"}`)); err != nil {
			t.Errorf("listen %q rejected: %v", listen, err)
		}
	}
}

func TestLoadRejectsBadJSON(t *testing.T) {
	if _, err := Load(writeConfig(t, `{"listen":`)); err == nil {
		t.Error("broken JSON accepted")
	}
	if _, err := Load(writeConfig(t, `{"listn":"127.0.0.1:1"}`)); err == nil {
		t.Error("unknown key accepted")
	}
}

func TestLoadRejectsBadIdleExit(t *testing.T) {
	_, err := Load(writeConfig(t, `{"idle_exit":"soon"}`))
	if err == nil || !strings.Contains(err.Error(), "idle_exit") {
		t.Errorf("err = %v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/x")
	if got := DefaultPath(); got != "/home/x/.config/skra/config.json" {
		t.Errorf("DefaultPath = %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := DefaultPath(); got != "/xdg/skra/config.json" {
		t.Errorf("DefaultPath = %q", got)
	}
}
