// SPDX-License-Identifier: AGPL-3.0-or-later

// Command skra serves the print and scan web interface.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/nemesis398/skra/internal/activation"
	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/escl"
	"github.com/nemesis398/skra/internal/printer"
	"github.com/nemesis398/skra/internal/scan"
	"github.com/nemesis398/skra/internal/store"
	"github.com/nemesis398/skra/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "skra:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	defer db.Close()
	scans, err := scan.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	token, err := web.LoadToken(cfg.StateDir)
	if err != nil {
		return err
	}
	ln, activated, err := activation.Listener(cfg.Listen)
	if err != nil {
		return err
	}
	log.Printf("listening on %s (socket-activated: %t)", ln.Addr(), activated)

	deps := web.Deps{
		CUPS:  cups.New(cfg.CUPSSocket, cfg.User),
		Store: db,
		Query: func(ctx context.Context, deviceURI string) printer.Live {
			return printer.Query(ctx, deviceURI, printer.Avahi{})
		},
		Scanners: func(ctx context.Context) []escl.Scanner {
			return escl.Find(ctx, cfg.Scanners, printer.Avahi{})
		},
		Scanner: func(url string) (web.ScannerClient, error) {
			c, err := escl.New(url)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		Scans: scans,
	}
	// Only a service that systemd can start again on the next request may
	// exit when idle; one started by hand stays.
	var idle time.Duration
	if activated {
		if idle, err = time.ParseDuration(cfg.IdleExit); err != nil {
			return fmt.Errorf("idle_exit: %w", err)
		}
	}
	return web.New(cfg, deps, token).Serve(ln, idle)
}
