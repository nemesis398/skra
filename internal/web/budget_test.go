// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package web

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// p95 is the 95th percentile of d.
func p95(d []time.Duration) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)*95+99)/100-1]
}

// TestBudgets holds skra to the budgets of the design against the harness
// CUPS: a page under 50 ms at p95, a CUPS action under 150 ms at p95, and
// a page with its style and script under 200 KB.
func TestBudgets(t *testing.T) {
	s, c := harnessServer(t)
	h := s.Handler()

	t.Run("page", func(t *testing.T) {
		var d []time.Duration
		for range 200 {
			start := time.Now()
			rec := get(h, "/")
			d = append(d, time.Since(start))
			if rec.Code != 200 {
				t.Fatalf("GET /: status %d", rec.Code)
			}
		}
		got := p95(d)
		t.Logf("GET / p95 = %v over %d requests", got, len(d))
		if got >= 50*time.Millisecond {
			t.Errorf("GET / p95 = %v, budget 50 ms", got)
		}
		for _, path := range []string{"/history", "/printers"} {
			d = d[:0]
			for range 200 {
				start := time.Now()
				rec := get(h, path)
				d = append(d, time.Since(start))
				if rec.Code != 200 {
					t.Fatalf("GET %s: status %d", path, rec.Code)
				}
			}
			got = p95(d)
			t.Logf("GET %s p95 = %v over %d requests", path, got, len(d))
			if got >= 50*time.Millisecond {
				t.Errorf("GET %s p95 = %v, budget 50 ms", path, got)
			}
		}
		d = d[:0]
		for range 200 {
			start := time.Now()
			rec := get(h, "/scan")
			d = append(d, time.Since(start))
			if rec.Code != 200 {
				t.Fatalf("GET /scan: status %d", rec.Code)
			}
		}
		got = p95(d)
		t.Logf("GET /scan p95 = %v", got)
		if got >= 50*time.Millisecond {
			t.Errorf("GET /scan p95 = %v, budget 50 ms", got)
		}
	})

	t.Run("action", func(t *testing.T) {
		name := fmt.Sprintf("skra-test budget %d.txt", time.Now().UnixNano())
		cancelNamed(t, c, name)
		fields := [][2]string{{"printer", "Virtual-MFC"}, {"hold", "true"}}
		if rec := upload(s, h, fields, name, strings.NewReader("budget\n"), -1); rec.Code != 200 {
			t.Fatalf("upload: status %d: %s", rec.Code, rec.Body.String())
		}
		jobs := jobsNamed(t, c, name)
		if len(jobs) != 1 || jobs[0].State != 4 {
			t.Fatalf("jobs = %+v, want one held job", jobs)
		}
		get(h, "/") // learn the job name
		id := strconv.Itoa(jobs[0].ID)
		var d []time.Duration
		for i := range 50 {
			for _, act := range []string{"release", "hold"} {
				start := time.Now()
				rec := postTo(s, h, "/jobs/"+id+"/"+act, url.Values{})
				d = append(d, time.Since(start))
				if rec.Code != 200 {
					t.Fatalf("pair %d, %s: status %d: %s", i, act, rec.Code, rec.Body.String())
				}
			}
		}
		got := p95(d)
		t.Logf("hold/release p95 = %v over %d actions", got, len(d))
		if got >= 150*time.Millisecond {
			t.Errorf("action p95 = %v, budget 150 ms", got)
		}
	})

	t.Run("weight", func(t *testing.T) {
		total := 0
		for _, p := range []string{"/", "/static/skra.css", "/static/skra.js"} {
			rec := get(h, p)
			if rec.Code != 200 {
				t.Fatalf("GET %s: status %d", p, rec.Code)
			}
			total += rec.Body.Len()
		}
		t.Logf("page, style and script = %d bytes", total)
		if total >= 200*1024 {
			t.Errorf("page weight %d bytes, budget 200 KB", total)
		}
	})
}
