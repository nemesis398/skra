// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrJournalNotReadable is what ReadLog answers when the journal cannot be read
// by this user, or journalctl is not installed.
var ErrJournalNotReadable = errors.New("The journal is not readable for this user.")

// ErrNotBack is what SetLogLevel answers when CUPS does not report the new
// level within logPollFor of the change: it is still restarting, or it did not
// take the file.
var ErrNotBack = errors.New("CUPS has not come back yet; look again in a moment.")

// The refusals of a change of cupsd.conf. CUPS takes PeerCred for a PUT from
// root alone (scheduler/auth.c), so any other user sends the login password
// with Basic, and CUPS checks it through PAM and the user against its
// SystemGroup.
var (
	// ErrNeedsPassword is CUPS's 401 to a PUT that carried no password.
	ErrNeedsPassword = errors.New("CUPS changes cupsd.conf only with your password.")
	// ErrWrongPassword is CUPS's 401 to a PUT with a password.
	ErrWrongPassword = errors.New("CUPS did not accept the password. Check it and try again; after several wrong tries the system may lock the account for a while.")
	// ErrNotSystemGroup is CUPS's 403: the login was good, the user is not
	// in the system group.
	ErrNotSystemGroup = errors.New("CUPS refused this: only members of its system group may change cupsd.conf.")
)

// ErrReloadWaits is what SetLogLevel answers when cupsd.conf has the new
// level but a job kept printing all along. CUPS puts off the reload that
// makes it use the file until its printing jobs are done, or until its
// ReloadTimeout, so the level may not be in use yet. The change itself is
// made.
var ErrReloadWaits = errors.New("cupsd.conf sets the new level; CUPS starts using it once the job that is printing is done.")

const (
	// confPath is the resource CUPS serves its configuration file at.
	confPath = "/admin/conf/cupsd.conf"
	// maxConf caps what LogLevel reads of cupsd.conf. A real one is a few kB.
	maxConf = 1 << 20
	// maxStderr caps what is kept of journalctl's error output.
	maxStderr = 16 << 10
	// minLines and maxLines bound how much of the journal ReadLog asks for.
	minLines, maxLines = 1, 1000
	// logPollTry bounds one look at cupsd.conf while CUPS restarts.
	logPollTry = 2 * time.Second
	// maxDrain caps what is read of CUPS's answer to a PUT, which is a short
	// status page.
	maxDrain = 64 << 10
	// maxPassword is the longest password SetLogLevel sends.
	maxPassword = 1024
)

// The journal read is a variable each, so tests can lower or replace them.
var (
	// journalTimeout bounds one run of journalctl.
	journalTimeout = 5 * time.Second
	// maxJournal is the most of journalctl's output that is kept.
	maxJournal = 2 << 20
	// runJournal runs a command and returns what it printed. A command that
	// ran and failed is a result with an exit code; an error means it did not
	// run or was stopped. Tests replace it, so no test runs the real one.
	runJournal = runCommand
	// logPollEvery and logPollFor are how often SetLogLevel looks at CUPS after
	// a change, and for how long.
	logPollEvery = 500 * time.Millisecond
	logPollFor   = 10 * time.Second
)

// cmdResult is the output of a command that ran to its end.
type cmdResult struct {
	Stdout, Stderr string
	ExitCode       int
}

// ReadLog returns the last lines of the CUPS journal, oldest first, so the
// newest line is the last. lines is clamped to 1..1000. It runs journalctl
// without a shell and gives it five seconds. A journal this user may not read
// gives ErrJournalNotReadable.
func ReadLog(ctx context.Context, lines int) ([]string, error) {
	lines = min(max(lines, minLines), maxLines)
	ctx, cancel := context.WithTimeout(ctx, journalTimeout)
	defer cancel()
	res, err := runJournal(ctx, []string{"journalctl", "-u", "cups.service", "-n", strconv.Itoa(lines), "-o", "short-iso", "--no-pager"})
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, ErrJournalNotReadable
	case err != nil && ctx.Err() != nil:
		return nil, fmt.Errorf("journalctl did not answer in time: %w", ctx.Err())
	case err != nil:
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	var out []string
	for _, l := range splitLines(res.Stdout) {
		if l != "-- No entries --" {
			out = append(out, l)
		}
	}
	// Without the right to read, journalctl either fails or shows nothing and
	// says why on stderr.
	if (res.ExitCode != 0 || len(out) == 0) && unreadable(res.Stderr) {
		return nil, ErrJournalNotReadable
	}
	if res.ExitCode != 0 {
		why := strings.TrimSpace(res.Stderr)
		if why == "" {
			why = "exit status " + strconv.Itoa(res.ExitCode)
		}
		return nil, fmt.Errorf("journalctl failed: %s", firstLine(why))
	}
	return out, nil
}

// unreadable reports whether journalctl's error output says the journal is
// closed to this user.
func unreadable(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "permission") || strings.Contains(s, "no journal files") ||
		strings.Contains(s, "not seeing messages")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// splitLines splits text at line ends, dropping the \r of a CRLF and the empty
// piece after the last \n.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// runCommand runs argv[0] with the rest as its arguments, with no shell. It
// keeps the last maxJournal bytes of what it prints, on a line boundary, and
// the end of its error output.
func runCommand(ctx context.Context, argv []string) (cmdResult, error) {
	out, errOut := &tailBuffer{max: maxJournal}, &tailBuffer{max: maxStderr}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.WaitDelay = time.Second // a child that holds the pipes open does not hold us
	err := cmd.Run()
	res := cmdResult{Stdout: out.complete(), Stderr: errOut.String()}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return res, ctx.Err()
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
		return res, nil
	}
	return res, err
}

// tailBuffer is a writer that keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
	cut bool // bytes before the kept ones were dropped
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		t.cut = t.cut || len(p) > t.max || len(t.buf) > 0
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.buf = t.buf[:copy(t.buf, t.buf[over:])]
		t.cut = true
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// complete is the kept text without the line the cut went through.
func (t *tailBuffer) complete() string {
	if !t.cut {
		return string(t.buf)
	}
	_, rest, found := bytes.Cut(t.buf, []byte{'\n'})
	if !found {
		return ""
	}
	return string(rest)
}

// --- the LogLevel in cupsd.conf ---

// logDirective reads one line of cupsd.conf. When the line is an active
// LogLevel directive, whatever the case of its name, it answers the text
// before the directive (leading whitespace), its value, and the inline comment
// after it with the blanks before the "#", or "" when there is none.
func logDirective(line string) (indent, value, comment string, ok bool) {
	rest := strings.TrimLeft(line, " \t")
	indent = line[:len(line)-len(rest)]
	if len(rest) < len("LogLevel") || !strings.EqualFold(rest[:len("LogLevel")], "LogLevel") {
		return "", "", "", false
	}
	rest = rest[len("LogLevel"):]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return "", "", "", false // another directive that starts the same
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest, comment = rest[:i], rest[i:]
		trimmed := strings.TrimRight(rest, " \t")
		comment = rest[len(trimmed):] + comment
	}
	return indent, strings.TrimSpace(rest), comment, true
}

// eachLine calls fn with every line of text, without its line end, and the line
// end that followed it: "\n", "\r\n", or "" for a last line that has none.
func eachLine(text string, fn func(line, end string)) {
	for text != "" {
		line, end := text, ""
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			line, end, text = text[:i], "\n", text[i+1:]
			if strings.HasSuffix(line, "\r") {
				line, end = line[:len(line)-1], "\r\n"
			}
		} else {
			text = ""
		}
		fn(line, end)
	}
}

// parseLogLevel is the level cupsd.conf sets: the last active LogLevel
// directive, in lower case, or warn, which CUPS uses without one.
func parseLogLevel(conf string) string {
	level := "warn"
	eachLine(conf, func(line, _ string) {
		if _, v, _, ok := logDirective(line); ok && v != "" {
			level, _, _ = strings.Cut(strings.ToLower(v), " ")
		}
	})
	return level
}

// replaceLogLevel sets every active LogLevel line of conf to level and leaves
// every other byte as it was: comments, an inline comment on the LogLevel line
// itself, line ends, and a missing final line end. A conf without such a line
// gets one at the end.
func replaceLogLevel(conf, level string) string {
	var b strings.Builder
	found := false
	eachLine(conf, func(line, end string) {
		if indent, _, comment, ok := logDirective(line); ok {
			line, found = indent+"LogLevel "+level+comment, true
		}
		b.WriteString(line)
		b.WriteString(end)
	})
	if found {
		return b.String()
	}
	if conf != "" && !strings.HasSuffix(conf, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("LogLevel " + level + "\n")
	return b.String()
}

// readConf fetches cupsd.conf.
func (c *Client) readConf(ctx context.Context) (string, error) {
	resp, err := c.raw(ctx, http.MethodGet, confPath, nil, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", httpFailure(resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxConf+1))
	if err != nil {
		return "", fmt.Errorf("CUPS does not answer: %w", err)
	}
	if len(data) > maxConf {
		return "", errors.New("cupsd.conf is larger than 1 MiB; skrá does not read it")
	}
	return string(data), nil
}

// LogLevel is the log level of cupsd.conf. It reads the file, which is what
// the next start of the scheduler will use; warn when the file sets none.
func (c *Client) LogLevel(ctx context.Context) (string, error) {
	conf, err := c.readConf(ctx)
	if err != nil {
		return "", err
	}
	return parseLogLevel(conf), nil
}

// SetLogLevel sets the LogLevel of cupsd.conf to warn, info or debug through
// CUPS itself. Only the LogLevel line changes. A level that is set already is
// not written, so the scheduler does not restart for nothing.
//
// cupsd.conf is read with PeerCred. The PUT goes with PeerCred when password
// is empty, which CUPS takes from root alone, and otherwise with Basic and
// the configured user and password; the password is sent with that one
// request and kept nowhere. A refusal is ErrNeedsPassword, ErrWrongPassword
// or ErrNotSystemGroup.
//
// After the PUT it waits until CUPS reports the level with no job printing,
// which is when CUPS has reloaded or reloads at once. It answers ErrNotBack
// when CUPS does not report the level within ten seconds, and ErrReloadWaits
// when it does but a job keeps printing.
func (c *Client) SetLogLevel(ctx context.Context, level, password string) error {
	switch level {
	case "warn", "info", "debug":
	default:
		return fmt.Errorf("%q is not a log level: use warn, info or debug", level)
	}
	if len(password) > maxPassword {
		return errors.New("the password is longer than 1024 bytes")
	}
	if password != "" && strings.Contains(c.user, ":") {
		return errors.New("the user name has a colon, which a Basic login cannot carry")
	}
	conf, err := c.readConf(ctx)
	if err != nil {
		return err
	}
	if parseLogLevel(conf) == level {
		return nil
	}
	if err := c.putConf(ctx, replaceLogLevel(conf, level), password); err != nil {
		return err
	}
	return c.waitForLogLevel(ctx, level)
}

// putConf sends text as cupsd.conf, with password as SetLogLevel says, and
// answers how CUPS took it. It reads at most maxDrain of the answer.
func (c *Client) putConf(ctx context.Context, text, password string) error {
	resp, err := c.raw(ctx, http.MethodPut, confPath, strings.NewReader(text), password)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	case http.StatusUnauthorized:
		if password == "" {
			return ErrNeedsPassword
		}
		return ErrWrongPassword
	case http.StatusForbidden:
		return ErrNotSystemGroup
	}
	return httpFailure(resp.StatusCode)
}

// waitForLogLevel looks at CUPS every logPollEvery until it reports level in
// cupsd.conf and no job printing. CUPS restarts its scheduler after a change
// of its configuration, so the first looks may find it away; the first look
// waits one interval so it is not made before the restart has begun. GET
// serves the file as it is on disk, which says nothing of the reload, and
// CUPS puts the reload off while a job prints: a look with a printing job
// does not count.
func (c *Client) waitForLogLevel(ctx context.Context, level string) error {
	pctx, cancel := context.WithTimeout(ctx, logPollFor)
	defer cancel()
	tick := time.NewTicker(logPollEvery)
	defer tick.Stop()
	held := false // CUPS reported the level while a job was printing
	for {
		select {
		case <-pctx.Done():
			if ctx.Err() != nil {
				return fmt.Errorf("CUPS does not answer: %w", ctx.Err())
			}
			if held {
				return ErrReloadWaits
			}
			return ErrNotBack
		case <-tick.C:
		}
		actx, acancel := context.WithTimeout(pctx, logPollTry)
		got, err := c.LogLevel(actx)
		busy := false
		if err == nil && got == level {
			busy, err = c.printing(actx)
		}
		acancel()
		if err == nil && got == level {
			if !busy {
				return nil
			}
			held = true
		}
	}
}

// printing reports whether CUPS has a job that is printing.
func (c *Client) printing(ctx context.Context) (bool, error) {
	jobs, err := c.Jobs(ctx, "not-completed", 0)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if j.State == 5 {
			return true, nil
		}
	}
	return false, nil
}
