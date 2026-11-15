// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/store"
)

func (f *fakeCUPS) LogLevel(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logReads++
	return f.logLevel, f.logErr
}

func (f *fakeCUPS) SetLogLevel(_ context.Context, level, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setLogs = append(f.setLogs, level)
	f.setPasswords = append(f.setPasswords, password)
	if f.setLogErr == nil {
		f.logLevel = level
	}
	return f.setLogErr
}

func (f *fakeCUPS) levelsSet() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.setLogs...)
}

func (f *fakeCUPS) passwordsSent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.setPasswords...)
}

func (f *fakeCUPS) levelReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logReads
}

// journal is a stand-in for the journal: it counts its reads and records what
// it was asked for.
type journal struct {
	lines []string
	err   error
	reads atomic.Int32
	asked atomic.Int32
}

func (j *journal) read(_ context.Context, n int) ([]string, error) {
	j.reads.Add(1)
	j.asked.Store(int32(n))
	return j.lines, j.err
}

var sampleLog = []string{
	"2026-11-14T22:14:07+0100 host cupsd[812]: first line",
	"2026-11-14T22:14:08+0100 host cupsd[812]: <script>alert(1)</script> & \"quotes\"",
	"2026-11-14T22:14:09+0100 host cupsd[812]: newest line",
	"-- a line without a time stamp",
}

func logServer(t *testing.T, f *fakeCUPS, j *journal, managed bool) (*Server, http.Handler) {
	t.Helper()
	return logServerAs(t, "mika", f, j, managed)
}

func logServerAs(t *testing.T, user string, f *fakeCUPS, j *journal, managed bool) (*Server, http.Handler) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(config.Config{Listen: "127.0.0.1:7631", User: user, CupsdConfManaged: managed},
		Deps{CUPS: f, Store: db, ReadLog: j.read}, NewToken())
	return s, s.Handler()
}

var levelButton = func(level string) *regexp.Regexp {
	return regexp.MustCompile(`<button[^>]*data-level="` + level + `"[^>]*>`)
}

func TestLogFragmentShowsTheLinesNewestLastAndTheLevel(t *testing.T) {
	f := &fakeCUPS{logLevel: "info"}
	j := &journal{lines: sampleLog}
	_, h := logServer(t, f, j, false)

	rec := get(h, "/printers/log")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	body := rec.Body.String()
	if j.asked.Load() != 200 {
		t.Errorf("the journal was asked for %d lines, want 200", j.asked.Load())
	}
	first, last := strings.Index(body, "first line"), strings.Index(body, "newest line")
	if first < 0 || last < first {
		t.Errorf("order: first at %d, newest at %d", first, last)
	}
	if !strings.Contains(body, `<span class="ts">2026-11-14T22:14:07&#43;0100 </span>host cupsd[812]: first line`) {
		t.Errorf("the time stamp column is missing from:\n%s", body)
	}
	if !strings.Contains(body, "-- a line without a time stamp") {
		t.Error("a line without a time stamp is gone")
	}
	for _, level := range []string{"warn", "info", "debug"} {
		m := levelButton(level).FindString(body)
		if m == "" {
			t.Errorf("no %s button", level)
			continue
		}
		if on := strings.Contains(m, `class="on"`); on != (level == "info") {
			t.Errorf("%s button %s", level, m)
		}
		if strings.Contains(m, "disabled") {
			t.Errorf("%s button is disabled: %s", level, m)
		}
	}
	if !strings.Contains(body, `data-act="log-refresh"`) || !strings.Contains(body, "refresh log") {
		t.Error("no refresh button")
	}
}

func TestLogLinesAreEscaped(t *testing.T) {
	_, h := logServer(t, &fakeCUPS{logLevel: "warn"}, &journal{lines: sampleLog}, false)
	body := get(h, "/printers/log").Body.String()
	if strings.Contains(body, "<script>") || strings.Contains(body, "alert(1)</script>") {
		t.Errorf("a log line became markup:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt; &amp; &#34;quotes&#34;") {
		t.Errorf("the line is not shown as text:\n%s", body)
	}
}

func TestLogFragmentWhenTheJournalIsClosed(t *testing.T) {
	f := &fakeCUPS{logLevel: "warn"}
	_, h := logServer(t, f, &journal{err: cups.ErrJournalNotReadable}, false)
	rec := get(h, "/printers/log")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "The journal is not readable for this user.") {
		t.Errorf("status %d, body:\n%s", rec.Code, body)
	}
	// The switch is CUPS's, not the journal's: it still works.
	if m := levelButton("debug").FindString(body); m == "" || strings.Contains(m, "disabled") {
		t.Errorf("debug button = %q", m)
	}
}

func TestLogFragmentWhenTheJournalHasNoLines(t *testing.T) {
	_, h := logServer(t, &fakeCUPS{logLevel: "warn"}, &journal{}, false)
	if body := get(h, "/printers/log").Body.String(); !strings.Contains(body, "no CUPS lines") {
		t.Errorf("body:\n%s", body)
	}
}

func TestLogFragmentWhenCUPSDoesNotAnswer(t *testing.T) {
	f := &fakeCUPS{logErr: errors.New("CUPS does not answer: connection refused")}
	_, h := logServer(t, f, &journal{lines: sampleLog}, false)
	rec := get(h, "/printers/log")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "newest line") {
		t.Fatalf("status %d: the journal must still show:\n%s", rec.Code, body)
	}
	if !strings.Contains(body, "CUPS does not answer: connection refused") {
		t.Error("the hint does not say why the switch is off")
	}
	for _, level := range []string{"warn", "info", "debug"} {
		if m := levelButton(level).FindString(body); !strings.Contains(m, "disabled") || strings.Contains(m, `class="on"`) {
			t.Errorf("%s button = %q", level, m)
		}
	}
}

func TestManagedConfReadOnly(t *testing.T) {
	f := &fakeCUPS{logLevel: "debug"}
	s, h := logServer(t, f, &journal{lines: sampleLog}, true)

	rec := postTo(s, h, "/printers/log/level", url.Values{"level": {"warn"}})
	if rec.Code != 403 || rec.Body.String() != "cupsd.conf is managed by the system configuration." {
		t.Errorf("POST: status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := f.levelsSet(); len(got) != 0 {
		t.Errorf("CUPS was asked to set %v", got)
	}
	if f.levelReads() != 0 {
		t.Error("CUPS was asked before the refusal")
	}

	body := get(h, "/printers/log").Body.String()
	for _, level := range []string{"warn", "info", "debug"} {
		m := levelButton(level).FindString(body)
		if !strings.Contains(m, "disabled") {
			t.Errorf("%s button is not disabled: %q", level, m)
		}
		if on := strings.Contains(m, `class="on"`); on != (level == "debug") {
			t.Errorf("%s button still shows the level wrongly: %q", level, m)
		}
	}
	if !strings.Contains(body, "cupsd.conf is managed by the system configuration.") {
		t.Errorf("no hint:\n%s", body)
	}
	if f.levelReads() == 0 {
		t.Error("the level was not read for display")
	}
	if strings.Contains(body, `type="password"`) {
		t.Error("a managed cupsd.conf still asks for the password")
	}
}

// The managed hint says why the switch is off even when CUPS does not answer.
func TestManagedHintWinsOverACUPSError(t *testing.T) {
	f := &fakeCUPS{logErr: errors.New("CUPS does not answer: connection refused")}
	_, h := logServer(t, f, &journal{lines: sampleLog}, true)
	body := get(h, "/printers/log").Body.String()
	if !strings.Contains(body, managedText) || strings.Contains(body, "connection refused") {
		t.Errorf("hint:\n%s", body)
	}
}

func TestSetLogLevelAnswersAndLogsTheChange(t *testing.T) {
	for _, tc := range []struct{ from, to, say string }{
		{"warn", "debug", "Debug logging is on."},
		{"debug", "info", "Logging is at info."},
		{"info", "warn", "Logging is at warn."},
	} {
		f := &fakeCUPS{logLevel: tc.from}
		s, h := logServer(t, f, &journal{}, false)
		rec := postTo(s, h, "/printers/log/level", url.Values{"level": {tc.to}, "password": {testPassword}})
		if rec.Code != 200 || rec.Body.String() != tc.say {
			t.Errorf("%s: status %d, body %q", tc.to, rec.Code, rec.Body.String())
		}
		if got := f.levelsSet(); len(got) != 1 || got[0] != tc.to {
			t.Errorf("%s: CUPS was asked to set %v", tc.to, got)
		}
		if got := f.passwordsSent(); len(got) != 1 || got[0] != testPassword {
			t.Errorf("%s: CUPS got the passwords %q", tc.to, got)
		}
		ev := lastEvent(t, s)
		if ev.Action != "cups.loglevel" || ev.Detail != tc.from+" → "+tc.to || !ev.OK || ev.Actor != "mika" {
			t.Errorf("%s: event = %+v", tc.to, ev)
		}
	}
}

func TestSetLogLevelRefusesOtherLevelsWithoutCUPS(t *testing.T) {
	f := &fakeCUPS{logLevel: "warn"}
	s, h := logServer(t, f, &journal{}, false)
	for _, level := range []string{"", "error", "none", "debug2", "DEBUG", " debug", "warn\nListen *:631"} {
		rec := postTo(s, h, "/printers/log/level", url.Values{"level": {level}})
		if rec.Code != 400 {
			t.Errorf("level %q: status %d, body %q", level, rec.Code, rec.Body.String())
		}
	}
	if rec := postTo(s, h, "/printers/log/level", nil); rec.Code != 400 {
		t.Errorf("no level: status %d", rec.Code)
	}
	if f.levelReads() != 0 || len(f.levelsSet()) != 0 {
		t.Errorf("CUPS was called: reads %d, sets %v", f.levelReads(), f.levelsSet())
	}
	if ev, _ := s.deps.Store.Recent(context.Background(), 1); len(ev) != 0 {
		t.Errorf("an event was logged: %+v", ev)
	}
}

func TestSetLogLevelFailures(t *testing.T) {
	t.Run("CUPS refuses", func(t *testing.T) {
		f := &fakeCUPS{logLevel: "warn", setLogErr: errors.New("CUPS refused this: the user may not do it (403)")}
		s, h := logServer(t, f, &journal{}, false)
		rec := postTo(s, h, "/printers/log/level", url.Values{"level": {"debug"}})
		if rec.Code != 502 || rec.Body.String() != "CUPS refused this: the user may not do it (403)" {
			t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
		}
		ev := lastEvent(t, s)
		if ev.Action != "cups.loglevel" || ev.OK || !strings.HasPrefix(ev.Detail, "warn → debug") || !strings.Contains(ev.Detail, "refused") {
			t.Errorf("event = %+v", ev)
		}
	})
	t.Run("CUPS does not come back", func(t *testing.T) {
		f := &fakeCUPS{logLevel: "warn", setLogErr: cups.ErrNotBack}
		s, h := logServer(t, f, &journal{}, false)
		rec := postTo(s, h, "/printers/log/level", url.Values{"level": {"debug"}})
		if rec.Code != 502 || rec.Body.String() != "CUPS has not come back yet; look again in a moment." {
			t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
		}
		if ev := lastEvent(t, s); ev.OK || !strings.HasPrefix(ev.Detail, "warn → debug") {
			t.Errorf("event = %+v", ev)
		}
	})
	t.Run("the old level cannot be read", func(t *testing.T) {
		f := &fakeCUPS{logErr: errors.New("CUPS does not answer: gone")}
		s, h := logServer(t, f, &journal{}, false)
		rec := postTo(s, h, "/printers/log/level", url.Values{"level": {"debug"}})
		if rec.Code != 502 || rec.Body.String() != "CUPS does not answer: gone" {
			t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
		}
		if got := f.levelsSet(); len(got) != 0 {
			t.Errorf("CUPS was asked to set %v after it could not be read", got)
		}
		if ev := lastEvent(t, s); ev.Action != "cups.loglevel" || ev.OK || !strings.HasPrefix(ev.Detail, "? → debug") {
			t.Errorf("event = %+v", ev)
		}
	})
}

// testPassword is what the tests type into the password field. None of them
// may find it anywhere but in what CUPS was given.
const testPassword = "hunter2-skra"

// The password goes to CUPS and nowhere else: not into the answer, the
// history, the log or the page, whatever CUPS says, and also not when skrá
// refuses the request before CUPS sees it.
func TestPasswordNeverStored(t *testing.T) {
	var logged strings.Builder
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	tooLong := testPassword + strings.Repeat("x", 300)
	for _, tc := range []struct {
		name     string
		password string
		noToken  bool
		readErr  error // what reading the old level answers
		err      error // what SetLogLevel answers
		code     int
		body     string
		toCUPS   bool   // CUPS gets the password
		detail   string // the history row, or "" for none
	}{
		{name: "success", code: 200, body: "Debug logging is on.", toCUPS: true, detail: "warn → debug"},
		{name: "wrong password", err: cups.ErrWrongPassword, code: 502, toCUPS: true, detail: "warn → debug: "},
		{name: "not in the system group", err: cups.ErrNotSystemGroup, code: 502, toCUPS: true, detail: "warn → debug: "},
		{name: "no password", err: cups.ErrNeedsPassword, code: 502, toCUPS: true, detail: "warn → debug: "},
		{name: "not back", err: cups.ErrNotBack, code: 502, toCUPS: true, detail: "warn → debug: "},
		{name: "reload waits", err: cups.ErrReloadWaits, code: 200, toCUPS: true, detail: "warn → debug"},
		{name: "other", err: errors.New("CUPS answered HTTP 500"), code: 502, toCUPS: true, detail: "warn → debug: "},
		{name: "the level cannot be read", readErr: errors.New("CUPS does not answer: gone"), code: 502,
			body: "CUPS does not answer: gone", detail: "? → debug: CUPS does not answer: gone"},
		{name: "too long", password: tooLong, code: 400,
			body: "The password is longer than CUPS accepts (at most 250 bytes for this user). Use cupsctl in a terminal instead."},
		{name: "no token", noToken: true, code: 403},
	} {
		password := tc.password
		if password == "" {
			password = testPassword
		}
		f := &fakeCUPS{logLevel: "warn", logErr: tc.readErr, setLogErr: tc.err}
		s, h := logServer(t, f, &journal{lines: sampleLog}, false)
		form := url.Values{"level": {"debug"}, "password": {password}}
		var rec *httptest.ResponseRecorder
		if tc.noToken {
			req := httptest.NewRequest("POST", "/printers/log/level", strings.NewReader(form.Encode()))
			req.Host = "127.0.0.1:7631"
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, req)
		} else {
			rec = postTo(s, h, "/printers/log/level", form)
		}
		body := rec.Body.String()
		want := tc.body
		if want == "" && tc.err != nil {
			want = tc.err.Error()
		}
		if rec.Code != tc.code || (want != "" && body != want) {
			t.Errorf("%s: status %d, body %q; want %d, %q", tc.name, rec.Code, body, tc.code, want)
		}
		got := f.passwordsSent()
		if tc.toCUPS && (len(got) != 1 || got[0] != password) || !tc.toCUPS && len(got) != 0 {
			t.Errorf("%s: CUPS got %q", tc.name, got)
		}
		for name, h := range rec.Header() {
			if strings.Contains(strings.Join(h, " "), testPassword) {
				t.Errorf("%s: the password is in the header %s", tc.name, name)
			}
		}
		if strings.Contains(body, testPassword) {
			t.Errorf("%s: the password is in the answer %q", tc.name, body)
		}
		events, err := s.deps.Store.Recent(context.Background(), 100)
		if err != nil {
			t.Fatal(err)
		}
		if n := map[bool]int{true: 1, false: 0}[tc.detail != ""]; len(events) != n {
			t.Errorf("%s: %d history rows, want %d", tc.name, len(events), n)
		}
		for _, ev := range events {
			if row := fmt.Sprintf("%+v", ev); strings.Contains(row, testPassword) {
				t.Errorf("%s: the password is in the history: %s", tc.name, row)
			}
			if !strings.HasPrefix(ev.Detail, tc.detail) {
				t.Errorf("%s: detail %q, want it to start %q", tc.name, ev.Detail, tc.detail)
			}
		}
		if page := get(h, "/printers/log").Body.String(); strings.Contains(page, testPassword) {
			t.Errorf("%s: the password is in the log section", tc.name)
		}
	}
	if strings.Contains(logged.String(), testPassword) {
		t.Errorf("the password is in the log output:\n%s", logged.String())
	}
}

// When CUPS waits for a printing job before it uses the new level, the change
// is made and the answer says so instead of the level.
func TestSetLogLevelSaysWhenTheReloadWaits(t *testing.T) {
	f := &fakeCUPS{logLevel: "warn", setLogErr: cups.ErrReloadWaits}
	s, h := logServer(t, f, &journal{}, false)
	rec := postTo(s, h, "/printers/log/level", url.Values{"level": {"debug"}, "password": {testPassword}})
	if rec.Code != 200 || rec.Body.String() != cups.ErrReloadWaits.Error() {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, s); !ev.OK || ev.Detail != "warn → debug" {
		t.Errorf("event = %+v", ev)
	}
}

// CUPS reads at most 255 bytes of "user:password", so for mika the password
// may have 250.
func TestSetLogLevelRefusesALongPassword(t *testing.T) {
	f := &fakeCUPS{logLevel: "warn"}
	s, h := logServer(t, f, &journal{}, false)
	long := strings.Repeat("p", 251)
	rec := postTo(s, h, "/printers/log/level", url.Values{"level": {"debug"}, "password": {long}})
	want := "The password is longer than CUPS accepts (at most 250 bytes for this user). Use cupsctl in a terminal instead."
	if rec.Code != 400 || rec.Body.String() != want {
		t.Errorf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if len(f.levelsSet()) != 0 || f.levelReads() != 0 {
		t.Errorf("CUPS was called: reads %d, sets %v", f.levelReads(), f.levelsSet())
	}
	if ev, _ := s.deps.Store.Recent(context.Background(), 1); len(ev) != 0 {
		t.Errorf("an event was logged: %+v", ev)
	}
	rec = postTo(s, h, "/printers/log/level", url.Values{"level": {"debug"}, "password": {long[:250]}})
	if rec.Code != 200 || len(f.passwordsSent()) != 1 || f.passwordsSent()[0] != long[:250] {
		t.Errorf("250 bytes: status %d, sent %d", rec.Code, len(f.passwordsSent()))
	}
}

func TestSwitchShowsThePasswordRow(t *testing.T) {
	f := &fakeCUPS{logLevel: "warn"}
	_, h := logServerAs(t, "mika", f, &journal{lines: sampleLog}, false)
	body := get(h, "/printers/log").Body.String()
	row := regexp.MustCompile(`<input[^>]*type="password"[^>]*>`).FindString(body)
	if row == "" || !strings.Contains(row, `autocomplete="current-password"`) || !strings.Contains(row, `name="password"`) {
		t.Errorf("password field = %q in:\n%s", row, body)
	}
	for _, want := range []string{"password for mika", "change log level", `data-act="log-send"`, `data-act="log-cancel"`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<form") {
		t.Error("the field is in a form, which a browser may submit with the password in the address")
	}
	// The level buttons stay on: they open the row.
	if m := levelButton("debug").FindString(body); m == "" || strings.Contains(m, "disabled") {
		t.Errorf("debug button = %q", m)
	}

	_, h = logServerAs(t, "root", &fakeCUPS{logLevel: "warn"}, &journal{lines: sampleLog}, false)
	body = get(h, "/printers/log").Body.String()
	if strings.Contains(body, `type="password"`) || strings.Contains(body, "current-password") || strings.Contains(body, "password for") {
		t.Errorf("root is asked for a password:\n%s", body)
	}
}

func TestSetLogLevelNeedsTheToken(t *testing.T) {
	f := &fakeCUPS{logLevel: "warn"}
	_, h := logServer(t, f, &journal{}, false)
	req := httptest.NewRequest("POST", "/printers/log/level", strings.NewReader("level=debug"))
	req.Host = "127.0.0.1:7631"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 || len(f.levelsSet()) != 0 {
		t.Errorf("status %d, sets %v", rec.Code, f.levelsSet())
	}
}

func TestPrintersPageLoadsTheLogLater(t *testing.T) {
	f := &fakeCUPS{printers: []cups.Printer{brother()}, logLevel: "warn"}
	j := &journal{lines: sampleLog}
	_, h := logServer(t, f, j, false)
	rec := get(h, "/printers")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="log"`) || !strings.Contains(body, "CUPS log") {
		t.Error("the page has no log section")
	}
	if j.reads.Load() != 0 || f.levelReads() != 0 {
		t.Errorf("the page read the journal (%d) or cupsd.conf (%d) itself", j.reads.Load(), f.levelReads())
	}
	if strings.Contains(body, "first line") {
		t.Error("log lines are in the page")
	}
	if strings.Index(body, `id="log"`) < strings.Index(body, `id="add-panel"`) {
		t.Error("the log is not at the bottom of the tab")
	}
}
