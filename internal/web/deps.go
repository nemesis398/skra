// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"io"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/printer"
	"github.com/nemesis398/skra/internal/store"
)

// CUPS lists the *cups.Client methods the web layer calls, so tests can
// stand in a fake. Later tasks add the methods their handlers need.
type CUPS interface {
	Printers(ctx context.Context) ([]cups.Printer, error)
	Default(ctx context.Context) (string, error)
	Jobs(ctx context.Context, which string, limit int) ([]cups.Job, error)

	Hold(ctx context.Context, id int) error
	Release(ctx context.Context, id int) error
	Cancel(ctx context.Context, id int) error
	Restart(ctx context.Context, id int) error
	Move(ctx context.Context, id int, to string) error
	Print(ctx context.Context, printer, name, format string, o cups.JobOptions, doc io.Reader) (int, error)
	PrintTestPage(ctx context.Context, printer string, hold bool) (int, error)
}

var _ CUPS = (*cups.Client)(nil)

// Deps are the collaborators the handlers use. Query asks one printer for its
// live status; it may be nil, and then no printer is ever queried.
type Deps struct {
	CUPS  CUPS
	Store *store.Store
	Query func(ctx context.Context, deviceURI string) printer.Live
}
