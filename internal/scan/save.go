// SPDX-License-Identifier: AGPL-3.0-or-later

package scan

import (
	"bufio"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// maxStemRunes is the longest file name, in characters, before its
	// extension: the date, the title and any number added to keep a name free.
	maxStemRunes = 120
	// maxNameBytes is what a file system takes for one name. A title of
	// emoji can reach it before it reaches maxStemRunes.
	maxNameBytes = 255
	// maxClaimTries bounds the search for a free name.
	maxClaimTries = 1000
	// jpegQuality is for pages that have to be turned and written again.
	jpegQuality = 90
)

var errNoPages = errors.New("scan: there is nothing to save, as the scan has no pages")

// FileName returns the default name of a saved scan: the date of now, a
// space, the title and the extension, such as "2026-11-14 Invoice.pdf". The
// title is made safe for a file name: slashes and control characters become
// spaces, runs of spaces become one, leading dots go, and it is cut to fit
// maxStemRunes. Umlauts and emoji stay. An empty title gives "Scan 09-41",
// with the time of now.
func FileName(title string, now time.Time, ext string) string {
	return stemName(title, now, ext, "", "") + "." + ext
}

// stemName is the name without its extension. tail goes at the end, such as
// " (2)" or " (2) 3", and counts toward the length. widest is the longest tail
// in a set of names, so every name in the set is cut at the same place.
func stemName(title string, now time.Time, ext, tail, widest string) string {
	if len(widest) < len(tail) {
		widest = tail
	}
	base := now.Format("2006-01-02") + " " + cleanTitle(title, now)
	runes := maxStemRunes - utf8.RuneCountInString(widest)
	bytes := maxNameBytes - len(ext) - 1 - len(widest)

	end, count := 0, 0
	for i, r := range base {
		size := utf8.RuneLen(r)
		if count == runes || i+size > bytes {
			break
		}
		count++
		end = i + size
	}
	return strings.TrimRight(base[:end], " .") + tail
}

// cleanTitle makes a title fit for a file name, as FileName describes.
func cleanTitle(title string, now time.Time) string {
	var b strings.Builder
	gap := true // so a leading gap is dropped
	for _, r := range strings.ToValidUTF8(title, " ") {
		if r == '/' || unicode.IsControl(r) || unicode.IsSpace(r) {
			if !gap {
				b.WriteByte(' ')
			}
			gap = true
			continue
		}
		gap = false
		b.WriteRune(r)
	}
	t := strings.TrimLeft(b.String(), ". ")
	t = strings.TrimRight(t, " ")
	if t == "" {
		return "Scan " + now.Format("15-04")
	}
	return t
}

// Save writes the pages of sess into folder and returns the paths it wrote.
// format is "pdf", for one file, or "jpeg" or "png", for one file a page,
// named "… 1.jpg", "… 2.jpg" when there are several pages.
//
// Each file is written whole to a hidden temp file in the folder, then linked
// to its name. A link fails if the name exists, so a file is never
// overwritten and two saves cannot take the same name, and nobody sees a name
// that is empty or half written. When a name is taken, " (2)", " (3)" and so
// on is added, to the whole set of names. A missing folder is created with
// mode 0700. If anything fails, Save removes everything it made, folders
// included, and the folder is as it was.
//
// Save only reads the session, and does not discard it.
func Save(sess *Session, folder, title, format string, now time.Time) (paths []string, err error) {
	var ext string
	var perPage bool
	switch format {
	case "pdf":
		ext = "pdf"
	case "jpeg":
		ext, perPage = "jpg", true
	case "png":
		ext, perPage = "png", true
	default:
		return nil, fmt.Errorf("scan: %q is not a format to save in", format)
	}
	if folder == "" {
		return nil, errors.New("scan: there is no folder to save in")
	}
	pages := sess.PageList()
	if len(pages) == 0 {
		return nil, errNoPages
	}

	folder = filepath.Clean(folder)
	created, err := makeFolder(folder)
	var temps, linked []string
	defer func() {
		for _, p := range temps {
			_ = os.Remove(p)
		}
		if err == nil {
			return
		}
		paths = nil
		for _, p := range linked {
			_ = os.Remove(p)
		}
		for i := len(created) - 1; i >= 0; i-- {
			_ = os.Remove(created[i])
		}
	}()
	if err != nil {
		return nil, err
	}

	n := 1
	if perPage {
		n = len(pages)
	}
	for i := range n {
		tmp, err := writeTemp(folder, func(w io.Writer) error {
			if !perPage {
				return WritePDF(w, pages, func(p Page) (io.ReadCloser, error) {
					return os.Open(filepath.Join(sess.dir, p.File))
				})
			}
			return writeImage(w, filepath.Join(sess.dir, pages[i].File), pages[i], format)
		})
		if tmp != "" {
			temps = append(temps, tmp)
		}
		if err != nil {
			return nil, err
		}
	}

	for try := 1; ; try++ {
		if try > maxClaimTries {
			return nil, errors.New("scan: could not find a free name for the scan")
		}
		tail := ""
		if try > 1 {
			tail = " (" + strconv.Itoa(try) + ")"
		}
		var want []string
		for i := range n {
			t, widest := tail, tail
			if perPage && n > 1 {
				t = tail + " " + strconv.Itoa(i+1)
				widest = tail + " " + strconv.Itoa(n)
			}
			want = append(want, filepath.Join(folder, stemName(title, now, ext, t, widest)+"."+ext))
		}
		got, err := linkAll(temps, want)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		linked = got
		if err != nil {
			return nil, err
		}
		break
	}
	return linked, nil
}

// makeFolder creates folder and the levels above it that are missing, with
// mode 0700, and returns what it created, outermost first. Levels that exist
// keep their mode.
func makeFolder(folder string) (created []string, err error) {
	var missing []string
	for d := folder; ; d = filepath.Dir(d) {
		_, err := os.Stat(d)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("scan: %w", err)
		}
		missing = append(missing, d)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], dirMode); err != nil {
			return created, fmt.Errorf("scan: %w", err)
		}
		created = append(created, missing[i])
	}
	return created, nil
}

// linkAll links each temp file to the path at the same index. A link fails if
// the name exists, so an existing file is never touched. If any link fails
// it removes the names it made and returns the error; for a name that was
// taken the error satisfies errors.Is(err, fs.ErrExist).
func linkAll(temps, paths []string) ([]string, error) {
	var got []string
	for i, p := range paths {
		if err := os.Link(temps[i], p); err != nil {
			for _, g := range got {
				_ = os.Remove(g)
			}
			return nil, fmt.Errorf("scan: %w", err)
		}
		got = append(got, p)
	}
	return got, nil
}

// writeTemp has fill write the content to a new hidden temp file in dir and
// returns its path, with the data on disk. On an error it removes the file
// and returns no path.
func writeTemp(dir string, fill func(io.Writer) error) (string, error) {
	// CreateTemp makes the file 0600 and never reuses a name.
	f, err := os.CreateTemp(dir, ".skra-*.tmp")
	if err != nil {
		return "", fmt.Errorf("scan: %w", err)
	}
	tmp := f.Name()
	bw := bufio.NewWriterSize(f, 256<<10)
	err = fill(bw)
	if err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	err = bw.Flush()
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("scan: %w", err)
	}
	return tmp, nil
}

// writeImage writes one page as a JPEG or PNG. An unturned JPEG page is copied
// as the scanner sent it. Anything else is decoded, turned and encoded.
func writeImage(w io.Writer, path string, p Page, format string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	defer f.Close()
	if format == "jpeg" && p.Rotate == 0 {
		if _, err := io.Copy(w, f); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		return nil
	}
	img, err := jpeg.Decode(bufio.NewReader(f))
	if err != nil {
		return fmt.Errorf("scan: %s: %w", p.File, err)
	}
	img = turn(img, p.Rotate)
	if format == "png" {
		err = png.Encode(w, img)
	} else {
		err = jpeg.Encode(w, img, &jpeg.Options{Quality: jpegQuality})
	}
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	return nil
}

// turn returns img turned clockwise by deg, 0, 90, 180 or 270. The result is
// an *image.Gray if img is gray and an *image.RGBA otherwise.
func turn(img image.Image, deg int) image.Image {
	var pix []byte
	var bpp int
	var rect image.Rectangle
	var stride int
	switch src := img.(type) {
	case *image.Gray:
		pix, bpp, rect, stride = src.Pix, 1, src.Rect, src.Stride
	default:
		// draw has fast paths for the usual JPEG outputs.
		rgba, ok := img.(*image.RGBA)
		if !ok {
			rgba = image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
			draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
		}
		pix, bpp, rect, stride = rgba.Pix, 4, rgba.Rect, rgba.Stride
	}
	w, h := rect.Dx(), rect.Dy()
	ow, oh := w, h
	if deg == 90 || deg == 270 {
		ow, oh = h, w
	}
	out := pix
	if deg != 0 {
		out = make([]byte, ow*oh*bpp)
		for y := range h {
			for x := range w {
				var dx, dy int
				switch deg {
				case 90:
					dx, dy = h-1-y, x
				case 180:
					dx, dy = w-1-x, h-1-y
				default: // 270
					dx, dy = y, w-1-x
				}
				copy(out[(dy*ow+dx)*bpp:(dy*ow+dx+1)*bpp], pix[y*stride+x*bpp:y*stride+(x+1)*bpp])
			}
		}
		stride = ow * bpp
	}
	if bpp == 1 {
		return &image.Gray{Pix: out, Stride: stride, Rect: image.Rect(0, 0, ow, oh)}
	}
	return &image.RGBA{Pix: out, Stride: stride, Rect: image.Rect(0, 0, ow, oh)}
}
