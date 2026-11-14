// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/store"
)

// CUPS lists the *cups.Client methods the web layer calls, so tests can
// stand in a fake. Later tasks add the methods their handlers need.
type CUPS interface {
	Printers(ctx context.Context) ([]cups.Printer, error)
	Default(ctx context.Context) (string, error)
	Jobs(ctx context.Context, which string, limit int) ([]cups.Job, error)
}

var _ CUPS = (*cups.Client)(nil)

// Deps are the collaborators the handlers use. A later task adds the printer
// query.
type Deps struct {
	CUPS  CUPS
	Store *store.Store
}
