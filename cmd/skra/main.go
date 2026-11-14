// SPDX-License-Identifier: AGPL-3.0-or-later

// Command skra serves the print and scan web interface.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/nemesis398/skra/internal/activation"
	"github.com/nemesis398/skra/internal/config"
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
	ln, activated, err := activation.Listener(cfg.Listen)
	if err != nil {
		return err
	}
	log.Printf("listening on %s (socket-activated: %t)", ln.Addr(), activated)

	return http.Serve(ln, web.New(cfg, web.Deps{}).Handler())
}
