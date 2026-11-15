// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cups talks IPP to the local CUPS daemon over its unix socket,
// authenticated by peer credentials.
package cups

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/nemesis398/skra/internal/ipp"
)

// maxResponse caps what Do reads from CUPS. A queue list with every media
// size is a few hundred kilobytes at most.
const maxResponse = 32 << 20

// headerTimeout bounds the wait for CUPS to start answering, counted from the
// end of the request body, so a streamed upload is not cut short. A variable so
// a test can lower it. It is 45 s because CUPS-Get-Devices waits out its own
// 10 s timeout and adding a driverless printer makes cupsd query it first.
var headerTimeout = 45 * time.Second

// dialTimeout bounds the connect to the socket.
const dialTimeout = 5 * time.Second

// idleConnTimeout closes a kept-alive connection to cupsd that nothing used
// for that long. An open connection keeps Fedora's on-demand cupsd from
// exiting when idle; a poll every 2 s while a page is open still reuses it.
const idleConnTimeout = 30 * time.Second

// Client is a CUPS client for one socket and one user.
type Client struct {
	user string
	http *http.Client
}

// New returns a client that dials the unix socket and acts as user. CUPS
// checks the name against the socket peer, so it has to be the user the
// process runs as.
func New(socket, user string) *Client {
	d := net.Dialer{Timeout: dialTimeout}
	return &Client{user: user, http: &http.Client{Transport: &http.Transport{
		ResponseHeaderTimeout: headerTimeout,
		IdleConnTimeout:       idleConnTimeout,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

// Do posts req to the CUPS resource at path ("/", "/admin/", "/printers/NAME")
// and decodes the answer. It adds requesting-user-name to the operation group
// and sends Authorization: PeerCred up front, so each call is one request
// with no retry. ctx should carry a deadline for the whole call: the client
// stops waiting for CUPS to start answering after 45 s and for the connect
// after 5 s, but it sets no limit on the body, so a long upload or a stalled
// read is bounded only by ctx. When doc is not nil it is streamed after the request.
//
// A reply with an IPP error status comes back as both the decoded response
// and its *ipp.StatusError. Req must not be reused: a second Do would add the
// user name again.
func (c *Client) Do(ctx context.Context, path string, req *ipp.Request, doc io.Reader) (*ipp.Response, error) {
	if c.user == "" {
		return nil, errors.New("no user configured")
	}
	var body io.Reader = bytes.NewReader(req.Op(ipp.Name("requesting-user-name", c.user)).Encode())
	if doc != nil {
		body = io.MultiReader(body, doc)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+path, body)
	if err != nil {
		return nil, fmt.Errorf("CUPS request: %w", err)
	}
	hr.Header.Set("Content-Type", "application/ipp")
	hr.Header.Set("Authorization", "PeerCred "+c.user)
	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("CUPS does not answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpFailure(resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("CUPS does not answer: %w", err)
	}
	res, err := ipp.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("CUPS sent an unreadable answer: %w", err)
	}
	return res, res.Err()
}

// httpFailure is the error for an HTTP status that is not the success of the
// request: the two refusals CUPS gives for the login and for the right, and
// the bare status for anything else.
func httpFailure(code int) error {
	switch code {
	case http.StatusUnauthorized:
		return errors.New("CUPS refused the login: it did not accept this user over the local socket")
	case http.StatusForbidden:
		return errors.New("CUPS refused this: the user may not do it (403)")
	}
	return fmt.Errorf("CUPS answered HTTP %d", code)
}

// raw sends a plain HTTP request to the CUPS resource at path, over the same
// socket and with the same PeerCred login as Do. The caller closes the body of
// the response and checks its status. A body is sent as text/plain, which is
// what CUPS takes for a configuration file. Do stays IPP only.
func (c *Client) raw(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	if c.user == "" {
		return nil, errors.New("no user configured")
	}
	hr, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, body)
	if err != nil {
		return nil, fmt.Errorf("CUPS request: %w", err)
	}
	if body != nil {
		hr.Header.Set("Content-Type", "text/plain")
	}
	hr.Header.Set("Authorization", "PeerCred "+c.user)
	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("CUPS does not answer: %w", err)
	}
	return resp, nil
}
