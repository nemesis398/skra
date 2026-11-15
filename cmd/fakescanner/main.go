// SPDX-License-Identifier: AGPL-3.0-or-later

// Command fakescanner serves a fake eSCL scanner for `make dev`. See
// dev/harness/README.md for how to drive it.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/nemesis398/skra/internal/escl/fakescanner"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakescanner:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8632", "address to listen on")
	pages := flag.Int("adf-pages", 3, "sheets in the feeder")
	flag.Parse()

	srv := &http.Server{
		Addr:              *listen,
		Handler:           fakescanner.New(fakescanner.Options{AdfPages: *pages}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("fake eSCL scanner on http://%s/eSCL/", *listen)
	return srv.ListenAndServe()
}
