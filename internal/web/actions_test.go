// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/store"
)

func (f *fakeCUPS) act(what string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acts = append(f.acts, what)
	if f.failOnly != "" && !strings.HasPrefix(what, f.failOnly) {
		return nil
	}
	return f.actErr
}

func (f *fakeCUPS) Hold(_ context.Context, id int) error    { return f.act(fmt.Sprint("hold ", id)) }
func (f *fakeCUPS) Release(_ context.Context, id int) error { return f.act(fmt.Sprint("release ", id)) }
func (f *fakeCUPS) Cancel(_ context.Context, id int) error  { return f.act(fmt.Sprint("cancel ", id)) }
func (f *fakeCUPS) Restart(_ context.Context, id int) error { return f.act(fmt.Sprint("restart ", id)) }
func (f *fakeCUPS) Move(_ context.Context, id int, to string) error {
	return f.act(fmt.Sprintf("move %d %s", id, to))
}

func (f *fakeCUPS) Print(_ context.Context, printer, name, format string, o cups.JobOptions, doc io.Reader) (int, error) {
	f.mu.Lock()
	f.printed++
	fn, err := f.print, f.actErr
	f.mu.Unlock()
	if fn != nil {
		return fn(printer, name, format, o, doc)
	}
	if _, cerr := io.Copy(io.Discard, doc); cerr != nil {
		return 0, fmt.Errorf("CUPS does not answer: %w", cerr)
	}
	return 31, err
}

func (f *fakeCUPS) PrintTestPage(_ context.Context, printer string, hold bool) (int, error) {
	return 32, f.act(fmt.Sprintf("testpage %s hold=%t", printer, hold))
}

func (f *fakeCUPS) did() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acts...)
}

func (f *fakeCUPS) prints() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.printed
}

// actionServer is a server over f with a real event store in a temp dir, the
// given declared queues, and a first page load so the server knows the job
// and queue names.
func actionServer(t *testing.T, f *fakeCUPS, declared ...string) (*Server, http.Handler) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(config.Config{Listen: "127.0.0.1:7631", User: "mika", DeclaredQueues: declared}, Deps{CUPS: f, Store: db}, NewToken())
	h := s.Handler()
	if rec := get(h, "/"); rec.Code != 200 {
		t.Fatalf("GET /: %d", rec.Code)
	}
	return s, h
}

func postTo(s *Server, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:7631"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Skra-Token", s.token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func lastEvent(t *testing.T, s *Server) store.Event {
	t.Helper()
	ev, err := s.deps.Store.Recent(context.Background(), 1)
	if err != nil || len(ev) != 1 {
		t.Fatalf("events = %v, err %v", ev, err)
	}
	return ev[0]
}

func twoJobs() *fakeCUPS {
	return &fakeCUPS{
		printers: []cups.Printer{brother()},
		def:      "Virtual-MFC",
		active: []cups.Job{
			{ID: 12, Name: "Invoice.pdf", Printer: "Virtual-MFC", State: 5},
			{ID: 13, Name: "Held.pdf", Printer: "Virtual-MFC", State: 4},
			{ID: 14, Name: "Waiting.pdf", Printer: "Virtual-MFC", State: 3},
		},
		done: []cups.Job{{ID: 9, Name: "Old.pdf", Printer: "Virtual-MFC", State: 9}},
	}
}

func TestCancelLogsEvent(t *testing.T) {
	f := twoJobs()
	s, h := actionServer(t, f)

	rec := postTo(s, h, "/jobs/12/cancel", nil)
	if rec.Code != 200 || rec.Body.String() != `Canceled "Invoice.pdf".` {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.did(); len(got) != 1 || got[0] != "cancel 12" {
		t.Errorf("CUPS was asked %v", got)
	}
	ev := lastEvent(t, s)
	if ev.Action != "job.cancel" || ev.Target != "job 12" || !ev.OK || ev.Actor != "mika" {
		t.Errorf("event = %+v", ev)
	}

	f.set(func(f *fakeCUPS) { f.actErr = errors.New("Job #12 is already canceled - can't cancel.") })
	rec = postTo(s, h, "/jobs/12/cancel", nil)
	if rec.Code != http.StatusBadGateway || rec.Body.String() != "Job #12 is already canceled - can't cancel." {
		t.Errorf("refused: status %d, body %q", rec.Code, rec.Body.String())
	}
	ev = lastEvent(t, s)
	if ev.Action != "job.cancel" || ev.Target != "job 12" || ev.OK || !strings.Contains(ev.Detail, "already canceled") {
		t.Errorf("event after refusal = %+v", ev)
	}
}

func TestJobActionsAnswerWithTheFooterMessage(t *testing.T) {
	for _, tc := range []struct {
		path, form, want, act, did string
	}{
		{"/jobs/13/release", "", `Released "Held.pdf".`, "job.release", "release 13"},
		{"/jobs/14/hold", "", `Held "Waiting.pdf".`, "job.hold", "hold 14"},
		{"/jobs/9/restart", "", `Reprinting "Old.pdf".`, "job.restart", "restart 9"},
		{"/jobs/14/move", "to=Virtual-MFC-2", `Moved "Waiting.pdf" to Virtual-MFC-2.`, "job.move", "move 14 Virtual-MFC-2"},
		{"/jobs/77/hold", "", `Held "job 77".`, "job.hold", "hold 77"}, // not in the snapshot: the id stands in
	} {
		f := twoJobs()
		s, h := actionServer(t, f)
		form, _ := url.ParseQuery(tc.form)
		rec := postTo(s, h, tc.path, form)
		if rec.Code != 200 || rec.Body.String() != tc.want {
			t.Errorf("%s: status %d, body %q, want %q", tc.path, rec.Code, rec.Body.String(), tc.want)
		}
		if got := f.did(); len(got) != 1 || got[0] != tc.did {
			t.Errorf("%s: CUPS was asked %v, want %q", tc.path, got, tc.did)
		}
		if ev := lastEvent(t, s); ev.Action != tc.act || !ev.OK {
			t.Errorf("%s: event = %+v", tc.path, ev)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("%s: Content-Type = %q", tc.path, ct)
		}
	}
}

func TestJobActionsRejectBadRequests(t *testing.T) {
	f := twoJobs()
	s, h := actionServer(t, f)
	for _, tc := range []struct{ path, form string }{
		{"/jobs/abc/cancel", ""},
		{"/jobs/0/cancel", ""},
		{"/jobs/-3/hold", ""},
		{"/jobs/12x/release", ""},
		{"/jobs/12/move", ""},
		{"/jobs/12/move", "to="},
	} {
		form, _ := url.ParseQuery(tc.form)
		if rec := postTo(s, h, tc.path, form); rec.Code != 400 || rec.Body.Len() == 0 {
			t.Errorf("%s %q: status %d, body %q, want 400 with a message", tc.path, tc.form, rec.Code, rec.Body.String())
		}
	}
	if got := f.did(); len(got) != 0 {
		t.Errorf("CUPS was asked %v for bad requests", got)
	}
}

func TestActionsNeedTheToken(t *testing.T) {
	f := twoJobs()
	_, h := actionServer(t, f)
	for _, path := range []string{"/jobs/12/cancel", "/jobs/12/move", "/print", "/printers/Virtual-MFC/testpage"} {
		req := httptest.NewRequest("POST", path, nil)
		req.Host = "127.0.0.1:7631"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("%s without a token: status %d", path, rec.Code)
		}
	}
	if len(f.did()) != 0 || f.prints() != 0 {
		t.Error("an action ran without a token")
	}
}

func TestActionRefreshesOpenPages(t *testing.T) {
	f := twoJobs()
	s, h := actionServer(t, f)
	withPollEvery(s, time.Hour) // only a Refresh can produce the second snapshot

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.hub.Subscribe(ctx)
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no first snapshot")
	}
	f.set(func(f *fakeCUPS) { f.active = f.active[:1] })
	postTo(s, h, "/jobs/13/cancel", nil)
	select {
	case snap := <-ch:
		if len(snap.Active) != 1 {
			t.Errorf("snapshot after the action has %d active jobs", len(snap.Active))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the action did not refresh the open page")
	}
}

func TestTestPageRoute(t *testing.T) {
	f := &fakeCUPS{printers: []cups.Printer{brother(), {Name: "Fußball-Karte", State: 3}}}
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/printers/Virtual-MFC/testpage", url.Values{"hold": {"true"}})
	if rec.Code != 200 || rec.Body.String() != "Test page sent to Brother as job 32." {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.did(); len(got) != 1 || got[0] != "testpage Virtual-MFC hold=true" {
		t.Errorf("CUPS was asked %v", got)
	}
	if ev := lastEvent(t, s); ev.Action != "print.testpage" || ev.Target != "Virtual-MFC" || !ev.OK || !strings.Contains(ev.Detail, "32") {
		t.Errorf("event = %+v", ev)
	}

	postTo(s, h, "/printers/Fu%C3%9Fball-Karte/testpage", nil)
	if got := f.did(); got[len(got)-1] != "testpage Fußball-Karte hold=false" {
		t.Errorf("queue name not decoded: %v", got)
	}

	f.set(func(f *fakeCUPS) { f.actErr = errors.New("The printer or class does not exist.") })
	rec = postTo(s, h, "/printers/Virtual-MFC/testpage", nil)
	if rec.Code != 502 || rec.Body.String() != "The printer or class does not exist." {
		t.Errorf("refused: status %d, body %q", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, s); ev.Action != "print.testpage" || ev.OK {
		t.Errorf("event after refusal = %+v", ev)
	}
}

// --- queue buttons ---

func TestQueueButtonsFollowTheJobState(t *testing.T) {
	body := pageFor(t, twoJobs())
	row := func(id string) string {
		i := strings.Index(body, `<td class="jid">`+id+`</td>`)
		if i < 0 {
			t.Fatalf("no row for job %s", id)
		}
		return body[i : i+strings.Index(body[i:], "</tr>")]
	}
	printing, held, waiting := row("12"), row("13"), row("14")

	if !strings.Contains(printing, `data-act="cancel" data-job="12"`) {
		t.Errorf("printing job has no cancel button: %s", printing)
	}
	for _, want := range []string{
		`data-confirm="It is printing on the Brother. The sheet in the printer may still come out."`,
		`data-confirm-label="Cancel job 12"`,
		`data-confirm-title="Cancel &#34;Invoice.pdf&#34;?"`,
	} {
		if !strings.Contains(printing, want) {
			t.Errorf("printing job lacks %s in %s", want, printing)
		}
	}
	if strings.Contains(printing, `data-act="hold"`) || strings.Contains(printing, `data-act="release"`) {
		t.Errorf("a printing job offers hold or release: %s", printing)
	}

	if !strings.Contains(held, `data-act="release" data-job="13"`) || !strings.Contains(held, `data-act="cancel" data-job="13"`) {
		t.Errorf("held job buttons: %s", held)
	}
	if strings.Contains(held, "data-confirm") || strings.Contains(held, `data-act="hold"`) {
		t.Errorf("held job: %s", held)
	}
	if !strings.Contains(waiting, `data-act="hold" data-job="14"`) || strings.Contains(waiting, "data-confirm") {
		t.Errorf("waiting job buttons: %s", waiting)
	}
}

func TestPageOffersThePrintForm(t *testing.T) {
	body := pageFor(t, twoJobs())
	for _, want := range []string{`id="opts"`, `data-printers=`, `Virtual-MFC`, `id="dialog"`, `id="dlg-yes"`, `Keep printing`, `data-act="testpage" data-printer="Virtual-MFC"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, `id="choose" disabled`) {
		t.Error("the choose button is still disabled")
	}
}

// --- printing a file ---

// fileBody is a file of n bytes that starts with head and is fill (zeros when
// fill is 0) after.
type fileBody struct {
	head []byte
	fill byte
	n    int64
	pos  int64
}

func (r *fileBody) Read(p []byte) (int, error) {
	if r.pos >= r.n {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n-r.pos {
		p = p[:r.n-r.pos]
	}
	for i := range p {
		p[i] = r.fill
		if j := r.pos + int64(i); j < int64(len(r.head)) {
			p[i] = r.head[j]
		}
	}
	r.pos += int64(len(p))
	return len(p), nil
}

// upload posts a multipart form whose fields come in the given order. A
// contentLength of -1 sends no length, as a chunked request does.
func upload(s *Server, h http.Handler, fields [][2]string, filename string, file io.Reader, contentLength int64) *httptest.ResponseRecorder {
	return uploadCtx(context.Background(), s, h, fields, filename, file, contentLength)
}

// uploadCtx is upload for a request whose context the caller owns, so a test
// can make the browser leave.
func uploadCtx(ctx context.Context, s *Server, h http.Handler, fields [][2]string, filename string, file io.Reader, contentLength int64) *httptest.ResponseRecorder {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		for _, kv := range fields {
			mw.WriteField(kv[0], kv[1])
		}
		if file != nil {
			part, err := mw.CreateFormFile("file", filename)
			if err != nil { // the handler stopped reading and the reader was closed
				return
			}
			if _, err := io.Copy(part, file); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		mw.Close()
		pw.Close()
	}()
	req := httptest.NewRequest("POST", "/print", pr).WithContext(ctx)
	req.Host = "127.0.0.1:7631"
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Skra-Token", s.token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	pr.Close() // frees the writer when the handler stopped reading early
	return rec
}

var printFields = [][2]string{{"printer", "Virtual-MFC"}, {"sides", "two-sided-long-edge"}, {"color", "monochrome"}, {"hold", "true"}}

func TestPrintStreamsTheFileToCUPS(t *testing.T) {
	f := twoJobs()
	var got struct {
		printer, name, format string
		o                     cups.JobOptions
		doc                   string
	}
	f.print = func(printer, name, format string, o cups.JobOptions, doc io.Reader) (int, error) {
		b, _ := io.ReadAll(doc)
		got.printer, got.name, got.format, got.o, got.doc = printer, name, format, o, string(b)
		return 31, nil
	}
	s, h := actionServer(t, f)
	pdf := "%PDF-1.4\n" + strings.Repeat("x", 3000)
	rec := upload(s, h, printFields, "Karte.pdf", strings.NewReader(pdf), -1)
	if rec.Code != 200 || rec.Body.String() != `Sent "Karte.pdf" to Brother as job 31.` {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got.printer != "Virtual-MFC" || got.name != "Karte.pdf" || got.format != "application/pdf" {
		t.Errorf("Print got %+v", got)
	}
	want := cups.JobOptions{Sides: "two-sided-long-edge", Color: "monochrome", Hold: true}
	if got.o != want {
		t.Errorf("options = %+v, want %+v", got.o, want)
	}
	if got.doc != pdf {
		t.Errorf("CUPS got %d bytes, want the %d of the file, unchanged", len(got.doc), len(pdf))
	}
	ev := lastEvent(t, s)
	if ev.Action != "print.file" || ev.Target != "Virtual-MFC" || !ev.OK || !strings.Contains(ev.Detail, "Karte.pdf") || !strings.Contains(ev.Detail, "31") {
		t.Errorf("event = %+v", ev)
	}
}

func TestPrintSniffsTheFormatNotTheExtension(t *testing.T) {
	jpeg := "\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01" + strings.Repeat("\x00", 100)
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 100)
	for _, tc := range []struct{ file, content, want string }{
		{"a.pdf", "%PDF-1.7\nx", "application/pdf"},
		{"photo.bin", jpeg, "image/jpeg"},
		{"photo.png", png, "image/png"},
		{"notes.txt", "Grüße\nHello\n", "text/plain"},
		{"noext", "plain words", "text/plain"},
		{"fake.pdf", "MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xff\xff", ""},
		{"page.html", "<!DOCTYPE html><html><body>x</body></html>", ""},
		{"utf16.txt", "\xff\xfeh\x00i\x00", ""},
		{"tool.exe", "MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xff\xff", ""},
	} {
		f := twoJobs()
		var format string
		f.print = func(_, _, fm string, _ cups.JobOptions, doc io.Reader) (int, error) {
			io.Copy(io.Discard, doc)
			format = fm
			return 31, nil
		}
		s, h := actionServer(t, f)
		rec := upload(s, h, printFields, tc.file, strings.NewReader(tc.content), -1)
		if tc.want == "" {
			if rec.Code != 415 || rec.Body.String() != "Only PDF, JPEG, PNG and plain text can be printed from here." || f.prints() != 0 {
				t.Errorf("%s: status %d, body %q, prints %d", tc.file, rec.Code, rec.Body.String(), f.prints())
			}
			continue
		}
		if rec.Code != 200 || format != tc.want {
			t.Errorf("%s: status %d (%s), format %q, want %q", tc.file, rec.Code, rec.Body.String(), format, tc.want)
		}
	}
}

const mib = 1 << 20

func TestPrintRejectsLargeAndUnknown(t *testing.T) {
	pdf := []byte("%PDF-1.4\n")
	// The tolerance is formOverhead (64 KiB) over 200 MB. A body that announces
	// more than that is refused before CUPS is called.
	t.Run("Content-Length above 200 MB plus the tolerance never calls Print", func(t *testing.T) {
		f := twoJobs()
		s, h := actionServer(t, f)
		size := maxUpload + formOverhead + 1024
		rec := upload(s, h, printFields, "big.pdf", &fileBody{head: pdf, n: size}, size+300)
		if rec.Code != 413 || rec.Body.String() != "Files up to 200 MB can be printed from here." {
			t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
		}
		if f.prints() != 0 {
			t.Error("Print was called for a body that announced it was too large")
		}
		if ev, _ := s.deps.Store.Recent(context.Background(), 1); len(ev) != 0 {
			t.Errorf("a refused upload was logged: %+v", ev[0])
		}
	})
	// Over 200 MB but not announced as such: either no length at all, or one
	// within the tolerance. The counter stops the file, so Print has been called
	// and sees its reader fail, and returns no job id.
	for _, tc := range []struct {
		name   string
		length func(size int64) int64
	}{
		{"200 MB and one byte, no length announced", func(int64) int64 { return -1 }},
		{"200 MB and one byte, length within the tolerance", func(size int64) int64 { return size + 300 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := twoJobs()
			var readErr error
			var gotID int
			f.print = func(_, _, _ string, _ cups.JobOptions, doc io.Reader) (int, error) {
				_, readErr = io.Copy(io.Discard, doc)
				if readErr != nil {
					return 0, fmt.Errorf("CUPS does not answer: %w", readErr)
				}
				gotID = 31
				return gotID, nil
			}
			s, h := actionServer(t, f)
			size := maxUpload + 1
			rec := upload(s, h, printFields, "big.pdf", &fileBody{head: pdf, n: size}, tc.length(size))
			if rec.Code != 413 || rec.Body.String() != "Files up to 200 MB can be printed from here." {
				t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
			}
			if f.prints() != 1 {
				t.Errorf("Print was called %d times, want 1", f.prints())
			}
			if readErr == nil || gotID != 0 {
				t.Errorf("Print's reader did not fail (err %v) or a job id %d came back", readErr, gotID)
			}
			ev := lastEvent(t, s)
			if ev.Action != "print.file" || ev.OK || ev.Target != "Virtual-MFC" {
				t.Errorf("event = %+v, want print.file with OK false", ev)
			}
		})
	}
	t.Run("exactly 200 MB", func(t *testing.T) {
		f := twoJobs()
		s, h := actionServer(t, f)
		rec := upload(s, h, printFields, "edge.pdf", &fileBody{head: pdf, n: 200 * mib}, -1)
		if rec.Code != 200 {
			t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("unknown type", func(t *testing.T) {
		f := twoJobs()
		s, h := actionServer(t, f)
		rec := upload(s, h, printFields, "setup.exe", &fileBody{head: []byte("MZ\x90\x00\x03\x00"), n: 4096}, -1)
		if rec.Code != 415 || rec.Body.String() != "Only PDF, JPEG, PNG and plain text can be printed from here." {
			t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
		}
		if f.prints() != 0 {
			t.Error("Print was called for an unknown type")
		}
	})
	t.Run("empty", func(t *testing.T) {
		f := twoJobs()
		s, h := actionServer(t, f)
		rec := upload(s, h, printFields, "empty.pdf", strings.NewReader(""), -1)
		if rec.Code != 400 || rec.Body.String() != "The file is empty." {
			t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
		}
		if f.prints() != 0 {
			t.Error("Print was called for an empty file")
		}
	})
}

func TestPrintRejectsABadForm(t *testing.T) {
	f := twoJobs()
	s, h := actionServer(t, f)
	pdf := func() io.Reader { return strings.NewReader("%PDF-1.4\nx") }
	for _, tc := range []struct {
		name   string
		fields [][2]string
		file   io.Reader
	}{
		{"no printer", [][2]string{{"sides", "one-sided"}}, pdf()},
		{"empty printer", [][2]string{{"printer", ""}}, pdf()},
		{"no file", printFields, nil},
		{"odd option", [][2]string{{"printer", "Virtual-MFC"}, {"sides", "one sided;\n"}}, pdf()},
	} {
		rec := upload(s, h, tc.fields, "a.pdf", tc.file, -1)
		if rec.Code != 400 || rec.Body.Len() == 0 {
			t.Errorf("%s: status %d, body %q", tc.name, rec.Code, rec.Body.String())
		}
	}
	// A printer field that came empty says so, and a missing one is the
	// wrong order of the form.
	for _, tc := range []struct {
		name   string
		fields [][2]string
		want   string
	}{
		{"no printer", [][2]string{{"sides", "one-sided"}}, fieldOrderText},
		{"empty printer", [][2]string{{"printer", ""}}, "Choose the printer to print on."},
		{"blank printer", [][2]string{{"printer", "  "}}, "Choose the printer to print on."},
	} {
		if rec := upload(s, h, tc.fields, "a.pdf", pdf(), -1); rec.Code != 400 || rec.Body.String() != tc.want {
			t.Errorf("%s: status %d, body %q, want 400 %q", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
	// skrá's own refusals before it asks CUPS are not actions for the history.
	if ev, _ := s.deps.Store.Recent(context.Background(), 1); len(ev) != 0 {
		t.Errorf("a refused form was logged: %+v", ev[0])
	}
	// The file part before the printer field.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", "a.pdf")
		if err != nil {
			return
		}
		io.WriteString(part, "%PDF-1.4\nx")
		mw.WriteField("printer", "Virtual-MFC")
		mw.Close()
		pw.Close()
	}()
	req := httptest.NewRequest("POST", "/print", pr)
	req.Host = "127.0.0.1:7631"
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Skra-Token", s.token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	pr.Close()
	if rec.Code != 400 {
		t.Errorf("file before printer: status %d, body %q", rec.Code, rec.Body.String())
	}
	if f.prints() != 0 {
		t.Error("Print was called for a bad form")
	}
}

// The printer of an upload is found like the queue of every per-queue
// route: a name no queue can have is refused, one CUPS does not have is not
// found, and one in another case is sent under the name CUPS knows.
func TestPrintFindsThePrinterLikeEveryQueueRoute(t *testing.T) {
	pdf := "%PDF-1.4\nx"
	for _, tc := range []struct {
		printer string
		code    int
		body    string
	}{
		{"a/b", 400, "That is not a queue name."},
		{"a b", 400, "That is not a queue name."},
		{strings.Repeat("x", 128), 400, "That is not a queue name."},
		{"Nowhere", 404, "There is no queue named Nowhere."},
	} {
		f := twoJobs()
		s, h := actionServer(t, f)
		rec := upload(s, h, [][2]string{{"printer", tc.printer}}, "a.pdf", strings.NewReader(pdf), -1)
		if rec.Code != tc.code || rec.Body.String() != tc.body {
			t.Errorf("%.20q: status %d, body %q, want %d %q", tc.printer, rec.Code, rec.Body.String(), tc.code, tc.body)
		}
		if f.prints() != 0 {
			t.Errorf("%.20q: Print was called", tc.printer)
		}
	}

	f := twoJobs()
	var to string
	f.print = func(printer, _, _ string, _ cups.JobOptions, doc io.Reader) (int, error) {
		io.Copy(io.Discard, doc)
		to = printer
		return 32, nil
	}
	s, h := actionServer(t, f)
	if rec := upload(s, h, [][2]string{{"printer", "virtual-mfc"}}, "a.pdf", strings.NewReader(pdf), -1); rec.Code != 200 {
		t.Fatalf("lowercase name: status %d, body %q", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, s); to != "Virtual-MFC" || ev.Target != "Virtual-MFC" {
		t.Errorf("printed to %q, event about %q; want the name CUPS knows", to, ev.Target)
	}
}

// A browser that leaves in the middle of the file is not CUPS failing: the
// history and the reply say the upload stopped, whatever the CUPS client made
// of the read that failed.
func TestPrintTellsAnUploadThatStoppedFromCUPSTrouble(t *testing.T) {
	const stopped = "The upload stopped before the end. Send the file again."
	pdf := "%PDF-1.4\n" + strings.Repeat("x", 3000)
	check := func(t *testing.T, s *Server, rec *httptest.ResponseRecorder) {
		t.Helper()
		ev := lastEvent(t, s)
		if ev.Action != "print.file" || ev.OK || ev.Detail != `"a.pdf" as job 0: `+stopped {
			t.Errorf("event = %+v, want the upload stopped, with the file name kept", ev)
		}
		if strings.Contains(rec.Body.String(), "CUPS does not answer") || !strings.Contains(rec.Body.String(), stopped) {
			t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
		}
	}
	t.Run("the body read fails", func(t *testing.T) {
		s, h := actionServer(t, twoJobs())
		file := io.MultiReader(strings.NewReader(pdf), iotest.ErrReader(errors.New("connection reset by peer")))
		rec := upload(s, h, printFields, "a.pdf", file, -1)
		check(t, s, rec)
	})
	t.Run("the request is canceled", func(t *testing.T) {
		f := twoJobs()
		ctx, cancel := context.WithCancel(context.Background())
		f.print = func(_ string, _ string, _ string, _ cups.JobOptions, doc io.Reader) (int, error) {
			cancel()
			return 0, errors.New("CUPS does not answer: context canceled")
		}
		s, h := actionServer(t, f)
		rec := uploadCtx(ctx, s, h, printFields, "a.pdf", strings.NewReader(pdf), -1)
		check(t, s, rec)
	})
	t.Run("a CUPS that fails with the whole file sent keeps its own words", func(t *testing.T) {
		f := twoJobs()
		f.actErr = errors.New("CUPS is busy. Try again in a moment.")
		s, h := actionServer(t, f)
		rec := upload(s, h, printFields, "a.pdf", strings.NewReader(pdf), -1)
		if ev := lastEvent(t, s); ev.Detail != `"a.pdf" as job 31: CUPS is busy. Try again in a moment.` || rec.Body.String() != "CUPS is busy. Try again in a moment." {
			t.Errorf("event %+v, body %q", ev, rec.Body.String())
		}
	})
}

// countedBody is a request body that counts what is read from it.
type countedBody struct {
	r io.Reader
	n int64
}

func (c *countedBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// rawUpload posts a multipart body that was built beforehand and returns how
// many of its bytes the handler read.
func rawUpload(s *Server, h http.Handler, body []byte, contentType string) (*httptest.ResponseRecorder, int64) {
	cb := &countedBody{r: bytes.NewReader(body)}
	req := httptest.NewRequest("POST", "/print", cb)
	req.Host = "127.0.0.1:7631"
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Skra-Token", s.token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, cb.n
}

// bigForm is a print form with the printer field and a file part of the given
// header and content, followed by a megabyte in another part that the handler
// has no use for.
func bigForm(t *testing.T, header textproto.MIMEHeader, content string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("printer", "Virtual-MFC")
	part, err := mw.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(part, content)
	mw.WriteField("pad", strings.Repeat("x", mib))
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func pdfPart() textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="a.pdf"`)
	return h
}

// Every reply that is made before the whole body has been read drains the
// body first, so the browser gets the answer and not a reset connection.
func TestPrintAnswersWithTheWholeBodyRead(t *testing.T) {
	t.Run("CUPS refuses before it has read the document", func(t *testing.T) {
		f := twoJobs()
		f.print = func(string, string, string, cups.JobOptions, io.Reader) (int, error) {
			return 0, errors.New("CUPS refused the document. Check the queue and try again.")
		}
		s, h := actionServer(t, f)
		body, ct := bigForm(t, pdfPart(), "%PDF-1.4\n")
		rec, read := rawUpload(s, h, body, ct)
		if rec.Code != 502 {
			t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
		}
		if read != int64(len(body)) {
			t.Errorf("the handler read %d of %d bytes before it answered", read, len(body))
		}
	})
	t.Run("the file cannot be read at its start", func(t *testing.T) {
		f := twoJobs()
		s, h := actionServer(t, f)
		hdr := pdfPart()
		hdr.Set("Content-Transfer-Encoding", "quoted-printable")
		body, ct := bigForm(t, hdr, "=") // a lone = cannot be decoded
		rec, read := rawUpload(s, h, body, ct)
		if rec.Code != 400 || rec.Body.String() != "The file could not be read. Try again." {
			t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
		}
		if read != int64(len(body)) {
			t.Errorf("the handler read %d of %d bytes before it answered", read, len(body))
		}
		if f.prints() != 0 {
			t.Error("Print was called for a file that could not be read")
		}
	})
}

func TestPrintNotMultipart(t *testing.T) {
	s, h := actionServer(t, twoJobs())
	rec := postTo(s, h, "/print", url.Values{"printer": {"x"}})
	if rec.Code != 400 {
		t.Errorf("status %d", rec.Code)
	}
}

func TestPrintCUPSRefusal(t *testing.T) {
	f := twoJobs()
	f.actErr = errors.New("The printer or class does not exist.")
	s, h := actionServer(t, f)
	rec := upload(s, h, printFields, "a.pdf", strings.NewReader("%PDF-1.4\nx"), -1)
	if rec.Code != 502 || rec.Body.String() != "The printer or class does not exist." {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, s); ev.Action != "print.file" || ev.OK {
		t.Errorf("event = %+v", ev)
	}
}

// --- reprint and move buttons ---

func rowOf(t *testing.T, body, table, id string) string {
	t.Helper()
	start := strings.Index(body, `<div id="`+table+`">`)
	if start < 0 {
		t.Fatalf("no %s fragment", table)
	}
	body = body[start:]
	i := strings.Index(body, `<td class="jid">`+id+`</td>`)
	if i < 0 {
		t.Fatalf("no row for job %s in %s", id, table)
	}
	return body[i : i+strings.Index(body[i:], "</tr>")]
}

func TestReprintButtonOnEveryRecentRow(t *testing.T) {
	f := twoJobs()
	f.done = append(f.done, cups.Job{ID: 8, Name: "Broken.pdf", Printer: "Virtual-MFC", State: 8}, cups.Job{ID: 7, Name: "Gone.pdf", Printer: "Virtual-MFC", State: 7})
	body := pageFor(t, f)
	for _, id := range []string{"9", "8", "7"} {
		if row := rowOf(t, body, "recent", id); !strings.Contains(row, `data-act="reprint" data-job="`+id+`"`) {
			t.Errorf("recent row %s has no reprint button: %s", id, row)
		}
	}
	if strings.Contains(rowOf(t, body, "queue", "13"), "reprint") {
		t.Error("a queued job offers reprint")
	}
}

func TestRestartLogsEventAndAnswersReprinting(t *testing.T) {
	f := twoJobs()
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/jobs/9/restart", nil)
	if rec.Code != 200 || rec.Body.String() != `Reprinting "Old.pdf".` {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, s); ev.Action != "job.restart" || ev.Target != "job 9" || !ev.OK {
		t.Errorf("event = %+v", ev)
	}
	f.set(func(f *fakeCUPS) { f.actErr = errors.New("Job #9 cannot be restarted: its files are gone.") })
	rec = postTo(s, h, "/jobs/9/restart", nil)
	if rec.Code != 502 || rec.Body.String() != "Job #9 cannot be restarted: its files are gone." {
		t.Errorf("refused: status %d, body %q", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, s); ev.Action != "job.restart" || ev.OK {
		t.Errorf("event after refusal = %+v", ev)
	}
}

func second() cups.Printer {
	p := brother()
	p.Name, p.Info = "Virtual-MFC-2", "Hall laser"
	return p
}

func TestMoveButtonNeedsTwoPrintersAndAWaitingOrHeldJob(t *testing.T) {
	one := pageFor(t, twoJobs())
	if strings.Contains(one, `data-act="move"`) {
		t.Error("the move button is offered with a single printer")
	}

	f := twoJobs()
	f.printers = []cups.Printer{brother(), second()}
	body := pageFor(t, f)
	for id, want := range map[string]bool{"12": false, "13": true, "14": true} {
		row := rowOf(t, body, "queue", id)
		if got := strings.Contains(row, `data-act="move" data-job="`+id+`"`); got != want {
			t.Errorf("job %s: move button = %v, want %v: %s", id, got, want, row)
		}
	}
	row := rowOf(t, body, "queue", "13")
	if !strings.Contains(row, `&#34;name&#34;:&#34;Virtual-MFC-2&#34;`) || strings.Contains(row, `&#34;name&#34;:&#34;Virtual-MFC&#34;`) {
		t.Errorf("the move choices are not the other queue only: %s", row)
	}
	if !strings.Contains(row, `data-move-title="Move &#34;Held.pdf&#34;"`) {
		t.Errorf("no move title: %s", row)
	}
	if strings.Contains(rowOf(t, body, "recent", "9"), `data-act="move"`) {
		t.Error("a finished job offers move")
	}
}

func TestMoveLogsEvent(t *testing.T) {
	f := twoJobs()
	f.printers = []cups.Printer{brother(), second()}
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/jobs/14/move", url.Values{"to": {"Virtual-MFC-2"}})
	if rec.Code != 200 || rec.Body.String() != `Moved "Waiting.pdf" to Hall.` {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, s); ev.Action != "job.move" || ev.Target != "job 14" || ev.Detail != "to Virtual-MFC-2" || !ev.OK {
		t.Errorf("event = %+v", ev)
	}
	f.set(func(f *fakeCUPS) { f.actErr = errors.New("The printer or class does not exist.") })
	rec = postTo(s, h, "/jobs/14/move", url.Values{"to": {"Nowhere"}})
	if rec.Code != 502 {
		t.Errorf("refused: status %d", rec.Code)
	}
	if ev := lastEvent(t, s); ev.Action != "job.move" || ev.Target != "job 14" || ev.OK {
		t.Errorf("event after refusal = %+v", ev)
	}
}

// A failed action keeps what the action was about and adds the reason, so the
// log still says which queue a failed move was meant for.
func TestRecordKeepsTheDetailOnFailure(t *testing.T) {
	f := twoJobs()
	s, h := actionServer(t, f)
	f.set(func(f *fakeCUPS) { f.actErr = errors.New("The printer or class does not exist.") })
	postTo(s, h, "/jobs/14/move", url.Values{"to": {"Nowhere"}})
	ev := lastEvent(t, s)
	if ev.OK || ev.Detail != "to Nowhere: The printer or class does not exist." {
		t.Errorf("event after a failed move = %+v", ev)
	}
	postTo(s, h, "/jobs/12/cancel", nil)
	if ev = lastEvent(t, s); ev.OK || ev.Detail != `Invoice.pdf: The printer or class does not exist.` {
		t.Errorf("event after a failed cancel = %+v", ev)
	}
	s.record(httptest.NewRequest("POST", "/", nil), "x.y", "t", "", errors.New("boom"))
	if ev = lastEvent(t, s); ev.Detail != "boom" {
		t.Errorf("a failure with no detail logs %q, want just the reason", ev.Detail)
	}
}

// With CUPS down, an action's reply and its history row give ruling 2's next
// step and not the dial error under it. The client is a real one, on a socket
// that is not there.
func TestActionsWithCUPSDownSayWhatToDo(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := cups.New(filepath.Join(t.TempDir(), "no-such.sock"), "root")
	s := New(config.Config{Listen: "127.0.0.1:7631", User: "root"}, Deps{CUPS: c, Store: db}, NewToken())
	h := s.Handler()
	want := cups.ErrNotRunning.Error()
	// A queue that cannot be looked up is not an attempt, so pause leaves no
	// row; the other two do.
	for _, tc := range []struct{ path, action string }{
		{"/printers/Hall/pause", ""},
		{"/jobs/7/cancel", "job.cancel"},
		{"/printers/log/level", "cups.loglevel"},
	} {
		rec := postTo(s, h, tc.path, url.Values{"level": {"debug"}})
		if rec.Code != http.StatusBadGateway || rec.Body.String() != want {
			t.Errorf("%s: status %d, body %q, want 502 %q", tc.path, rec.Code, rec.Body.String(), want)
		}
		if tc.action == "" {
			continue
		}
		if ev := lastEvent(t, s); ev.Action != tc.action || ev.OK || !strings.HasSuffix(ev.Detail, want) || strings.Contains(ev.Detail, "dial") {
			t.Errorf("%s: event %+v, want it to end with %q", tc.path, ev, want)
		}
	}
	if body := get(h, "/printers/log").Body.String(); !strings.Contains(body, want) || strings.Contains(body, "dial unix") {
		t.Errorf("the log section does not give %q alone", want)
	}
}
