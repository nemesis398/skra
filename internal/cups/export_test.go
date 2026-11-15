// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"context"
	"net/http"
	"strings"
)

// These let the harness tests, which live in package cups_test because testenv
// imports cups, see the cupsd.conf text.

func (c *Client) ReadConf(ctx context.Context) (string, error) { return c.readConf(ctx) }

// WriteConf puts text as cupsd.conf, as it is, and waits until CUPS reports
// the level that text sets.
func (c *Client) WriteConf(ctx context.Context, text string) error {
	resp, err := c.raw(ctx, http.MethodPut, confPath, strings.NewReader(text))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return httpFailure(resp.StatusCode)
	}
	return c.waitForLogLevel(ctx, parseLogLevel(text))
}

var ReplaceLogLevel = replaceLogLevel
