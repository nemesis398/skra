// SPDX-License-Identifier: AGPL-3.0-or-later

// Package testenv gives integration tests a CUPS client for the harness
// container, so no test ever touches the laptop's own CUPS.
package testenv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nemesis398/skra/internal/cups"
)

// user is root because the host user maps to root inside the rootless
// container, and CUPS checks the PeerCred name against the socket peer.
const user = "root"

// Socket returns the harness socket path under the repository root.
func Socket(tb testing.TB) string {
	tb.Helper()
	dir, err := os.Getwd()
	if err != nil {
		tb.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, ".dev", "run", "cups.sock")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			tb.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// Client returns a client for the harness CUPS. It skips the test when the
// harness socket does not exist; "make integration" starts the harness.
func Client(tb testing.TB) *cups.Client {
	tb.Helper()
	sock := Socket(tb)
	if _, err := os.Stat(sock); err != nil {
		tb.Skipf("harness is not running (%s: %v); run \"make harness-up\"", sock, err)
	}
	return cups.New(sock, user)
}
