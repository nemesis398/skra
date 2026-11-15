// SPDX-License-Identifier: AGPL-3.0-or-later

// Package escl talks to scanners over eSCL (AirScan): it reads what a scanner
// can do and how it is, starts a scan job, fetches its pages one by one, and
// cancels it. The pages always come as JPEG.
package escl

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/nemesis398/skra/internal/printer"
)

// What a scan can go wrong with, in words the person at the scanner can act
// on. The web layer shows this text as it is.
var (
	ErrFeederEmpty = errors.New("The feeder is empty. Put the pages in and scan more.")
	ErrJam         = errors.New("The paper jammed in the feeder. Clear it and scan the rest.")
	ErrBusy        = errors.New("The scanner is busy. Try again in a moment.")
	// ErrAway is a scanner that could not be dialed, did not answer in time,
	// or hung up without an answer.
	ErrAway = errors.New("The scanner does not answer. It may be off or on another network.")
)

// How long one request may take. A feeder page at 600 dpi is slow to come.
const (
	shortTimeout = 5 * time.Second
	pageTimeout  = 60 * time.Second
)

// maxDocument caps the capabilities and status documents, which are a few
// kilobytes.
const maxDocument = 1 << 20

// newTransport builds the transport of a client. It is a variable only so a
// test can watch the dials, which no test may let reach port 631.
var newTransport = printer.GuardedTransport

// Client talks to one scanner. Make one with New.
type Client struct {
	base *url.URL // scheme, host and the path of the eSCL root, without a trailing slash
	http *http.Client
}

// New returns a client for the scanner whose eSCL root is base, such as
// http://192.0.2.7:80/eSCL. Only http and https are taken, with a host and
// without a user name or password. Scanners on the LAN carry self-signed
// certificates, so https does not verify them.
func New(base string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("escl: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("escl: %q is not an http or https address", base)
	}
	if u.User != nil {
		return nil, fmt.Errorf("escl: %q carries a user name", base)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("escl: no host in %q", base)
	}
	// The transport maps a host through IDNA, which turns some characters
	// outside ASCII into nothing and so a name into an empty host.
	if strings.ContainsFunc(host, func(r rune) bool { return r > unicode.MaxASCII }) {
		return nil, fmt.Errorf("escl: the host in %q is not ASCII", base)
	}
	return &Client{
		base: &url.URL{Scheme: u.Scheme, Host: u.Host, Path: strings.TrimRight(u.Path, "/")},
		http: &http.Client{
			Transport: newTransport(u.Scheme == "https"),
			// A scanner on the LAN could otherwise send the client to any
			// host, this machine's CUPS included.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// at is the URL of path below the eSCL root, or of an absolute path when abs
// is set. The host is always the scanner's.
func (c *Client) at(path string, abs bool) string {
	if !abs {
		path = c.base.Path + "/" + path
	}
	u := url.URL{Scheme: c.base.Scheme, Host: c.base.Host, Path: path}
	return u.String()
}

// do sends req and maps a failure to get any answer to ErrAway. When ctx, the
// caller's context, is done, the caller gave up and the error says so instead.
func (c *Client) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("escl: %w", err)
		}
		return nil, fmt.Errorf("%w (%w)", ErrAway, err)
	}
	return resp, nil
}

// get fetches path below the eSCL root and decodes the XML answer into v.
func (c *Client) get(ctx context.Context, path string, v any) error {
	rctx, cancel := context.WithTimeout(ctx, shortTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, c.at(path, false), nil)
	if err != nil {
		return fmt.Errorf("escl: %w", err)
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("escl: %s answered HTTP %d", path, resp.StatusCode)
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(v); err != nil {
		if ctx.Err() == nil && rctx.Err() != nil {
			return fmt.Errorf("%w (%w)", ErrAway, err)
		}
		return fmt.Errorf("escl: reading %s: %w", path, err)
	}
	return nil
}

// Caps is what a scanner says it can do. Colors come richest first, as
// RGB24, Grayscale8, BlackAndWhite1, then any others in the order the scanner
// lists them. Resolutions are in dots per inch, ascending; MaxWidth and
// MaxHeight are in three-hundredths of an inch, the largest over all sources.
type Caps struct {
	Model       string
	Sources     []Source
	Colors      []string
	Resolutions []int
	Formats     []string
	MaxWidth    int
	MaxHeight   int
}

// Source is where a scan can take its paper from: "Platen" (the glass) or
// "Feeder". Duplex says the feeder can scan both sides.
type Source struct {
	Name   string
	Duplex bool
}

// The parts of ScannerCapabilities the client reads. encoding/xml matches the
// local names, so the namespace prefixes do not matter.
type capsDoc struct {
	Model  string `xml:"MakeAndModel"`
	Platen *struct {
		Caps *inputCaps `xml:"PlatenInputCaps"`
	} `xml:"Platen"`
	Adf *struct {
		Simplex *inputCaps `xml:"AdfSimplexInputCaps"`
		Duplex  *inputCaps `xml:"AdfDuplexInputCaps"`
		Options []string   `xml:"AdfOptions>AdfOption"`
	} `xml:"Adf"`
}

type inputCaps struct {
	MaxWidth  int `xml:"MaxWidth"`
	MaxHeight int `xml:"MaxHeight"`
	Profiles  []struct {
		Colors      []string `xml:"ColorModes>ColorMode"`
		Formats     []string `xml:"DocumentFormats>DocumentFormat"`
		FormatsExt  []string `xml:"DocumentFormats>DocumentFormatExt"`
		Resolutions []struct {
			X int `xml:"XResolution"`
			Y int `xml:"YResolution"`
		} `xml:"SupportedResolutions>DiscreteResolutions>DiscreteResolution"`
	} `xml:"SettingProfiles>SettingProfile"`
}

// colorRank orders the color modes skrá offers, richest first.
var colorRank = map[string]int{"RGB24": 0, "Grayscale8": 1, "BlackAndWhite1": 2}

// Capabilities asks the scanner what it can do.
func (c *Client) Capabilities(ctx context.Context) (Caps, error) {
	var doc capsDoc
	if err := c.get(ctx, "ScannerCapabilities", &doc); err != nil {
		return Caps{}, err
	}
	caps := Caps{Model: strings.TrimSpace(doc.Model)}
	var all []*inputCaps
	if doc.Platen != nil && doc.Platen.Caps != nil {
		caps.Sources = append(caps.Sources, Source{Name: "Platen"})
		all = append(all, doc.Platen.Caps)
	}
	if a := doc.Adf; a != nil && (a.Simplex != nil || a.Duplex != nil) {
		duplex := a.Duplex != nil || slices.ContainsFunc(a.Options, func(o string) bool { return strings.TrimSpace(o) == "Duplex" })
		caps.Sources = append(caps.Sources, Source{Name: "Feeder", Duplex: duplex})
		for _, ic := range []*inputCaps{a.Simplex, a.Duplex} {
			if ic != nil {
				all = append(all, ic)
			}
		}
	}
	addUnique := func(list []string, v string) []string {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(list, v) {
			list = append(list, v)
		}
		return list
	}
	for _, ic := range all {
		caps.MaxWidth = max(caps.MaxWidth, ic.MaxWidth)
		caps.MaxHeight = max(caps.MaxHeight, ic.MaxHeight)
		for _, p := range ic.Profiles {
			for _, m := range p.Colors {
				caps.Colors = addUnique(caps.Colors, m)
			}
			for _, f := range slices.Concat(p.Formats, p.FormatsExt) {
				caps.Formats = addUnique(caps.Formats, f)
			}
			for _, r := range p.Resolutions {
				if r.X > 0 && r.X == r.Y && !slices.Contains(caps.Resolutions, r.X) {
					caps.Resolutions = append(caps.Resolutions, r.X)
				}
			}
		}
	}
	slices.SortStableFunc(caps.Colors, func(a, b string) int { return rank(a) - rank(b) })
	slices.Sort(caps.Resolutions)
	return caps, nil
}

func rank(mode string) int {
	if r, ok := colorRank[mode]; ok {
		return r
	}
	return len(colorRank)
}

// Status is how the scanner is: State is the pwg:State it reports ("Idle",
// "Processing", …) and Adf the feeder's scan:AdfState ("ScannerAdfLoaded",
// "ScannerAdfEmpty", "ScannerAdfJam", …), empty when it reports none.
type Status struct {
	State string
	Adf   string
}

type statusDoc struct {
	State string `xml:"State"`
	Adf   string `xml:"AdfState"`
}

// Status asks the scanner how it is.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var doc statusDoc
	if err := c.get(ctx, "ScannerStatus", &doc); err != nil {
		return Status{}, err
	}
	return Status{State: strings.TrimSpace(doc.State), Adf: strings.TrimSpace(doc.Adf)}, nil
}

// Settings is a scan to start. Source is "Platen" or "Feeder"; Duplex counts
// only for the feeder. Color is an eSCL color mode such as "Grayscale8",
// Resolution is in dots per inch, and Width and Height are the region to scan
// from the top left corner, in three-hundredths of an inch.
type Settings struct {
	Source     string
	Duplex     bool
	Color      string
	Resolution int
	Width      int
	Height     int
}

// scanSettings is the ScanSettings document for s, asking for JPEG.
func scanSettings(s Settings) string {
	var b strings.Builder
	esc := func(v string) string {
		var e strings.Builder
		xml.EscapeText(&e, []byte(v)) //nolint:errcheck // a strings.Builder does not fail
		return e.String()
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<scan:ScanSettings xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm">
<pwg:Version>2.0</pwg:Version>
<pwg:ScanRegions>
<pwg:ScanRegion>
<pwg:ContentRegionUnits>escl:ThreeHundredthsOfInches</pwg:ContentRegionUnits>
<pwg:XOffset>0</pwg:XOffset>
<pwg:YOffset>0</pwg:YOffset>
`)
	fmt.Fprintf(&b, "<pwg:Width>%d</pwg:Width>\n<pwg:Height>%d</pwg:Height>\n</pwg:ScanRegion>\n</pwg:ScanRegions>\n", s.Width, s.Height)
	fmt.Fprintf(&b, "<pwg:InputSource>%s</pwg:InputSource>\n", esc(s.Source))
	if s.Source == "Feeder" {
		fmt.Fprintf(&b, "<scan:Duplex>%t</scan:Duplex>\n", s.Duplex)
	}
	fmt.Fprintf(&b, "<scan:ColorMode>%s</scan:ColorMode>\n", esc(s.Color))
	fmt.Fprintf(&b, "<scan:XResolution>%d</scan:XResolution>\n<scan:YResolution>%d</scan:YResolution>\n", s.Resolution, s.Resolution)
	b.WriteString("<pwg:DocumentFormat>image/jpeg</pwg:DocumentFormat>\n<scan:DocumentFormatExt>image/jpeg</scan:DocumentFormatExt>\n</scan:ScanSettings>\n")
	return b.String()
}

// Start starts a scan and returns its job: the path of the Location the
// scanner answered with, such as /eSCL/ScanJobs/1. Only the path counts: a
// scanner may name itself by another name or address than base, and the job
// is always fetched from base's scheme and host. A path outside base's
// ScanJobs is refused. A scanner that turns the scan down because of the
// feeder or another job gives ErrFeederEmpty, ErrJam or ErrBusy.
func (c *Client) Start(ctx context.Context, s Settings) (job string, err error) {
	if s.Source != "Platen" && s.Source != "Feeder" {
		return "", fmt.Errorf("escl: unknown source %q", s.Source)
	}
	rctx, cancel := context.WithTimeout(ctx, shortTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, c.at("ScanJobs", false), strings.NewReader(scanSettings(s)))
	if err != nil {
		return "", fmt.Errorf("escl: %w", err)
	}
	req.Header.Set("Content-Type", "text/xml")
	resp, err := c.do(ctx, req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	switch code := resp.StatusCode; {
	case code == http.StatusConflict || code == http.StatusServiceUnavailable:
		return "", c.turnedDown(ctx, code)
	case code != http.StatusCreated && code != http.StatusOK:
		return "", fmt.Errorf("escl: ScanJobs answered HTTP %d", code)
	}
	loc, err := resp.Location() // resolved against the request's URL
	if err != nil {
		return "", fmt.Errorf("escl: the scan job has no address: %w", err)
	}
	job = strings.TrimRight(loc.Path, "/")
	if err := c.checkJob(job); err != nil {
		return "", err
	}
	return job, nil
}

// turnedDown reads the scanner's status once to say why it answered a scan
// with code (409 or 503).
func (c *Client) turnedDown(ctx context.Context, code int) error {
	st, err := c.Status(ctx)
	if err != nil {
		return fmt.Errorf("escl: the scanner answered HTTP %d and then no status: %w", code, err)
	}
	switch {
	case st.Adf == "ScannerAdfJam":
		return ErrJam
	case code == http.StatusServiceUnavailable || st.State == "Processing":
		return ErrBusy
	case st.Adf == "ScannerAdfEmpty":
		return ErrFeederEmpty
	}
	return fmt.Errorf("escl: the scanner answered HTTP %d (state %s)", code, st.State)
}

// checkJob refuses a job that is not a clean path below base's ScanJobs, so
// a job can only ever lead to a scan job on the scanner's own host.
func (c *Client) checkJob(job string) error {
	jobs := c.base.Path + "/ScanJobs/"
	if !strings.HasPrefix(job, jobs) || len(job) == len(jobs) || path.Clean(job) != job {
		return fmt.Errorf("escl: %q is not a scan job below %s", job, jobs)
	}
	return nil
}

// NextPage fetches the next page of job as JPEG. The caller reads it and
// closes it; the whole page, reading included, has 60 seconds. When the job has
// no more pages it returns io.EOF.
func (c *Client) NextPage(ctx context.Context, job string) (io.ReadCloser, error) {
	if err := c.checkJob(job); err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, pageTimeout)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, c.at(job+"/NextDocument", true), nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("escl: %w", err)
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return &pageBody{ReadCloser: resp.Body, cancel: cancel}, nil
	}
	resp.Body.Close()
	cancel()
	switch code := resp.StatusCode; code {
	case http.StatusNotFound:
		return nil, io.EOF
	case http.StatusConflict, http.StatusServiceUnavailable:
		return nil, c.turnedDown(ctx, code)
	default:
		return nil, fmt.Errorf("escl: NextDocument answered HTTP %d", code)
	}
}

// pageBody ends the page's timeout when the caller closes it.
type pageBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (p *pageBody) Close() error {
	err := p.ReadCloser.Close()
	p.cancel()
	return err
}

// Cancel ends job on the scanner. A job the scanner no longer knows counts as
// ended.
func (c *Client) Cancel(ctx context.Context, job string) error {
	if err := c.checkJob(job); err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, shortTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodDelete, c.at(job, true), nil)
	if err != nil {
		return fmt.Errorf("escl: %w", err)
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("escl: cancelling %s answered HTTP %d", job, resp.StatusCode)
	}
	return nil
}
