// SPDX-License-Identifier: AGPL-3.0-or-later

package scan

import (
	"bytes"
	"errors"
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
	"runtime/debug"
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

	var paths []string
	growth := heapGrowth(func() { paths, err = Save(s, folder, "Stream", "pdf", saveNow) })
	if err != nil {
		t.Fatal(err)
	}
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

// heapGrowth runs f and returns by how much the heap grew above where it
// was before f at its highest. A sampler watches the heap while f runs, so a
// buffer shows at its peak, not after the garbage collector has run.
func heapGrowth(f func()) int64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
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
	f()
	close(stop)
	<-done
	mu.Lock()
	defer mu.Unlock()
	return int64(peak) - int64(base)
}

// retained is the heap memory the process holds from the system, whether in
// use or not: what shows as resident.
func retained() int64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapSys - ms.HeapReleased)
}

// a4Color is a color page of A4 at 600 dpi as the scanner sends it: a JPEG
// in 4:2:0, with noise in the top rows so the file is about a megabyte.
func a4Color(t *testing.T) (page []byte, yccBytes int64) {
	t.Helper()
	const w, h = 4960, 7016
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	rng := rand.New(rand.NewPCG(5, 6))
	for i := range 200 * w {
		img.Y[i] = uint8(rng.UintN(256))
	}
	for i := range img.Cb {
		img.Cb[i], img.Cr[i] = 128, 128
	}
	for i := range 100 * img.CStride {
		img.Cb[i], img.Cr[i] = uint8(rng.UintN(256)), uint8(rng.UintN(256))
	}
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return enc.Bytes(), int64(len(img.Y) + len(img.Cb) + len(img.Cr))
}

// TestSaveImagePeak saves a color A4 page at 600 dpi as a picture, turned
// and as it came, and checks that the heap never holds more than the page in
// its decoded form and one copy of it, with some room: never a page of four
// bytes a pixel. After the save the memory goes back to the system, so an
// idle service is small again.
func TestSaveImagePeak(t *testing.T) {
	if testing.Short() {
		t.Skip("decodes and encodes 35 megapixels several times")
	}
	page, ycc := a4Color(t)
	st, _ := newStore(t)
	settings := testSettings
	settings.Resolution, settings.Width, settings.Height = 600, 4960, 7016
	limit := ycc * 5 / 2
	for _, c := range []struct {
		format string
		rotate int
	}{{"jpeg", 90}, {"png", 90}, {"png", 0}, {"jpeg", 180}, {"jpeg", 270}} {
		s, err := st.New(testScanner, settings)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddPage(bytes.NewReader(page)); err != nil {
			t.Fatal(err)
		}
		for range c.rotate / 90 {
			if err := s.Rotate(1, 90); err != nil {
				t.Fatal(err)
			}
		}
		folder := t.TempDir()
		debug.FreeOSMemory()
		before := retained()
		growth := heapGrowth(func() { _, err = Save(s, folder, "Peak", c.format, saveNow) })
		if err != nil {
			t.Fatal(err)
		}
		kept := retained() - before
		t.Logf("%s turned %d: heap peak +%d MB, decoded page %d MB, still held +%d MB", c.format, c.rotate, growth>>20, ycc>>20, kept>>20)
		if growth >= limit {
			t.Errorf("%s turned %d: the heap grew by %d MB, want under %d (2.5 times the decoded page)", c.format, c.rotate, growth>>20, limit>>20)
		}
		if kept >= 20<<20 {
			t.Errorf("%s turned %d: the process still holds %d MB more after the save, want under 20", c.format, c.rotate, kept>>20)
		}
		if err := st.Discard(s.ID); err != nil {
			t.Fatal(err)
		}
	}
}

// oldTurn is how a page was turned before it was kept in its own color
// model: drawn into RGBA, then turned pixel by pixel.
func oldTurn(img image.Image, deg int) *image.RGBA {
	b := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
	w, h := b.Dx(), b.Dy()
	ow, oh := w, h
	if deg == 90 || deg == 270 {
		ow, oh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, ow, oh))
	for y := range h {
		for x := range w {
			dx, dy := x, y
			switch deg {
			case 90:
				dx, dy = h-1-y, x
			case 180:
				dx, dy = w-1-x, h-1-y
			case 270:
				dx, dy = y, w-1-x
			}
			out.SetRGBA(dx, dy, rgba.RGBAAt(x, y))
		}
	}
	return out
}

// rgbAt is the color of a pixel of a turned page in RGB, as draw computes it.
func rgbAt(t *testing.T, img image.Image, x, y int) color.RGBA {
	t.Helper()
	switch m := img.(type) {
	case *image.YCbCr:
		c := m.YCbCrAt(x, y)
		r, g, b := color.YCbCrToRGB(c.Y, c.Cb, c.Cr)
		return color.RGBA{R: r, G: g, B: b, A: 255}
	case *image.Gray:
		v := m.GrayAt(x, y).Y
		return color.RGBA{R: v, G: v, B: v, A: 255}
	}
	t.Fatalf("a turned page is a %T, want the decoder's YCbCr or Gray", img)
	return color.RGBA{}
}

// TestTurnKeepsTheColorModel turns pages of every shape the decoder gives,
// odd sizes included, which do not split into whole chroma blocks, and
// checks that each pixel is what the old way through RGBA gave.
func TestTurnKeepsTheColorModel(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	var pages []image.Image
	for _, ratio := range []image.YCbCrSubsampleRatio{
		image.YCbCrSubsampleRatio420, image.YCbCrSubsampleRatio422,
		image.YCbCrSubsampleRatio440, image.YCbCrSubsampleRatio444,
	} {
		for _, size := range [][2]int{{6, 4}, {8, 6}, {7, 5}, {6, 5}, {7, 4}} {
			// A larger image cut down, as the decoder pads to whole blocks.
			m := image.NewYCbCr(image.Rect(0, 0, 16, 16), ratio)
			for i := range m.Y {
				m.Y[i] = uint8(rng.UintN(256))
			}
			for i := range m.Cb {
				m.Cb[i], m.Cr[i] = uint8(rng.UintN(256)), uint8(rng.UintN(256))
			}
			pages = append(pages, m.SubImage(image.Rect(0, 0, size[0], size[1])))
		}
	}
	gray := image.NewGray(image.Rect(0, 0, 16, 16))
	for i := range gray.Pix {
		gray.Pix[i] = uint8(rng.UintN(256))
	}
	pages = append(pages, gray.SubImage(image.Rect(0, 0, 7, 5)), gray.SubImage(image.Rect(0, 0, 6, 4)))

	for _, src := range pages {
		for _, deg := range []int{0, 90, 180, 270} {
			name := fmt.Sprintf("%T %v turned %d", src, src.Bounds().Size(), deg)
			if m, ok := src.(*image.YCbCr); ok {
				name = fmt.Sprintf("%s %v %v turned %d", "YCbCr", m.SubsampleRatio, m.Rect.Size(), deg)
			}
			got := turn(src, deg)
			want := oldTurn(src, deg)
			if got.Bounds() != want.Bounds() {
				t.Errorf("%s: bounds %v, want %v", name, got.Bounds(), want.Bounds())
				continue
			}
			bad := 0
			for y := range want.Rect.Dy() {
				for x := range want.Rect.Dx() {
					if rgbAt(t, got, x, y) != want.RGBAAt(x, y) {
						bad++
					}
				}
			}
			if bad > 0 {
				t.Errorf("%s: %d pixels differ from the old turn", name, bad)
			}
			// A page of whole chroma blocks keeps its subsampling, so it
			// takes no more memory turned than it did before.
			if m, ok := src.(*image.YCbCr); ok && deg != 0 && m.Rect.Dx()%2 == 0 && m.Rect.Dy()%2 == 0 {
				want := m.SubsampleRatio
				if deg != 180 && want == image.YCbCrSubsampleRatio422 {
					want = image.YCbCrSubsampleRatio440
				} else if deg != 180 && want == image.YCbCrSubsampleRatio440 {
					want = image.YCbCrSubsampleRatio422
				}
				if r := got.(*image.YCbCr).SubsampleRatio; r != want {
					t.Errorf("%s: turned into %v, want %v", name, r, want)
				}
			}
		}
	}
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

// Names is what Save will write, so the page can show it first: with a
// title long enough to be cut, with several numbered pictures, and with the
// plain name taken.
func TestNamesAreWhatSaveWrites(t *testing.T) {
	st, _ := newStore(t)
	s := newSession(t, st)
	addPages(t, s, 3)
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "2026-11-14 Taken.pdf"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("Beitragsbescheid ", 18) // about 300 characters
	for _, c := range []struct{ title, format string }{
		{long, "jpeg"}, {long, "png"}, {long, "pdf"}, {"Taken", "pdf"}, {"Fresh", "jpeg"},
	} {
		shown, err := Names(folder, c.title, c.format, 3, saveNow)
		if err != nil {
			t.Fatalf("%s: %v", c.format, err)
		}
		written, err := Save(s, folder, c.title, c.format, saveNow)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(shown, written) {
			t.Errorf("%.20q as %s: shown %q, written %q", c.title, c.format, shown, written)
		}
	}
	if got, _ := Names(folder, "Taken", "pdf", 3, saveNow); len(got) != 1 || filepath.Base(got[0]) != "2026-11-14 Taken (3).pdf" {
		t.Errorf("a taken name shows as %q", got)
	}
	if _, err := Names(folder, "X", "tiff", 3, saveNow); err == nil {
		t.Error("Names took a format Save does not")
	}
	if _, err := Names(folder, "X", "pdf", 0, saveNow); err == nil {
		t.Error("Names named a save of no pages")
	}
}

// A session discarded under a running scan gives ErrGone, which a caller
// can tell apart from a failure.
func TestGoneIsExported(t *testing.T) {
	st, _ := newStore(t)
	s := newSession(t, st)
	if err := st.Discard(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPage(strings.NewReader("x")); !errors.Is(err, ErrGone) {
		t.Errorf("AddPage after Discard: %v, want ErrGone", err)
	}
}
