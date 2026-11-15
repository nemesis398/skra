// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads skra's JSON configuration. A missing file is not an
// error: every key has a default, so the service runs without any config.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the content of ~/.config/skra/config.json.
type Config struct {
	Listen           string   `json:"listen"`
	CUPSSocket       string   `json:"cups_socket"`
	User             string   `json:"user"`
	StateDir         string   `json:"state_dir"`
	DeclaredQueues   []string `json:"declared_queues"`
	CupsdConfManaged bool     `json:"cupsd_conf_managed"`
	IdleExit         string   `json:"idle_exit"`
	// Scanners are scanners Mika names by hand, listed before the ones the
	// network announces.
	Scanners []Scanner `json:"scanners"`
	// ScanFolder is the folder scans are saved to unless Mika picks another.
	ScanFolder string `json:"scan_folder"`
}

// Scanner is a scanner in the config: a name to show and the URL of its eSCL
// root, such as http://192.0.2.7:80/eSCL.
type Scanner struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func defaults() Config {
	return Config{
		Listen:     "127.0.0.1:7631",
		CUPSSocket: "/run/cups/cups.sock",
		User:       os.Getenv("USER"),
		StateDir:   filepath.Join(os.Getenv("HOME"), ".local", "state", "skra"),
		IdleExit:   "5m",
		ScanFolder: filepath.Join(os.Getenv("HOME"), "Nextcloud", "Documents", "Scans"),
	}
}

// DefaultPath returns $XDG_CONFIG_HOME/skra/config.json, or
// ~/.config/skra/config.json when XDG_CONFIG_HOME is unset.
func DefaultPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(base, "skra", "config.json")
}

// Load reads path over the defaults. A missing file gives the defaults. An
// unknown key, a listen address on another host than 127.0.0.1 or localhost,
// an idle_exit that is not a duration, a scanner without a name or without a
// plain http or https URL, or a scan_folder that is empty or neither absolute
// nor under ~/ is an error.
func Load(path string) (Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return cfg, nil
	case err != nil:
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.ScanFolder, err = scanFolder(cfg.ScanFolder); err != nil {
		return Config{}, fmt.Errorf("scan_folder: %w", err)
	}
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// scanFolder turns a leading ~/ into the home directory and requires an
// absolute path after that, so a scan is never saved relative to wherever
// skra was started. The path comes back cleaned.
func scanFolder(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("empty; name the folder scans are saved to")
	}
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		dir = filepath.Join(os.Getenv("HOME"), rest)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%q is not an absolute path; write it as /… or ~/…", dir)
	}
	return filepath.Clean(dir), nil
}

func validate(cfg Config) error {
	if err := requireLoopback(cfg.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if _, err := time.ParseDuration(cfg.IdleExit); err != nil {
		return fmt.Errorf("idle_exit: %w", err)
	}
	for i, sc := range cfg.Scanners {
		if err := checkScanner(sc); err != nil {
			return fmt.Errorf("scanners[%d]: %w", i, err)
		}
	}
	return nil
}

// checkScanner accepts a scanner with a name and an http or https URL that
// has a host and no user name or password. The scanner client refuses the
// same URLs; the check here only reports them when skra starts.
func checkScanner(sc Scanner) error {
	if strings.TrimSpace(sc.Name) == "" {
		return errors.New("no name")
	}
	u, err := url.Parse(sc.URL)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q is not an http or https address", sc.URL)
	}
	if u.User != nil {
		return fmt.Errorf("%q carries a user name", sc.URL)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("no host in %q", sc.URL)
	}
	return nil
}

// requireLoopback accepts host:port where host is 127.0.0.1 or localhost,
// the two names the request guard allows in the Host header. An empty host,
// 0.0.0.0 and [::] would listen on every interface; another loopback address
// such as [::1] would be reached under a name the guard refuses.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %w", addr, err)
	}
	if host == "127.0.0.1" || host == "localhost" {
		return nil
	}
	return fmt.Errorf("%q is not allowed; skra only listens on 127.0.0.1 or localhost", addr)
}
