// SPDX-License-Identifier: AGPL-3.0-or-later

package scan

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func grayJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pdfObject returns the text of object n, from its "n 0 obj" line to its
// "endobj", found through the xref table.
func pdfObject(t *testing.T, doc []byte, offsets []int, n int) []byte {
	t.Helper()
	if n < 1 || n >= len(offsets) {
		t.Fatalf("object %d is not in the xref table of %d entries", n, len(offsets))
	}
	rest := doc[offsets[n]:]
	end := bytes.Index(rest, []byte("endobj"))
	if end < 0 {
		t.Fatalf("object %d has no endobj", n)
	}
	return rest[:end]
}

// pdfXref reads the xref table the startxref line points at and returns the
// offset of every object, indexed by object number. It checks that each
// offset points at "N 0 obj".
func pdfXref(t *testing.T, doc []byte) []int {
	t.Helper()
	m := regexp.MustCompile(`startxref\n(\d+)\n%%EOF\n$`).FindSubmatch(doc)
	if m == nil {
		t.Fatalf("no startxref at the end: %q", doc[max(0, len(doc)-60):])
	}
	start, _ := strconv.Atoi(string(m[1]))
	if !bytes.HasPrefix(doc[start:], []byte("xref\n0 ")) {
		t.Fatalf("startxref %d does not point at the xref table", start)
	}
	lines := strings.Split(string(doc[start:]), "\n")
	count, err := strconv.Atoi(strings.TrimPrefix(lines[1], "0 "))
	if err != nil {
		t.Fatalf("bad xref header %q", lines[1])
	}
	offsets := make([]int, count)
	for i := range count {
		entry := lines[2+i]
		if len(entry) != 19 { // the 20th byte is the newline Split removed
			t.Fatalf("xref entry %d is %q, not 20 bytes", i, entry+"\n")
		}
		if i == 0 {
			if entry != "0000000000 65535 f " {
				t.Fatalf("xref entry 0 is %q", entry)
			}
			continue
		}
		off, err := strconv.Atoi(entry[:10])
		if err != nil || !strings.HasSuffix(entry, " 00000 n ") {
			t.Fatalf("xref entry %d is %q", i, entry)
		}
		want := strconv.Itoa(i) + " 0 obj\n"
		if !bytes.HasPrefix(doc[off:], []byte(want)) {
			t.Fatalf("xref entry %d points at %q, want %q", i, doc[off:off+min(20, len(doc)-off)], want)
		}
		offsets[i] = off
	}
	if !bytes.Contains(doc[start:], []byte("/Size "+strconv.Itoa(count))) {
		t.Fatalf("trailer does not say /Size %d", count)
	}
	return offsets
}

func TestPDFHasEveryPageTheRightWay(t *testing.T) {
	files := map[string][]byte{
		"a": jpegBytes(t, 8, 6),
		"b": jpegBytes(t, 9, 6),
		"c": grayJPEG(t, 10, 6),
	}
	pages := []Page{
		{File: "a", Width: 2480, Height: 3508, DPI: 300},
		{File: "b", Width: 2480, Height: 3508, DPI: 300, Rotate: 90},
		{File: "c", Width: 2480, Height: 3508, DPI: 300},
	}
	var doc bytes.Buffer
	err := WritePDF(&doc, pages, func(p Page) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(files[p.File])), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b := doc.Bytes()
	if !bytes.HasPrefix(b, []byte("%PDF-1.4")) {
		t.Fatalf("header is %q", b[:12])
	}
	offsets := pdfXref(t, b)

	pageObjs := regexp.MustCompile(`<< /Type /Page /Parent[^\n]*>>`).FindAll(b, -1)
	if len(pageObjs) != 3 {
		t.Fatalf("%d page objects, want 3", len(pageObjs))
	}
	if n := len(regexp.MustCompile(`/Type /Page[^s]`).FindAll(b, -1)); n != 3 {
		t.Fatalf("%d /Type /Page, want 3", n)
	}
	if !bytes.Contains(b, []byte("/Type /Pages")) || !bytes.Contains(b, []byte("/Type /Catalog")) {
		t.Fatal("no pages tree or catalog")
	}

	wantSpace := []string{"/DeviceRGB", "/DeviceRGB", "/DeviceGray"}
	for i, po := range pageObjs {
		rotated := bytes.Contains(po, []byte("/Rotate 90"))
		if rotated != (i == 1) {
			t.Errorf("page %d: /Rotate 90 present = %v", i+1, rotated)
		}
		if bytes.Contains(po, []byte("/Rotate")) != (i == 1) {
			t.Errorf("page %d: unexpected /Rotate in %s", i+1, po)
		}
		mb := regexp.MustCompile(`/MediaBox \[0 0 ([0-9.]+) ([0-9.]+)\]`).FindSubmatch(po)
		if mb == nil {
			t.Fatalf("page %d has no MediaBox: %s", i+1, po)
		}
		w, _ := strconv.ParseFloat(string(mb[1]), 64)
		h, _ := strconv.ParseFloat(string(mb[2]), 64)
		if w < 594 || w > 596 || h < 841 || h > 843 {
			t.Errorf("page %d MediaBox is %v x %v, want about 595 x 842", i+1, w, h)
		}

		im := regexp.MustCompile(`/Im0 (\d+) 0 R`).FindSubmatch(po)
		if im == nil {
			t.Fatalf("page %d names no image: %s", i+1, po)
		}
		imgNum, _ := strconv.Atoi(string(im[1]))
		img := pdfObject(t, b, offsets, imgNum)
		for _, want := range []string{"/Subtype /Image", "/Filter /DCTDecode", "/BitsPerComponent 8", "/Width 2480", "/Height 3508", "/ColorSpace " + wantSpace[i]} {
			if !bytes.Contains(img, []byte(want)) {
				t.Errorf("page %d image lacks %q: %.200s", i+1, want, img)
			}
		}
		// The length is an indirect object after the stream, so the
		// writer need not know it before copying.
		ln := regexp.MustCompile(`/Length (\d+) 0 R`).FindSubmatch(img)
		if ln == nil {
			t.Fatalf("page %d image has no indirect /Length: %.200s", i+1, img)
		}
		lnNum, _ := strconv.Atoi(string(ln[1]))
		length, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(string(pdfObject(t, b, offsets, lnNum)), strconv.Itoa(lnNum)+" 0 obj\n")))
		if err != nil {
			t.Fatalf("page %d: bad length object: %v", i+1, err)
		}
		want := files[string(rune('a'+i))]
		if length != len(want) {
			t.Errorf("page %d: /Length %d, JPEG is %d bytes", i+1, length, len(want))
		}
		_, after, ok := bytes.Cut(img, []byte("stream\n"))
		if !ok {
			t.Fatalf("page %d image has no stream", i+1)
		}
		if got := after[:len(after)-len("\nendstream\n")]; !bytes.Equal(got, want) {
			t.Errorf("page %d: stream is not the JPEG unchanged (%d bytes, want %d)", i+1, len(got), len(want))
		}

		ct := regexp.MustCompile(`/Contents (\d+) 0 R`).FindSubmatch(po)
		if ct == nil {
			t.Fatalf("page %d has no contents: %s", i+1, po)
		}
		cn, _ := strconv.Atoi(string(ct[1]))
		if !regexp.MustCompile(`q [0-9.]+ 0 0 [0-9.]+ 0 0 cm /Im0 Do Q`).Match(pdfObject(t, b, offsets, cn)) {
			t.Errorf("page %d content stream is %s", i+1, pdfObject(t, b, offsets, cn))
		}
	}
}

func TestPDFFailsWhenAPageCannotBeOpened(t *testing.T) {
	boom := errors.New("gone")
	err := WritePDF(io.Discard, []Page{{File: "a", Width: 8, Height: 6, DPI: 300}}, func(Page) (io.ReadCloser, error) {
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
	if err := WritePDF(io.Discard, nil, nil); err == nil {
		t.Fatal("a PDF with no pages was written")
	}
}
