// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ipp encodes and decodes the Internet Printing Protocol binary
// format (RFC 8010). It covers what skra asks CUPS for: requests made of
// integers, booleans, enums and strings, and responses read the same way.
package ipp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Tag is a value tag.
type Tag byte

const (
	TagInteger  Tag = 0x21
	TagBoolean  Tag = 0x22
	TagEnum     Tag = 0x23
	TagText     Tag = 0x41
	TagName     Tag = 0x42
	TagKeyword  Tag = 0x44
	TagURI      Tag = 0x45
	TagCharset  Tag = 0x47
	TagLanguage Tag = 0x48
	TagMime     Tag = 0x49
)

// The collection delimiters. The decoder skips a collection whole.
const (
	tagBegCollection Tag = 0x34
	tagEndCollection Tag = 0x37
)

// Group is a delimiter tag that opens an attribute group.
type Group byte

const (
	GroupOperation Group = 0x01
	GroupJob       Group = 0x02
	GroupEnd       Group = 0x03
	GroupPrinter   Group = 0x04
)

// Attr is one attribute. Values hold int32, bool or string; a value the
// decoder does not interpret (dateTime, resolution, rangeOfInteger, a value
// of the wrong length, an out-of-band marker) is nil.
type Attr struct {
	Tag    Tag
	Name   string
	Values []any
}

func strs(tag Tag, name string, v []string) Attr {
	a := Attr{Tag: tag, Name: name}
	for _, s := range v {
		a.Values = append(a.Values, s)
	}
	return a
}

func ints(tag Tag, name string, v []int) Attr {
	a := Attr{Tag: tag, Name: name}
	for _, n := range v {
		a.Values = append(a.Values, int32(n))
	}
	return a
}

func Keyword(name string, v ...string) Attr { return strs(TagKeyword, name, v) }
func Name(name string, v ...string) Attr    { return strs(TagName, name, v) }
func Text(name string, v ...string) Attr    { return strs(TagText, name, v) }
func URI(name string, v ...string) Attr     { return strs(TagURI, name, v) }
func Mime(name string, v ...string) Attr    { return strs(TagMime, name, v) }
func Int(name string, v ...int) Attr        { return ints(TagInteger, name, v) }
func Enum(name string, v ...int) Attr       { return ints(TagEnum, name, v) }

func Bool(name string, v bool) Attr {
	return Attr{Tag: TagBoolean, Name: name, Values: []any{v}}
}

// AttrGroup is the attributes under one group delimiter.
type AttrGroup struct {
	Group Group
	Attrs []Attr
}

func (g AttrGroup) find(name string) *Attr {
	for i := range g.Attrs {
		if g.Attrs[i].Name == name {
			return &g.Attrs[i]
		}
	}
	return nil
}

// String returns the first string value of name, or "".
func (g AttrGroup) String(name string) string {
	if s := g.Strings(name); len(s) > 0 {
		return s[0]
	}
	return ""
}

// Strings returns the string values of name; other value types are left out.
func (g AttrGroup) Strings(name string) []string {
	a := g.find(name)
	if a == nil {
		return nil
	}
	var out []string
	for _, v := range a.Values {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Int returns the first integer or enum value of name.
func (g AttrGroup) Int(name string) (int, bool) {
	if n := g.Ints(name); len(n) > 0 {
		return n[0], true
	}
	return 0, false
}

// Ints returns the integer and enum values of name; other types are left out.
func (g AttrGroup) Ints(name string) []int {
	a := g.find(name)
	if a == nil {
		return nil
	}
	var out []int
	for _, v := range a.Values {
		if n, ok := v.(int32); ok {
			out = append(out, int(n))
		}
	}
	return out
}

// Bool returns the first boolean value of name.
func (g AttrGroup) Bool(name string) (bool, bool) {
	a := g.find(name)
	if a == nil {
		return false, false
	}
	for _, v := range a.Values {
		if b, ok := v.(bool); ok {
			return b, true
		}
	}
	return false, false
}

// Request is an IPP request under construction.
type Request struct {
	op                 uint16
	oper, job, printer []Attr
}

// NewRequest starts a request for op. The operation group already holds
// attributes-charset and attributes-natural-language, as IPP requires.
func NewRequest(op uint16) *Request {
	return &Request{op: op, oper: []Attr{
		{Tag: TagCharset, Name: "attributes-charset", Values: []any{"utf-8"}},
		{Tag: TagLanguage, Name: "attributes-natural-language", Values: []any{"en"}},
	}}
}

// Op adds attributes to the operation group.
func (r *Request) Op(a ...Attr) *Request { r.oper = append(r.oper, a...); return r }

// Job adds attributes to the job group.
func (r *Request) Job(a ...Attr) *Request { r.job = append(r.job, a...); return r }

// Printer adds attributes to the printer group.
func (r *Request) Printer(a ...Attr) *Request { r.printer = append(r.printer, a...); return r }

// Encode returns the wire form: version 2.0, request id 1, then the operation,
// job and printer groups (the last two only when not empty) and the end tag.
// A name or value longer than 65535 bytes is cut to fit its 16-bit length.
func (r *Request) Encode() []byte {
	b := []byte{2, 0, byte(r.op >> 8), byte(r.op), 0, 0, 0, 1}
	for _, g := range []struct {
		tag   Group
		attrs []Attr
	}{{GroupOperation, r.oper}, {GroupJob, r.job}, {GroupPrinter, r.printer}} {
		if len(g.attrs) == 0 {
			continue
		}
		b = append(b, byte(g.tag))
		for _, a := range g.attrs {
			for i, v := range a.Values {
				name := a.Name
				if i > 0 {
					name = "" // an additional value carries no name
				}
				b = append(b, byte(a.Tag))
				b = appendField(b, []byte(name))
				b = appendField(b, valueBytes(v))
			}
		}
	}
	return append(b, byte(GroupEnd))
}

func appendField(b, f []byte) []byte {
	if len(f) > math.MaxUint16 {
		f = f[:math.MaxUint16]
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(f)))
	return append(b, f...)
}

func valueBytes(v any) []byte {
	switch v := v.(type) {
	case int32:
		return binary.BigEndian.AppendUint32(nil, uint32(v))
	case int:
		return binary.BigEndian.AppendUint32(nil, uint32(int32(v)))
	case bool:
		if v {
			return []byte{1}
		}
		return []byte{0}
	case string:
		return []byte(v)
	}
	return nil
}

// Response is a decoded IPP message. Status is the status code of a response
// (the operation id when the bytes came from a request). The operation group,
// which holds status-message, is in Groups like any other.
type Response struct {
	Status uint16
	Groups []AttrGroup
}

// Of returns the groups of kind g in wire order.
func (r *Response) Of(g Group) []AttrGroup {
	var out []AttrGroup
	for _, x := range r.Groups {
		if x.Group == g {
			out = append(out, x)
		}
	}
	return out
}

// StatusError is an IPP error status with the server's status-message.
type StatusError struct {
	Status  uint16
	Message string
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("IPP status 0x%04x", e.Status)
}

// Err returns a *StatusError when Status is 0x0100 or higher, else nil.
func (r *Response) Err() error {
	if r.Status < 0x0100 {
		return nil
	}
	e := &StatusError{Status: r.Status}
	for _, g := range r.Of(GroupOperation) {
		if m := g.String("status-message"); m != "" {
			e.Message = m
			break
		}
	}
	return e
}

var errTruncated = errors.New("ipp: message truncated")

// Decode parses an IPP message. Collections are skipped whole; dateTime,
// resolution and rangeOfInteger values decode to nil. Bytes after the
// end-of-attributes tag (document data) are ignored. It returns an error, never
// panics, on malformed input.
func Decode(b []byte) (*Response, error) {
	if len(b) < 8 {
		return nil, errTruncated
	}
	res := &Response{Status: binary.BigEndian.Uint16(b[2:4])}
	i := 8
	var (
		group *AttrGroup
		cur   = -1 // index in group.Attrs that additional values attach to; -1 when skipped
		depth int
	)
	// Attrs of the open group are flushed into Groups when it closes.
	flush := func() {
		if group != nil {
			res.Groups = append(res.Groups, *group)
			group = nil
		}
	}
	for {
		if i >= len(b) {
			return nil, errTruncated
		}
		t := Tag(b[i])
		i++
		if t < 0x10 {
			if Group(t) == GroupEnd {
				if depth != 0 {
					return nil, errors.New("ipp: unterminated collection")
				}
				flush()
				return res, nil
			}
			if depth != 0 {
				return nil, errors.New("ipp: group inside a collection")
			}
			flush()
			group = &AttrGroup{Group: Group(t)}
			cur = -1
			continue
		}
		if i+2 > len(b) {
			return nil, errTruncated
		}
		nl := int(binary.BigEndian.Uint16(b[i:]))
		i += 2
		if i+nl+2 > len(b) {
			return nil, errTruncated
		}
		name := string(b[i : i+nl])
		i += nl
		vl := int(binary.BigEndian.Uint16(b[i:]))
		i += 2
		if i+vl > len(b) {
			return nil, errTruncated
		}
		val := b[i : i+vl]
		i += vl

		switch {
		case t == tagBegCollection:
			depth++
			if depth == 1 {
				cur = -1 // the whole attribute is dropped, extra values included
			}
			continue
		case t == tagEndCollection:
			depth--
			if depth < 0 {
				return nil, errors.New("ipp: end of collection without a start")
			}
			continue
		case depth > 0:
			continue
		case group == nil:
			return nil, errors.New("ipp: attribute outside a group")
		}

		var v any
		switch {
		case (t == TagInteger || t == TagEnum) && vl == 4:
			v = int32(binary.BigEndian.Uint32(val))
		case t == TagBoolean && vl == 1:
			v = val[0] == 1
		case t == TagInteger || t == TagEnum || t == TagBoolean:
			v = nil // wrong length
		case t < 0x40:
			v = nil // out-of-band, dateTime, resolution, range
		default:
			v = string(val)
		}
		if name != "" {
			group.Attrs = append(group.Attrs, Attr{Tag: t, Name: name, Values: []any{v}})
			cur = len(group.Attrs) - 1
		} else if cur >= 0 {
			group.Attrs[cur].Values = append(group.Attrs[cur].Values, v)
		}
	}
}
