// SPDX-License-Identifier: AGPL-3.0-or-later

package scan

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

var saveNow = time.Date(2026, 11, 14, 9, 41, 7, 0, time.UTC)

func TestFileName(t *testing.T) {
	if got, want := FileName("Krankenkasse Beitragsbescheid", saveNow, "pdf"), "2026-11-14 Krankenkasse Beitragsbescheid.pdf"; got != want {
		t.Errorf("FileName = %q, want %q", got, want)
	}
	if got, want := FileName("", saveNow, "pdf"), "2026-11-14 Scan 09-41.pdf"; got != want {
		t.Errorf("empty title: %q, want %q", got, want)
	}
	if got, want := FileName(" \t\x00 ", saveNow, "pdf"), "2026-11-14 Scan 09-41.pdf"; got != want {
		t.Errorf("blank title: %q, want %q", got, want)
	}

	long := strings.Repeat("ä😀x", 100)
	for _, title := range []string{
		"a/b", "../x", "/etc/passwd", "..", ".hidden", "tab\there", "nul\x00byte", "del\x7fdel", "c1\u0085\u009fgone",
		"line\nbreak", "bad \xff utf8", long, strings.Repeat("a", 300), strings.Repeat("a", 119) + "   ...", "Rechnung für Müller 🧾",
	} {
		t.Run(fmt.Sprintf("%.20q", title), func(t *testing.T) {
			name := FileName(title, saveNow, "pdf")
			stem, ok := strings.CutSuffix(name, ".pdf")
			if !ok {
				t.Fatalf("%q lacks .pdf", name)
			}
			if !utf8.ValidString(name) {
				t.Errorf("%q is not valid UTF-8", name)
			}
			if strings.ContainsRune(name, '/') || strings.HasPrefix(name, ".") {
				t.Errorf("%q has a slash or starts with a dot", name)
			}
			for _, r := range name {
				if unicode.IsControl(r) {
					t.Errorf("%q has control character %U", name, r)
				}
			}
			if n := utf8.RuneCountInString(stem); n > 120 {
				t.Errorf("stem is %d runes, want at most 120", n)
			}
			if len(name) > 255 {
				t.Errorf("name is %d bytes, more than a file name may be", len(name))
			}
			if strings.Contains(stem, "  ") || strings.HasSuffix(stem, " ") || strings.HasSuffix(stem, ".") {
				t.Errorf("stem %q has a double space or ends in space or dot", stem)
			}
			if !strings.HasPrefix(name, "2026-11-14 ") {
				t.Errorf("%q lacks the date", name)
			}
		})
	}

	if got := FileName("Rechnung für Müller 🧾", saveNow, "pdf"); got != "2026-11-14 Rechnung für Müller 🧾.pdf" {
		t.Errorf("umlauts or emoji lost: %q", got)
	}
	if got := FileName("a/b", saveNow, "pdf"); got != "2026-11-14 a b.pdf" {
		t.Errorf("slash: %q", got)
	}
	// Cutting happens on a rune boundary, never inside an emoji's bytes.
	if got := FileName(long, saveNow, "pdf"); !utf8.ValidString(got) || utf8.RuneCountInString(got) > 124 {
		t.Errorf("long emoji title gives %q", got)
	}
	// The collision suffix counts toward the 120.
	if got := stemName(strings.Repeat("a", 300), saveNow, "pdf", " (12)", " (12)"); utf8.RuneCountInString(got) != 120 || !strings.HasSuffix(got, " (12)") {
		t.Errorf("suffixed stem is %q", got)
	}
}

// listing returns the sorted names, sizes and modes of a directory.
func listing(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %d %v", e.Name(), fi.Size(), fi.Mode()))
	}
	slices.Sort(out)
	return out
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

func sessionDir(t *testing.T, s *Session) string {
	t.Helper()
	p, err := s.PagePath(1)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(p)
}

func TestSaveNeverOverwrites(t *testing.T) {
	st, _ := newStore(t)
	s := newSession(t, st)
	addPages(t, s, 2)
	folder := t.TempDir()
	keep := filepath.Join(folder, "2026-11-14 X.pdf")
	if err := os.WriteFile(keep, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Save(s, folder, "X", "pdf", saveNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(folder, "2026-11-14 X (2).pdf")}; !slices.Equal(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
	got, err = Save(s, folder, "X", "pdf", saveNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(folder, "2026-11-14 X (3).pdf")}; !slices.Equal(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(keep); string(b) != "keep me" {
		t.Errorf("the existing file now holds %q", b)
	}

	// A taken name in a set of numbered files moves the whole set.
	if err := os.WriteFile(filepath.Join(folder, "2026-11-14 Y 2.jpg"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = Save(s, folder, "Y", "jpeg", saveNow)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(folder, "2026-11-14 Y (2) 1.jpg"), filepath.Join(folder, "2026-11-14 Y (2) 2.jpg")}
	if !slices.Equal(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(folder, "2026-11-14 Y 2.jpg")); string(b) != "keep" {
		t.Errorf("the existing numbered file now holds %q", b)
	}
	if n := names(t, folder); len(n) != 6 {
		t.Errorf("folder holds %q", n)
	}
}

func TestSaveModesAndFolder(t *testing.T) {
	defer umaskZero()()
	st, _ := newStore(t)
	s := newSession(t, st)
	addPages(t, s, 3)
	sdir := sessionDir(t, s)
	parent := t.TempDir()
	parentMode := mode(t, parent)
	folder := filepath.Join(parent, "Scans", "2026")

	before := listing(t, sdir)
	for _, format := range []string{"pdf", "jpeg", "png"} {
		sub := filepath.Join(folder, format)
		got, err := Save(s, sub, "Mode", format, saveNow)
		if err != nil {
			t.Fatal(format, err)
		}
		if len(got) == 0 {
			t.Fatal(format, "no paths")
		}
		if n := names(t, sub); len(n) != len(got) {
			t.Errorf("%s: folder holds %q for paths %q", format, n, got)
		}
		for _, p := range got {
			if m := mode(t, p); m != 0o600 {
				t.Errorf("%s is %v, want 0600", p, m)
			}
		}
	}
	for _, d := range []string{filepath.Join(parent, "Scans"), folder, filepath.Join(folder, "pdf")} {
		if m := mode(t, d); m != 0o700 {
			t.Errorf("%s is %v, want 0700", d, m)
		}
	}
	if m := mode(t, parent); m != parentMode {
		t.Errorf("an existing parent changed from %v to %v", parentMode, m)
	}
	if after := listing(t, sdir); !slices.Equal(before, after) {
		t.Errorf("the session directory changed:\nbefore %q\nafter  %q", before, after)
	}
}

func TestSaveFailureLeavesFolderAsItWas(t *testing.T) {
	st, _ := newStore(t)
	s := newSession(t, st)
	addPages(t, s, 3)
	p3, _ := s.PagePath(3)
	if err := os.Remove(p3); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "other.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := listing(t, parent)

	for _, format := range []string{"pdf", "jpeg", "png"} {
		// Into a folder that must be created, and into one that exists.
		for _, folder := range []string{filepath.Join(parent, "new", "deeper"), parent} {
			if got, err := Save(s, folder, "Fail", format, saveNow); err == nil || got != nil {
				t.Fatalf("%s into %s: got %q, %v; want an error", format, folder, got, err)
			}
			if after := listing(t, parent); !slices.Equal(before, after) {
				t.Errorf("%s into %s left the folder changed:\nbefore %q\nafter  %q", format, folder, before, after)
			}
		}
	}

	empty := newSession(t, st)
	if _, err := Save(empty, parent, "Nothing", "pdf", saveNow); err == nil {
		t.Error("a scan with no pages was saved")
	}
	if _, err := Save(s, parent, "Odd", "tiff", saveNow); err == nil {
		t.Error("an unknown format was accepted")
	}
	if _, err := Save(s, "", "Odd", "pdf", saveNow); err == nil {
		t.Error("an empty folder was accepted")
	}
	if after := listing(t, parent); !slices.Equal(before, after) {
		t.Errorf("refusals left the folder changed: %q", after)
	}
}

// twoHalves is a w by h picture, white on the left half and black on the
// right, as a JPEG. With w a multiple of 16 the halves meet on a block edge,
// so the JPEG keeps them clean.
func twoHalves(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.Gray{Y: 255}), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(w/2, 0, w, h), image.NewUniform(color.Gray{Y: 0}), image.Point{}, draw.Src)
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestImagesOnePerPage(t *testing.T) {
	st, _ := newStore(t)
	s := newSession(t, st)
	orig := [][]byte{jpegBytes(t, 8, 6), twoHalves(t, 32, 16), jpegBytes(t, 8, 6)}
	for _, b := range orig {
		if _, err := s.AddPage(bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Rotate(2, 90); err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()

	got, err := Save(s, folder, "Photos", "jpeg", saveNow)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(folder, "2026-11-14 Photos 1.jpg"),
		filepath.Join(folder, "2026-11-14 Photos 2.jpg"),
		filepath.Join(folder, "2026-11-14 Photos 3.jpg"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
	for _, i := range []int{0, 2} {
		if b, _ := os.ReadFile(got[i]); !bytes.Equal(b, orig[i]) {
			t.Errorf("page %d is not the scanner's bytes unchanged", i+1)
		}
	}
	b, _ := os.ReadFile(got[1])
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if r := img.Bounds(); r.Dx() != 16 || r.Dy() != 32 {
		t.Errorf("rotated page is %dx%d, want 16x32", r.Dx(), r.Dy())
	}
	// Turned clockwise, the white left half is on top.
	if top, _ := color.GrayModel.Convert(img.At(8, 4)).(color.Gray); top.Y < 200 {
		t.Errorf("top of the rotated page is %d, want white", top.Y)
	}
	if bot, _ := color.GrayModel.Convert(img.At(8, 28)).(color.Gray); bot.Y > 60 {
		t.Errorf("bottom of the rotated page is %d, want black", bot.Y)
	}

	gotPNG, err := Save(s, folder, "Photos", "png", saveNow)
	if err != nil {
		t.Fatal(err)
	}
	wantSizes := [][2]int{{8, 6}, {16, 32}, {8, 6}}
	for i, p := range gotPNG {
		if !strings.HasSuffix(p, fmt.Sprintf("2026-11-14 Photos %d.png", i+1)) {
			t.Errorf("png path %q", p)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(f)
		f.Close()
		if err != nil {
			t.Fatalf("%s does not decode: %v", p, err)
		}
		if r := img.Bounds(); r.Dx() != wantSizes[i][0] || r.Dy() != wantSizes[i][1] {
			t.Errorf("%s is %dx%d, want %v", p, r.Dx(), r.Dy(), wantSizes[i])
		}
		if i == 1 {
			if top, _ := color.GrayModel.Convert(img.At(8, 4)).(color.Gray); top.Y < 200 {
				t.Errorf("top of the rotated png is %d, want white", top.Y)
			}
		}
	}

	// A one-page scan has no number in its name.
	s1 := newSession(t, st)
	addPages(t, s1, 1)
	one, err := Save(s1, folder, "One", "jpeg", saveNow)
	if err != nil || len(one) != 1 || filepath.Base(one[0]) != "2026-11-14 One.jpg" {
		t.Errorf("one page: %q, %v", one, err)
	}
}

func TestTurn(t *testing.T) {
	// 3 wide, 2 high: pixel values 1..6 row by row.
	src := image.NewGray(image.Rect(0, 0, 3, 2))
	for i := range src.Pix {
		src.Pix[i] = uint8(i + 1)
	}
	for _, tc := range []struct {
		deg  int
		w, h int
		pix  []uint8
	}{
		{0, 3, 2, []uint8{1, 2, 3, 4, 5, 6}},
		{90, 2, 3, []uint8{4, 1, 5, 2, 6, 3}},
		{180, 3, 2, []uint8{6, 5, 4, 3, 2, 1}},
		{270, 2, 3, []uint8{3, 6, 2, 5, 1, 4}},
	} {
		got := turn(src, tc.deg).(*image.Gray)
		if got.Rect.Dx() != tc.w || got.Rect.Dy() != tc.h || !slices.Equal(got.Pix, tc.pix) {
			t.Errorf("turn %d: %dx%d %v, want %dx%d %v", tc.deg, got.Rect.Dx(), got.Rect.Dy(), got.Pix, tc.w, tc.h, tc.pix)
		}
	}
	rgba := image.NewRGBA(image.Rect(0, 0, 2, 1))
	rgba.SetRGBA(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	rgba.SetRGBA(1, 0, color.RGBA{R: 9, G: 8, B: 7, A: 255})
	got := turn(rgba, 90).(*image.RGBA)
	if got.Rect.Dx() != 1 || got.Rect.Dy() != 2 || got.RGBAAt(0, 0) != (color.RGBA{R: 1, G: 2, B: 3, A: 255}) || got.RGBAAt(0, 1) != (color.RGBA{R: 9, G: 8, B: 7, A: 255}) {
		t.Errorf("rgba turn: %v", got.Pix)
	}
}

// TestSaveStreams saves forty 600 dpi A4 pages and checks that the heap never
// grows by 20 MB, so a document is never held in memory. The pages are one
// JPEG written forty times, so the test pays for encoding it once.
func TestSaveStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("writes about a hundred megabytes")
	}
	const w, h = 4960, 7016
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 200*w*4; i++ { // noise in the top rows makes the file about a megabyte
		img.Pix[i] = uint8(rng.UintN(256))
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, img, nil); err != nil {
		t.Fatal(err)
	}
	img = nil
	page := enc.Bytes()
	if len(page)*40 < 40<<20 {
		t.Fatalf("a page is only %d bytes, too small to prove streaming", len(page))
	}

	st, _ := newStore(t)
	settings := testSettings
	settings.Resolution, settings.Width, settings.Height = 600, w, h
	s, err := st.New(testScanner, settings)
	if err != nil {
		t.Fatal(err)
	}
	for range 40 {
		if _, err := s.AddPage(bytes.NewReader(page)); err != nil {
			t.Fatal(err)
		}
	}
	folder := t.TempDir()

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc

	// Watch the heap while Save runs: a writer that buffers the document
	// shows at its peak, not after the garbage collector has run.
	var (
		mu   sync.Mutex
		peak = base
		stop = make(chan struct{})
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			mu.Lock()
			peak = max(peak, m.HeapAlloc)
			mu.Unlock()
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	paths, err := Save(s, folder, "Stream", "pdf", saveNow)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	growth := int64(peak) - int64(base)
	mu.Unlock()
	if growth >= 20<<20 {
		t.Errorf("the heap grew by %d MB while saving, want under 20", growth>>20)
	}
	fi, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() < int64(len(page))*40 {
		t.Errorf("the PDF is %d bytes, smaller than its forty pages", fi.Size())
	}
	t.Logf("page %d bytes, pdf %d bytes, heap grew by %d KB", len(page), fi.Size(), growth>>10)
}

// noisyJPEG is a w by h JPEG of random pixels, which makes a large file.
func noisyJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewPCG(3, 4))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.UintN(256))
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestSaveShowsNoEmptyFile watches the folder while Save runs: a name other
// than a hidden temp file must never be there before its content is whole.
func TestSaveShowsNoEmptyFile(t *testing.T) {
	st, _ := newStore(t)
	s := newSession(t, st)
	page := noisyJPEG(t, 800, 600)
	for range 40 {
		if _, err := s.AddPage(bytes.NewReader(page)); err != nil {
			t.Fatal(err)
		}
	}
	for _, format := range []string{"pdf", "jpeg"} {
		folder := t.TempDir()
		var (
			bad  string
			seen int
			stop = make(chan struct{})
			done = make(chan struct{})
		)
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				ents, _ := os.ReadDir(folder)
				for _, e := range ents {
					if strings.HasPrefix(e.Name(), ".") {
						continue
					}
					seen++
					if fi, err := e.Info(); err == nil && fi.Size() == 0 && bad == "" {
						bad = e.Name()
					}
				}
			}
		}()
		paths, err := Save(s, folder, "Watch", format, saveNow)
		close(stop)
		<-done
		if err != nil {
			t.Fatal(format, err)
		}
		if bad != "" {
			t.Errorf("%s: %q was empty while Save ran", format, bad)
		}
		if n := names(t, folder); len(n) != len(paths) {
			t.Errorf("%s: folder holds %d names after Save, want %d: %q", format, len(n), len(paths), n)
		}
		t.Logf("%s: saw %d final names while Save ran", format, seen)
	}
}
