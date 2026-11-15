// SPDX-License-Identifier: AGPL-3.0-or-later

// Package store keeps skra's local event store: every action the service
// takes is one row in a SQLite file, with the user who did it. The file lives
// in the state directory, so a later household server can read the same
// history without a rewrite.
//
// Action names are dotted: job.cancel, job.hold, job.release, job.restart,
// job.move, print.file, print.testpage, printer.add, printer.modify,
// printer.delete, printer.default, printer.accept, printer.reject,
// printer.pause, printer.resume, printer.cancelall, printer.moveall,
// printer.command and cups.loglevel.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver, pure Go
)

// Event is one recorded action.
type Event struct {
	At     time.Time
	Actor  string // the configured user who did it
	Action string // dotted name, for example job.cancel
	Target string // queue or job the action touched
	Detail string
	OK     bool
}

// Store is an open event store. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// migrations[i] takes the schema from version i to i+1. The version lives in
// PRAGMA user_version, so a later plan appends here and never edits an entry.
var migrations = []string{
	`CREATE TABLE events (
		at     INTEGER NOT NULL,
		actor  TEXT    NOT NULL,
		action TEXT    NOT NULL,
		target TEXT    NOT NULL,
		detail TEXT    NOT NULL,
		ok     INTEGER NOT NULL
	)`,
}

// Open creates dir with mode 0700 (and tightens an existing one), creates
// skra.db with mode 0600 and migrates it to the current schema.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: create %s: %w", dir, err)
	}
	// MkdirAll applies the umask and leaves an existing directory alone.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: restrict %s: %w", dir, err)
	}
	path := filepath.Join(dir, "skra.db")
	// Create the file ourselves so it never exists with a wider mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create %s: %w", path, err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, fmt.Errorf("store: restrict %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("store: create %s: %w", path, err)
	}

	// The path goes into a SQLite URI, where "?" and "#" end it and "%" starts
	// an escape. _txlock=immediate makes every write transaction take the
	// write lock when it begins, so a second process that migrates the same
	// new file waits for the first instead of failing halfway.
	db, err := sql.Open("sqlite", "file:"+uriPath.Replace(path)+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One connection: the service is local and low-traffic, and it keeps
	// writes serialized without lock errors.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// uriPath escapes the characters of a file name that mean something in a
// SQLite URI.
var uriPath = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

// migrate brings the schema to the current version in one transaction. The
// transaction holds the write lock from its start, and the version is read
// inside it, so a process that waited for another one sees the schema that
// one made and has nothing left to do.
func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // does nothing once committed
	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this skra (%d)", version, len(migrations))
	}
	if version == len(migrations) {
		return nil
	}
	for ; version < len(migrations); version++ {
		if _, err := tx.Exec(migrations[version]); err != nil {
			return err
		}
	}
	// PRAGMA does not take bound parameters; len(migrations) is our own int.
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
}

// Log records e. A zero At is replaced by the current time.
func (s *Store) Log(ctx context.Context, e Event) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	ok := 0
	if e.OK {
		ok = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (at, actor, action, target, detail, ok) VALUES (?, ?, ?, ?, ?, ?)`,
		e.At.UnixMilli(), e.Actor, e.Action, e.Target, e.Detail, ok)
	if err != nil {
		return fmt.Errorf("store: log %s: %w", e.Action, err)
	}
	return nil
}

// Recent returns up to n events, newest first; an n of 0 or less returns none.
// Events with the same time come back in reverse insertion order.
func (s *Store) Recent(ctx context.Context, n int) ([]Event, error) {
	if n <= 0 {
		return nil, nil // SQLite reads a negative LIMIT as "no limit"
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, actor, action, target, detail, ok FROM events ORDER BY at DESC, rowid DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("store: recent: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var (
			e  Event
			ms int64
			ok int
		)
		if err := rows.Scan(&ms, &e.Actor, &e.Action, &e.Target, &e.Detail, &ok); err != nil {
			return nil, fmt.Errorf("store: recent: %w", err)
		}
		e.At = time.UnixMilli(ms).UTC()
		e.OK = ok != 0
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: recent: %w", err)
	}
	return out, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
