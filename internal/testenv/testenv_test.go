// SPDX-License-Identifier: AGPL-3.0-or-later

package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// recorder is a testing.TB that notes whether it was skipped or failed
// instead of ending the test, so requireSocket's choice can be checked.
type recorder struct {
	testing.TB
	skipped, failed bool
}

func (r *recorder) Helper() {}
func (r *recorder) Skipf(format string, args ...any) {
	r.skipped = true
	_ = fmt.Sprintf(format, args...)
}
func (r *recorder) Fatalf(format string, args ...any) {
	r.failed = true
	_ = fmt.Sprintf(format, args...)
}

func TestRequireSocket(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		sock          string
		skip, failure bool
	}{
		{"missing socket skips", filepath.Join(dir, "none"), true, false},
		{"present path goes on", file, false, false},
		// ENOTDIR is not "does not exist": the harness directory is broken.
		{"other Stat error fails", filepath.Join(file, "cups.sock"), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &recorder{TB: t}
			requireSocket(r, tt.sock)
			if r.skipped != tt.skip || r.failed != tt.failure {
				t.Errorf("skipped %v, failed %v; want skipped %v, failed %v", r.skipped, r.failed, tt.skip, tt.failure)
			}
		})
	}
}
