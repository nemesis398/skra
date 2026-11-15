// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nemesis398/skra/internal/ipp"
	"github.com/nemesis398/skra/internal/ipp/ipptest"
)

// seen is what a fake CUPS got: the path, the decoded request and the bytes
// after the end-of-attributes tag.
type seen struct {
	path string
	op   uint16
	req  *ipp.Response
	doc  string
}

func (s seen) group(g ipp.Group) ipp.AttrGroup {
	if s.req == nil {
		return ipp.AttrGroup{}
	}
	if gs := s.req.Of(g); len(gs) > 0 {
		return gs[0]
	}
	return ipp.AttrGroup{}
}

// capture returns a client for a fake CUPS that answers with jobID in a job
// group (or a bare success when jobID is 0) and records the request.
func capture(t *testing.T, jobID int) (*Client, *seen) {
	t.Helper()
	got := &seen{}
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.path = r.URL.EscapedPath()
		got.op = uint16(body[2])<<8 | uint16(body[3])
		got.req, _ = ipp.Decode(body)
		if i := strings.IndexByte(string(body), byte(ipp.GroupEnd)); i >= 0 {
			got.doc = string(body[i+1:])
		}
		m := ipptest.NewWire(0)
		if jobID != 0 {
			m.Group(ipp.GroupJob).Num(ipp.TagInteger, "job-id", jobID)
		}
		reply(w, m)
	})
	return New(sock, "mika"), got
}

func TestHoldSendsHoldUntil(t *testing.T) {
	c, got := capture(t, 0)
	if err := c.Hold(context.Background(), 12); err != nil {
		t.Fatal(err)
	}
	if got.op != ipp.OpHoldJob || got.path != "/jobs/" {
		t.Errorf("op %#x on %q", got.op, got.path)
	}
	op := got.group(ipp.GroupOperation)
	if op.String("job-hold-until") != "indefinite" {
		t.Errorf("operation group lacks job-hold-until indefinite: %+v", op)
	}
	if op.String("job-uri") != "ipp://localhost/jobs/12" {
		t.Errorf("job-uri = %q", op.String("job-uri"))
	}
	if op.String("requesting-user-name") != "mika" {
		t.Errorf("requesting-user-name = %q", op.String("requesting-user-name"))
	}
}

func TestJobOpsUseTheirOperation(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Client) error
		op   uint16
	}{
		{"release", func(c *Client) error { return c.Release(context.Background(), 7) }, ipp.OpReleaseJob},
		{"cancel", func(c *Client) error { return c.Cancel(context.Background(), 7) }, ipp.OpCancelJob},
		{"restart", func(c *Client) error { return c.Restart(context.Background(), 7) }, ipp.OpRestartJob},
	} {
		c, got := capture(t, 0)
		if err := tc.call(c); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got.op != tc.op {
			t.Errorf("%s: op %#x, want %#x", tc.name, got.op, tc.op)
		}
		op := got.group(ipp.GroupOperation)
		if op.String("job-uri") != "ipp://localhost/jobs/7" {
			t.Errorf("%s: job-uri = %q", tc.name, op.String("job-uri"))
		}
		if op.String("job-hold-until") != "" {
			t.Errorf("%s sends job-hold-until", tc.name)
		}
	}
}

func TestJobOpReturnsTheCUPSMessage(t *testing.T) {
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, ipptest.NewWire(0x0407).Str(ipp.TagText, "status-message", "Job #7 is already canceled - can't cancel."))
	})
	err := New(sock, "mika").Cancel(context.Background(), 7)
	if err == nil || err.Error() != "Job #7 is already canceled - can't cancel." {
		t.Errorf("err = %v", err)
	}
}

func TestMoveSendsTheTargetQueue(t *testing.T) {
	c, got := capture(t, 0)
	if err := c.Move(context.Background(), 12, "Büro Laser"); err != nil {
		t.Fatal(err)
	}
	if got.op != ipp.OpCUPSMoveJob {
		t.Errorf("op %#x", got.op)
	}
	if u := got.group(ipp.GroupJob).String("job-printer-uri"); u != "ipp://localhost/printers/B%C3%BCro%20Laser" {
		t.Errorf("job-printer-uri = %q", u)
	}
	if err := c.Move(context.Background(), 12, ""); err == nil {
		t.Error("Move to no queue succeeded")
	}
}

func TestPrintSendsOnlyTheOptionsThatAreSet(t *testing.T) {
	c, got := capture(t, 31)
	id, err := c.Print(context.Background(), "Fußball Drucker", "Karte.pdf", "application/pdf",
		JobOptions{Sides: "two-sided-long-edge", Copies: 2, Hold: true}, strings.NewReader("%PDF-doc"))
	if err != nil {
		t.Fatal(err)
	}
	if id != 31 {
		t.Errorf("id = %d", id)
	}
	if got.op != ipp.OpPrintJob || got.path != "/printers/Fu%C3%9Fball%20Drucker" {
		t.Errorf("op %#x on %q", got.op, got.path)
	}
	if got.doc != "%PDF-doc" {
		t.Errorf("document = %q", got.doc)
	}
	op, job := got.group(ipp.GroupOperation), got.group(ipp.GroupJob)
	if op.String("printer-uri") != "ipp://localhost/printers/Fu%C3%9Fball%20Drucker" ||
		op.String("job-name") != "Karte.pdf" || op.String("document-format") != "application/pdf" {
		t.Errorf("operation group = %+v", op)
	}
	if n, _ := job.Int("copies"); n != 2 {
		t.Errorf("copies = %d", n)
	}
	if job.String("sides") != "two-sided-long-edge" || job.String("job-hold-until") != "indefinite" {
		t.Errorf("job group = %+v", job)
	}
	for _, unset := range []string{"print-color-mode", "media", "print-quality"} {
		if len(job.Strings(unset))+len(job.Ints(unset)) != 0 {
			t.Errorf("%s was sent although it is zero", unset)
		}
	}
}

func TestPrintSendsEveryOption(t *testing.T) {
	c, got := capture(t, 1)
	_, err := c.Print(context.Background(), "q", "n", "image/png",
		JobOptions{Sides: "one-sided", Color: "monochrome", Media: "iso_a4_210x297mm", Copies: 3, Quality: 5},
		strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	job := got.group(ipp.GroupJob)
	if job.String("print-color-mode") != "monochrome" || job.String("media") != "iso_a4_210x297mm" {
		t.Errorf("job group = %+v", job)
	}
	if n, _ := job.Int("print-quality"); n != 5 {
		t.Errorf("print-quality = %d", n)
	}
	if job.String("job-hold-until") != "" {
		t.Error("job-hold-until sent without Hold")
	}
}

func TestPrintClipsALongName(t *testing.T) {
	c, got := capture(t, 1)
	long := strings.Repeat("ü", 200) // 400 bytes
	if _, err := c.Print(context.Background(), "q", long, "text/plain", JobOptions{}, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	name := got.group(ipp.GroupOperation).String("job-name")
	if len(name) > 255 || strings.Trim(name, "ü") != "" {
		t.Errorf("job-name = %d bytes, %q", len(name), name[len(name)-4:])
	}
}

// An invalid byte becomes U+FFFD instead of cutting the name off there, and
// a short name with one is made valid too.
func TestClipNameWithInvalidUTF8(t *testing.T) {
	long := "Brief\xffan " + strings.Repeat("ü", 150) // 309 bytes, invalid at byte 5
	got := clipName(long)
	if !utf8.ValidString(got) || len(got) > 255 || len(got) < 252 || !strings.HasPrefix(got, "Brief\uFFFDan ü") {
		t.Errorf("clipName(long) = %d bytes, valid %v, %q…", len(got), utf8.ValidString(got), got[:min(len(got), 16)])
	}
	if got := clipName("Brief\xffan"); got != "Brief\uFFFDan" {
		t.Errorf("clipName(short) = %q, want %q", got, "Brief\uFFFDan")
	}
	if got := clipName(strings.Repeat("a", 254) + "ü"); got != strings.Repeat("a", 254) {
		t.Errorf("clipName cut inside ü: %q", got[250:])
	}
}

func TestPrintWithoutJobIDFails(t *testing.T) {
	c, _ := capture(t, 0)
	if _, err := c.Print(context.Background(), "q", "n", "text/plain", JobOptions{}, strings.NewReader("x")); err == nil {
		t.Error("a reply without a job id counted as success")
	}
}

func TestPrintTestPage(t *testing.T) {
	c, got := capture(t, 9)
	id, err := c.PrintTestPage(context.Background(), "Virtual-MFC", true)
	if err != nil || id != 9 {
		t.Fatalf("id %d, err %v", id, err)
	}
	const banner = "#PDF-BANNER\nTemplate default-testpage.pdf\nShow printer-name printer-info printer-location " +
		"printer-make-and-model printer-driver-name printer-driver-version paper-size imageable-area job-id options " +
		"time-at-creation time-at-processing\n\n"
	if got.doc != banner {
		t.Errorf("document = %q", got.doc)
	}
	op := got.group(ipp.GroupOperation)
	if op.String("document-format") != "application/vnd.cups-pdf-banner" || op.String("job-name") != "Test Page" {
		t.Errorf("operation group = %+v", op)
	}
	if got.group(ipp.GroupJob).String("job-hold-until") != "indefinite" {
		t.Error("held test page lacks job-hold-until")
	}
}
