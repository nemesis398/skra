// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web holds the page templates and static files embedded in the binary.
package web

import "embed"

// Assets is the templates/ and static/ tree.
//
//go:embed templates static
var Assets embed.FS
