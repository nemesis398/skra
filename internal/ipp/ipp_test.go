// SPDX-License-Identifier: AGPL-3.0-or-later

package ipp

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEncodeGetPrinterAttributes(t *testing.T) {
	b := NewRequest(OpGetPrinterAttributes).
		Op(URI("printer-uri", "ipp://localhost/printers/A"),
			Keyword("requested-attributes", "printer-state", "marker-levels")).
		Encode()

	want := []byte{0x02, 0x00, 0x00, 0x0b, 0x00, 0x00, 0x00, 0x01, 0x01, 0x47}
	if !bytes.HasPrefix(b, want) {
		t.Fatalf("header = % x, want prefix % x", b[:len(want)], want)
	}
	if b[len(b)-1] != 0x03 {
		t.Errorf("last byte = %#x, want end-of-attributes 0x03", b[len(b)-1])
	}

	// The second value of a multi-valued attribute: keyword tag, zero-length
	// name, then the value.
	second := append([]byte{0x44, 0x00, 0x00, 0x00, byte(len("marker-levels"))}, "marker-levels"...)
	if !bytes.Contains(b, second) {
		t.Errorf("second value not encoded with a zero-length name:\n% x", b)
	}

	// charset and language lead the operation group.
	lead := []byte{0x47, 0x00, 0x12}
	lead = append(lead, "attributes-charset"...)
	lead = append(lead, 0x00, 0x05)
	lead = append(lead, "utf-8"...)
	lead = append(lead, 0x48, 0x00, 0x1b)
	lead = append(lead, "attributes-natural-language"...)
	lead = append(lead, 0x00, 0x02)
	lead = append(lead, "en"...)
	if !bytes.HasPrefix(b[8:], append([]byte{0x01}, lead...)) {
		t.Errorf("operation group does not begin with charset and language:\n% x", b[8:])
	}
}

func TestEncodeGroupOrderAndEmptyGroups(t *testing.T) {
	// Added out of order on purpose.
	b := NewRequest(OpPrintJob).
		Printer(Name("printer-name", "A")).
		Job(Int("copies", 2)).
		Encode()
	resp, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	var order []Group
	for _, g := range resp.Groups {
		order = append(order, g.Group)
	}
	if want := []Group{GroupOperation, GroupJob, GroupPrinter}; !reflect.DeepEqual(order, want) {
		t.Errorf("group order = %v, want %v", order, want)
	}

	// No job or printer group: operation group and end only.
	resp, err = Decode(NewRequest(OpCUPSGetPrinters).Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Groups) != 1 || resp.Groups[0].Group != GroupOperation {
		t.Errorf("groups = %+v, want only the operation group", resp.Groups)
	}
}

func TestRoundTrip(t *testing.T) {
	b := NewRequest(OpGetPrinterAttributes).
		Op(URI("printer-uri", "ipp://localhost/printers/A")).
		Printer(
			Name("printer-name", "Fußball"),
			Enum("printer-state", 3),
			Int("marker-levels", 68, 48, 56, 78),
			Bool("printer-is-accepting-jobs", true),
			Bool("off", false),
			Keyword("printer-state-reasons", "none"),
		).Encode()

	resp, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != OpGetPrinterAttributes {
		t.Errorf("Status = %#x", resp.Status)
	}
	if resp.Err() != nil {
		t.Errorf("Err() = %v, want nil", resp.Err())
	}
	pg := resp.Of(GroupPrinter)
	if len(pg) != 1 {
		t.Fatalf("printer groups = %d, want 1", len(pg))
	}
	p := pg[0]
	if got := p.Strings("marker-levels"); len(got) != 0 {
		t.Errorf("Strings(marker-levels) = %v, want empty", got)
	}
	if got := p.Ints("marker-levels"); !reflect.DeepEqual(got, []int{68, 48, 56, 78}) {
		t.Errorf("Ints(marker-levels) = %v", got)
	}
	if v, ok := p.Bool("printer-is-accepting-jobs"); !ok || !v {
		t.Errorf("Bool(accepting) = %v, %v", v, ok)
	}
	if v, ok := p.Bool("off"); !ok || v {
		t.Errorf("Bool(off) = %v, %v", v, ok)
	}
	if v, ok := p.Int("printer-state"); !ok || v != 3 {
		t.Errorf("Int(printer-state) = %v, %v", v, ok)
	}
	if got := p.String("printer-name"); got != "Fußball" {
		t.Errorf("String(printer-name) = %q", got)
	}
	if _, ok := p.Int("absent"); ok {
		t.Error("Int(absent) reported ok")
	}
	if _, ok := p.Bool("absent"); ok {
		t.Error("Bool(absent) reported ok")
	}
	if p.String("absent") != "" || p.Strings("absent") != nil || p.Ints("absent") != nil {
		t.Error("absent attribute should give zero values")
	}
	if got := resp.Of(GroupOperation)[0].String("attributes-charset"); got != "utf-8" {
		t.Errorf("attributes-charset = %q", got)
	}
	if len(resp.Of(GroupJob)) != 0 {
		t.Error("unexpected job group")
	}
}

// attr appends one IPP attribute (tag, name, value) to b.
func attr(b []byte, tag Tag, name string, value []byte) []byte {
	b = append(b, byte(tag), byte(len(name)>>8), byte(len(name)))
	b = append(b, name...)
	b = append(b, byte(len(value)>>8), byte(len(value)))
	return append(b, value...)
}

func header(status uint16) []byte {
	return []byte{0x02, 0x00, byte(status >> 8), byte(status), 0, 0, 0, 1}
}

func TestDecodeSkipsCollections(t *testing.T) {
	b := header(0)
	b = append(b, byte(GroupPrinter))
	b = attr(b, 0x34, "media-col-default", nil)
	b = attr(b, 0x4A, "", []byte("media-size"))
	b = attr(b, 0x34, "", nil) // nested collection
	b = attr(b, TagInteger, "x-dimension", []byte{0, 0, 0x52, 0x08})
	b = attr(b, 0x37, "", nil)
	b = attr(b, 0x37, "", nil)
	b = attr(b, TagName, "printer-name", []byte("A"))
	b = append(b, byte(GroupEnd))

	resp, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	g := resp.Of(GroupPrinter)
	if len(g) != 1 || len(g[0].Attrs) != 1 || g[0].Attrs[0].Name != "printer-name" {
		t.Fatalf("groups = %+v, want only printer-name", resp.Groups)
	}
	if g[0].String("printer-name") != "A" {
		t.Errorf("printer-name = %q", g[0].String("printer-name"))
	}
}

func TestDecodeSkippedValueTypesAreNil(t *testing.T) {
	b := header(0)
	b = append(b, byte(GroupJob))
	b = attr(b, 0x31, "time", make([]byte, 11))
	b = attr(b, 0x32, "res", make([]byte, 9))
	b = attr(b, 0x33, "rng", make([]byte, 8))
	b = attr(b, TagKeyword, "after", []byte("ok"))
	b = append(b, byte(GroupEnd))
	resp, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	g := resp.Of(GroupJob)[0]
	for _, n := range []string{"time", "res", "rng"} {
		var found *Attr
		for i := range g.Attrs {
			if g.Attrs[i].Name == n {
				found = &g.Attrs[i]
			}
		}
		if found == nil || len(found.Values) != 1 || found.Values[0] != nil {
			t.Errorf("%s = %+v, want a single nil value", n, found)
		}
	}
	if g.String("after") != "ok" {
		t.Error("attribute after the skipped ones lost")
	}
}

func TestDecodeMultipleGroups(t *testing.T) {
	b := header(0)
	b = append(b, byte(GroupOperation))
	b = attr(b, TagCharset, "attributes-charset", []byte("utf-8"))
	b = append(b, byte(GroupJob))
	b = attr(b, TagInteger, "job-id", []byte{0, 0, 0, 7})
	b = append(b, byte(GroupJob))
	b = attr(b, TagInteger, "job-id", []byte{0, 0, 0, 8})
	b = append(b, byte(GroupEnd))
	resp, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	jobs := resp.Of(GroupJob)
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	if v, _ := jobs[0].Int("job-id"); v != 7 {
		t.Errorf("first job-id = %d", v)
	}
	if v, _ := jobs[1].Int("job-id"); v != 8 {
		t.Errorf("second job-id = %d", v)
	}
}

func TestStatusError(t *testing.T) {
	b := header(0x0406)
	b = append(b, byte(GroupOperation))
	b = attr(b, TagText, "status-message", []byte("No PPDs"))
	b = append(b, byte(GroupEnd))

	resp, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	var se *StatusError
	if !errors.As(resp.Err(), &se) {
		t.Fatalf("Err() = %v, want *StatusError", resp.Err())
	}
	if se.Status != StatusNotFound || se.Message != "No PPDs" {
		t.Errorf("StatusError = %+v", se)
	}
	if se.Error() != "No PPDs" {
		t.Errorf("Error() = %q", se.Error())
	}
	if (&StatusError{Status: 0x0406}).Error() == "" {
		t.Error("Error() empty without a message")
	}

	// Below 0x0100 is success.
	ok, _ := Decode(append(header(0x0001), byte(GroupEnd)))
	if ok.Err() != nil {
		t.Errorf("Err() on 0x0001 = %v", ok.Err())
	}
}

func TestDecodeTruncated(t *testing.T) {
	if _, err := Decode([]byte{0x02, 0x00, 0x00, 0x00, 0x00}); err == nil {
		t.Error("5-byte input decoded without error")
	}
	if _, err := Decode(nil); err == nil {
		t.Error("nil input decoded without error")
	}

	// Every prefix of a valid message must fail or succeed, never panic.
	full := NewRequest(OpGetPrinterAttributes).
		Op(Keyword("requested-attributes", "a", "b")).
		Printer(Int("n", 1, 2), Bool("b", true)).Encode()
	for i := 0; i < len(full); i++ {
		if _, err := Decode(full[:i]); err == nil {
			t.Errorf("prefix of %d/%d bytes decoded without error", i, len(full))
		}
	}
	if _, err := Decode(full); err != nil {
		t.Errorf("full message: %v", err)
	}
}

func TestDecodeMalformed(t *testing.T) {
	cases := map[string][]byte{
		"value before group":        append(attr(header(0), TagName, "n", []byte("x")), byte(GroupEnd)),
		"unbalanced end collection": append(append(append(header(0), byte(GroupJob)), attr(nil, 0x37, "", nil)...), byte(GroupEnd)),
		"length past the end":       append(append(header(0), byte(GroupJob)), byte(TagName), 0x00, 0x01, 'n', 0xff, 0xff, 'x'),
		"no end tag":                attr(append(header(0), byte(GroupJob)), TagName, "n", []byte("x")),
		"reserved tag 0x00":         append(attr(append(header(0), 0x00), TagName, "n", []byte("x")), byte(GroupEnd)),
		"group inside a collection": append(append(attr(append(header(0), byte(GroupPrinter)), 0x34, "col", nil), byte(GroupJob)), attr(nil, 0x37, "", nil)...),
	}
	for name, b := range cases {
		if _, err := Decode(b); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// A value after a skipped collection carries no name, so it belongs to the
// collection and is dropped with it, not attached to the attribute before.
func TestDecodeDropsAdditionalValueAfterSkippedCollection(t *testing.T) {
	b := header(0)
	b = append(b, byte(GroupPrinter))
	b = attr(b, TagName, "printer-name", []byte("A"))
	b = attr(b, 0x34, "media-col-ready", nil)
	b = attr(b, 0x37, "", nil)
	b = attr(b, 0x34, "", nil) // the collection's second value
	b = attr(b, 0x37, "", nil)
	b = attr(b, TagKeyword, "", []byte("stray"))
	b = attr(b, TagKeyword, "sides-default", []byte("one-sided"))
	b = append(b, byte(GroupEnd))
	resp, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	g := resp.Of(GroupPrinter)[0]
	if got := g.Strings("printer-name"); !reflect.DeepEqual(got, []string{"A"}) {
		t.Errorf("printer-name = %q, want only A", got)
	}
	if g.String("sides-default") != "one-sided" {
		t.Errorf("sides-default = %q", g.String("sides-default"))
	}
}

// An integer of the wrong length decodes to nil in its place, so the values
// after it keep their positions.
func TestDecodeWrongLengthIntegerKeepsItsPlace(t *testing.T) {
	b := header(0)
	b = append(b, byte(GroupPrinter))
	b = attr(b, TagInteger, "marker-levels", []byte{0, 0, 0, 50})
	b = attr(b, TagInteger, "", []byte{0, 60})
	b = attr(b, TagInteger, "", []byte{0, 0, 0, 70})
	b = append(b, byte(GroupEnd))
	resp, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	g := resp.Of(GroupPrinter)[0]
	if got := g.Attrs[0].Values; !reflect.DeepEqual(got, []any{int32(50), nil, int32(70)}) {
		t.Errorf("Values = %#v, want 50, nil, 70", got)
	}
	if got := g.Ints("marker-levels"); !reflect.DeepEqual(got, []int{50, 70}) {
		t.Errorf("Ints = %v, want [50 70]", got)
	}
	if got := g.IntsAt("marker-levels", -1); !reflect.DeepEqual(got, []int{50, -1, 70}) {
		t.Errorf("IntsAt = %v, want [50 -1 70]", got)
	}
	if g.IntsAt("absent", -1) != nil {
		t.Error("IntsAt(absent) should be nil")
	}
}

func TestStringsAtKeepsPositions(t *testing.T) {
	g := AttrGroup{Attrs: []Attr{{Tag: TagName, Name: "marker-names", Values: []any{"Black", nil, int32(3), "Yellow"}}}}
	if got := g.StringsAt("marker-names"); !reflect.DeepEqual(got, []string{"Black", "", "", "Yellow"}) {
		t.Errorf("StringsAt = %q", got)
	}
	if g.StringsAt("absent") != nil {
		t.Error("StringsAt(absent) should be nil")
	}
}

// A value longer than its 16-bit length is cut back to the start of a UTF-8
// character, and one of exactly 65535 bytes goes through whole.
func TestLongestField(t *testing.T) {
	long := strings.Repeat("a", 65534) + "ü" // 65536 bytes; byte 65535 is inside ü
	resp, err := Decode(NewRequest(OpPrintJob).Op(Name("job-name", long)).Encode())
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Of(GroupOperation)[0].String("job-name")
	if got != long[:65534] || !utf8.ValidString(got) {
		t.Errorf("cut value = %d bytes, valid %v, want the 65534 a's", len(got), utf8.ValidString(got))
	}

	exact := strings.Repeat("b", 65535)
	resp, err = Decode(NewRequest(OpPrintJob).Op(Name("job-name", exact)).Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Of(GroupOperation)[0].String("job-name"); got != exact {
		t.Errorf("65535-byte value came back as %d bytes", len(got))
	}
}

// With leaves the request it copies alone, also when either is extended later.
func TestRequestWith(t *testing.T) {
	r := NewRequest(OpCUPSGetPrinters).Op(Keyword("requested-attributes", "printer-name"))
	w := r.With(Name("requesting-user-name", "mika"))
	r.Op(Name("other", "x"))

	op := func(req *Request) AttrGroup {
		t.Helper()
		resp, err := Decode(req.Encode())
		if err != nil {
			t.Fatal(err)
		}
		return resp.Of(GroupOperation)[0]
	}
	if g := op(r); g.String("requesting-user-name") != "" || g.String("other") != "x" {
		t.Errorf("original = %+v", g.Attrs)
	}
	if g := op(w); g.String("requesting-user-name") != "mika" || g.String("other") != "" || g.String("requested-attributes") != "printer-name" {
		t.Errorf("copy = %+v", g.Attrs)
	}
}

func FuzzDecode(f *testing.F) {
	f.Add(NewRequest(OpGetPrinterAttributes).
		Op(Keyword("requested-attributes", "a", "b")).
		Printer(Int("n", 1, 2), Bool("b", true), Name("x", "y")).Encode())
	c := header(0)
	c = append(c, byte(GroupPrinter))
	c = attr(c, 0x34, "col", nil)
	c = attr(c, 0x4A, "", []byte("m"))
	c = attr(c, 0x37, "", nil)
	c = append(c, byte(GroupEnd))
	f.Add(c)
	f.Fuzz(func(t *testing.T, b []byte) {
		resp, err := Decode(b)
		if err == nil {
			_ = resp.Err()
			for _, g := range resp.Groups {
				_ = g.String("x")
				_ = g.Ints("x")
			}
		}
	})
}
