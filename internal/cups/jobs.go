// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nemesis398/skra/internal/ipp"
)

// JobOptions are the print options skra offers. A zero field is not sent, so
// the queue's own default applies. Hold asks CUPS to keep the job held until
// somebody releases it.
type JobOptions struct {
	Sides, Color, Media string
	Copies, Quality     int
	Hold                bool
}

// testPageFormat and testPageBanner are CUPS's own test page: a banner file
// that the filters fill in with the printer's details.
const (
	testPageFormat = "application/vnd.cups-pdf-banner"
	testPageBanner = "#PDF-BANNER\nTemplate default-testpage.pdf\nShow printer-name printer-info printer-location " +
		"printer-make-and-model printer-driver-name printer-driver-version paper-size imageable-area job-id options " +
		"time-at-creation time-at-processing\n\n"
)

// maxJobName is the longest IPP name value CUPS accepts, in bytes.
const maxJobName = 255

func jobURI(id int) string { return "ipp://localhost/jobs/" + strconv.Itoa(id) }

// Hold keeps a waiting job from printing until it is released.
func (c *Client) Hold(ctx context.Context, id int) error {
	return c.jobOp(ctx, ipp.OpHoldJob, id, ipp.Keyword("job-hold-until", "indefinite"))
}

// Release lets a held job print.
func (c *Client) Release(ctx context.Context, id int) error {
	return c.jobOp(ctx, ipp.OpReleaseJob, id)
}

// Cancel cancels a job. CUPS keeps it in its history as canceled.
func (c *Client) Cancel(ctx context.Context, id int) error {
	return c.jobOp(ctx, ipp.OpCancelJob, id)
}

// Restart prints a finished job again from the files CUPS kept.
func (c *Client) Restart(ctx context.Context, id int) error {
	return c.jobOp(ctx, ipp.OpRestartJob, id)
}

// jobOp sends a job operation whose target is the job alone; extra goes into
// the operation group.
func (c *Client) jobOp(ctx context.Context, op uint16, id int, extra ...ipp.Attr) error {
	_, err := c.Do(ctx, "/jobs/", ipp.NewRequest(op).Op(ipp.URI("job-uri", jobURI(id))).Op(extra...), nil)
	return err
}

// Move sends a job to another queue.
func (c *Client) Move(ctx context.Context, id int, to string) error {
	if to == "" {
		return errors.New("no queue to move the job to")
	}
	_, err := c.Do(ctx, "/jobs/", ipp.NewRequest(ipp.OpCUPSMoveJob).
		Op(ipp.URI("job-uri", jobURI(id))).
		Job(ipp.URI("job-printer-uri", PrinterURI(to))), nil)
	return err
}

// Print sends doc to the queue as a new job and returns its id. format is the
// document's MIME type. The document is streamed, never held whole. When doc
// fails midway the job is not created.
func (c *Client) Print(ctx context.Context, printer, name, format string, o JobOptions, doc io.Reader) (int, error) {
	if printer == "" {
		return 0, errors.New("no printer to print on")
	}
	req := ipp.NewRequest(ipp.OpPrintJob).Op(
		ipp.URI("printer-uri", PrinterURI(printer)),
		ipp.Name("job-name", clipName(name)),
		ipp.Mime("document-format", format))
	if o.Copies != 0 {
		req.Job(ipp.Int("copies", o.Copies))
	}
	if o.Sides != "" {
		req.Job(ipp.Keyword("sides", o.Sides))
	}
	if o.Color != "" {
		req.Job(ipp.Keyword("print-color-mode", o.Color))
	}
	if o.Media != "" {
		req.Job(ipp.Keyword("media", o.Media))
	}
	if o.Quality != 0 {
		req.Job(ipp.Enum("print-quality", o.Quality))
	}
	if o.Hold {
		req.Job(ipp.Keyword("job-hold-until", "indefinite"))
	}
	res, err := c.Do(ctx, "/printers/"+url.PathEscape(printer), req, doc)
	if err != nil {
		return 0, err
	}
	for _, g := range res.Of(ipp.GroupJob) {
		if id, ok := g.Int("job-id"); ok {
			return id, nil
		}
	}
	return 0, fmt.Errorf("CUPS accepted the job but sent no job id")
}

// PrintTestPage prints CUPS's own test page on the queue.
func (c *Client) PrintTestPage(ctx context.Context, printer string, hold bool) (int, error) {
	return c.Print(ctx, printer, "Test Page", testPageFormat, JobOptions{Hold: hold}, strings.NewReader(testPageBanner))
}

// clipName cuts name to what an IPP name value can hold, on a rune boundary.
func clipName(name string) string {
	if len(name) <= maxJobName {
		return name
	}
	name = name[:maxJobName]
	for !utf8.ValidString(name) {
		name = name[:len(name)-1]
	}
	return name
}
