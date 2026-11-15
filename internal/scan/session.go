// SPDX-License-Identifier: AGPL-3.0-or-later

// Package scan keeps a half-done scan on disk. A session is a directory under
// the state directory holding the pages scanned so far, as JPEG files, and a
// manifest that says which pages are in it, in what order and how far each is
// turned. Nothing waits in memory, so a session survives the service
// exiting when it is idle, and a person can come back to it later.
//
// Everything here is private to the owner: directories are 0700 and files are
// 0600. Nothing is written outside the scans directory.
package scan

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nemesis398/skra/internal/escl"
)

const (
	// maxPages is how many pages one session may hold.
	maxPages = 200
	// maxPageNumber is the highest page file number, as pages are named with
	// three digits. A dropped page's number is never used again, so this also
	// bounds how many pages one session can ever take.
	maxPageNumber = 999

	manifestName = "session.json"
	dirMode      = 0o700
	fileMode     = 0o600
)

// maxPageBytes caps one page, so a scanner that never stops cannot fill the
// disk. It is a variable only so a test can lower it.
var maxPageBytes int64 = 256 << 20

var (
	// ErrTooManyPages is the refusal of a page past the cap.
	ErrTooManyPages = fmt.Errorf("A scan holds at most %d pages. Save this one and start another.", maxPages)
	// ErrNotJPEG is the refusal of a page that is not a JPEG picture.
	ErrNotJPEG = errors.New("The scanner sent a page that is not a JPEG picture.")
	// ErrPageTooBig is the refusal of a page past the size cap.
	ErrPageTooBig = errors.New("The scanner sent a page that is too large to keep.")

	errBadPage = errors.New("scan: there is no such page")
	errBadTurn = errors.New("scan: a page turns by 90 or -90 degrees")
	errGone    = errors.New("scan: this scan has been discarded")
)

var (
	idPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	pagePattern = regexp.MustCompile(`^[0-9]{3}\.jpg$`)
)

// validID reports whether id has the shape of a session ID: 16 bytes in
// lower-case hex. Nothing else is ever joined onto a path.
func validID(id string) bool { return idPattern.MatchString(id) }

// Page is one scanned page.
type Page struct {
	File          string // name inside the session directory, such as 001.jpg
	Rotate        int    // clockwise turn to apply: 0, 90, 180 or 270
	Width, Height int    // pixels, as the JPEG stores them
	DPI           int    // from the settings the scan was asked for
}

// manifest is what session.json holds.
type manifest struct {
	ID       string
	Scanner  escl.Scanner
	Settings escl.Settings
	Pages    []Page
	Created  time.Time
	Next     int // the number the next page file gets
}

// Store is the scans directory. It is safe for concurrent use.
type Store struct {
	root string

	mu       sync.Mutex
	sessions map[string]*Session
}

// Open returns the store in stateDir/scans, creating the directory with mode
// 0700 if it is missing and tightening it if it exists with a wider mode.
func Open(stateDir string) (*Store, error) {
	root := filepath.Join(stateDir, "scans")
	if err := os.MkdirAll(root, dirMode); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("scan: %s is not a directory", root)
	}
	if fi.Mode().Perm() != dirMode {
		if err := os.Chmod(root, dirMode); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
	}
	return &Store{root: root, sessions: map[string]*Session{}}, nil
}

// New starts an empty session for a scan on scanner with settings s.
func (st *Store) New(scanner escl.Scanner, s escl.Settings) (*Session, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	id := hex.EncodeToString(raw[:])
	dir := filepath.Join(st.root, id)
	if err := os.Mkdir(dir, dirMode); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	sess := &Session{
		ID:       id,
		Scanner:  scanner,
		Settings: s,
		Created:  time.Now().UTC().Truncate(time.Millisecond),
		dir:      dir,
		next:     1,
	}
	if err := sess.save(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	st.mu.Lock()
	st.sessions[id] = sess
	st.mu.Unlock()
	return sess, nil
}

// Get returns the session with the given ID. Within one Store the same ID
// always gives the same *Session, so a handler and a scan job share its lock.
// A missing session gives an error that satisfies errors.Is(err,
// os.ErrNotExist).
func (st *Store) Get(id string) (*Session, error) {
	if !validID(id) {
		return nil, fmt.Errorf("scan: %q is not a scan ID", id)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if sess, ok := st.sessions[id]; ok {
		return sess, nil
	}
	sess, err := load(filepath.Join(st.root, id), id)
	if err != nil {
		return nil, err
	}
	st.sessions[id] = sess
	return sess, nil
}

// List returns every session, oldest first. A directory that holds no sound
// session is left out, so one broken scan does not hide the others.
func (st *Store) List() ([]*Session, error) {
	ents, err := os.ReadDir(st.root)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	var out []*Session
	for _, e := range ents {
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		if sess, err := st.Get(e.Name()); err == nil {
			out = append(out, sess)
		}
	}
	slices.SortStableFunc(out, func(a, b *Session) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return cmpString(a.ID, b.ID)
	})
	return out, nil
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Discard deletes a session and everything in it.
func (st *Store) Discard(id string) error {
	sess, err := st.Get(id)
	if err != nil {
		return err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.gone {
		return fmt.Errorf("scan: %w", os.ErrNotExist)
	}
	if err := os.RemoveAll(sess.dir); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	sess.gone = true
	st.mu.Lock()
	delete(st.sessions, id)
	st.mu.Unlock()
	return nil
}

// Session is one scan in progress or waiting to be saved. The exported fields
// are fixed once the session exists, except Pages, which only the methods may
// touch; read it through PageList. All methods may be called from several
// goroutines.
type Session struct {
	ID       string
	Scanner  escl.Scanner
	Settings escl.Settings
	Pages    []Page
	Created  time.Time

	dir string

	mu      sync.Mutex
	next    int  // the number the next page file gets
	pending int  // pages being streamed in, which count against the cap
	gone    bool // the session was discarded
}

// load reads the session in dir and checks it. It also removes what a crash
// leaves behind: a page file the manifest does not list and a temp manifest.
func load(dir, id string) (*Session, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("scan: %s: %w", id, err)
	}
	if m.ID != id {
		return nil, fmt.Errorf("scan: %s names another scan", id)
	}
	listed := map[string]bool{}
	next := max(m.Next, 1)
	for _, p := range m.Pages {
		if !pagePattern.MatchString(p.File) || listed[p.File] {
			return nil, fmt.Errorf("scan: %s: %q is not a page file", id, p.File)
		}
		if p.Rotate < 0 || p.Rotate >= 360 || p.Rotate%90 != 0 {
			return nil, fmt.Errorf("scan: %s: a page is turned by %d", id, p.Rotate)
		}
		listed[p.File] = true
		n, _ := strconv.Atoi(p.File[:3])
		next = max(next, n+1)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	for _, e := range ents {
		if name := e.Name(); name != manifestName && !listed[name] && (pagePattern.MatchString(name) || isTemp(name)) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
	return &Session{
		ID:       m.ID,
		Scanner:  m.Scanner,
		Settings: m.Settings,
		Pages:    m.Pages,
		Created:  m.Created,
		dir:      dir,
		next:     next,
	}, nil
}

func isTemp(name string) bool {
	return strings.HasPrefix(name, manifestName+".") && strings.HasSuffix(name, ".tmp")
}

// save writes the manifest to a temp file in the session directory and
// renames it over session.json, so a crash never leaves half a manifest. The
// caller holds s.mu, or owns s alone.
func (s *Session) save() error {
	data, err := json.MarshalIndent(manifest{
		ID:       s.ID,
		Scanner:  s.Scanner,
		Settings: s.Settings,
		Pages:    s.Pages,
		Created:  s.Created,
		Next:     s.next,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	// CreateTemp makes the file 0600 and never reuses a name.
	f, err := os.CreateTemp(s.dir, manifestName+".*.tmp")
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, filepath.Join(s.dir, manifestName))
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("scan: %w", werr)
	}
	return nil
}

// PageList returns a copy of the pages, in order.
func (s *Session) PageList() []Page {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.Pages)
}

// AddPage streams r into the next page file and reads the JPEG header for the
// size. A reader that is not a JPEG, that fails, or that runs past the size
// cap leaves no file behind. The session is not locked while r is read, so a
// slow scanner does not hold up Rotate or Drop.
func (s *Session) AddPage(r io.Reader) (Page, error) {
	s.mu.Lock()
	if s.gone {
		s.mu.Unlock()
		return Page{}, errGone
	}
	if len(s.Pages)+s.pending >= maxPages {
		s.mu.Unlock()
		return Page{}, ErrTooManyPages
	}
	if s.next > maxPageNumber {
		s.mu.Unlock()
		return Page{}, ErrTooManyPages
	}
	num := s.next
	s.next++
	s.pending++
	s.mu.Unlock()

	name := fmt.Sprintf("%03d.jpg", num)
	path := filepath.Join(s.dir, name)
	page, err := s.writePage(r, path, name)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending--
	if err == nil && s.gone {
		err = errGone
	}
	if err == nil {
		s.Pages = append(s.Pages, page)
		if err = s.save(); err != nil {
			s.Pages = s.Pages[:len(s.Pages)-1]
		}
	}
	if err != nil {
		_ = os.Remove(path)
		// Give the number back unless a later page already took one, so
		// a scanner that sends junk does not use up the numbers.
		if s.next == num+1 {
			s.next = num
		}
		return Page{}, err
	}
	return page, nil
}

// writePage copies r to path and reads the picture size from the result. On an
// error the caller removes the file.
func (s *Session) writePage(r io.Reader, path, name string) (Page, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return Page{}, fmt.Errorf("scan: %w", err)
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(r, maxPageBytes+1))
	if err != nil {
		return Page{}, fmt.Errorf("scan: %w", err)
	}
	if n > maxPageBytes {
		return Page{}, ErrPageTooBig
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Page{}, fmt.Errorf("scan: %w", err)
	}
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return Page{}, ErrNotJPEG
	}
	if err := f.Sync(); err != nil {
		return Page{}, fmt.Errorf("scan: %w", err)
	}
	return Page{File: name, Width: cfg.Width, Height: cfg.Height, DPI: s.Settings.Resolution}, nil
}

// page returns the index of page n, counted from 1. The caller holds s.mu.
func (s *Session) page(n int) (int, error) {
	if s.gone {
		return 0, errGone
	}
	if n < 1 || n > len(s.Pages) {
		return 0, errBadPage
	}
	return n - 1, nil
}

// Rotate turns page n, counted from 1, by 90 or -90 degrees. The turn wraps
// around, so four turns of 90 are none.
func (s *Session) Rotate(n, by int) error {
	if by != 90 && by != -90 {
		return errBadTurn
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.page(n)
	if err != nil {
		return err
	}
	old := s.Pages[i].Rotate
	s.Pages[i].Rotate = ((old+by)%360 + 360) % 360
	if err := s.save(); err != nil {
		s.Pages[i].Rotate = old
		return err
	}
	return nil
}

// Drop removes page n, counted from 1, and deletes its file. The other pages
// keep their files and names. A dropped page's number is not used again.
func (s *Session) Drop(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.page(n)
	if err != nil {
		return err
	}
	dropped := s.Pages[i]
	s.Pages = slices.Delete(slices.Clone(s.Pages), i, i+1)
	if err := s.save(); err != nil {
		s.Pages = slices.Insert(slices.Clone(s.Pages), i, dropped)
		return err
	}
	// The manifest no longer lists the file, so a failure here leaves only
	// an orphan, which the next load removes.
	if err := os.Remove(filepath.Join(s.dir, dropped.File)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("scan: %w", err)
	}
	return nil
}

// PagePath returns the path of the JPEG file of page n, counted from 1.
func (s *Session) PagePath(n int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.page(n)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, s.Pages[i].File), nil
}
