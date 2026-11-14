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
		if p.State != 3 {
			t.Errorf("state = %d, want 3 (idle)", p.State)
		}
		if !p.Accepting {
			t.Error("Virtual-MFC does not accept jobs")
		}
		if len(p.Markers) != 4 {
			t.Errorf("markers = %d, want 4 (Black, Cyan, Magenta, Yellow)", len(p.Markers))
		}
		return
	}
	t.Fatalf("no Virtual-MFC among %d queues", len(ps))
}
