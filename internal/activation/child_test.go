// SPDX-License-Identifier: AGPL-3.0-or-later

package activation

import (
	"os"
	"os/exec"
	"testing"
)

// runChild re-runs this test binary with f as fd 3. LISTEN_PID has to be the
// child's own pid, which only the child knows, so the child pid is not
// available before Start; a shell wrapper sets it from $$ after exec.
func runChild(t *testing.T, f *os.File) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c",
		`LISTEN_PID=$$ LISTEN_FDS=1 SKRA_ACTIVATION_CHILD=1 exec "$0" -test.run '^TestListenerSocketActivated$'`,
		os.Args[0])
	cmd.ExtraFiles = []*os.File{f}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}
