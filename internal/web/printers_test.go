// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/store"
)

func (f *fakeCUPS) Modify(_ context.Context, name string, ch cups.Changes) error {
	f.mu.Lock()
	f.changes = append(f.changes, ch)
	f.mu.Unlock()
	return f.act("modify " + name)
}
func (f *fakeCUPS) Delete(_ context.Context, name string) error { return f.act("delete " + name) }
func (f *fakeCUPS) SetDefault(_ context.Context, name string) error {
	return f.act("default " + name)
}
func (f *fakeCUPS) Accept(_ context.Context, name string, on bool) error {
	return f.act(fmt.Sprintf("accept %s %t", name, on))
}
func (f *fakeCUPS) Pause(_ context.Context, name string, on bool) error {
	return f.act(fmt.Sprintf("pause %s %t", name, on))
}
func (f *fakeCUPS) CancelAll(_ context.Context, name string) error {
	return f.act("cancel-all " + name)
}
func (f *fakeCUPS) MoveAll(_ context.Context, from, to string) error {
	return f.act(fmt.Sprintf("move-all %s %s", from, to))
}
func (f *fakeCUPS) Command(_ context.Context, printer, command string) (int, error) {
	return 51, f.act(fmt.Sprintf("command %s %s", printer, command))
}

func (f *fakeCUPS) modified() []cups.Changes {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cups.Changes(nil), f.changes...)
}

// full is the Brother with everything the settings panel shows.
func full() cups.Printer {
	p := brother()
	p.Accepting, p.Shared = true, true
	p.SidesSupported = []string{"one-sided", "two-sided-long-edge", "two-sided-short-edge"}
	p.SidesDefault = "one-sided"
	p.ColorSupported = []string{"monochrome", "color"}
	p.ColorDefault = "color"
	p.QualitySupported, p.QualityDefault = []int{3, 4, 5}, 4
	p.MediaSupported = []string{"iso_a4_210x297mm", "iso_a5_148x210mm", "na_letter_8.5x11in", "custom_min_75x127mm"}
	p.MediaDefault = "iso_a4_210x297mm"
	p.ErrorPolicies = []string{"abort-job", "retry-current-job", "retry-job", "stop-printer"}
	p.ErrorPolicy = "stop-printer"
	p.Commands = []string{"Clean", "PrintSelfTestPage", "ReportLevels", "Frobnicate"}
	return p
}

func hall() cups.Printer {
	return cups.Printer{Name: "Hall", Info: "Hall Laser", DeviceURI: "ipp://hall.local/ipp/print", State: 3, Accepting: true}
}

// queuesFake is two queues, two waiting jobs on the first.
func queuesFake() *fakeCUPS {
	return &fakeCUPS{
		printers: []cups.Printer{hall(), full()},
		def:      "Virtual-MFC",
		active: []cups.Job{
			{ID: 12, Name: "Invoice.pdf", Printer: "Virtual-MFC", State: 3},
			{ID: 13, Name: "Held.pdf", Printer: "Virtual-MFC", State: 4},
			{ID: 14, Name: "Elsewhere.pdf", Printer: "Hall", State: 3},
		},
	}
}

// same is the settings form of a queue as it stands: posting it changes nothing.
func same(p cups.Printer) url.Values {
	v := url.Values{
		"info": {p.Info}, "location": {p.Location}, "address": {p.DeviceURI},
		"state": {"running"}, "accept": {"accepted"}, "shared": {"no"},
		"sides": {p.SidesDefault}, "color": {p.ColorDefault}, "quality": {fmt.Sprint(p.QualityDefault)},
		"media": {p.MediaDefault}, "policy": {p.ErrorPolicy},
	}
	if p.State == 5 {
		v.Set("state", "paused")
	}
	if !p.Accepting {
		v.Set("accept", "rejected")
	}
	if p.Shared {
		v.Set("shared", "yes")
	}
	return v
}

func events(t *testing.T, s *Server, n int) []store.Event {
	t.Helper()
	ev, err := s.deps.Store.Recent(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestDeclaredQueueGuards(t *testing.T) {
	const refusal = "Virtual-MFC is declared in ravnix; remove it there."
	f := queuesFake()
	s, h := actionServer(t, f, "Virtual-MFC")

	rec := postTo(s, h, "/printers/Virtual-MFC/delete", nil)
	if rec.Code != 403 || rec.Body.String() != refusal {
		t.Errorf("delete: status %d, body %q", rec.Code, rec.Body.String())
	}
	form := same(full())
	form.Set("address", "ipp://elsewhere.local/ipp/print")
	rec = postTo(s, h, "/printers/Virtual-MFC/settings", form)
	if rec.Code != 403 || rec.Body.String() != refusal {
		t.Errorf("address change: status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.did(); len(got) != 0 {
		t.Errorf("CUPS was asked %v although both were refused", got)
	}

	// The same address as before, with another change, goes through.
	form = same(full())
	form.Set("sides", "two-sided-long-edge")
	if rec = postTo(s, h, "/printers/Virtual-MFC/settings", form); rec.Code != 200 {
		t.Fatalf("sides only: status %d, body %q", rec.Code, rec.Body.String())
	}
	ch := f.modified()
	if len(ch) != 1 || ch[0].Sides == nil || *ch[0].Sides != "two-sided-long-edge" || ch[0].DeviceURI != nil {
		t.Errorf("Modify got %+v", ch)
	}

	// A queue that ravnix does not declare can be re-addressed and removed.
	hf := same(hall())
	hf.Set("address", "ipp://hall2.local/ipp/print")
	if rec = postTo(s, h, "/printers/Hall/settings", hf); rec.Code != 200 {
		t.Errorf("Hall address: status %d, body %q", rec.Code, rec.Body.String())
	}
	if rec = postTo(s, h, "/printers/Hall/delete", nil); rec.Code != 200 {
		t.Errorf("Hall delete: status %d, body %q", rec.Code, rec.Body.String())
	}

	// The form shows the address read-only and offers no remove button.
	body := get(h, "/printers/Virtual-MFC/settings").Body.String()
	if !regexp.MustCompile(`<input[^>]*name="address"[^>]*readonly`).MatchString(body) || !strings.Contains(body, "declared in ravnix") {
		t.Errorf("the address is not read-only with its reason:\n%s", body)
	}
	if strings.Contains(body, `data-act="remove"`) {
		t.Error("the settings form of a declared queue has a remove button")
	}
	body = get(h, "/printers/Hall/settings").Body.String()
	if regexp.MustCompile(`<input[^>]*name="address"[^>]*readonly`).MatchString(body) || strings.Contains(body, "declared in ravnix") {
		t.Error("an undeclared queue shows its address read-only")
	}
	page := get(h, "/printers").Body.String()
	if strings.Contains(page, `data-act="remove" data-q="Virtual-MFC"`) || !strings.Contains(page, `data-act="remove" data-q="Hall"`) {
		t.Error("only the undeclared queue should carry a remove button in the table")
	}
	if !strings.Contains(page, "declared in ravnix") {
		t.Error("the table does not say which queue is declared")
	}
}

func TestDeleteNeedsConfirm(t *testing.T) {
	removeButton := func(page, name string) string {
		m := regexp.MustCompile(`<button[^>]*data-act="remove" data-q="` + regexp.QuoteMeta(name) + `"[^>]*>`).FindString(page)
		if m == "" {
			t.Fatalf("no remove button for %s in:\n%s", name, page)
		}
		return m
	}
	f := queuesFake()
	_, h := actionServer(t, f)
	page := get(h, "/printers").Body.String()

	two := removeButton(page, "Virtual-MFC")
	for _, want := range []string{`data-confirm-title="Remove the queue Virtual-MFC?"`, `data-confirm-label="Remove Virtual-MFC"`,
		"Its 2 waiting jobs are canceled with it.", "The printer itself is not touched"} {
		if !strings.Contains(two, want) {
			t.Errorf("remove button lacks %q: %s", want, two)
		}
	}
	if !strings.Contains(two, "data-confirm=") {
		t.Error("remove does not ask first")
	}
	one := removeButton(page, "Hall")
	if !strings.Contains(one, "Its 1 waiting job is canceled with it.") {
		t.Errorf("one job: %s", one)
	}

	f.set(func(f *fakeCUPS) { f.active = nil })
	_, h = actionServer(t, f)
	none := removeButton(get(h, "/printers").Body.String(), "Hall")
	if !strings.Contains(none, "data-confirm=") || strings.Contains(none, "waiting job") {
		t.Errorf("no jobs: the button still asks, and names no jobs: %s", none)
	}
}

func TestQueueActions(t *testing.T) {
	for _, tc := range []struct {
		path, want, event, did string
	}{
		{"/printers/Hall/default", "Hall is the default printer now.", "printer.default", "default Hall"},
		{"/printers/Hall/delete", "Removed the queue Hall.", "printer.delete", "delete Hall"},
		{"/printers/Hall/accept", "Hall accepts new jobs again.", "printer.accept", "accept Hall true"},
		{"/printers/Hall/reject", "Hall turns new jobs away now.", "printer.reject", "accept Hall false"},
		{"/printers/Hall/pause", "Paused the queue of Hall.", "printer.pause", "pause Hall true"},
		{"/printers/Hall/resume", "Resumed the queue of Hall.", "printer.resume", "pause Hall false"},
	} {
		f := queuesFake()
		s, h := actionServer(t, f)
		rec := postTo(s, h, tc.path, nil)
		if rec.Code != 200 || rec.Body.String() != tc.want {
			t.Errorf("%s: status %d, body %q, want %q", tc.path, rec.Code, rec.Body.String(), tc.want)
		}
		if got := f.did(); len(got) != 1 || got[0] != tc.did {
			t.Errorf("%s: CUPS was asked %v, want %q", tc.path, got, tc.did)
		}
		if ev := lastEvent(t, s); ev.Action != tc.event || ev.Target != "Hall" || !ev.OK || ev.Actor != "mika" {
			t.Errorf("%s: event = %+v", tc.path, ev)
		}

		f.set(func(f *fakeCUPS) { f.actErr = errors.New("The printer or class does not exist.") })
		rec = postTo(s, h, tc.path, nil)
		if rec.Code != http.StatusBadGateway || rec.Body.String() != "The printer or class does not exist." {
			t.Errorf("%s refused: status %d, body %q", tc.path, rec.Code, rec.Body.String())
		}
		if ev := lastEvent(t, s); ev.Action != tc.event || ev.OK || !strings.Contains(ev.Detail, "does not exist") {
			t.Errorf("%s refused: event = %+v", tc.path, ev)
		}
	}
}

func TestQueueActionsTakeAnAwkwardName(t *testing.T) {
	f := queuesFake()
	f.printers = append(f.printers, cups.Printer{Name: "Fußball-Drucker", State: 3, Accepting: true})
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/printers/Fu%C3%9Fball-Drucker/pause", nil)
	if rec.Code != 200 || rec.Body.String() != "Paused the queue of Fußball-Drucker." {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.did(); len(got) != 1 || got[0] != "pause Fußball-Drucker true" {
		t.Errorf("CUPS was asked %v", got)
	}
}

func TestCancelAllNamesTheCount(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/printers/Virtual-MFC/cancel-all", nil)
	if rec.Code != 200 || rec.Body.String() != "Canceled 2 jobs on Brother." {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.did(); len(got) != 1 || got[0] != "cancel-all Virtual-MFC" {
		t.Errorf("CUPS was asked %v", got)
	}
	if ev := lastEvent(t, s); ev.Action != "printer.cancelall" || ev.Target != "Virtual-MFC" || ev.Detail != "2 jobs" || !ev.OK {
		t.Errorf("event = %+v", ev)
	}

	page := get(h, "/printers").Body.String()
	btn := regexp.MustCompile(`<button[^>]*data-act="cancel-all" data-q="Virtual-MFC"[^>]*>`).FindString(page)
	if !strings.Contains(btn, "data-confirm-title=") || !strings.Contains(btn, "2 jobs") || !strings.Contains(btn, "data-confirm-label=") || !strings.Contains(btn, `data-keep="Keep them"`) {
		t.Errorf("cancel all does not ask first, or does not name the count: %q", btn)
	}
	if hallBtn := regexp.MustCompile(`<button[^>]*data-act="cancel-all" data-q="Hall"[^>]*>`).FindString(page); !strings.Contains(hallBtn, "1 job") {
		t.Errorf("the Hall row does not name its job: %q", hallBtn)
	}
	f.set(func(f *fakeCUPS) { f.active = nil })
	if strings.Contains(get(h, "/printers").Body.String(), `data-act="cancel-all"`) {
		t.Error("cancel all is offered for queues with no jobs")
	}
}

func TestMoveAll(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/printers/Virtual-MFC/move-all", url.Values{"to": {"Hall"}})
	if rec.Code != 200 || rec.Body.String() != "Moved 2 jobs from Brother to Hall." {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.did(); len(got) != 1 || got[0] != "move-all Virtual-MFC Hall" {
		t.Errorf("CUPS was asked %v", got)
	}
	if ev := lastEvent(t, s); ev.Action != "printer.moveall" || ev.Target != "Virtual-MFC" || ev.Detail != "to Hall" || !ev.OK {
		t.Errorf("event = %+v", ev)
	}

	f.set(func(f *fakeCUPS) { f.actErr = errors.New("The printer or class does not exist.") })
	rec = postTo(s, h, "/printers/Virtual-MFC/move-all", url.Values{"to": {"Nowhere"}})
	if rec.Code != 502 {
		t.Errorf("refused: status %d", rec.Code)
	}
	if ev := lastEvent(t, s); ev.Action != "printer.moveall" || ev.OK || !strings.HasPrefix(ev.Detail, "to Nowhere") {
		t.Errorf("event after refusal = %+v", ev)
	}

	f.set(func(f *fakeCUPS) { f.actErr = nil; f.acts = nil })
	for _, to := range []string{"", "  ", "Virtual-MFC"} {
		if rec = postTo(s, h, "/printers/Virtual-MFC/move-all", url.Values{"to": {to}}); rec.Code != 400 || rec.Body.Len() == 0 {
			t.Errorf("to %q: status %d, body %q, want 400 with a message", to, rec.Code, rec.Body.String())
		}
	}
	if got := f.did(); len(got) != 0 {
		t.Errorf("CUPS was asked %v for bad requests", got)
	}
}

func TestMoveAllIsOfferedFromTwoQueues(t *testing.T) {
	f := queuesFake()
	_, h := actionServer(t, f)
	btn := regexp.MustCompile(`<button[^>]*data-act="move-all" data-q="Virtual-MFC"[^>]*>`).FindString(get(h, "/printers").Body.String())
	if btn == "" || !strings.Contains(btn, `data-targets="[{&#34;name&#34;:&#34;Hall&#34;,&#34;label&#34;:&#34;Hall&#34;}]"`) {
		t.Errorf("move all with the other queue as its only target is missing: %q", btn)
	}
	f.set(func(f *fakeCUPS) { f.printers = f.printers[1:] })
	_, h = actionServer(t, f)
	if strings.Contains(get(h, "/printers").Body.String(), `data-act="move-all"`) {
		t.Error("move all is offered with one queue")
	}
}

func TestCommand(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/printers/Virtual-MFC/command", url.Values{"command": {"Clean"}})
	if rec.Code != 200 || rec.Body.String() != `Sent "clean print heads" to Brother as job 51.` {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.did(); len(got) != 1 || got[0] != "command Virtual-MFC Clean" {
		t.Errorf("CUPS was asked %v", got)
	}
	if ev := lastEvent(t, s); ev.Action != "printer.command" || ev.Target != "Virtual-MFC" || !ev.OK || !strings.Contains(ev.Detail, "Clean") {
		t.Errorf("event = %+v", ev)
	}

	f.set(func(f *fakeCUPS) { f.acts = nil })
	for _, c := range []string{"", "Reboot", "clean", "Clean\nAbortJob", "none"} {
		if rec = postTo(s, h, "/printers/Virtual-MFC/command", url.Values{"command": {c}}); rec.Code != 400 || rec.Body.Len() == 0 {
			t.Errorf("command %q: status %d, body %q, want 400 with a message", c, rec.Code, rec.Body.String())
		}
	}
	if rec = postTo(s, h, "/printers/Hall/command", url.Values{"command": {"Clean"}}); rec.Code != 400 {
		t.Errorf("a queue with no commands: status %d, want 400", rec.Code)
	}
	if rec = postTo(s, h, "/printers/Nowhere/command", url.Values{"command": {"Clean"}}); rec.Code != 404 {
		t.Errorf("an unknown queue: status %d, want 404", rec.Code)
	}
	if got := f.did(); len(got) != 0 {
		t.Errorf("CUPS was asked %v for commands the printer does not list", got)
	}
}

func TestMaintenanceButtonsFollowThePrintersList(t *testing.T) {
	f := queuesFake()
	_, h := actionServer(t, f)
	body := get(h, "/printers/Virtual-MFC/settings").Body.String()
	for _, want := range []string{
		`data-command="Clean"`, ">clean print heads<", `data-command="PrintSelfTestPage"`, ">print a self-test page<",
		`data-command="ReportLevels"`, ">ask for ink levels<", `data-command="Frobnicate"`, ">Frobnicate<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel lacks %q", want)
		}
	}
	if n := strings.Count(body, `data-act="command"`); n != 4 {
		t.Errorf("%d maintenance buttons, want 4", n)
	}
	if hallPanel := get(h, "/printers/Hall/settings").Body.String(); strings.Contains(hallPanel, `data-act="command"`) {
		t.Error("a queue with no commands has maintenance buttons")
	}
}

func TestSettingsPanelRows(t *testing.T) {
	f := queuesFake()
	_, h := actionServer(t, f)
	rec := get(h, "/printers/Virtual-MFC/settings")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("status %d, Content-Type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, want := range []string{
		">description<", ">location<", ">address<", ">queue<", ">new jobs<", ">shared on the network<", ">default sides<",
		">default color<", ">default quality<", ">default paper<", ">when printing fails<",
		">running<", ">paused<", ">accepted<", ">rejected<",
		">1-sided<", ">2-sided, long edge<", ">2-sided, short edge<", ">gray<", ">color<", ">draft<", ">normal<", ">high<",
		">A4 · 210 × 297mm<", ">A5 · 148 × 210mm<", ">Letter · 8.5 × 11in<",
		">try the job again later<", ">try the job again now<", ">drop the job, carry on<", ">pause the queue<",
		`value="Brother MFC-L2710DW"`, `value="Study"`, `value="ipp://printer.local/ipp/print"`,
		`value="iso_a4_210x297mm" selected`, `value="stop-printer" selected`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel lacks %q", want)
		}
	}
	if strings.Contains(body, "custom_min") {
		t.Error("the panel offers a custom paper size")
	}
	on := regexp.MustCompile(`<button type="button" class="on" data-pick="([^"]*)"`).FindAllStringSubmatch(body, -1)
	var picked []string
	for _, m := range on {
		picked = append(picked, m[1])
	}
	if got := strings.Join(picked, " "); got != "running accepted yes one-sided color 4" {
		t.Errorf("selected choices = %q", got)
	}

	// One choice is no choice: those rows stay out.
	f.set(func(f *fakeCUPS) {
		f.printers[1].SidesSupported = []string{"one-sided"}
		f.printers[1].QualitySupported = nil
		f.printers[1].MediaSupported = nil
	})
	body = get(h, "/printers/Virtual-MFC/settings").Body.String()
	for _, gone := range []string{">default sides<", ">default quality<", ">default paper<"} {
		if strings.Contains(body, gone) {
			t.Errorf("the panel still has %q", gone)
		}
	}
}

func TestSettingsPanelEscapesAndKeepsAnUnlistedDefault(t *testing.T) {
	f := queuesFake()
	f.printers[1].Info = `<b>x</b> "q"`
	f.printers[1].MediaDefault = "om_odd_1x1mm"
	_, h := actionServer(t, f)
	body := get(h, "/printers/Virtual-MFC/settings").Body.String()
	if strings.Contains(body, "<b>x</b>") || !strings.Contains(body, "&lt;b&gt;x&lt;/b&gt;") {
		t.Error("the description is not escaped")
	}
	if !strings.Contains(body, `value="om_odd_1x1mm" selected`) {
		t.Error("a default paper outside the list is dropped, so a save would change it")
	}
}

func TestSettingsPanelForAnUnknownQueue(t *testing.T) {
	_, h := actionServer(t, queuesFake())
	if rec := get(h, "/printers/Nowhere/settings"); rec.Code != 404 || rec.Body.Len() == 0 {
		t.Errorf("status %d, body %q, want 404 with a message", rec.Code, rec.Body.String())
	}
}

func TestSettingsPostSendsOnlyWhatChanged(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	form := same(full())
	form.Set("info", "  Study printer ")
	form.Set("color", "monochrome")
	form.Set("quality", "5")
	form.Set("shared", "no")
	rec := postTo(s, h, "/printers/Virtual-MFC/settings", form)
	if rec.Code != 200 || rec.Body.String() != "Saved Virtual-MFC: 4 settings." {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.did(); len(got) != 1 || got[0] != "modify Virtual-MFC" {
		t.Fatalf("CUPS was asked %v, want one modify", got)
	}
	ch := f.modified()[0]
	if ch.Info == nil || *ch.Info != "Study printer" || ch.Color == nil || *ch.Color != "monochrome" ||
		ch.Quality == nil || *ch.Quality != 5 || ch.Shared == nil || *ch.Shared {
		t.Errorf("Modify got %+v", ch)
	}
	if ch.Location != nil || ch.DeviceURI != nil || ch.Sides != nil || ch.Media != nil || ch.ErrorPolicy != nil {
		t.Errorf("Modify also got unchanged fields: %+v", ch)
	}
	if ev := lastEvent(t, s); ev.Action != "printer.modify" || ev.Target != "Virtual-MFC" || !ev.OK ||
		ev.Detail != "description, shared, color, quality" {
		t.Errorf("event = %+v", ev)
	}
}

func TestSettingsPostMapsEveryField(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	form := url.Values{
		"info": {"Brother"}, "location": {"Hall"}, "address": {"ipp://other.local/ipp/print"}, "shared": {"no"},
		"sides": {"two-sided-short-edge"}, "color": {"monochrome"}, "quality": {"3"}, "media": {"na_letter_8.5x11in"}, "policy": {"retry-job"},
	}
	if rec := postTo(s, h, "/printers/Virtual-MFC/settings", form); rec.Code != 200 {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	ch := f.modified()[0]
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	got := strings.Join([]string{str(ch.Info), str(ch.Location), str(ch.DeviceURI), str(ch.Sides), str(ch.Color), str(ch.Media), str(ch.ErrorPolicy)}, "|")
	if got != "Brother|Hall|ipp://other.local/ipp/print|two-sided-short-edge|monochrome|na_letter_8.5x11in|retry-job" {
		t.Errorf("Modify got %q", got)
	}
	if ch.Quality == nil || *ch.Quality != 3 || ch.Shared == nil || *ch.Shared {
		t.Errorf("quality and shared: %+v", ch)
	}
	_ = s
}

func TestSettingsPostMapsStateAndAcceptToTheirOwnCalls(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	form := same(full())
	form.Set("state", "paused")
	form.Set("accept", "rejected")
	rec := postTo(s, h, "/printers/Virtual-MFC/settings", form)
	if rec.Code != 200 || rec.Body.String() != "Saved Virtual-MFC: rejects new jobs, paused." {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := strings.Join(f.did(), "; "); got != "accept Virtual-MFC false; pause Virtual-MFC true" {
		t.Errorf("CUPS was asked %q", got)
	}
	if len(f.modified()) != 0 {
		t.Error("Modify was called although no setting changed")
	}
	ev := events(t, s, 2)
	if len(ev) != 2 || ev[0].Action != "printer.pause" || ev[1].Action != "printer.reject" || !ev[0].OK || !ev[1].OK {
		t.Errorf("events = %+v", ev)
	}

	// Back the other way, from a queue that is paused and rejecting.
	g := queuesFake()
	g.printers[1].State, g.printers[1].Accepting = 5, false
	s, h = actionServer(t, g)
	form = same(full())
	form.Set("state", "running")
	form.Set("accept", "accepted")
	if rec = postTo(s, h, "/printers/Virtual-MFC/settings", form); rec.Code != 200 {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := strings.Join(g.did(), "; "); got != "accept Virtual-MFC true; pause Virtual-MFC false" {
		t.Errorf("CUPS was asked %q", got)
	}
	ev = events(t, s, 2)
	if len(ev) != 2 || ev[0].Action != "printer.resume" || ev[1].Action != "printer.accept" {
		t.Errorf("events = %+v", ev)
	}
}

func TestSettingsPostWithNothingChanged(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/printers/Virtual-MFC/settings", func() url.Values { v := same(full()); v.Set("shared", "yes"); return v }())
	if rec.Code != 200 || rec.Body.String() != "Nothing changed." {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.did(); len(got) != 0 {
		t.Errorf("CUPS was asked %v", got)
	}
	if ev, _ := s.deps.Store.Recent(context.Background(), 1); len(ev) != 0 {
		t.Errorf("nothing changed, and yet %+v was logged", ev[0])
	}
}

func TestSettingsPostRejectsWhatThePrinterDoesNotOffer(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	for _, bad := range []struct{ field, value string }{
		{"sides", "sideways"}, {"sides", ""}, {"color", "rainbow"}, {"quality", "9"}, {"quality", "high"}, {"quality", ""},
		{"media", "iso_a0_841x1189mm"}, {"policy", "explode"}, {"state", "sleeping"}, {"accept", "maybe"}, {"shared", "perhaps"},
		{"address", "not an address"}, {"address", "ipp://"}, {"info", "two\nlines"}, {"location", strings.Repeat("x", 2000)},
	} {
		form := same(full())
		form.Set(bad.field, bad.value)
		if rec := postTo(s, h, "/printers/Virtual-MFC/settings", form); rec.Code != 400 || rec.Body.Len() == 0 {
			t.Errorf("%s=%.20q: status %d, body %q, want 400 with a message", bad.field, bad.value, rec.Code, rec.Body.String())
		}
	}
	if got := f.did(); len(got) != 0 || len(f.modified()) != 0 {
		t.Errorf("CUPS was asked %v for values it does not offer", got)
	}
	if rec := postTo(s, h, "/printers/Nowhere/settings", url.Values{"info": {"x"}}); rec.Code != 404 {
		t.Errorf("an unknown queue: status %d, want 404", rec.Code)
	}
}

func TestSettingsPostKeepsAPartlyBadFormFromSendingAnything(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f)
	form := same(full())
	form.Set("info", "Fine")
	form.Set("sides", "sideways")
	form.Set("state", "paused")
	if rec := postTo(s, h, "/printers/Virtual-MFC/settings", form); rec.Code != 400 {
		t.Fatalf("status %d", rec.Code)
	}
	if got := f.did(); len(got) != 0 {
		t.Errorf("CUPS was asked %v although the form had a bad value", got)
	}
}

func TestSettingsPostStopsAtTheFirstRefusal(t *testing.T) {
	f := queuesFake()
	f.actErr = errors.New("Forbidden")
	s, h := actionServer(t, f)
	form := same(full())
	form.Set("info", "New")
	form.Set("state", "paused")
	rec := postTo(s, h, "/printers/Virtual-MFC/settings", form)
	if rec.Code != 502 || rec.Body.String() != "Forbidden" {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.did(); len(got) != 1 {
		t.Errorf("CUPS was asked %v, want it to stop after the first refusal", got)
	}
	if ev := lastEvent(t, s); ev.Action != "printer.modify" || ev.OK {
		t.Errorf("event = %+v", ev)
	}
}

func TestPrinterRoutesNeedTheToken(t *testing.T) {
	f := queuesFake()
	_, h := actionServer(t, f)
	for _, act := range []string{"settings", "default", "delete", "accept", "reject", "pause", "resume", "cancel-all", "move-all", "command"} {
		req := httptest.NewRequest("POST", "/printers/Hall/"+act, nil)
		req.Host = "127.0.0.1:7631"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("POST %s without a token: status %d, want 403", act, rec.Code)
		}
	}
	if got := f.did(); len(got) != 0 {
		t.Errorf("CUPS was asked %v", got)
	}
}

func TestPrintersPageIsAFullPageWithTheTabBar(t *testing.T) {
	f := queuesFake()
	_, h := actionServer(t, f)
	rec := get(h, "/printers")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, headers %v", rec.Code, rec.Header())
	}
	page := rec.Body.String()
	for _, want := range []string{`<meta name="skra-token"`, `href="/"`, `href="/printers"`, `id="queues"`, `id="panel"`, `id="foot"`, `id="dialog"`,
		`<a class="tab on" id="tab-printers"`, `<script src="/static/skra.js">`} {
		if !strings.Contains(page, want) {
			t.Errorf("the printers page lacks %q", want)
		}
	}
	if strings.Contains(page, `id="drop"`) || strings.Contains(page, `id="queue"`) {
		t.Error("the printers page carries the overview")
	}
	over := get(h, "/").Body.String()
	if !strings.Contains(over, `<a class="tab on" id="tab-overview"`) || !strings.Contains(over, `href="/printers"`) || strings.Contains(over, `id="queues"`) {
		t.Error("the overview does not link the printers page, or shows its table")
	}
}

func TestQueueTable(t *testing.T) {
	f := queuesFake()
	f.printers[0].Accepting = false
	f.printers[1].Reasons = []string{"offline-report"}
	_, h := actionServer(t, f)
	page := get(h, "/printers").Body.String()
	for _, want := range []string{
		"<th>queue</th><th>printer</th><th>connection</th><th>state</th><th>notes</th>",
		`<td class="jn" title="Virtual-MFC">Virtual-MFC</td><td>Brother MFC-L2710DW</td>`, `<td class="jn" title="Hall">Hall</td><td>Hall Laser</td>`,
		"network", `<td class="off">away</td>`, ">default<", ">rejects jobs<",
		`data-act="configure" data-q="Virtual-MFC"`, `data-act="default" data-q="Hall"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the table lacks %q", want)
		}
	}
	if strings.Contains(page, `data-act="default" data-q="Virtual-MFC"`) {
		t.Error("the default queue offers to become the default")
	}
	if !strings.Contains(page, "· 2") {
		t.Error("the count is missing")
	}
}

func TestQueueTableEscapesNames(t *testing.T) {
	f := queuesFake()
	f.printers = append(f.printers, cups.Printer{Name: `A_b.c-1"<b>`, Info: "<i>x</i>", State: 3, Accepting: true})
	_, h := actionServer(t, f)
	page := get(h, "/printers").Body.String()
	if strings.Contains(page, `"<b>`) || strings.Contains(page, "<i>x</i>") {
		t.Error("a queue name or description is not escaped")
	}
	if strings.Count(page, `A_b.c-1&#34;&lt;b&gt;`) < 3 {
		t.Error("the queue name is not in its title, cell and buttons")
	}
	if strings.Count(page, "&lt;i&gt;x&lt;/i&gt;") != 1 {
		t.Error("the description is not shown exactly once")
	}
}

func TestQueueTableWithoutQueuesAndWhenCUPSIsDown(t *testing.T) {
	f := &fakeCUPS{}
	_, h := actionServer(t, f)
	if page := get(h, "/printers").Body.String(); !strings.Contains(page, `id="queues"`) || !strings.Contains(page, "CUPS has no printers") || strings.Contains(page, "<table") {
		t.Errorf("empty list: %s", page)
	}
	f.set(func(f *fakeCUPS) { f.err = errors.New("CUPS does not answer: socket gone") })
	rec := get(h, "/printers")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "CUPS does not answer: socket gone") {
		t.Errorf("CUPS down: status %d", rec.Code)
	}
	if rec := get(h, "/printers/Hall/settings"); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "CUPS does not answer") {
		t.Errorf("settings with CUPS down: status %d, body %q", rec.Code, rec.Body.String())
	}
}

func TestEventsCarryTheQueueTable(t *testing.T) {
	f := queuesFake()
	old := pollEvery
	pollEvery = time.Hour
	defer func() { pollEvery = old }()
	s := serverFor(f)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c := openEvents(t, ts)
	got := map[string]string{}
	for range 7 {
		name, data := c.event(t)
		got[name] = data
	}
	q, ok := got["queues"]
	if !ok || !strings.HasPrefix(q, `<div id="queues">`) || !strings.Contains(q, "Hall Laser") {
		t.Errorf("queues event: %q", q)
	}
}

// The panel holds the settings that wait for "save changes". Whatever acts at
// once lives in the table or the overview, so an instant action can never throw
// away what was typed into an open panel.
func TestSettingsPanelHasNoInstantActions(t *testing.T) {
	f := queuesFake()
	_, h := actionServer(t, f)
	body := get(h, "/printers/Virtual-MFC/settings").Body.String()
	for _, act := range []string{"pause", "resume", "accept", "reject", "default", "remove", "cancel-all", "move-all"} {
		if strings.Contains(body, `data-act="`+act+`"`) {
			t.Errorf("the panel has an instant %q button", act)
		}
	}
	for _, want := range []string{`data-act="save"`, `data-act="close"`, `data-group="state"`, `data-group="accept"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel lacks %q", want)
		}
	}
}

// The prototype puts pause and resume on the overview's printer card, and
// accept and reject only among the panel's settings: the table has neither.
func TestQueueTableLeavesPauseAndAcceptToTheirOwnPlaces(t *testing.T) {
	f := queuesFake()
	_, h := actionServer(t, f)
	page := get(h, "/printers").Body.String()
	for _, act := range []string{"pause", "resume", "accept", "reject"} {
		if strings.Contains(page, `data-act="`+act+`"`) {
			t.Errorf("the queue table has a %q button", act)
		}
	}
}

// Cleaning uses ink, so it asks first; the other commands go at once.
func TestOnlyCleanAsksFirst(t *testing.T) {
	f := queuesFake()
	_, h := actionServer(t, f)
	body := get(h, "/printers/Virtual-MFC/settings").Body.String()
	buttons := regexp.MustCompile(`<button[^>]*data-act="command"[^>]*>`).FindAllString(body, -1)
	if len(buttons) != 4 {
		t.Fatalf("%d maintenance buttons, want 4", len(buttons))
	}
	for _, b := range buttons {
		asks := strings.Contains(b, "data-confirm=")
		if clean := strings.Contains(b, `data-command="Clean"`); asks != clean {
			t.Errorf("asks first = %v for %s", asks, b)
		}
		if strings.Contains(b, `data-command="Clean"`) {
			for _, want := range []string{`data-confirm="Cleaning the print heads uses ink. Clean them now?"`,
				`data-confirm-label="Clean print heads"`, `data-keep="Not now"`, "data-confirm-title="} {
				if !strings.Contains(b, want) {
					t.Errorf("the Clean button lacks %q: %s", want, b)
				}
			}
		}
	}
}

func TestOverviewHasPauseAndResumeOnTheCard(t *testing.T) {
	f := queuesFake()
	f.printers[0].State = 5
	_, h := actionServer(t, f)
	page := get(h, "/").Body.String()
	if !strings.Contains(page, `data-act="resume" data-q="Hall"`) || !strings.Contains(page, ">resume queue<") ||
		!strings.Contains(page, `data-act="pause" data-q="Virtual-MFC"`) || !strings.Contains(page, ">pause queue<") {
		t.Error("the overview cards do not offer pause or resume")
	}
}

// CUPS finds a queue whatever the case of its name, so the guard must too:
// the name of the path is resolved to the queue's own name before anything
// else, and every call to CUPS and every guard uses that name.
func TestQueueNamesAreResolvedWhateverTheCase(t *testing.T) {
	const refusal = "Virtual-MFC is declared in ravnix; remove it there."
	f := queuesFake()
	s, h := actionServer(t, f, "Virtual-MFC")
	for _, path := range []string{"/printers/virtual-mfc/delete", "/printers/VIRTUAL-MFC/delete", "/printers/vIRTUAL-mfc/delete"} {
		if rec := postTo(s, h, path, nil); rec.Code != 403 || rec.Body.String() != refusal {
			t.Errorf("POST %s: status %d, body %q", path, rec.Code, rec.Body.String())
		}
	}
	form := same(full())
	form.Set("address", "ipp://elsewhere.local/ipp/print")
	if rec := postTo(s, h, "/printers/virtual-mfc/settings", form); rec.Code != 403 || rec.Body.String() != refusal {
		t.Errorf("address change: status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.did(); len(got) != 0 {
		t.Errorf("CUPS was asked %v although the declared queue was refused", got)
	}

	// An allowed call reaches CUPS with the queue's own name.
	form = same(full())
	form.Set("sides", "two-sided-long-edge")
	if rec := postTo(s, h, "/printers/virtual-MFC/settings", form); rec.Code != 200 || rec.Body.String() != "Saved Virtual-MFC: 1 setting." {
		t.Errorf("sides only: status %d, body %q", rec.Code, rec.Body.String())
	}
	for _, act := range []string{"pause", "default", "cancel-all"} {
		postTo(s, h, "/printers/VIRTUAL-MFC/"+act, nil)
	}
	postTo(s, h, "/printers/virtual-mfc/move-all", url.Values{"to": {"HALL"}})
	postTo(s, h, "/printers/virtual-mfc/command", url.Values{"command": {"Clean"}})
	if rec := postTo(s, h, "/printers/virtual-mfc/testpage", url.Values{"hold": {"true"}}); rec.Code != 200 || rec.Body.String() != "Test page sent to Brother as job 32." {
		t.Errorf("test page by a lowercase name: status %d, body %q", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, s); ev.Action != "print.testpage" || ev.Target != "Virtual-MFC" {
		t.Errorf("the test page event is about %q (%s), not the queue's own name", ev.Target, ev.Action)
	}
	want := "modify Virtual-MFC; pause Virtual-MFC true; default Virtual-MFC; cancel-all Virtual-MFC; move-all Virtual-MFC Hall; command Virtual-MFC Clean; testpage Virtual-MFC hold=true"
	if got := strings.Join(f.did(), "; "); got != want {
		t.Errorf("CUPS was asked\n%s\nwant\n%s", got, want)
	}
	if ev := lastEvent(t, s); ev.Target != "Virtual-MFC" {
		t.Errorf("the event is about %q, not the queue's own name", ev.Target)
	}
	if rec := get(h, "/printers/virtual-mfc/settings"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `data-q="Virtual-MFC"`) {
		t.Errorf("the panel by a lowercase name: status %d", rec.Code)
	}

	// An undeclared queue is not found under a wrong name either.
	f.set(func(f *fakeCUPS) { f.acts = nil })
	for _, act := range []string{"delete", "pause", "default", "cancel-all", "settings", "testpage"} {
		if rec := postTo(s, h, "/printers/virtual-mfc-2/"+act, nil); rec.Code != 404 || rec.Body.Len() == 0 {
			t.Errorf("POST %s on an unknown queue: status %d, body %q, want 404 with a message", act, rec.Code, rec.Body.String())
		}
	}
	if got := f.did(); len(got) != 0 {
		t.Errorf("CUPS was asked %v for a queue it does not have", got)
	}
}

// A name CUPS would never create is refused before CUPS hears of it.
func TestQueueNamesOfTheWrongShapeAreRefused(t *testing.T) {
	f := queuesFake()
	s, h := actionServer(t, f, "Virtual-MFC")
	for _, name := range []string{"..%2Fx", "a%2Fb", "%2E%2E%2F%2E%2E%2Fetc", "a%23b", "a%20b", "a%09b", "a%00b", "a%0Ab", "a%7Fb", strings.Repeat("x", 128)} {
		before := f.callCount()
		for _, route := range []struct{ method, path string }{
			{"POST", "/delete"}, {"POST", "/default"}, {"POST", "/pause"}, {"POST", "/settings"}, {"GET", "/settings"},
			{"POST", "/cancel-all"}, {"POST", "/move-all"}, {"POST", "/command"}, {"POST", "/testpage"},
		} {
			var rec *httptest.ResponseRecorder
			if route.method == "GET" {
				rec = get(h, "/printers/"+name+route.path)
			} else {
				rec = postTo(s, h, "/printers/"+name+route.path, url.Values{"to": {"Hall"}, "command": {"Clean"}, "info": {"x"}})
			}
			if rec.Code != 400 || rec.Body.String() != "That is not a queue name." {
				t.Errorf("%s %s%.20s: status %d, body %q", route.method, name, route.path, rec.Code, rec.Body.String())
			}
		}
		if f.callCount() != before {
			t.Errorf("name %.20q: CUPS was called", name)
		}
	}
	if got := f.did(); len(got) != 0 {
		t.Errorf("CUPS was asked %v", got)
	}
	// The longest name CUPS takes, 127 bytes, and the usual odd ones still pass the shape check.
	for _, name := range []string{strings.Repeat("x", 127), "A_b.c-1", "Fußball-Drucker"} {
		if rec := get(h, "/printers/"+url.PathEscape(name)+"/settings"); rec.Code != 404 {
			t.Errorf("%.20q: status %d, want 404 (well-formed, but unknown)", name, rec.Code)
		}
	}
}

// The panel is never live-replaced, so it can be stale. Only the fields it
// posts are looked at; a field that is not posted is never changed, whatever
// the queue looks like now.
func TestSettingsPostOfOneFieldLeavesTheRestAlone(t *testing.T) {
	f := queuesFake()
	f.printers[1].State, f.printers[1].Accepting = 5, false // the panel was loaded before this
	f.printers[1].SidesDefault, f.printers[1].Shared = "two-sided-long-edge", false
	s, h := actionServer(t, f)
	rec := postTo(s, h, "/printers/Virtual-MFC/settings", url.Values{"location": {"Attic"}})
	if rec.Code != 200 || rec.Body.String() != "Saved Virtual-MFC: 1 setting." {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.did(); len(got) != 1 || got[0] != "modify Virtual-MFC" {
		t.Errorf("CUPS was asked %v, want one modify", got)
	}
	if len(f.modified()) != 1 {
		t.Fatalf("Modify was called %d times", len(f.modified()))
	}
	ch := f.modified()[0]
	if ch.Location == nil || *ch.Location != "Attic" || ch.Info != nil || ch.DeviceURI != nil || ch.Sides != nil || ch.Color != nil ||
		ch.Media != nil || ch.ErrorPolicy != nil || ch.Shared != nil || ch.Quality != nil {
		t.Errorf("Modify got %+v, want the location alone", ch)
	}
}

// The panel remembers what it was loaded with, so the script can post only
// the fields that the user changed.
func TestSettingsPanelRecordsTheLoadedValues(t *testing.T) {
	f := queuesFake()
	f.printers[1].Info = `Brother "B"`
	f.printers[1].State = 5
	_, h := actionServer(t, f)
	body := get(h, "/printers/Virtual-MFC/settings").Body.String()
	for _, want := range []string{
		`name="info" data-was="Brother &#34;B&#34;"`, `name="location" data-was="Study"`, `name="address" data-was="ipp://printer.local/ipp/print"`,
		`data-group="state" data-was="paused"`, `data-group="accept" data-was="accepted"`, `data-group="shared" data-was="yes"`,
		`data-group="sides" data-was="one-sided"`, `data-group="color" data-was="color"`, `data-group="quality" data-was="4"`,
		`name="media" data-was="iso_a4_210x297mm"`, `name="policy" data-was="stop-printer"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel lacks %q", want)
		}
	}
}
