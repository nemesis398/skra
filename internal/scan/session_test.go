// SPDX-License-Identifier: AGPL-3.0-or-later

package scan

import (
	"bytes"
	"crypto/rand"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nemesis398/skra/internal/escl"
)

var (
	testScanner  = escl.Scanner{Name: "Office", URL: "http://192.0.2.7:80/eSCL", UUID: "u-1", Declared: true}
	testSettings = escl.Settings{Source: "Feeder", Duplex: true, Color: "RGB24", Resolution: 300, Width: 2480, Height: 3508}
)

// jpegBytes is a small JPEG of w by h pixels.
func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st, dir
}

func newSession(t *testing.T, st *Store) *Session {
	t.Helper()
	s, err := st.New(testScanner, testSettings)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func addPages(t *testing.T, s *Session, n int) {
	t.Helper()
	for range n {
		if _, err := s.AddPage(bytes.NewReader(jpegBytes(t, 8, 6))); err != nil {
			t.Fatal(err)
		}
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestSessionModes(t *testing.T) {
	// A loose umask must not widen anything.
	old := umaskZero()
	defer old()
	st, dir := newStore(t)
	s := newSession(t, st)
	addPages(t, s, 1)

	scans := filepath.Join(dir, "scans")
	if m := mode(t, scans); m != 0o700 {
		t.Errorf("scans dir mode = %o, want 700", m)
	}
	sd := filepath.Join(scans, s.ID)
	if m := mode(t, sd); m != 0o700 {
		t.Errorf("session dir mode = %o, want 700", m)
	}
	if m := mode(t, filepath.Join(sd, "001.jpg")); m != 0o600 {
		t.Errorf("page mode = %o, want 600", m)
	}
	if m := mode(t, filepath.Join(sd, "session.json")); m != 0o600 {
		t.Errorf("session.json mode = %o, want 600", m)
	}
	ents, err := os.ReadDir(sd)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Errorf("session dir holds %d entries, want 2 (no temp files left)", len(ents))
	}
}

func TestOpenTightensExistingDir(t *testing.T) {
	dir := t.TempDir()
	scans := filepath.Join(dir, "scans")
	if err := os.Mkdir(scans, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(scans, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, scans); m != 0o700 {
		t.Errorf("scans dir mode = %o, want 700", m)
	}
}

func TestSessionSurvivesReopen(t *testing.T) {
	st, dir := newStore(t)
	s := newSession(t, st)
	addPages(t, s, 3)
	if err := s.Rotate(3, 90); err != nil {
		t.Fatal(err)
	}
	if err := s.Drop(2); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := st2.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("List = %d sessions, want 1", len(list))
	}
	got := list[0]
	if got.ID != s.ID || got.Scanner != testScanner || got.Settings != testSettings {
		t.Errorf("session = %+v / %+v / %+v, want the one stored", got.ID, got.Scanner, got.Settings)
	}
	if !got.Created.Equal(s.Created) {
		t.Errorf("Created = %v, want %v", got.Created, s.Created)
	}
	pages := got.PageList()
	if len(pages) != 2 || pages[0].File != "001.jpg" || pages[1].File != "003.jpg" {
		t.Fatalf("pages = %+v, want 001.jpg and 003.jpg", pages)
	}
	if pages[0].Rotate != 0 || pages[1].Rotate != 90 {
		t.Errorf("rotations = %d, %d; want 0, 90", pages[0].Rotate, pages[1].Rotate)
	}
	if pages[0].Width != 8 || pages[0].Height != 6 || pages[0].DPI != 300 {
		t.Errorf("page 1 = %+v, want 8x6 at 300 dpi", pages[0])
	}
	// The counter survives too: the dropped number 2 and the used 3 stay gone.
	p, err := got.AddPage(bytes.NewReader(jpegBytes(t, 4, 4)))
	if err != nil {
		t.Fatal(err)
	}
	if p.File != "004.jpg" {
		t.Errorf("next page = %s, want 004.jpg", p.File)
	}
}

func TestGetReturnsTheSameSession(t *testing.T) {
	st, _ := newStore(t)
	s := newSession(t, st)
	g, err := st.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g != s {
		t.Error("Get returned another *Session for the same id")
	}
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != s {
		t.Error("List returned another *Session for the same id")
	}
}

func TestListOldestFirst(t *testing.T) {
	st, _ := newStore(t)
	for range 3 {
		newSession(t, st)
	}
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("List = %d, want 3", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i].Created.Before(list[i-1].Created) {
			t.Errorf("list[%d] is older than list[%d]", i, i-1)
		}
	}
}

func TestRotateWraps(t *testing.T) {
	st, _ := newStore(t)
	s := newSession(t, st)
	addPages(t, s, 1)
	for range 4 {
		if err := s.Rotate(1, 90); err != nil {
			t.Fatal(err)
		}
	}
	if r := s.PageList()[0].Rotate; r != 0 {
		t.Errorf("after four turns Rotate = %d, want 0", r)
	}
	if err := s.Rotate(1, -90); err != nil {
		t.Fatal(err)
	}
	if r := s.PageList()[0].Rotate; r != 270 {
		t.Errorf("after -90 Rotate = %d, want 270", r)
	}
	for _, by := range []int{0, 45, 180, 360, -180} {
		if err := s.Rotate(1, by); err == nil {
			t.Errorf("Rotate by %d was taken", by)
		}
	}
	if r := s.PageList()[0].Rotate; r != 270 {
		t.Errorf("a refused turn changed Rotate to %d", r)
	}
	for _, n := range []int{0, 2, -1} {
		if err := s.Rotate(n, 90); err == nil {
			t.Errorf("Rotate of page %d was taken", n)
		}
	}
}

func TestDropRenumbersNothingOnDisk(t *testing.T) {
	st, dir := newStore(t)
	s := newSession(t, st)
	addPages(t, s, 3)
	sd := filepath.Join(dir, "scans", s.ID)
	if err := s.Drop(2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sd, "002.jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("002.jpg still there: %v", err)
	}
	for _, f := range []string{"001.jpg", "003.jpg"} {
		if _, err := os.Stat(filepath.Join(sd, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	pages := s.PageList()
	if len(pages) != 2 || pages[0].File != "001.jpg" || pages[1].File != "003.jpg" {
		t.Errorf("pages = %+v", pages)
	}
	// Position 2 now is the file 003.jpg.
	p, err := s.PagePath(2)
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(sd, "003.jpg") {
		t.Errorf("PagePath(2) = %s", p)
	}
	// A dropped number is not used again.
	np, err := s.AddPage(bytes.NewReader(jpegBytes(t, 4, 4)))
	if err != nil {
		t.Fatal(err)
	}
	if np.File != "004.jpg" {
		t.Errorf("new page = %s, want 004.jpg", np.File)
	}
	for _, n := range []int{0, 4, -1} {
		if err := s.Drop(n); err == nil {
			t.Errorf("Drop of page %d was taken", n)
		}
		if _, err := s.PagePath(n); err == nil {
			t.Errorf("PagePath of page %d was given", n)
		}
	}
}

func TestAddPageRefusesNonJPEG(t *testing.T) {
	st, dir := newStore(t)
	s := newSession(t, st)
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	junk := make([]byte, 10)
	if _, err := rand.Read(junk); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"png": pngBuf.Bytes(), "random": junk, "empty": nil} {
		if _, err := s.AddPage(bytes.NewReader(data)); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	if n := len(s.PageList()); n != 0 {
		t.Errorf("%d pages after refusals", n)
	}
	ents, err := os.ReadDir(filepath.Join(dir, "scans", s.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != "session.json" {
			t.Errorf("left behind: %s", e.Name())
		}
	}
	// A refusal does not use up a number.
	p, err := s.AddPage(bytes.NewReader(jpegBytes(t, 4, 4)))
	if err != nil {
		t.Fatal(err)
	}
	if p.File != "001.jpg" {
		t.Errorf("page after refusals = %s, want 001.jpg", p.File)
	}
}

type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errors.New("scanner went away")
	}
	f.n--
	return copy(p, "xx"), nil
}

func TestAddPageReadErrorLeavesNoFile(t *testing.T) {
	st, dir := newStore(t)
	s := newSession(t, st)
	if _, err := s.AddPage(io.MultiReader(bytes.NewReader(jpegBytes(t, 8, 6)), &failingReader{n: 1})); err == nil {
		t.Fatal("a broken read was taken")
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "scans", s.ID))
	if len(ents) != 1 {
		t.Errorf("%d entries left, want only session.json", len(ents))
	}
}

func TestAddPageCaps(t *testing.T) {
	st, _ := newStore(t)
	s := newSession(t, st)
	small := jpegBytes(t, 1, 1)
	for i := range maxPages {
		if _, err := s.AddPage(bytes.NewReader(small)); err != nil {
			t.Fatalf("page %d: %v", i+1, err)
		}
	}
	if _, err := s.AddPage(bytes.NewReader(small)); err == nil {
		t.Error("page 201 was taken")
	}
	// Dropping makes room again, under a number never used before.
	if err := s.Drop(1); err != nil {
		t.Fatal(err)
	}
	p, err := s.AddPage(bytes.NewReader(small))
	if err != nil {
		t.Fatal(err)
	}
	if p.File != "201.jpg" {
		t.Errorf("page = %s, want 201.jpg", p.File)
	}
}

func TestAddPageRefusesAnOversizedPage(t *testing.T) {
	old := maxPageBytes
	maxPageBytes = 100
	defer func() { maxPageBytes = old }()
	st, dir := newStore(t)
	s := newSession(t, st)
	if _, err := s.AddPage(bytes.NewReader(jpegBytes(t, 64, 64))); err == nil {
		t.Fatal("an oversized page was taken")
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "scans", s.ID))
	if len(ents) != 1 {
		t.Errorf("%d entries left, want only session.json", len(ents))
	}
}

func TestDiscardRemovesEverything(t *testing.T) {
	st, dir := newStore(t)
	s := newSession(t, st)
	addPages(t, s, 2)
	if err := st.Discard(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "scans", s.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("session dir still there: %v", err)
	}
	if _, err := st.Get(s.ID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Get after Discard = %v, want not-exist", err)
	}
	list, err := st.List()
	if err != nil || len(list) != 0 {
		t.Errorf("List = %v, %v; want empty", list, err)
	}
	// A holder of the old *Session cannot bring it back.
	if _, err := s.AddPage(bytes.NewReader(jpegBytes(t, 4, 4))); err == nil {
		t.Error("AddPage on a discarded session was taken")
	}
	if _, err := os.Stat(filepath.Join(dir, "scans", s.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("session dir came back: %v", err)
	}
	if err := s.Rotate(1, 90); err == nil {
		t.Error("Rotate on a discarded session was taken")
	}
	if err := st.Discard(s.ID); err == nil {
		t.Error("a second Discard succeeded")
	}
}

func TestIDsCannotEscape(t *testing.T) {
	st, dir := newStore(t)
	if err := os.Mkdir(filepath.Join(dir, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	ids := []string{"../x", "", "a/b", "..", ".", "/etc", strings.Repeat("A", 32), strings.Repeat("a", 31), strings.Repeat("a", 33), "../scans/" + strings.Repeat("a", 32)}
	for _, id := range ids {
		if _, err := st.Get(id); err == nil {
			t.Errorf("Get(%q) was taken", id)
		}
		if err := st.Discard(id); err == nil {
			t.Errorf("Discard(%q) was taken", id)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err != nil {
		t.Errorf("Discard reached outside the scans directory: %v", err)
	}
}

func TestIDsAreRandomHex(t *testing.T) {
	st, _ := newStore(t)
	a, b := newSession(t, st), newSession(t, st)
	if a.ID == b.ID || !validID(a.ID) || !validID(b.ID) {
		t.Errorf("ids %q, %q", a.ID, b.ID)
	}
}

func TestLoadRefusesATamperedManifest(t *testing.T) {
	st, dir := newStore(t)
	s := newSession(t, st)
	addPages(t, s, 1)
	mf := filepath.Join(dir, "scans", s.ID, "session.json")
	data, err := os.ReadFile(mf)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(data), "001.jpg", "../../x.jpg", 1)
	if bad == string(data) {
		t.Fatal("test did not alter the manifest")
	}
	if err := os.WriteFile(mf, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st2.Get(s.ID); err == nil {
		t.Error("a manifest naming a file outside the session was taken")
	}
	// One broken session does not hide the others.
	other := newSession(t, st2)
	list, err := st2.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != other.ID {
		t.Errorf("List = %d sessions, want just the sound one", len(list))
	}
}

func TestReopenRemovesAnOrphanedPage(t *testing.T) {
	st, dir := newStore(t)
	s := newSession(t, st)
	addPages(t, s, 1)
	orphan := filepath.Join(dir, "scans", s.ID, "002.jpg")
	if err := os.WriteFile(orphan, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	g, err := st2.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("orphan still there: %v", err)
	}
	p, err := g.AddPage(bytes.NewReader(jpegBytes(t, 4, 4)))
	if err != nil {
		t.Fatal(err)
	}
	if p.File != "002.jpg" {
		t.Errorf("page = %s, want 002.jpg", p.File)
	}
}

func TestConcurrentUse(t *testing.T) {
	st, _ := newStore(t)
	s := newSession(t, st)
	addPages(t, s, 2)
	data := jpegBytes(t, 8, 6)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			if _, err := s.AddPage(bytes.NewReader(data)); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := s.Rotate(1, 90); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			_ = s.PageList()
			if g, err := st.Get(s.ID); err != nil || g != s {
				t.Errorf("Get = %v, %v", g, err)
			}
		}()
	}
	wg.Wait()
	if n := len(s.PageList()); n != 6 {
		t.Errorf("%d pages, want 6", n)
	}
}
