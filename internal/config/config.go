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
	"os"
	"path/filepath"
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
}

func defaults() Config {
	return Config{
		Listen:     "127.0.0.1:7631",
		CUPSSocket: "/run/cups/cups.sock",
		User:       os.Getenv("USER"),
		StateDir:   filepath.Join(os.Getenv("HOME"), ".local", "state", "skra"),
		IdleExit:   "5m",
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
// unknown key, a listen address that is not loopback, or an idle_exit that is
// not a duration is an error.
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
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validate(cfg Config) error {
	if err := requireLoopback(cfg.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if _, err := time.ParseDuration(cfg.IdleExit); err != nil {
		return fmt.Errorf("idle_exit: %w", err)
	}
	return nil
}

// requireLoopback accepts host:port where host is localhost or a loopback IP.
// An empty host, 0.0.0.0 and [::] would listen on every interface.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%q is not a loopback address; skra only listens on 127.0.0.1, ::1 or localhost", addr)
}
