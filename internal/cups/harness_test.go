// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package cups_test

import (
	"context"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/testenv"
)

// TestHarnessPrinters checks that the harness CUPS has its virtual printer
// queue, idle, accepting jobs and reporting its four inks.
func TestHarnessPrinters(t *testing.T) {
	c := testenv.Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ps, err := c.Printers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		t.Logf("%s: state %d, accepting %v, markers %d", p.Name, p.State, p.Accepting, len(p.Markers))
		for _, m := range p.Markers {
			t.Logf("  marker name=%q color=%s type=%s level=%d low=%d", m.Name, m.Color, m.Type, m.Level, m.Low)
		}
		if p.Name != "Virtual-MFC" {
			continue
		}
		// Idle or processing: other packages print on Virtual-MFC at the
		// same time. Only stopped (5) means the queue is broken.
		if p.State != 3 && p.State != 4 {
			t.Errorf("state = %d, want 3 (idle) or 4 (processing)", p.State)
		}
		if !p.Accepting {
			t.Error("Virtual-MFC does not accept jobs")
		}
		// The values are the ones in dev/harness/printer.attrs.
		want := []struct {
			name       string
			level, low int
		}{{"Black", 68, 10}, {"Cyan", 48, 10}, {"Magenta", 56, 10}, {"Yellow", 12, 10}}
		if len(p.Markers) != len(want) {
			t.Fatalf("markers = %d, want %d (Black, Cyan, Magenta, Yellow)", len(p.Markers), len(want))
		}
		for i, w := range want {
			if m := p.Markers[i]; m.Name != w.name || m.Level != w.level || m.Low != w.low {
				t.Errorf("marker %d = %q level %d low %d, want %q level %d low %d", i, m.Name, m.Level, m.Low, w.name, w.level, w.low)
			}
		}
		return
	}
	t.Fatalf("no Virtual-MFC among %d queues", len(ps))
}
