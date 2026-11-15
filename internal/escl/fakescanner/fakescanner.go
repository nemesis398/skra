// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fakescanner is an eSCL scanner for tests and for `make dev`. It
// answers like the Brother MFC-J6975DW does, but every page it "scans" is a
// white sheet with one dark band, and the position of the band says which
// page it is. It has no hardware behind it, so a test can set the feeder to
// empty or jammed, or make the scanner busy, whenever it likes.
//
// It always answers a scan with JPEG, whatever format the job asks for.
package fakescanner

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

//go:embed capabilities.xml
var capabilities []byte

// The feeder states SetState takes.
const (
	adfLoaded = "loaded"
	adfEmpty  = "empty"
	adfJam    = "jam"
)

// An A4 sheet in three-hundredths of an inch, which is what a scan region
// without a size gets.
const (
	a4Width  = 2480
	a4Height = 3508
)

// Options sets up a Scanner.
type Options struct {
	// AdfPages is how many sheets the feeder holds. Zero means 3. The feeder
	// is full again for every job.
	AdfPages int
}

// Settings is what the scanner read from the last ScanSettings body it got.
// Width, Height and ContentRegionUnits belong to the first scan region.
type Settings struct {
	InputSource        string
	Duplex             bool
	ColorMode          string
	XResolution        int
	YResolution        int
	DocumentFormatExt  string
	Width              int
	Height             int
	ContentRegionUnits string
}

// scanSettings is the part of an eSCL ScanSettings document the fake reads.
// encoding/xml matches the local names and skips the rest, so the namespace
// prefixes and unknown elements do not matter.
type scanSettings struct {
	InputSource       string `xml:"InputSource"`
	Duplex            bool   `xml:"Duplex"`
	ColorMode         string `xml:"ColorMode"`
	XResolution       int    `xml:"XResolution"`
	YResolution       int    `xml:"YResolution"`
	DocumentFormatExt string `xml:"DocumentFormatExt"`
	Region            struct {
		Width              int    `xml:"Width"`
		Height             int    `xml:"Height"`
		ContentRegionUnits string `xml:"ContentRegionUnits"`
	} `xml:"ScanRegions>ScanRegion"`
}

type job struct {
	settings Settings
	// next is the page NextDocument hands out; total is how many there are.
	next, total int
}

// Scanner is the fake. Make one with New and serve it with ServeHTTP.
type Scanner struct {
	mux      *http.ServeMux
	adfPages int

	mu     sync.Mutex
	adf    string
	busy   bool
	jobs   map[string]*job
	nextID int
	last   Settings
}

// New returns a scanner with a loaded feeder that is not busy.
func New(o Options) *Scanner {
	if o.AdfPages <= 0 {
		o.AdfPages = 3
	}
	s := &Scanner{
		mux:      http.NewServeMux(),
		adfPages: o.AdfPages,
		adf:      adfLoaded,
		jobs:     map[string]*job{},
	}
	s.mux.HandleFunc("GET /eSCL/ScannerCapabilities", s.capabilities)
	s.mux.HandleFunc("GET /eSCL/ScannerStatus", s.status)
	s.mux.HandleFunc("POST /eSCL/ScanJobs", s.createJob)
	s.mux.HandleFunc("GET /eSCL/ScanJobs/{id}/NextDocument", s.nextDocument)
	s.mux.HandleFunc("DELETE /eSCL/ScanJobs/{id}", s.deleteJob)
	s.mux.HandleFunc("POST /_fake/state", s.setState)
	return s
}

// ServeHTTP makes Scanner an http.Handler.
func (s *Scanner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// SetState sets the feeder to "loaded", "empty" or "jam", and says whether
// the scanner is busy with someone else's job. Any other feeder state panics,
// because it can only be a mistake in the test.
func (s *Scanner) SetState(adf string, busy bool) {
	if !validAdf(adf) {
		panic(fmt.Sprintf("fakescanner: unknown feeder state %q", adf))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adf, s.busy = adf, busy
}

func validAdf(adf string) bool {
	return adf == adfLoaded || adf == adfEmpty || adf == adfJam
}

// LastSettings returns the most recent ScanSettings that was POSTed, even if
// the scanner turned the job down. It is the zero value before the first one.
func (s *Scanner) LastSettings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Scanner) capabilities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/xml")
	w.Write(capabilities)
}

const statusHead = `<?xml version="1.0" encoding="UTF-8"?>
<scan:ScannerStatus xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm">
<pwg:Version>2.93</pwg:Version>
`

func (s *Scanner) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	adf, busy := s.adf, s.busy
	s.mu.Unlock()
	state := "Idle"
	if busy {
		state = "Processing"
	}
	adfState := map[string]string{
		adfLoaded: "ScannerAdfLoaded",
		adfEmpty:  "ScannerAdfEmpty",
		adfJam:    "ScannerAdfJam",
	}[adf]
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprintf(w, "%s<pwg:State>%s</pwg:State>\n<scan:AdfState>%s</scan:AdfState>\n</scan:ScannerStatus>\n",
		statusHead, state, adfState)
}

func (s *Scanner) createJob(w http.ResponseWriter, r *http.Request) {
	var in scanSettings
	if err := xml.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, "ScanSettings: "+err.Error(), http.StatusBadRequest)
		return
	}
	set := Settings{
		InputSource:        strings.TrimSpace(in.InputSource),
		Duplex:             in.Duplex,
		ColorMode:          strings.TrimSpace(in.ColorMode),
		XResolution:        in.XResolution,
		YResolution:        in.YResolution,
		DocumentFormatExt:  strings.TrimSpace(in.DocumentFormatExt),
		Width:              in.Region.Width,
		Height:             in.Region.Height,
		ContentRegionUnits: strings.TrimSpace(in.Region.ContentRegionUnits),
	}
	if set.InputSource == "" {
		set.InputSource = "Platen"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = set
	if s.busy {
		http.Error(w, "scanner busy", http.StatusServiceUnavailable)
		return
	}
	pages := 1
	if set.InputSource == "Feeder" {
		if s.adf == adfEmpty {
			http.Error(w, "feeder empty", http.StatusConflict)
			return
		}
		pages = s.adfPages
		if set.Duplex {
			pages *= 2
		}
	}
	s.nextID++
	id := strconv.Itoa(s.nextID)
	s.jobs[id] = &job{settings: set, total: pages}
	w.Header().Set("Location", "/eSCL/ScanJobs/"+id)
	w.WriteHeader(http.StatusCreated)
}

func (s *Scanner) nextDocument(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	if j == nil || j.next >= j.total {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if j.settings.InputSource == "Feeder" && s.adf == adfJam {
		s.mu.Unlock()
		http.Error(w, "feeder jammed", http.StatusConflict)
		return
	}
	n, set := j.next, j.settings
	j.next++
	s.mu.Unlock()

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, drawPage(set, n), nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(buf.Bytes())
}

func (s *Scanner) deleteJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	if s.jobs[id] == nil {
		http.NotFound(w, r)
		return
	}
	delete(s.jobs, id)
}

// setState is the control endpoint. A field that is not in the form leaves
// that part of the state as it was.
func (s *Scanner) setState(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	adf, busy := s.adf, s.busy
	if v := r.PostForm.Get("adf"); v != "" {
		if !validAdf(v) {
			http.Error(w, "adf must be loaded, empty or jam", http.StatusBadRequest)
			return
		}
		adf = v
	}
	if v := r.PostForm.Get("busy"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			http.Error(w, "busy must be true or false", http.StatusBadRequest)
			return
		}
		busy = b
	}
	s.adf, s.busy = adf, busy
	w.WriteHeader(http.StatusNoContent)
}

// pixels turns a length in three-hundredths of an inch into pixels at the
// given resolution. A length or resolution that is missing gets the A4 or
// 300 dpi default.
func pixels(length, dflt, dpi int) int {
	if length <= 0 {
		length = dflt
	}
	if dpi <= 0 {
		dpi = 300
	}
	return (length*dpi + 150) / 300
}

// drawPage draws page n of a job: white, with a dark band across it. Page n
// has its band at n+1 twelfths of the height (the cycle restarts after ten
// pages), so the first dark row tells the pages apart.
func drawPage(set Settings, n int) image.Image {
	w := pixels(set.Width, a4Width, set.XResolution)
	h := pixels(set.Height, a4Height, set.YResolution)
	top := (n%10 + 1) * h / 12
	bottom := min(top+h/24+1, h)

	if set.ColorMode == "RGB24" {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for i := range img.Pix {
			img.Pix[i] = 0xff
		}
		for y := top; y < bottom; y++ {
			row := img.Pix[y*img.Stride : y*img.Stride+4*w]
			for x := 0; x < len(row); x += 4 {
				row[x], row[x+1], row[x+2] = 0x10, 0x20, 0x60
			}
		}
		return img
	}
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	for y := top; y < bottom; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w]
		for x := range row {
			row[x] = 0x20
		}
	}
	return img
}
