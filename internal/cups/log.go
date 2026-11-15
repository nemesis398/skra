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

func (t *tailBuffer) lines() []string { return splitLines(t.complete()) }

// --- the LogLevel in cupsd.conf ---

// logDirective reads one line of cupsd.conf. It answers the text before the
// directive (leading whitespace) and what follows its value, when the line is
// an active LogLevel directive, whatever the case of its name.
func logDirective(line string) (indent, value string, ok bool) {
	rest := strings.TrimLeft(line, " \t")
	indent = line[:len(line)-len(rest)]
	if len(rest) < len("LogLevel") || !strings.EqualFold(rest[:len("LogLevel")], "LogLevel") {
		return "", "", false
	}
	rest = rest[len("LogLevel"):]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return "", "", false // another directive that starts the same
	}
	rest, _, _ = strings.Cut(rest, "#")
	return indent, strings.TrimSpace(rest), true
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
		if _, v, ok := logDirective(line); ok && v != "" {
			level, _, _ = strings.Cut(strings.ToLower(v), " ")
		}
	})
	return level
}

// replaceLogLevel sets every active LogLevel line of conf to level and leaves
// every other byte as it was: comments, line ends, and a missing final line
// end. A conf without such a line gets one at the end.
func replaceLogLevel(conf, level string) string {
	var b strings.Builder
	found := false
	eachLine(conf, func(line, end string) {
		if indent, _, ok := logDirective(line); ok {
			line, found = indent+"LogLevel "+level, true
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
	resp, err := c.raw(ctx, http.MethodGet, confPath, nil)
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
// CUPS itself and returns once CUPS has restarted its scheduler and reports
// the level, or with ErrNotBack when that takes more than ten seconds. Only
// the LogLevel line changes. A level that is set already is not written, so
// the scheduler does not restart for nothing.
func (c *Client) SetLogLevel(ctx context.Context, level string) error {
	switch level {
	case "warn", "info", "debug":
	default:
		return fmt.Errorf("%q is not a log level: use warn, info or debug", level)
	}
	conf, err := c.readConf(ctx)
	if err != nil {
		return err
	}
	if parseLogLevel(conf) == level {
		return nil
	}
	resp, err := c.raw(ctx, http.MethodPut, confPath, strings.NewReader(replaceLogLevel(conf, level)))
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
	default:
		return httpFailure(resp.StatusCode)
	}
	return c.waitForLogLevel(ctx, level)
}

// waitForLogLevel looks at cupsd.conf every logPollEvery until CUPS answers
// and reports level. CUPS restarts its scheduler after a change of its
// configuration, so the first looks may find it away; the first look waits
// one interval so it is not made before the restart has begun.
func (c *Client) waitForLogLevel(ctx context.Context, level string) error {
	pctx, cancel := context.WithTimeout(ctx, logPollFor)
	defer cancel()
	tick := time.NewTicker(logPollEvery)
	defer tick.Stop()
	for {
		select {
		case <-pctx.Done():
			if ctx.Err() != nil {
				return fmt.Errorf("CUPS does not answer: %w", ctx.Err())
			}
			return ErrNotBack
		case <-tick.C:
		}
		actx, acancel := context.WithTimeout(pctx, logPollTry)
		got, err := c.LogLevel(actx)
		acancel()
		if err == nil && got == level {
			return nil
		}
	}
}
