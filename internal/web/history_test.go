// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"strings"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/cups"
)

func historyFake() *fakeCUPS {
	now := time.Now()
	f := &fakeCUPS{printers: []cups.Printer{brother()}, def: "Virtual-MFC"}
	for i := 1; i <= 8; i++ {
		f.done = append(f.done, cups.Job{ID: 100 - i, Name: "Doc" + string(rune('A'+i)) + ".pdf", Printer: "Virtual-MFC",
			State: 9, Sheets: i, Completed: now.Add(-time.Duration(i) * time.Hour)})
	}
	f.done = append(f.done,
		cups.Job{ID: 50, Name: "Broken.pdf", Printer: "Virtual-MFC", State: 8, Message: "paper jam", Completed: now.Add(-30 * time.Hour)},
		cups.Job{ID: 49, Name: "Dropped.pdf", Printer: "Virtual-MFC", State: 7, Completed: now.Add(-31 * time.Hour)})
	return f
}

func TestHistoryPageListsEveryCompletedJob(t *testing.T) {
	rec := get(serverFor(historyFake()).Handler(), "/history")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="tab on" id="tab-history" href="/history"`, `id="tab-overview" href="/"`,
		`class="brand"`, `id="need"`, `id="foot"`,
		`id="history"`, "the last 10 jobs CUPS keeps",
		"DocB.pdf", "DocI.pdf", // all eight, not the overview's five
		"Broken.pdf", "failed · paper jam", `class="off">canceled`,
		`data-act="reprint" data-job="92"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("history page lacks %q", want)
		}
	}
	if strings.Contains(body, `id="recent"`) || strings.Contains(body, `id="queue"`) {
		t.Error("history page carries overview fragments")
	}
	if n := strings.Count(body, `data-act="reprint"`); n != 10 {
		t.Errorf("%d reprint buttons, want 10", n)
	}
}

func TestHistoryPageWithoutJobs(t *testing.T) {
	rec := get(serverFor(&fakeCUPS{printers: []cups.Printer{brother()}}).Handler(), "/history")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "CUPS keeps no finished jobs") {
		t.Errorf("status %d, body lacks the empty text", rec.Code)
	}
}

func TestOtherPagesLinkHistoryTab(t *testing.T) {
	h := serverFor(historyFake()).Handler()
	for _, p := range []string{"/", "/printers"} {
		body := get(h, p).Body.String()
		if !strings.Contains(body, `class="tab" id="tab-history" href="/history"`) {
			t.Errorf("%s lacks the History link", p)
		}
	}
}

func TestEventsPageHistorySendsOnlyItsFragments(t *testing.T) {
	s := serverFor(historyFake())
	snap := s.poll(t.Context(), true)
	names := func(fr []fragment) (out []string) {
		for _, f := range fr {
			out = append(out, f.event)
		}
		return
	}
	if got := strings.Join(names(s.fragmentsFor("history", snap)), ","); got != "history,need,foot" {
		t.Errorf("history fragments = %s", got)
	}
	if got := strings.Join(names(s.fragmentsFor("", snap)), ","); !strings.Contains(got, "recent") || strings.Contains(got, "history") {
		t.Errorf("overview fragments = %s", got)
	}
}
