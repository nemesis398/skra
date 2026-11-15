// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"io"
	"time"

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

	Modify(ctx context.Context, name string, ch cups.Changes) error
	Delete(ctx context.Context, name string) error
	SetDefault(ctx context.Context, name string) error
	Accept(ctx context.Context, name string, on bool) error
	Pause(ctx context.Context, name string, on bool) error
	CancelAll(ctx context.Context, name string) error
	MoveAll(ctx context.Context, from, to string) error
	Command(ctx context.Context, printer, command string) (int, error)

	Devices(ctx context.Context, timeout time.Duration) ([]cups.Device, error)
	PPDs(ctx context.Context, deviceID, make string) ([]cups.PPD, error)
	Add(ctx context.Context, spec cups.AddSpec) error

	LogLevel(ctx context.Context) (string, error)
	SetLogLevel(ctx context.Context, level string) error
}

var _ CUPS = (*cups.Client)(nil)

// Deps are the collaborators the handlers use. Query asks one printer for its
// live status; it may be nil, and then no printer is ever queried. ReadLog
// returns the last n lines of the CUPS journal; it may be nil, and then
// cups.ReadLog runs journalctl. Tests set it so none of them does.
type Deps struct {
	CUPS    CUPS
	Store   *store.Store
	Query   func(ctx context.Context, deviceURI string) printer.Live
	ReadLog func(ctx context.Context, n int) ([]string, error)
}
