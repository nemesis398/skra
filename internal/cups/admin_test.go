// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"context"
	"net/http"
	"testing"

	"github.com/nemesis398/skra/internal/ipp"
	"github.com/nemesis398/skra/internal/ipp/ipptest"
)

func ptr[T any](v T) *T { return &v }

func attrNames(g ipp.AttrGroup) []string {
	var out []string
	for _, a := range g.Attrs {
		out = append(out, a.Name)
	}
	return out
}

func TestModifySendsOnlyChanges(t *testing.T) {
	c, got := capture(t, 0)
	if err := c.Modify(context.Background(), "Virtual-MFC", Changes{Sides: ptr("one-sided")}); err != nil {
		t.Fatal(err)
	}
	if got.op != ipp.OpCUPSAddModifyPrinter || got.path != "/admin/" {
		t.Errorf("op %#x on %q", got.op, got.path)
	}
	if u := got.group(ipp.GroupOperation).String("printer-uri"); u != "ipp://localhost/printers/Virtual-MFC" {
		t.Errorf("printer-uri = %q", u)
	}
	pg := got.group(ipp.GroupPrinter)
	if names := attrNames(pg); len(names) != 1 || names[0] != "sides-default" || pg.String("sides-default") != "one-sided" {
		t.Errorf("printer group = %+v, want exactly sides-default one-sided", pg.Attrs)
	}
	if len(pg.Attrs) > 0 && pg.Attrs[0].Tag != ipp.TagKeyword {
		t.Errorf("sides-default tag = %#x, want keyword", pg.Attrs[0].Tag)
	}
	if len(got.group(ipp.GroupJob).Attrs) != 0 {
		t.Error("Modify sent a job group")
	}
}

func TestModifyMapsEveryField(t *testing.T) {
	c, got := capture(t, 0)
	err := c.Modify(context.Background(), "Büro Laser", Changes{
		Info: ptr("Brother"), Location: ptr("Study"), DeviceURI: ptr("ipp://printer.local/ipp/print"),
		Sides: ptr("two-sided-long-edge"), Color: ptr("monochrome"), Media: ptr("iso_a4_210x297mm"),
		ErrorPolicy: ptr("abort-job"), Shared: ptr(true), Quality: ptr(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/admin/" || got.group(ipp.GroupOperation).String("printer-uri") != "ipp://localhost/printers/B%C3%BCro%20Laser" {
		t.Errorf("path %q, operation group %+v", got.path, got.group(ipp.GroupOperation))
	}
	pg := got.group(ipp.GroupPrinter)
	want := map[string]struct {
		tag ipp.Tag
		val string
	}{
		"printer-info":             {ipp.TagText, "Brother"},
		"printer-location":         {ipp.TagText, "Study"},
		"device-uri":               {ipp.TagURI, "ipp://printer.local/ipp/print"},
		"sides-default":            {ipp.TagKeyword, "two-sided-long-edge"},
		"print-color-mode-default": {ipp.TagKeyword, "monochrome"},
		"media-default":            {ipp.TagKeyword, "iso_a4_210x297mm"},
		"printer-error-policy":     {ipp.TagName, "abort-job"},
	}
	for _, a := range pg.Attrs {
		w, ok := want[a.Name]
		switch {
		case ok:
			if a.Tag != w.tag || pg.String(a.Name) != w.val {
				t.Errorf("%s = tag %#x %v, want tag %#x %q", a.Name, a.Tag, a.Values, w.tag, w.val)
			}
			delete(want, a.Name)
		case a.Name == "printer-is-shared":
			if b, _ := pg.Bool(a.Name); a.Tag != ipp.TagBoolean || !b {
				t.Errorf("printer-is-shared = %+v", a)
			}
		case a.Name == "print-quality-default":
			if n, _ := pg.Int(a.Name); a.Tag != ipp.TagEnum || n != 5 {
				t.Errorf("print-quality-default = %+v, want enum 5", a)
			}
		default:
			t.Errorf("unexpected attribute %s", a.Name)
		}
	}
	if len(want) != 0 || len(pg.Attrs) != 9 {
		t.Errorf("missing %v, %d attributes", want, len(pg.Attrs))
	}
}

func TestModifySendsAFalseSharedAndAnEmptyLocation(t *testing.T) {
	c, got := capture(t, 0)
	if err := c.Modify(context.Background(), "q", Changes{Shared: ptr(false), Location: ptr("")}); err != nil {
		t.Fatal(err)
	}
	pg := got.group(ipp.GroupPrinter)
	if b, ok := pg.Bool("printer-is-shared"); !ok || b {
		t.Errorf("printer-is-shared = %v, %v", b, ok)
	}
	if len(pg.Attrs) != 2 {
		t.Errorf("printer group = %+v, want shared and location", pg.Attrs)
	}
}

func TestModifyWithNoChangeSendsNothing(t *testing.T) {
	c, got := capture(t, 0)
	if err := c.Modify(context.Background(), "q", Changes{}); err != nil {
		t.Fatal(err)
	}
	if got.op != 0 || got.path != "" {
		t.Errorf("an empty Changes still sent op %#x to %q", got.op, got.path)
	}
}

func TestQueueOpsUseTheirOperation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func(*Client) error
		op   uint16
	}{
		{"delete", func(c *Client) error { return c.Delete(ctx, "Fußball") }, ipp.OpCUPSDeletePrinter},
		{"default", func(c *Client) error { return c.SetDefault(ctx, "Fußball") }, ipp.OpCUPSSetDefault},
		{"accept", func(c *Client) error { return c.Accept(ctx, "Fußball", true) }, ipp.OpCUPSAcceptJobs},
		{"reject", func(c *Client) error { return c.Accept(ctx, "Fußball", false) }, ipp.OpCUPSRejectJobs},
		{"pause", func(c *Client) error { return c.Pause(ctx, "Fußball", true) }, ipp.OpPausePrinter},
		{"resume", func(c *Client) error { return c.Pause(ctx, "Fußball", false) }, ipp.OpResumePrinter},
		{"cancel all", func(c *Client) error { return c.CancelAll(ctx, "Fußball") }, ipp.OpCancelJobs},
	} {
		c, got := capture(t, 0)
		if err := tc.call(c); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got.op != tc.op || got.path != "/admin/" {
			t.Errorf("%s: op %#x on %q, want %#x on /admin/", tc.name, got.op, got.path, tc.op)
		}
		if u := got.group(ipp.GroupOperation).String("printer-uri"); u != "ipp://localhost/printers/Fu%C3%9Fball" {
			t.Errorf("%s: printer-uri = %q", tc.name, u)
		}
		if got.group(ipp.GroupOperation).String("requesting-user-name") != "mika" {
			t.Errorf("%s: no requesting-user-name", tc.name)
		}
		if len(got.group(ipp.GroupPrinter).Attrs)+len(got.group(ipp.GroupJob).Attrs) != 0 {
			t.Errorf("%s sent attributes beyond the operation group", tc.name)
		}
	}
}

func TestQueueOpReturnsTheCUPSMessage(t *testing.T) {
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, ipptest.NewWire(0x0406).Str(ipp.TagText, "status-message", "The printer or class does not exist."))
	})
	err := New(sock, "mika").Delete(context.Background(), "gone")
	if err == nil || err.Error() != "The printer or class does not exist." {
		t.Errorf("err = %v", err)
	}
}

func TestMoveAllSendsTheSourceAndTheTarget(t *testing.T) {
	c, got := capture(t, 0)
	if err := c.MoveAll(context.Background(), "Büro Laser", "Fußball"); err != nil {
		t.Fatal(err)
	}
	if got.op != ipp.OpCUPSMoveJob {
		t.Errorf("op %#x", got.op)
	}
	if u := got.group(ipp.GroupOperation).String("printer-uri"); u != "ipp://localhost/printers/B%C3%BCro%20Laser" {
		t.Errorf("printer-uri = %q", u)
	}
	if got.group(ipp.GroupOperation).String("job-uri") != "" {
		t.Error("MoveAll names a single job")
	}
	if u := got.group(ipp.GroupJob).String("job-printer-uri"); u != "ipp://localhost/printers/Fu%C3%9Fball" {
		t.Errorf("job-printer-uri = %q", u)
	}
	if err := c.MoveAll(context.Background(), "a", ""); err == nil {
		t.Error("MoveAll to no queue succeeded")
	}
	if err := c.MoveAll(context.Background(), "a", "a"); err == nil {
		t.Error("MoveAll to the same queue succeeded")
	}
}

func TestCommandSendsACommandJob(t *testing.T) {
	c, got := capture(t, 41)
	id, err := c.Command(context.Background(), "Virtual-MFC", "Clean")
	if err != nil || id != 41 {
		t.Fatalf("id %d, err %v", id, err)
	}
	if got.op != ipp.OpPrintJob || got.path != "/printers/Virtual-MFC" {
		t.Errorf("op %#x on %q", got.op, got.path)
	}
	if got.doc != "#CUPS-COMMAND\nClean\n" {
		t.Errorf("document = %q", got.doc)
	}
	if f := got.group(ipp.GroupOperation).String("document-format"); f != "application/vnd.cups-command" {
		t.Errorf("document-format = %q", f)
	}
}

func TestCommandRefusesAnEmptyOrMultiLineCommand(t *testing.T) {
	c, got := capture(t, 41)
	for _, cmd := range []string{"", "Clean\nAbortJob", "Clean\r"} {
		if _, err := c.Command(context.Background(), "q", cmd); err == nil {
			t.Errorf("command %q was sent", cmd)
		}
	}
	if got.op != 0 {
		t.Error("a bad command reached CUPS")
	}
}

// A queue with no maintenance commands lists the keyword "none", which is
// not a command to offer.
func TestPrinterCommandsLeaveOutNone(t *testing.T) {
	for _, tc := range []struct {
		sent []string
		want int
	}{{[]string{"none"}, 0}, {[]string{"Clean", "none"}, 1}, {[]string{"Clean", "ReportLevels"}, 2}} {
		sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
			reply(w, ipptest.NewWire(0).Group(ipp.GroupPrinter).
				Str(ipp.TagName, "printer-name", "q").Str(ipp.TagKeyword, "printer-commands", tc.sent...))
		})
		p, err := New(sock, "mika").Printer(context.Background(), "q")
		if err != nil || len(p.Commands) != tc.want {
			t.Errorf("sent %v: commands %v, err %v, want %d", tc.sent, p.Commands, err, tc.want)
		}
	}
}
