// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nemesis398/skra/internal/ipp"
)

// printerAttrs is what the overview shows; adminAttrs adds what the queue
// settings need; printer-commands lists the maintenance commands.
var printerAttrs = []string{"printer-name", "printer-info", "printer-location", "printer-make-and-model", "printer-state",
	"printer-state-reasons", "printer-state-message", "printer-is-accepting-jobs", "device-uri", "queued-job-count",
	"marker-names", "marker-levels", "marker-low-levels", "marker-colors", "marker-types", "marker-change-time",
	"sides-supported", "sides-default", "print-color-mode-supported", "print-color-mode-default"}

var adminAttrs = []string{"printer-is-shared", "printer-error-policy", "printer-error-policy-supported",
	"media-supported", "media-default", "print-quality-supported", "print-quality-default"}

func requestedPrinterAttrs() []string {
	return slices.Concat(printerAttrs, adminAttrs, []string{"printer-commands"})
}

var jobAttrs = []string{"job-id", "job-name", "job-state", "job-state-reasons", "job-printer-uri", "job-printer-state-message",
	"job-media-sheets-completed", "time-at-creation", "time-at-completed", "job-originating-user-name"}

// Printers lists every queue, sorted by name.
func (c *Client) Printers(ctx context.Context) ([]Printer, error) {
	res, err := c.Do(ctx, "/", ipp.NewRequest(ipp.OpCUPSGetPrinters).
		Op(ipp.Keyword("requested-attributes", requestedPrinterAttrs()...)), nil)
	if err != nil {
		return nil, err
	}
	var out []Printer
	for _, g := range res.Of(ipp.GroupPrinter) {
		out = append(out, printerFrom(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Printer returns one queue.
func (c *Client) Printer(ctx context.Context, name string) (Printer, error) {
	res, err := c.Do(ctx, "/printers/"+url.PathEscape(name), ipp.NewRequest(ipp.OpGetPrinterAttributes).
		Op(ipp.URI("printer-uri", PrinterURI(name)), ipp.Keyword("requested-attributes", requestedPrinterAttrs()...)), nil)
	if err != nil {
		return Printer{}, err
	}
	gs := res.Of(ipp.GroupPrinter)
	if len(gs) == 0 {
		return Printer{}, fmt.Errorf("CUPS sent no attributes for %q", name)
	}
	return printerFrom(gs[0]), nil
}

// Default returns the name of the default queue, or "" when none is set.
func (c *Client) Default(ctx context.Context) (string, error) {
	res, err := c.Do(ctx, "/", ipp.NewRequest(ipp.OpCUPSGetDefault).
		Op(ipp.Keyword("requested-attributes", "printer-name")), nil)
	var se *ipp.StatusError
	if errors.As(err, &se) && se.Status == ipp.StatusNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, g := range res.Of(ipp.GroupPrinter) {
		if n := g.String("printer-name"); n != "" {
			return n, nil
		}
	}
	return "", nil
}

// Jobs lists jobs of every queue. which is "not-completed" (sorted by id) or
// "completed" (newest first); limit 0 asks for no limit.
func (c *Client) Jobs(ctx context.Context, which string, limit int) ([]Job, error) {
	if which != "not-completed" && which != "completed" {
		return nil, fmt.Errorf("unknown job selection %q", which)
	}
	req := ipp.NewRequest(ipp.OpGetJobs).Op(ipp.URI("printer-uri", "ipp://localhost/"), ipp.Keyword("which-jobs", which))
	if limit > 0 {
		req.Op(ipp.Int("limit", limit))
	}
	res, err := c.Do(ctx, "/", req.Op(ipp.Keyword("requested-attributes", jobAttrs...)), nil)
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, g := range res.Of(ipp.GroupJob) {
		out = append(out, jobFrom(g))
	}
	if which == "completed" {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Completed.After(out[j].Completed) })
	} else {
		sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	}
	return out, nil
}

func printerFrom(g ipp.AttrGroup) Printer {
	p := Printer{
		Name:             g.String("printer-name"),
		Info:             g.String("printer-info"),
		Location:         g.String("printer-location"),
		MakeModel:        g.String("printer-make-and-model"),
		DeviceURI:        g.String("device-uri"),
		Message:          g.String("printer-state-message"),
		ErrorPolicy:      g.String("printer-error-policy"),
		SidesDefault:     g.String("sides-default"),
		ColorDefault:     g.String("print-color-mode-default"),
		MediaDefault:     g.String("media-default"),
		Reasons:          g.Strings("printer-state-reasons"),
		SidesSupported:   g.Strings("sides-supported"),
		ColorSupported:   g.Strings("print-color-mode-supported"),
		MediaSupported:   g.Strings("media-supported"),
		ErrorPolicies:    g.Strings("printer-error-policy-supported"),
		Commands:         slices.DeleteFunc(g.Strings("printer-commands"), func(c string) bool { return c == "none" }),
		QualitySupported: g.Ints("print-quality-supported"),
		Markers:          markers(g),
		MarkersAt:        unix(g, "marker-change-time"),
	}
	p.State, _ = g.Int("printer-state")
	p.QualityDefault, _ = g.Int("print-quality-default")
	p.QueuedJobs, _ = g.Int("queued-job-count")
	p.Accepting, _ = g.Bool("printer-is-accepting-jobs")
	p.Shared, _ = g.Bool("printer-is-shared")
	return p
}

// markers zips the five marker-* lists by index. They can differ in length (a
// Brother MFC sends levels and types but no names), so it runs to the longest
// and fills gaps with "" and -1.
func markers(g ipp.AttrGroup) []Marker {
	names, colors, types := g.Strings("marker-names"), g.Strings("marker-colors"), g.Strings("marker-types")
	levels, lows := g.Ints("marker-levels"), g.Ints("marker-low-levels")
	n := max(len(names), len(colors), len(types), len(levels), len(lows))
	if n == 0 {
		return nil
	}
	at := func(s []string, i int) string {
		if i < len(s) {
			return s[i]
		}
		return ""
	}
	num := func(s []int, i int) int {
		if i < len(s) {
			return s[i]
		}
		return -1
	}
	out := make([]Marker, n)
	for i := range out {
		out[i] = Marker{Name: at(names, i), Color: at(colors, i), Type: at(types, i), Level: num(levels, i), Low: num(lows, i)}
	}
	return out
}

func jobFrom(g ipp.AttrGroup) Job {
	j := Job{
		Name:      g.String("job-name"),
		User:      g.String("job-originating-user-name"),
		Printer:   printerFromURI(g.String("job-printer-uri")),
		Message:   g.String("job-printer-state-message"),
		Reasons:   g.Strings("job-state-reasons"),
		Created:   unix(g, "time-at-creation"),
		Completed: unix(g, "time-at-completed"),
	}
	j.ID, _ = g.Int("job-id")
	j.State, _ = g.Int("job-state")
	j.Sheets, _ = g.Int("job-media-sheets-completed")
	return j
}

// printerFromURI returns the queue name, the last path segment of a
// job-printer-uri such as ipp://localhost:631/printers/NAME.
func printerFromURI(uri string) string {
	i := strings.LastIndex(uri, "/")
	if i < 0 {
		return uri
	}
	seg := uri[i+1:]
	if name, err := url.PathUnescape(seg); err == nil {
		return name
	}
	return seg
}

// unix maps an integer attribute of Unix seconds to a time; 0 or missing is
// the zero time.
func unix(g ipp.AttrGroup, name string) time.Time {
	if n, ok := g.Int(name); ok && n > 0 {
		return time.Unix(int64(n), 0)
	}
	return time.Time{}
}

// MarkersFrom reads the marker-* attributes of a printer group, for callers
// that ask a printer directly instead of CUPS.
func MarkersFrom(g ipp.AttrGroup) []Marker { return markers(g) }
