// SPDX-License-Identifier: AGPL-3.0-or-later

package scan

import (
	"errors"
	"fmt"
	"image/color"
	"image/jpeg"
	"io"
)

// defaultDPI sizes a page whose resolution is not known.
const defaultDPI = 300

// countWriter counts the bytes that pass through it, so the PDF writer knows
// where every object starts without holding the document. It remembers the
// first error, so the writer can check once at the end.
type countWriter struct {
	w   io.Writer
	n   int64
	err error
}

func (c *countWriter) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.err = err
	return n, err
}

func (c *countWriter) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(c, format, args...)
}

// WritePDF writes pages to w as one PDF, a page per image, in order. open
// returns the JPEG file of a page; WritePDF opens each page twice, once for
// the header, to learn the colour space, and once to copy it. The JPEG bytes
// go into the PDF as they are (DCTDecode), so nothing is decoded or held in
// memory, and a page's Rotate becomes the PDF page's /Rotate. A page is
// Width by Height pixels at DPI, which sets its size in points.
//
// Objects 1 and 2 are the catalog and the pages tree. Each page then takes
// four: the image, its length (written after the stream, as the length is
// known only once the JPEG has been copied), the content stream and the page.
func WritePDF(w io.Writer, pages []Page, open func(Page) (io.ReadCloser, error)) error {
	if len(pages) == 0 {
		return errors.New("scan: a PDF needs at least one page")
	}
	c := &countWriter{w: w}
	offsets := make([]int64, 3+4*len(pages)) // by object number; 0 is the free entry
	begin := func(n int) {
		offsets[n] = c.n
		c.printf("%d 0 obj\n", n)
	}

	// A comment of high bytes tells file tools this is binary.
	c.printf("%%PDF-1.4\n%%\xe2\xe3\xcf\xd3\n")

	kids := make([]byte, 0, 16*len(pages))
	for i, p := range pages {
		img, ln, content, page := 3+4*i, 4+4*i, 5+4*i, 6+4*i
		space, err := colorSpace(p, open)
		if err != nil {
			return err
		}
		dpi := p.DPI
		if dpi <= 0 {
			dpi = defaultDPI
		}
		wPt := float64(p.Width) * 72 / float64(dpi)
		hPt := float64(p.Height) * 72 / float64(dpi)

		rc, err := open(p)
		if err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		begin(img)
		c.printf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s /BitsPerComponent 8 /Filter /DCTDecode /Length %d 0 R >>\nstream\n", p.Width, p.Height, space, ln)
		start := c.n
		_, cerr := io.Copy(c, rc)
		rc.Close()
		if cerr != nil {
			return fmt.Errorf("scan: %w", cerr)
		}
		size := c.n - start
		c.printf("\nendstream\nendobj\n")

		begin(ln)
		c.printf("%d\nendobj\n", size)

		draw := fmt.Sprintf("q %.2f 0 0 %.2f 0 0 cm /Im0 Do Q", wPt, hPt)
		begin(content)
		c.printf("<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(draw), draw)

		rotate := ""
		if p.Rotate != 0 {
			rotate = fmt.Sprintf(" /Rotate %d", p.Rotate)
		}
		begin(page)
		c.printf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.2f %.2f]%s /Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>\nendobj\n", wPt, hPt, rotate, img, content)
		kids = fmt.Appendf(kids, " %d 0 R", page)
	}

	begin(2)
	c.printf("<< /Type /Pages /Kids [%s ] /Count %d >>\nendobj\n", kids[1:], len(pages))
	begin(1)
	c.printf("<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")

	xref := c.n
	c.printf("xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, off := range offsets[1:] {
		c.printf("%010d 00000 n \n", off)
	}
	c.printf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	if c.err != nil {
		return fmt.Errorf("scan: %w", c.err)
	}
	return nil
}

// colorSpace reads the JPEG header of a page and names the PDF colour space
// that fits it.
func colorSpace(p Page, open func(Page) (io.ReadCloser, error)) (string, error) {
	rc, err := open(p)
	if err != nil {
		return "", fmt.Errorf("scan: %w", err)
	}
	defer rc.Close()
	cfg, err := jpeg.DecodeConfig(rc)
	if err != nil {
		return "", fmt.Errorf("scan: %s: %w", p.File, err)
	}
	switch cfg.ColorModel {
	case color.GrayModel:
		return "/DeviceGray", nil
	case color.CMYKModel:
		// Adobe-style CMYK JPEGs store the inks inverted, as Go's own
		// decoder assumes.
		return "/DeviceCMYK /Decode [1 0 1 0 1 0 1 0]", nil
	}
	return "/DeviceRGB", nil
}
