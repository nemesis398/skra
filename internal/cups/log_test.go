// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/ipp"
	"github.com/nemesis398/skra/internal/ipp/ipptest"
)

func TestReplaceLogLevel(t *testing.T) {
	for _, tc := range []struct{ name, in, level, want string }{
		{
			"one line among comments",
			"# LogLevel info is chatty\nLogLevel warn\nErrorLog stderr\n# LogLevel debug\n",
			"debug",
			"# LogLevel info is chatty\nLogLevel debug\nErrorLog stderr\n# LogLevel debug\n",
		},
		{"no line gets one appended", "ErrorLog stderr\nListen /run/cups/cups.sock\n", "info", "ErrorLog stderr\nListen /run/cups/cups.sock\nLogLevel info\n"},
		{"no line and no final newline", "ErrorLog stderr", "debug", "ErrorLog stderr\nLogLevel debug\n"},
		{"only a commented line", "#LogLevel warn\n", "debug", "#LogLevel warn\nLogLevel debug\n"},
		{"empty text", "", "warn", "LogLevel warn\n"},
		{"case and leading whitespace", "  \tloglevel WARN\nx\n", "info", "  \tLogLevel info\nx\n"},
		{"every active line", "LogLevel warn\nLogLevel info\n", "debug", "LogLevel debug\nLogLevel debug\n"},
		{"no final newline stays so", "A 1\nLogLevel warn", "debug", "A 1\nLogLevel debug"},
		{"CRLF stays", "A 1\r\nLogLevel warn\r\nB 2\r\n", "debug", "A 1\r\nLogLevel debug\r\nB 2\r\n"},
		{"a longer directive is another one", "LogLevelFoo bar\n", "debug", "LogLevelFoo bar\nLogLevel debug\n"},
		{"a tab after the name", "LogLevel\twarn\n", "info", "LogLevel info\n"},
		{"an inline comment stays", "LogLevel\twarn # why\n", "info", "LogLevel info # why\n"},
		{"a comment with no value", "  LogLevel #x\r\n", "debug", "  LogLevel debug #x\r\n"},
		{"a comment against the value", "LogLevel warn#x\n", "debug", "LogLevel debug#x\n"},
	} {
		if got := replaceLogLevel(tc.in, tc.level); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"LogLevel warn\n", "warn"},
		{"ErrorLog stderr\n", "warn"}, // CUPS's default
		{"", "warn"},
		{"# LogLevel debug\n", "warn"},
		{"LogLevel INFO\n", "info"},
		{"  loglevel debug # why\n", "debug"},
		{"LogLevel info\nLogLevel debug\n", "debug"}, // the later directive wins
		{"LogLevel debug2\r\n", "debug2"},
		{"LogLevelFoo debug\n", "warn"},
	} {
		if got := parseLogLevel(tc.in); got != tc.want {
			t.Errorf("parseLogLevel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// fakeJournal replaces the command runner for a test and records the argv.
func fakeJournal(t *testing.T, fn func(ctx context.Context, argv []string) (cmdResult, error)) *[][]string {
	t.Helper()
	old := runJournal
	var mu sync.Mutex
	var calls [][]string
	runJournal = func(ctx context.Context, argv []string) (cmdResult, error) {
		mu.Lock()
		calls = append(calls, argv)
		mu.Unlock()
		return fn(ctx, argv)
	}
	t.Cleanup(func() { runJournal = old })
	return &calls
}

func TestReadLogRunsJournalctlAndKeepsItsOrder(t *testing.T) {
	out := "2026-11-14T22:14:07+0100 host cupsd[1]: first\n2026-11-14T22:14:08+0100 host cupsd[1]: <b>last</b>\n"
	calls := fakeJournal(t, func(context.Context, []string) (cmdResult, error) { return cmdResult{Stdout: out}, nil })
	lines, err := ReadLog(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-11-14T22:14:07+0100 host cupsd[1]: first", "2026-11-14T22:14:08+0100 host cupsd[1]: <b>last</b>"}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("lines = %q", lines)
	}
	wantArgv := []string{"journalctl", "-u", "cups.service", "-n", "200", "-o", "short-iso", "--no-pager"}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], wantArgv) {
		t.Errorf("argv = %q", *calls)
	}
}

func TestReadLogClampsTheCount(t *testing.T) {
	calls := fakeJournal(t, func(context.Context, []string) (cmdResult, error) { return cmdResult{}, nil })
	for _, n := range []int{-5, 0, 1, 1000, 1001, 1 << 30} {
		if _, err := ReadLog(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, argv := range *calls {
		got = append(got, argv[4])
	}
	if want := []string{"1", "1", "1", "1000", "1000", "1000"}; !reflect.DeepEqual(got, want) {
		t.Errorf("-n values = %q, want %q", got, want)
	}
}

func TestReadLogEmpty(t *testing.T) {
	for _, out := range []string{"", "-- No entries --\n"} {
		fakeJournal(t, func(context.Context, []string) (cmdResult, error) { return cmdResult{Stdout: out}, nil })
		lines, err := ReadLog(context.Background(), 10)
		if err != nil || len(lines) != 0 {
			t.Errorf("stdout %q: lines %q, err %v", out, lines, err)
		}
	}
}

func TestReadLogNotReadable(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  cmdResult
		err  error
	}{
		{"permission", cmdResult{ExitCode: 1, Stderr: "Failed to open journal: Permission denied\n"}, nil},
		{"no journal files", cmdResult{ExitCode: 1, Stderr: "No journal files were found.\n"}, nil},
		{"insufficient, exit 0, nothing shown", cmdResult{Stderr: "Hint: not seeing messages. No journal files were opened due to insufficient permissions.\n", Stdout: "-- No entries --\n"}, nil},
	} {
		fakeJournal(t, func(context.Context, []string) (cmdResult, error) { return tc.res, tc.err })
		_, err := ReadLog(context.Background(), 10)
		if err == nil || err.Error() != "The journal is not readable for this user." {
			t.Errorf("%s: err = %v", tc.name, err)
		}
		if !errors.Is(err, ErrJournalNotReadable) {
			t.Errorf("%s: err is not ErrJournalNotReadable", tc.name)
		}
	}
}

func TestReadLogNoJournalctl(t *testing.T) {
	for name, runErr := range map[string]error{
		"not found":         exec.ErrNotFound,
		"not found wrapped": &exec.Error{Name: "journalctl", Err: exec.ErrNotFound},
	} {
		fakeJournal(t, func(context.Context, []string) (cmdResult, error) { return cmdResult{}, runErr })
		_, err := ReadLog(context.Background(), 10)
		if err == nil || err.Error() != "journalctl is not installed, so skrá cannot show the CUPS log." {
			t.Errorf("%s: err = %v", name, err)
		}
		if !errors.Is(err, ErrNoJournalctl) {
			t.Errorf("%s: err is not ErrNoJournalctl", name)
		}
		// Permissions are not what to look at.
		if errors.Is(err, ErrJournalNotReadable) {
			t.Errorf("%s: err is ErrJournalNotReadable", name)
		}
	}
}

func TestReadLogOtherFailures(t *testing.T) {
	// Lines on stdout with a permission hint on stderr are still lines.
	fakeJournal(t, func(context.Context, []string) (cmdResult, error) {
		return cmdResult{Stdout: "a\n", Stderr: "Hint: insufficient permissions to see all\n"}, nil
	})
	if lines, err := ReadLog(context.Background(), 10); err != nil || len(lines) != 1 {
		t.Errorf("lines %q, err %v", lines, err)
	}
	fakeJournal(t, func(context.Context, []string) (cmdResult, error) {
		return cmdResult{ExitCode: 1, Stderr: "Failed to determine timestamp: broke\n"}, nil
	})
	_, err := ReadLog(context.Background(), 10)
	if err == nil || errors.Is(err, ErrJournalNotReadable) || !strings.Contains(err.Error(), "Failed to determine timestamp") {
		t.Errorf("err = %v", err)
	}
	fakeJournal(t, func(context.Context, []string) (cmdResult, error) {
		return cmdResult{}, errors.New("fork/exec: out of memory")
	})
	if _, err := ReadLog(context.Background(), 10); err == nil || errors.Is(err, ErrJournalNotReadable) {
		t.Errorf("err = %v", err)
	}
}

func TestReadLogTimesOut(t *testing.T) {
	old := journalTimeout
	journalTimeout = 50 * time.Millisecond
	t.Cleanup(func() { journalTimeout = old })
	fakeJournal(t, func(ctx context.Context, _ []string) (cmdResult, error) {
		<-ctx.Done()
		return cmdResult{}, ctx.Err()
	})
	start := time.Now()
	_, err := ReadLog(context.Background(), 10)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestReadLogCancelledIsNotATimeout(t *testing.T) {
	fakeJournal(t, func(ctx context.Context, _ []string) (cmdResult, error) {
		<-ctx.Done()
		return cmdResult{}, ctx.Err()
	})
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ReadLog(parent, 10)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if err != nil && strings.Contains(err.Error(), "did not answer") {
		t.Errorf("a cancelled parent reads as a timeout: %v", err)
	}
}

func TestTailBufferKeepsTheNewest(t *testing.T) {
	b := &tailBuffer{max: 10}
	for _, p := range []string{"aaaa\n", "bbbb\n", "cccc\n", "d\n"} {
		if n, err := b.Write([]byte(p)); n != len(p) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", p, n, err)
		}
	}
	if got := b.String(); got != "bb\ncccc\nd\n" || !b.cut {
		t.Errorf("kept %q, cut %v", got, b.cut)
	}
	// A single write above the limit keeps its end.
	b = &tailBuffer{max: 4}
	b.Write([]byte("0123456789"))
	if b.String() != "6789" || !b.cut {
		t.Errorf("kept %q", b.String())
	}
}

func TestTailBufferExactSizeWrite(t *testing.T) {
	// One write of exactly max bytes into an empty buffer loses nothing, so
	// its first line is whole and stays.
	b := &tailBuffer{max: 12}
	b.Write([]byte("first\nsecond\n")[:12])
	if b.cut {
		t.Errorf("cut = true after %q went in whole", b.String())
	}
	b = &tailBuffer{max: 15}
	b.Write([]byte("first\nsecond\nxx"))
	if got := splitLines(b.complete()); !reflect.DeepEqual(got, []string{"first", "second", "xx"}) {
		t.Errorf("lines = %q, want the first line kept", got)
	}
	// With bytes already there, the write pushes them out, and where the new
	// ones start is not known to be a line start.
	b = &tailBuffer{max: 4}
	b.Write([]byte("ab"))
	b.Write([]byte("cdef"))
	if b.String() != "cdef" || !b.cut {
		t.Errorf("kept %q, cut %v; want cdef, true", b.String(), b.cut)
	}
	// A write above max cuts, empty buffer or not.
	b = &tailBuffer{max: 4}
	b.Write([]byte("abcde"))
	if b.String() != "bcde" || !b.cut {
		t.Errorf("kept %q, cut %v; want bcde, true", b.String(), b.cut)
	}
}

func TestTailBufferLinesDropTheCutLine(t *testing.T) {
	b := &tailBuffer{max: 14}
	b.Write([]byte("old line\nnew a\nnew b\n")) // keeps "e\nnew a\nnew b\n"
	if got := splitLines(b.complete()); !reflect.DeepEqual(got, []string{"new a", "new b"}) {
		t.Errorf("lines = %q", got)
	}
	whole := &tailBuffer{max: 100}
	whole.Write([]byte("a\r\nb\n\nc"))
	if got := splitLines(whole.complete()); !reflect.DeepEqual(got, []string{"a", "b", "", "c"}) {
		t.Errorf("lines = %q", got)
	}
}

func TestRunCommandCapsOutputAndKeepsTheEnd(t *testing.T) {
	old := maxJournal
	maxJournal = 64
	t.Cleanup(func() { maxJournal = old })
	res, err := runCommand(context.Background(), []string{"sh", "-c", "i=0; while [ $i -lt 100 ]; do echo line$i; i=$((i+1)); done; echo oops >&2; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || strings.TrimSpace(res.Stderr) != "oops" {
		t.Errorf("exit %d, stderr %q", res.ExitCode, res.Stderr)
	}
	if len(res.Stdout) > 64 || !strings.HasSuffix(res.Stdout, "line99\n") {
		t.Errorf("stdout %q", res.Stdout)
	}
	if strings.HasPrefix(res.Stdout, "ine") || strings.HasPrefix(res.Stdout, "e") {
		t.Errorf("a cut line was kept: %q", res.Stdout)
	}
	if _, err := runCommand(context.Background(), []string{"skra-no-such-command"}); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("missing command: err = %v", err)
	}
}

// --- cupsd.conf over the socket ---

// testPassword is the password of the users the fake CUPS knows.
const testPassword = "hunter2-skra"

// confServer is a CUPS whose cupsd.conf is the text in conf, guarded the way
// CUPS 2.4 guards Fedora's <Location /admin/conf> (scheduler/auth.c): a GET
// passes with any PeerCred, a PUT passes with PeerCred only for root and
// otherwise needs Basic with the password of a member of the system group.
// mika is a member, guest has the same password and is not. A PUT replaces
// the text and, like the restart of the scheduler, makes the next down GETs
// fail. Get-Jobs answers a printing job to the first printing asks.
type confServer struct {
	mu       sync.Mutex
	conf     string
	puts     []string
	types    []string
	auths    []string // the Authorization of every request, in order
	putAuths []string
	down     int
	printing int
	jobAsks  int
	gets     atomic.Int32
	status   int
}

// refusal is the status CUPS answers a request for cupsd.conf with before it
// looks at the resource, or 0 when it lets the request through.
func refusal(r *http.Request) int {
	if r.Method != "PUT" || r.Header.Get("Authorization") == "PeerCred root" {
		return 0
	}
	user, pw, ok := r.BasicAuth()
	switch {
	case !ok || pw != testPassword:
		return 401
	case user == "mika":
		return 0
	case user == "guest":
		return 403
	}
	return 401
}

func (cs *confServer) handler(w http.ResponseWriter, r *http.Request) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.auths = append(cs.auths, r.Header.Get("Authorization"))
	if r.Method == "POST" && r.URL.Path == "/" {
		cs.jobAsks++
		m := ipptest.NewWire(0)
		if cs.printing > 0 {
			cs.printing--
			m.Group(ipp.GroupJob).Num(ipp.TagInteger, "job-id", 7).Num(ipp.TagEnum, "job-state", 5)
		}
		m.Group(ipp.GroupJob).Num(ipp.TagInteger, "job-id", 8).Num(ipp.TagEnum, "job-state", 3)
		reply(w, m)
		return
	}
	if r.URL.Path != "/admin/conf/cupsd.conf" {
		w.WriteHeader(404)
		return
	}
	if cs.status != 0 {
		w.WriteHeader(cs.status)
		return
	}
	if r.Method == "PUT" {
		cs.putAuths = append(cs.putAuths, r.Header.Get("Authorization"))
	}
	if code := refusal(r); code != 0 {
		w.WriteHeader(code)
		return
	}
	switch r.Method {
	case "GET":
		cs.gets.Add(1)
		if cs.down > 0 {
			cs.down--
			w.WriteHeader(503)
			return
		}
		io.WriteString(w, cs.conf)
	case "PUT":
		body, _ := io.ReadAll(r.Body)
		cs.puts = append(cs.puts, string(body))
		cs.types = append(cs.types, r.Header.Get("Content-Type"))
		cs.conf = string(body)
		cs.down = 2
		w.WriteHeader(201)
	default:
		w.WriteHeader(405)
	}
}

func fastPolling(t *testing.T) {
	t.Helper()
	oldEvery, oldFor := logPollEvery, logPollFor
	logPollEvery, logPollFor = 5*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { logPollEvery, logPollFor = oldEvery, oldFor })
}

func TestLogLevelReadsTheConfig(t *testing.T) {
	cs := &confServer{conf: "ErrorLog stderr\nLogLevel info\n"}
	c := New(fake(t, cs.handler), "mika")
	got, err := c.LogLevel(context.Background())
	if err != nil || got != "info" {
		t.Errorf("LogLevel = %q, %v", got, err)
	}
	if len(cs.auths) == 0 || cs.auths[0] != "PeerCred mika" {
		t.Errorf("Authorization = %q", cs.auths)
	}
	cs.conf = "ErrorLog stderr\n"
	if got, err := c.LogLevel(context.Background()); err != nil || got != "warn" {
		t.Errorf("no line: LogLevel = %q, %v", got, err)
	}
}

// Root needs no password: CUPS takes PeerCred root for the PUT, as under
// "make dev" in the harness.
func TestSetLogLevelPutsOnlyTheLineAndWaitsForCUPS(t *testing.T) {
	fastPolling(t)
	cs := &confServer{conf: "# keep me\nLogLevel warn\nListen /run/cups/cups.sock\n"}
	c := New(fake(t, cs.handler), "root")
	if err := c.SetLogLevel(context.Background(), "debug", ""); err != nil {
		t.Fatal(err)
	}
	want := "# keep me\nLogLevel debug\nListen /run/cups/cups.sock\n"
	if len(cs.puts) != 1 || cs.puts[0] != want {
		t.Errorf("PUT bodies = %q, want %q", cs.puts, want)
	}
	if len(cs.types) == 0 || cs.types[0] != "text/plain" {
		t.Errorf("Content-Type = %q", cs.types)
	}
	if cs.down != 0 {
		t.Errorf("returned while CUPS was still down for %d more requests", cs.down)
	}
	for _, a := range cs.auths {
		if a != "PeerCred root" {
			t.Errorf("Authorization = %q", a)
		}
	}
	if cs.jobAsks == 0 {
		t.Error("CUPS was not asked whether a job is printing")
	}
}

func TestSetLogLevelSendsThePassword(t *testing.T) {
	fastPolling(t)
	cs := &confServer{conf: "LogLevel warn\n"}
	c := New(fake(t, cs.handler), "mika")
	if err := c.SetLogLevel(context.Background(), "info", testPassword); err != nil {
		t.Fatal(err)
	}
	if len(cs.puts) != 1 || cs.puts[0] != "LogLevel info\n" {
		t.Errorf("PUT bodies = %q", cs.puts)
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("mika:"+testPassword))
	if len(cs.putAuths) != 1 || cs.putAuths[0] != basic {
		t.Errorf("PUT Authorization = %q, want %q", cs.putAuths, basic)
	}
	// Everything else is PeerCred: the password goes with the PUT alone.
	puts := 0
	for _, a := range cs.auths {
		switch a {
		case basic:
			puts++
		case "PeerCred mika":
		default:
			t.Errorf("Authorization = %q", a)
		}
	}
	if puts != 1 || len(cs.auths) < 3 {
		t.Errorf("%d requests with the password among %q", puts, cs.auths)
	}
}

func TestSetLogLevelWithoutPasswordAsks(t *testing.T) {
	fastPolling(t)
	cs := &confServer{conf: "LogLevel warn\n"}
	err := New(fake(t, cs.handler), "mika").SetLogLevel(context.Background(), "debug", "")
	if !errors.Is(err, ErrNeedsPassword) || err.Error() != "CUPS changes cupsd.conf only with your password." {
		t.Errorf("err = %v", err)
	}
	if len(cs.putAuths) != 1 || cs.putAuths[0] != "PeerCred mika" || cs.conf != "LogLevel warn\n" {
		t.Errorf("PUT auths %q, conf %q", cs.putAuths, cs.conf)
	}
}

func TestWrongPassword(t *testing.T) {
	fastPolling(t)
	cs := &confServer{conf: "LogLevel warn\n"}
	err := New(fake(t, cs.handler), "mika").SetLogLevel(context.Background(), "debug", "hunter3-skra")
	want := "CUPS did not accept the password. Check it and try again; after several wrong tries the system may lock the account for a while."
	if !errors.Is(err, ErrWrongPassword) || err.Error() != want {
		t.Errorf("err = %v", err)
	}
	if cs.conf != "LogLevel warn\n" || cs.jobAsks != 0 {
		t.Errorf("conf %q, job asks %d", cs.conf, cs.jobAsks)
	}
}

func TestNotInSystemGroup(t *testing.T) {
	fastPolling(t)
	cs := &confServer{conf: "LogLevel warn\n"}
	err := New(fake(t, cs.handler), "guest").SetLogLevel(context.Background(), "debug", testPassword)
	if !errors.Is(err, ErrNotSystemGroup) || err.Error() != "CUPS refused this: only members of its system group may change cupsd.conf." {
		t.Errorf("err = %v", err)
	}
	if cs.conf != "LogLevel warn\n" {
		t.Errorf("conf %q", cs.conf)
	}
}

func TestCheckPassword(t *testing.T) {
	for _, tc := range []struct {
		user, password, want string
	}{
		{"mika", strings.Repeat("p", 250), ""},
		{"mika", strings.Repeat("p", 251), "The password is longer than CUPS accepts (at most 250 bytes for this user). Use cupsctl in a terminal instead."},
		{"root", "", ""},
		{"mi:ka", "", ""},
		{"mi:ka", testPassword, "the user name has a colon, which a Basic login cannot carry"},
		{strings.Repeat("u", 300), "p", "The password is longer than CUPS accepts (at most 0 bytes for this user). Use cupsctl in a terminal instead."},
	} {
		err := CheckPassword(tc.user, tc.password)
		if got := fmt.Sprint(err); (tc.want == "" && err != nil) || (tc.want != "" && got != tc.want) {
			t.Errorf("CheckPassword(%d-byte user, %d bytes) = %v, want %q", len(tc.user), len(tc.password), err, tc.want)
		}
	}
	if n := MaxPassword("mika"); n != 250 {
		t.Errorf("MaxPassword(mika) = %d", n)
	}
}

func TestSetLogLevelRefusesALongPassword(t *testing.T) {
	cs := &confServer{conf: "LogLevel warn\n"}
	err := New(fake(t, cs.handler), "mika").SetLogLevel(context.Background(), "debug", strings.Repeat("p", 251))
	if err == nil || !strings.Contains(err.Error(), "at most 250 bytes") || len(cs.auths) != 0 {
		t.Errorf("err = %v, requests %q", err, cs.auths)
	}
}

func TestSetLogLevelRefusesAColonInTheUser(t *testing.T) {
	cs := &confServer{conf: "LogLevel warn\n"}
	err := New(fake(t, cs.handler), "mi:ka").SetLogLevel(context.Background(), "debug", testPassword)
	if err == nil || strings.Contains(err.Error(), testPassword) {
		t.Errorf("err = %v", err)
	}
	if len(cs.putAuths) != 0 {
		t.Errorf("PUT sent: %q", cs.putAuths)
	}
}

// A job that is printing holds the reload of the scheduler back, so cupsd.conf
// says the new level before CUPS uses it.
func TestSetLogLevelWaitsForThePrintingJob(t *testing.T) {
	fastPolling(t)
	cs := &confServer{conf: "LogLevel warn\n", printing: 3}
	if err := New(fake(t, cs.handler), "root").SetLogLevel(context.Background(), "info", ""); err != nil {
		t.Fatal(err)
	}
	if cs.printing != 0 || cs.jobAsks != 4 {
		t.Errorf("returned with %d printing answers left, after %d asks", cs.printing, cs.jobAsks)
	}

	cs = &confServer{conf: "LogLevel warn\n", printing: 1 << 20}
	err := New(fake(t, cs.handler), "root").SetLogLevel(context.Background(), "info", "")
	want := "cupsd.conf sets the new level; CUPS starts using it once the job that is printing is done."
	if !errors.Is(err, ErrReloadWaits) || err.Error() != want {
		t.Errorf("err = %v", err)
	}
}

func TestSetLogLevelSkipsAPutWhenNothingChanges(t *testing.T) {
	fastPolling(t)
	cs := &confServer{conf: "LogLevel info\n"}
	c := New(fake(t, cs.handler), "mika")
	if err := c.SetLogLevel(context.Background(), "info", testPassword); err != nil {
		t.Fatal(err)
	}
	if len(cs.puts) != 0 {
		t.Errorf("PUT %q although the level was set already", cs.puts)
	}
	cs = &confServer{conf: "ErrorLog stderr\n"} // no line means warn
	c = New(fake(t, cs.handler), "mika")
	if err := c.SetLogLevel(context.Background(), "warn", ""); err != nil || len(cs.puts) != 0 {
		t.Errorf("err %v, PUT %q", err, cs.puts)
	}
}

func TestSetLogLevelRefusesOtherLevelsWithoutCUPS(t *testing.T) {
	var asked atomic.Int32
	c := New(fake(t, func(http.ResponseWriter, *http.Request) { asked.Add(1) }), "mika")
	for _, level := range []string{"", "error", "debug2", "none", "DEBUG ", "warn\nListen *:631"} {
		if err := c.SetLogLevel(context.Background(), level, testPassword); err == nil {
			t.Errorf("level %q accepted", level)
		}
	}
	if asked.Load() != 0 {
		t.Errorf("CUPS was asked %d times", asked.Load())
	}
}

// awayAfterPut answers like cs until the first PUT and then never gives
// cupsd.conf back.
func awayAfterPut(t *testing.T, cs *confServer) string {
	return fake(t, func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		put := len(cs.puts) > 0
		cs.mu.Unlock()
		if r.Method == "GET" && put {
			w.WriteHeader(503) // the scheduler never comes back
			return
		}
		cs.handler(w, r)
	})
}

func TestSetLogLevelGivesUpWhenCUPSStaysAway(t *testing.T) {
	fastPolling(t)
	cs := &confServer{conf: "LogLevel warn\n"}
	start := time.Now()
	err := New(awayAfterPut(t, cs), "root").SetLogLevel(context.Background(), "debug", "")
	if !errors.Is(err, ErrNotBack) || err.Error() != "CUPS has not come back yet; look again in a moment." {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Errorf("gave up after %v", time.Since(start))
	}
}

func TestSetLogLevelStopsWhenTheContextEnds(t *testing.T) {
	old := logPollFor
	logPollFor = time.Minute
	t.Cleanup(func() { logPollFor = old })
	oldEvery := logPollEvery
	logPollEvery = 5 * time.Millisecond
	t.Cleanup(func() { logPollEvery = oldEvery })
	cs := &confServer{conf: "LogLevel warn\n"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := New(awayAfterPut(t, cs), "root").SetLogLevel(ctx, "debug", "")
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNotBack) {
		t.Errorf("err = %v, want the deadline of the context", err)
	}
	if !errors.Is(err, ErrBusy) || err.Error() != ErrBusy.Error() {
		t.Errorf("err = %q, want it to read as ErrBusy", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

// The answer to the PUT is read to a bound only, so a CUPS that keeps
// talking does not hold the change.
func TestSetLogLevelBoundsTheAnswerToThePut(t *testing.T) {
	fastPolling(t)
	cs := &confServer{conf: "LogLevel warn\n"}
	stop := make(chan struct{})
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			cs.handler(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.puts, cs.conf = append(cs.puts, string(body)), string(body)
		cs.mu.Unlock()
		w.WriteHeader(201)
		chunk := []byte(strings.Repeat("x", 4096))
		for {
			select {
			case <-stop:
				return
			case <-r.Context().Done():
				return
			default:
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	t.Cleanup(func() { close(stop) }) // runs before the fake closes, which waits for the handler
	done := make(chan error, 1)
	go func() { done <- New(sock, "root").SetLogLevel(context.Background(), "debug", "") }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetLogLevel is still reading the answer to the PUT")
	}
}

func TestLogLevelMapsHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{401, "CUPS refused the login"},
		{403, "CUPS refused this"},
		{500, "CUPS answered HTTP 500"},
	} {
		cs := &confServer{conf: "LogLevel warn\n", status: tc.status}
		c := New(fake(t, cs.handler), "mika")
		if _, err := c.LogLevel(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("GET %d: err = %v, want %q", tc.status, err, tc.want)
		}
		if err := c.SetLogLevel(context.Background(), "debug", testPassword); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("SetLogLevel %d: err = %v, want %q", tc.status, err, tc.want)
		}
	}
	// A PUT that fails otherwise, with the GET allowed.
	var puts atomic.Int32
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			puts.Add(1)
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, "LogLevel warn\n")
	})
	err := New(sock, "mika").SetLogLevel(context.Background(), "debug", testPassword)
	if err == nil || err.Error() != "CUPS answered HTTP 500" || puts.Load() != 1 {
		t.Errorf("failed PUT: err = %v, puts %d", err, puts.Load())
	}
}

func TestLogLevelCapsTheConfig(t *testing.T) {
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "LogLevel debug\n")
		io.WriteString(w, strings.Repeat("# filler\n", 200000)) // 1.8 MB
	})
	_, err := New(sock, "mika").LogLevel(context.Background())
	if err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Errorf("err = %v", err)
	}
}

func TestLogLevelWithoutUserOrSocket(t *testing.T) {
	if _, err := New("/nonexistent/skra.sock", "").LogLevel(context.Background()); err == nil || !strings.Contains(err.Error(), "no user configured") {
		t.Errorf("err = %v", err)
	}
	_, err := New("/nonexistent/skra.sock", "mika").LogLevel(context.Background())
	if !errors.Is(err, ErrNotRunning) || err.Error() != ErrNotRunning.Error() {
		t.Errorf("err = %v, want ErrNotRunning", err)
	}
}
