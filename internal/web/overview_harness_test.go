// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package web

import (
	"strings"
	"testing"

	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/testenv"
)

// TestOverviewAgainstHarness draws the overview from the harness CUPS: the
// virtual queue, its four inks and no failure in the footer.
func TestOverviewAgainstHarness(t *testing.T) {
	c := testenv.Client(t)
	s := New(config.Config{Listen: "127.0.0.1:7631", User: "root"}, Deps{CUPS: c}, NewToken())
	rec := get(s.Handler(), "/")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Virtual-MFC", `<span class="k">B</span>`, `<span class="k">Y</span>`, "CUPS answers"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	if strings.Contains(body, "CUPS does not answer") {
		t.Error("the harness CUPS did not answer")
	}
}
