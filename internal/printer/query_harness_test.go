// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package printer

import (
	"context"
	"testing"

	"github.com/nemesis398/skra/internal/testenv"
)

// TestQueryHarnessPrinter asks the harness's ippeveprinter directly, the way
// the host-side service does: it is published on 127.0.0.1:8631.
func TestQueryHarnessPrinter(t *testing.T) {
	testenv.Client(t) // skips when the harness is not running
	live := Query(context.Background(), "ipp://localhost:8631/ipp/print", nil)
	if !live.Reachable {
		t.Skip("ippeveprinter does not answer on 127.0.0.1:8631 (make printer-start)")
	}
	if len(live.Markers) != 4 || live.Markers[0].Name != "Black" || live.Markers[0].Level != 68 {
		t.Errorf("markers = %+v", live.Markers)
	}
}
