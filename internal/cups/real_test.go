// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build realcups

package cups

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestRealSocketReadOnly asks the laptop's own CUPS for its queues and its
// default. It calls only Printers and Default and changes nothing.
func TestRealSocketReadOnly(t *testing.T) {
	user := os.Getenv("USER")
	if user == "" {
		t.Skip("USER is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := New("/run/cups/cups.sock", user)
	ps, err := c.Printers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	def, err := c.Default(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("default: %q", def)
	for _, p := range ps {
		t.Logf("%s: state %d, accepting %v, markers %d, changed %v", p.Name, p.State, p.Accepting, len(p.Markers), p.MarkersAt)
		for _, m := range p.Markers {
			t.Logf("  marker name=%q color=%s type=%s level=%d low=%d", m.Name, m.Color, m.Type, m.Level, m.Low)
		}
	}
	for _, p := range ps {
		if p.Name == "Brother-MFC-J6975DW" && len(p.Markers) == 4 {
			return
		}
	}
	t.Errorf("no Brother-MFC-J6975DW with four markers in %d queues", len(ps))
}
