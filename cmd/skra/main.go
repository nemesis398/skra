// SPDX-License-Identifier: AGPL-3.0-or-later

// Command skra serves the print and scan web interface.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/nemesis398/skra/internal/activation"
	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/printer"
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
	}
	return http.Serve(ln, web.New(cfg, deps).Handler())
}
