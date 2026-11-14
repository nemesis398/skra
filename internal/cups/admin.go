// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"context"
	"errors"
	"strings"

	"github.com/nemesis398/skra/internal/ipp"
)

// Changes are the queue settings Modify sends. A nil field is left as it is.
type Changes struct {
	Info, Location, DeviceURI, Sides, Color, Media, ErrorPolicy *string
	Shared                                                      *bool
	Quality                                                     *int
}

// commandFormat is the document type of a printer maintenance command.
const commandFormat = "application/vnd.cups-command"

// adminOp sends an operation on one queue to the admin resource. The caller
// builds a fresh request, because Do adds the user name to it.
func (c *Client) adminOp(op uint16, name string) (*ipp.Request, error) {
	if name == "" {
		return nil, errors.New("no queue named")
	}
	return ipp.NewRequest(op).Op(ipp.URI("printer-uri", PrinterURI(name))), nil
}

func (c *Client) queueOp(ctx context.Context, op uint16, name string) error {
	req, err := c.adminOp(op, name)
	if err != nil {
		return err
	}
	_, err = c.Do(ctx, "/admin/", req, nil)
	return err
}

// Modify changes the settings of a queue. Only the fields that are set are
// sent, as printer attributes, so everything else stays as CUPS has it. With
// nothing set it sends nothing.
func (c *Client) Modify(ctx context.Context, name string, ch Changes) error {
	var attrs []ipp.Attr
	if ch.Info != nil {
		attrs = append(attrs, ipp.Text("printer-info", *ch.Info))
	}
	if ch.Location != nil {
		attrs = append(attrs, ipp.Text("printer-location", *ch.Location))
	}
	if ch.DeviceURI != nil {
		attrs = append(attrs, ipp.URI("device-uri", *ch.DeviceURI))
	}
	if ch.Shared != nil {
		attrs = append(attrs, ipp.Bool("printer-is-shared", *ch.Shared))
	}
	if ch.ErrorPolicy != nil {
		attrs = append(attrs, ipp.Name("printer-error-policy", *ch.ErrorPolicy))
	}
	if ch.Sides != nil {
		attrs = append(attrs, ipp.Keyword("sides-default", *ch.Sides))
	}
	if ch.Color != nil {
		attrs = append(attrs, ipp.Keyword("print-color-mode-default", *ch.Color))
	}
	if ch.Media != nil {
		attrs = append(attrs, ipp.Keyword("media-default", *ch.Media))
	}
	if ch.Quality != nil {
		attrs = append(attrs, ipp.Enum("print-quality-default", *ch.Quality))
	}
	if len(attrs) == 0 {
		return nil
	}
	req, err := c.adminOp(ipp.OpCUPSAddModifyPrinter, name)
	if err != nil {
		return err
	}
	_, err = c.Do(ctx, "/admin/", req.Printer(attrs...), nil)
	return err
}

// Delete removes a queue. The printer itself is not touched.
func (c *Client) Delete(ctx context.Context, name string) error {
	return c.queueOp(ctx, ipp.OpCUPSDeletePrinter, name)
}

// SetDefault makes a queue the default one.
func (c *Client) SetDefault(ctx context.Context, name string) error {
	return c.queueOp(ctx, ipp.OpCUPSSetDefault, name)
}

// Accept lets a queue take new jobs (on) or turns them away (off).
func (c *Client) Accept(ctx context.Context, name string, on bool) error {
	if on {
		return c.queueOp(ctx, ipp.OpCUPSAcceptJobs, name)
	}
	return c.queueOp(ctx, ipp.OpCUPSRejectJobs, name)
}

// Pause stops a queue from printing (on) or lets it print again (off).
func (c *Client) Pause(ctx context.Context, name string, on bool) error {
	if on {
		return c.queueOp(ctx, ipp.OpPausePrinter, name)
	}
	return c.queueOp(ctx, ipp.OpResumePrinter, name)
}

// CancelAll cancels every job of a queue. CUPS keeps them in its history.
func (c *Client) CancelAll(ctx context.Context, name string) error {
	return c.queueOp(ctx, ipp.OpCancelJobs, name)
}

// MoveAll sends every job of one queue to another.
func (c *Client) MoveAll(ctx context.Context, from, to string) error {
	switch {
	case to == "":
		return errors.New("no queue to move the jobs to")
	case to == from:
		return errors.New("the jobs are on that queue already")
	}
	req, err := c.adminOp(ipp.OpCUPSMoveJob, from)
	if err != nil {
		return err
	}
	_, err = c.Do(ctx, "/jobs/", req.Job(ipp.URI("job-printer-uri", PrinterURI(to))), nil)
	return err
}

// Command sends a maintenance command, one of the printer's
// printer-commands, as a job of CUPS's command format and returns the job id.
func (c *Client) Command(ctx context.Context, printer, command string) (int, error) {
	if command == "" || strings.ContainsAny(command, "\r\n") {
		return 0, errors.New("that is not a printer command")
	}
	return c.Print(ctx, printer, "Printer command: "+command, commandFormat, JobOptions{},
		strings.NewReader("#CUPS-COMMAND\n"+command+"\n"))
}
