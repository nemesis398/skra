// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// Stubs: the tests for the CUPS log come first and fail on these until the
// implementation of Task 12, Step 3, replaces them.

// ErrJournalNotReadable is what ReadLog answers when the journal cannot be read.
var ErrJournalNotReadable = errors.New("The journal is not readable for this user.")

// ErrNotBack is what SetLogLevel answers when CUPS does not come back in time.
var ErrNotBack = errors.New("CUPS has not come back yet; look again in a moment.")

const confPath = "/admin/conf/cupsd.conf"

var errStub = errors.New("not implemented")

type cmdResult struct {
	Stdout, Stderr string
	ExitCode       int
}

var (
	runJournal     = func(ctx context.Context, argv []string) (cmdResult, error) { return cmdResult{}, errStub }
	journalTimeout = 5 * time.Second
	maxJournal     = 2 << 20
	logPollEvery   = 500 * time.Millisecond
	logPollFor     = 10 * time.Second
)

type tailBuffer struct {
	max int
	buf []byte
	cut bool
}

func (t *tailBuffer) Write(p []byte) (int, error) { return len(p), nil }
func (t *tailBuffer) String() string              { return "" }
func (t *tailBuffer) lines() []string             { return nil }

func runCommand(ctx context.Context, argv []string) (cmdResult, error) { return cmdResult{}, errStub }

func replaceLogLevel(conf, level string) string { return conf }
func parseLogLevel(conf string) string          { return "" }

func httpFailure(code int) error { return errStub }

func (c *Client) raw(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return nil, errStub
}

func (c *Client) readConf(ctx context.Context) (string, error)            { return "", errStub }
func (c *Client) waitForLogLevel(ctx context.Context, level string) error { return errStub }

// ReadLog returns the last lines of the CUPS journal, oldest first.
func ReadLog(ctx context.Context, lines int) ([]string, error) { return nil, errStub }

// LogLevel is the log level of cupsd.conf.
func (c *Client) LogLevel(ctx context.Context) (string, error) { return "", errStub }

// SetLogLevel sets the LogLevel of cupsd.conf through CUPS.
func (c *Client) SetLogLevel(ctx context.Context, level string) error { return errStub }
