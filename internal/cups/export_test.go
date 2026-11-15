// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import "context"

// These let the harness tests, which live in package cups_test because testenv
// imports cups, see the cupsd.conf text.

func (c *Client) ReadConf(ctx context.Context) (string, error) { return c.readConf(ctx) }

// WriteConf puts text as cupsd.conf, as it is, with PeerCred, and waits until
// CUPS reports the level that text sets. Only a root client gets through.
func (c *Client) WriteConf(ctx context.Context, text string) error {
	if err := c.putConf(ctx, text, ""); err != nil {
		return err
	}
	return c.waitForLogLevel(ctx, parseLogLevel(text))
}

var ReplaceLogLevel = replaceLogLevel
