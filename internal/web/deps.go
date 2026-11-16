// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"io"
	"time"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/escl"
	"github.com/nemesis398/skra/internal/printer"
	"github.com/nemesis398/skra/internal/scan"
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
	SetLogLevel(ctx context.Context, level, password string) error
}

var _ CUPS = (*cups.Client)(nil)

// ScannerClient lists the *escl.Client methods the Scan tab calls, so tests
// can stand in the fake scanner or a client of their own.
type ScannerClient interface {
	Capabilities(ctx context.Context) (escl.Caps, error)
	Status(ctx context.Context) (escl.Status, error)
	Start(ctx context.Context, s escl.Settings) (job string, err error)
	NextPage(ctx context.Context, job string) (io.ReadCloser, error)
	Cancel(ctx context.Context, job string) error
}

var _ ScannerClient = (*escl.Client)(nil)

// Deps are the collaborators the handlers use. Query asks one printer for its
// live status; it may be nil, and then no printer is ever queried. ReadLog
// returns the last n lines of the CUPS journal; it may be nil, and then
// cups.ReadLog runs journalctl. Tests set it so none of them does.
//
// Resolver resolves a dnssd address that is added or set in the settings, so
// that the name of a queue this machine's CUPS shares itself is refused. It
// may be nil, and then a dnssd address is taken without that check; the live
// status query still never connects there.
//
// Scanners lists the scanners the Scan tab offers; it may be nil, and then
// there are none. Scanner makes a client for the scanner at an eSCL root; it
// may be nil, and then escl.New does. Scans keeps the scans while they are
// open; it may be nil, and then nothing can be scanned.
type Deps struct {
	CUPS     CUPS
	Store    *store.Store
	Query    func(ctx context.Context, deviceURI string) printer.Live
	ReadLog  func(ctx context.Context, n int) ([]string, error)
	Resolver printer.Resolver

	Scanners func(ctx context.Context) []escl.Scanner
	Scanner  func(url string) (ScannerClient, error)
	Scans    *scan.Store
}
