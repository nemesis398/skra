// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "skra")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 700", di.Mode().Perm())
	}
	fi, err := os.Stat(filepath.Join(dir, "skra.db"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("db mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestOpenTightensExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skra")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 700", di.Mode().Perm())
	}
}

func TestLogAndRecent(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	want := []Event{
		{At: base, Actor: "mika", Action: "job.cancel", Target: "Office-12", Detail: "", OK: true},
		{At: base.Add(time.Millisecond), Actor: "mika", Action: "printer.add", Target: "Fußball <b>", Detail: `uri "x"`, OK: false},
		{At: base.Add(2 * time.Millisecond), Actor: "mika", Action: "print.file", Target: "A_b.c-1", Detail: "karte.pdf", OK: true},
	}
	for _, e := range want {
		if err := s.Log(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Recent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	for i, g := range got {
		w := want[len(want)-1-i]
		if !g.At.Equal(w.At) || g.At.Location() != time.UTC {
			t.Errorf("event %d: At = %v, want %v in UTC", i, g.At, w.At)
		}
		if g.Actor != w.Actor || g.Action != w.Action || g.Target != w.Target || g.Detail != w.Detail || g.OK != w.OK {
			t.Errorf("event %d = %+v, want %+v", i, g, w)
		}
	}

	two, err := s.Recent(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(two) != 2 || two[0].Action != "print.file" {
		t.Errorf("Recent(2) = %+v", two)
	}
}

func TestRecentTiesNewestInsertedFirst(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, a := range []string{"job.hold", "job.release", "job.restart"} {
		if err := s.Log(ctx, Event{At: at, Actor: "mika", Action: a, OK: true}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Recent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Action != "job.restart" || got[2].Action != "job.hold" {
		t.Errorf("order = %v %v %v", got[0].Action, got[1].Action, got[2].Action)
	}
}

func TestLogZeroAtUsesNow(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := time.Now().Add(-time.Second)
	if err := s.Log(ctx, Event{Actor: "mika", Action: "job.cancel", OK: true}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Recent(ctx, 1)
	if len(got) != 1 || got[0].At.Before(before) || got[0].At.After(time.Now().Add(time.Second)) {
		t.Errorf("At = %v", got)
	}
}

func TestReopenKeepsEvents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	if err := s.Log(ctx, Event{At: at, Actor: "mika", Action: "printer.pause", Target: "Office", OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Recent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Action != "printer.pause" || !got[0].At.Equal(at) {
		t.Errorf("got %+v", got)
	}
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 1 {
		t.Errorf("user_version = %d, %v", v, err)
	}
}
