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

// The request guard allows a Host of 127.0.0.1 or localhost only, so a
// listen address that a browser would reach under another name is refused.
func TestLoadRejectsLoopbackTheGuardRefuses(t *testing.T) {
	for _, listen := range []string{"[::1]:7631", "127.0.0.2:7631", "127.1:7631", "[::ffff:127.0.0.1]:7631", "LOCALHOST:7631"} {
		_, err := Load(writeConfig(t, `{"listen":"`+listen+`"}`))
		if err == nil || !strings.Contains(err.Error(), "listen") || !strings.Contains(err.Error(), "127.0.0.1 or localhost") {
			t.Errorf("listen %q: err = %v", listen, err)
		}
	}
}

func TestLoadAcceptsLoopbackForms(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:0", "127.0.0.1:7631", "localhost:7631"} {
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

func TestConfigScanners(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"scanners":[{"name":"skrá fake scanner","url":"http://127.0.0.1:8632/eSCL"}]}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Scanners) != 1 || cfg.Scanners[0] != (Scanner{Name: "skrá fake scanner", URL: "http://127.0.0.1:8632/eSCL"}) {
		t.Errorf("Scanners = %+v", cfg.Scanners)
	}
	for _, sc := range []string{
		`{"name":"x","url":"http://mika:secret@192.0.2.7/eSCL"}`,
		`{"name":"x","url":"http://mika@192.0.2.7/eSCL"}`,
		`{"name":"x","url":"ftp://192.0.2.7/eSCL"}`,
		`{"name":"x","url":"ipp://192.0.2.7/eSCL"}`,
		`{"name":"x","url":"192.0.2.7/eSCL"}`,
		`{"name":"x","url":"http:///eSCL"}`,
		`{"name":"x","url":"http://%zz/eSCL"}`,
		`{"name":"x","url":""}`,
		`{"name":"","url":"http://192.0.2.7/eSCL"}`,
		`{"name":"  ","url":"http://192.0.2.7/eSCL"}`,
		`{"url":"http://192.0.2.7/eSCL"}`,
	} {
		_, err := Load(writeConfig(t, `{"scanners":[`+sc+`]}`))
		if err == nil || !strings.Contains(err.Error(), "scanners") {
			t.Errorf("scanner %s: err = %v", sc, err)
		}
	}
	if _, err := Load(writeConfig(t, `{"scanners":[{"name":"Brother","url":"https://192.0.2.7:443/eSCL"}]}`)); err != nil {
		t.Errorf("https scanner refused: %v", err)
	}
}

func TestScanFolder(t *testing.T) {
	t.Setenv("HOME", "/home/x")
	cfg, err := Load("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScanFolder != "/home/x/Nextcloud/Documents/Scans" {
		t.Errorf("ScanFolder = %q", cfg.ScanFolder)
	}
	if len(cfg.Scanners) != 0 {
		t.Errorf("Scanners = %v, want none by default", cfg.Scanners)
	}
	cfg, err = Load(writeConfig(t, `{"scan_folder":"/srv/scans"}`))
	if err != nil || cfg.ScanFolder != "/srv/scans" {
		t.Errorf("ScanFolder = %q, %v", cfg.ScanFolder, err)
	}
}

// A scan folder may start with ~/ for the home directory. Anything else must
// be an absolute path, so a save never lands relative to wherever skra was
// started, and an empty one is refused.
func TestScanFolderPath(t *testing.T) {
	t.Setenv("HOME", "/home/x")
	for in, want := range map[string]string{
		`~/Nextcloud/Documents/Scans`: "/home/x/Nextcloud/Documents/Scans",
		`~/Scans/`:                    "/home/x/Scans",
		`/srv//scans/`:                "/srv/scans",
	} {
		cfg, err := Load(writeConfig(t, `{"scan_folder":"`+in+`"}`))
		if err != nil || cfg.ScanFolder != want {
			t.Errorf("scan_folder %q: got %q, %v; want %q", in, cfg.ScanFolder, err, want)
		}
	}
	for _, in := range []string{``, `  `, `Scans`, `./Scans`, `~Scans`, `~`, `~mika/Scans`} {
		_, err := Load(writeConfig(t, `{"scan_folder":"`+in+`"}`))
		if err == nil || !strings.Contains(err.Error(), "scan_folder") {
			t.Errorf("scan_folder %q: err = %v", in, err)
		}
	}
}
