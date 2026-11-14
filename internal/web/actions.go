// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/store"
)

// maxUpload is the largest file that can be printed from the page. A variable
// so a test against the harness can lower it.
var maxUpload int64 = 200 << 20

// formOverhead is what the multipart framing and the small form fields may add
// to a body of maxUpload bytes.
const formOverhead = 64 << 10

const (
	// actionTimeout bounds a job action; printTimeout bounds an upload, which
	// runs at the speed of the browser's disk and CUPS's spooler.
	actionTimeout = 30 * time.Second
	printTimeout  = 10 * time.Minute

	// sniffLen is how much of a file http.DetectContentType looks at.
	sniffLen = 512
	// maxField caps a small form field.
	maxField = 1 << 10
)

const (
	tooLargeText   = "Files up to 200 MB can be printed from here."
	badFormatText  = "Only PDF, JPEG, PNG and plain text can be printed from here."
	emptyFileText  = "The file is empty."
	notUploadText  = "That was not a file upload. Reload the page and try again."
	fieldOrderText = "The form came in the wrong order. Reload the page and try again."
)

// errTooLarge is what the counting reader returns once a file passes maxUpload.
var errTooLarge = errors.New("file too large")

// A job action is the CUPS call, the event it logs and the verb of the
// footer message.
var jobActions = map[string]struct {
	event, verb string
	do          func(CUPS, context.Context, int) error
}{
	"hold":    {"job.hold", "Held", CUPS.Hold},
	"release": {"job.release", "Released", CUPS.Release},
	"cancel":  {"job.cancel", "Canceled", CUPS.Cancel},
	"restart": {"job.restart", "Reprinting", CUPS.Restart},
}

func (s *Server) actionRoutes(mux *http.ServeMux) {
	for name := range jobActions {
		mux.HandleFunc("POST /jobs/{id}/"+name, s.jobAction(name))
	}
	mux.HandleFunc("POST /jobs/{id}/move", s.moveJob)
	mux.HandleFunc("POST /print", s.printFile)
	mux.HandleFunc("POST /printers/{name}/testpage", s.testPage)
}

// reply answers with a short text that skra.js shows in the footer.
func reply(w http.ResponseWriter, status int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	io.WriteString(w, msg)
}

// record logs an action under the configured user. A failure to log is
// reported on stderr and never turned into a failed action.
func (s *Server) record(r *http.Request, action, target, detail string, err error) {
	if s.deps.Store == nil {
		return
	}
	if err != nil {
		detail = err.Error()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	e := store.Event{Actor: s.cfg.User, Action: action, Target: target, Detail: detail, OK: err == nil}
	if lerr := s.deps.Store.Log(ctx, e); lerr != nil {
		log.Printf("skra: %v", lerr)
	}
}

// known is the last good data of a poll, for names. It does not wait for a
// poll that is running.
func (s *Server) known() Snapshot {
	s.goodMu.RLock()
	defer s.goodMu.RUnlock()
	return s.good
}

// jobName is the name the last poll saw for the job, or "job N".
func (s *Server) jobName(id int) string {
	snap := s.known()
	for _, list := range [][]cups.Job{snap.Active, snap.Done} {
		for _, j := range list {
			if j.ID == id && j.Name != "" {
				return j.Name
			}
		}
	}
	return fmt.Sprintf("job %d", id)
}

// shortPrinter is the short name the overview uses for a queue, or the queue
// name itself when the last poll did not see it.
func (s *Server) shortPrinter(name string) string {
	if short, ok := shortNames(s.known().Printers)[name]; ok {
		return short
	}
	return name
}

func quoted(name string) string { return `"` + name + `"` }

// jobID reads the {id} of the route. It answers 400 itself when there is none.
func jobID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		reply(w, http.StatusBadRequest, "That is not a job number.")
		return 0, false
	}
	return id, true
}

func (s *Server) jobAction(name string) http.HandlerFunc {
	a := jobActions[name]
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := jobID(w, r)
		if !ok {
			return
		}
		jobName := s.jobName(id)
		ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
		defer cancel()
		err := a.do(s.deps.CUPS, ctx, id)
		s.record(r, a.event, fmt.Sprintf("job %d", id), jobName, err)
		s.hub.Refresh()
		if err != nil {
			reply(w, http.StatusBadGateway, err.Error())
			return
		}
		reply(w, http.StatusOK, fmt.Sprintf("%s %s.", a.verb, quoted(jobName)))
	}
}

func (s *Server) moveJob(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	to := strings.TrimSpace(r.PostFormValue("to"))
	if to == "" {
		reply(w, http.StatusBadRequest, "Choose the printer to move the job to.")
		return
	}
	jobName := s.jobName(id)
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	err := s.deps.CUPS.Move(ctx, id, to)
	s.record(r, "job.move", fmt.Sprintf("job %d", id), "to "+to, err)
	s.hub.Refresh()
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	reply(w, http.StatusOK, fmt.Sprintf("Moved %s to %s.", quoted(jobName), s.shortPrinter(to)))
}

func (s *Server) testPage(w http.ResponseWriter, r *http.Request) {
	printer := r.PathValue("name")
	hold, _ := strconv.ParseBool(r.PostFormValue("hold"))
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	id, err := s.deps.CUPS.PrintTestPage(ctx, printer, hold)
	s.record(r, "print.testpage", printer, fmt.Sprintf("job %d", id), err)
	s.hub.Refresh()
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	reply(w, http.StatusOK, fmt.Sprintf("Test page sent to %s as job %d.", s.shortPrinter(printer), id))
}

// --- printing a file ---

// optionValue is what a sides or color option may look like: an IPP keyword.
var optionValue = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// capReader fails once more than max bytes have passed through it, so a file
// without a declared length still stops at the limit.
type capReader struct {
	r    io.Reader
	n    int64
	max  int64
	over bool
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.over {
		return 0, errTooLarge
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		c.over = true
		return 0, errTooLarge
	}
	return n, err
}

func isTooLarge(err error) bool {
	var mb *http.MaxBytesError
	return errors.Is(err, errTooLarge) || errors.As(err, &mb)
}

// drain reads what is left of the body, within the body limit, so the browser
// receives the answer instead of a reset connection.
func drain(r *http.Request) { io.Copy(io.Discard, r.Body) }

// sniffFormat maps the first bytes of a file to a document format CUPS
// prints, or "" when it is none of them. The file name plays no part.
func sniffFormat(head []byte) string {
	switch ct := http.DetectContentType(head); ct {
	case "application/pdf", "image/jpeg", "image/png":
		return ct
	case "text/plain; charset=utf-8":
		return "text/plain"
	}
	return ""
}

// printFile streams an upload to CUPS. The form fields come first and the file
// last, so the file never has to be held: the handler reads the multipart body
// part by part and hands the file part to CUPS as it arrives.
func (s *Server) printFile(w http.ResponseWriter, r *http.Request) {
	// The tolerance: formOverhead (64 KiB) above maxUpload is let through here
	// for the form framing. A file that is a little over 200 MB is stopped by
	// the counter below instead, after CUPS has been called and aborted.
	limit := maxUpload + formOverhead
	if r.ContentLength > limit {
		reply(w, http.StatusRequestEntityTooLarge, tooLargeText)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	mr, err := r.MultipartReader()
	if err != nil {
		reply(w, http.StatusBadRequest, notUploadText)
		return
	}
	var (
		printer string
		opts    cups.JobOptions
	)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			reply(w, http.StatusBadRequest, "No file came with the request.")
			return
		}
		if err != nil {
			if isTooLarge(err) {
				reply(w, http.StatusRequestEntityTooLarge, tooLargeText)
			} else {
				reply(w, http.StatusBadRequest, notUploadText)
			}
			return
		}
		field := part.FormName()
		if field == "file" {
			s.printPart(w, r, part, printer, opts)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(part, maxField))
		if err != nil || len(raw) >= maxField {
			drain(r)
			reply(w, http.StatusBadRequest, notUploadText)
			return
		}
		val := strings.TrimSpace(string(raw))
		switch field {
		case "printer":
			printer = val
		case "sides", "color":
			if val != "" && !optionValue.MatchString(val) {
				drain(r)
				reply(w, http.StatusBadRequest, "That print option is not valid.")
				return
			}
			if field == "sides" {
				opts.Sides = val
			} else {
				opts.Color = val
			}
		case "hold":
			opts.Hold, _ = strconv.ParseBool(val)
		}
	}
}

func (s *Server) printPart(w http.ResponseWriter, r *http.Request, part *multipart.Part, printer string, opts cups.JobOptions) {
	name := part.FileName()
	if printer == "" {
		drain(r)
		reply(w, http.StatusBadRequest, fieldOrderText)
		return
	}
	if name == "" {
		name = "document"
	}
	counted := &capReader{r: part, max: maxUpload}
	// A reader of exactly sniffLen bytes buffers no more than it sniffs, and
	// is the same reader that goes on to CUPS.
	br := bufio.NewReaderSize(counted, sniffLen)
	head, err := br.Peek(sniffLen)
	switch {
	case isTooLarge(err):
		drain(r)
		reply(w, http.StatusRequestEntityTooLarge, tooLargeText)
		return
	case err != nil && err != io.EOF:
		reply(w, http.StatusBadRequest, "The file could not be read. Try again.")
		return
	case len(head) == 0:
		drain(r)
		reply(w, http.StatusBadRequest, emptyFileText)
		return
	}
	format := sniffFormat(head)
	if format == "" {
		drain(r)
		reply(w, http.StatusUnsupportedMediaType, badFormatText)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), printTimeout)
	defer cancel()
	id, err := s.deps.CUPS.Print(ctx, printer, name, format, opts, br)
	tooLarge := counted.over || isTooLarge(err)
	if tooLarge {
		err = errors.New(tooLargeText)
	}
	s.record(r, "print.file", printer, fmt.Sprintf("%s as job %d", quoted(name), id), err)
	s.hub.Refresh()
	switch {
	case tooLarge:
		reply(w, http.StatusRequestEntityTooLarge, tooLargeText)
	case err != nil:
		reply(w, http.StatusBadGateway, err.Error())
	default:
		reply(w, http.StatusOK, fmt.Sprintf("Sent %s to %s as job %d.", quoted(name), s.shortPrinter(printer), id))
	}
}
