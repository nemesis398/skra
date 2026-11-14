// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ipptest builds IPP responses by hand for tests, because package ipp
// only encodes requests.
package ipptest

import (
	"encoding/binary"

	"github.com/nemesis398/skra/internal/ipp"
)

// Wire is an IPP response under construction.
type Wire struct{ b []byte }

// NewWire starts a response with status and the operation group's charset
// and language.
func NewWire(status uint16) *Wire {
	w := &Wire{b: []byte{2, 0, byte(status >> 8), byte(status), 0, 0, 0, 1}}
	return w.Group(ipp.GroupOperation).
		Attr(ipp.TagCharset, "attributes-charset", []byte("utf-8")).
		Attr(ipp.TagLanguage, "attributes-natural-language", []byte("en"))
}

// Group starts an attribute group.
func (w *Wire) Group(g ipp.Group) *Wire { w.b = append(w.b, byte(g)); return w }

// Attr appends one attribute value as raw bytes.
func (w *Wire) Attr(tag ipp.Tag, name string, v []byte) *Wire {
	w.b = append(w.b, byte(tag))
	w.b = binary.BigEndian.AppendUint16(w.b, uint16(len(name)))
	w.b = append(w.b, name...)
	w.b = binary.BigEndian.AppendUint16(w.b, uint16(len(v)))
	w.b = append(w.b, v...)
	return w
}

// Str appends a string attribute with one or more values.
func (w *Wire) Str(tag ipp.Tag, name string, vs ...string) *Wire {
	for i, v := range vs {
		n := name
		if i > 0 {
			n = ""
		}
		w.Attr(tag, n, []byte(v))
	}
	return w
}

// Num appends an integer or enum attribute with one or more values.
func (w *Wire) Num(tag ipp.Tag, name string, vs ...int) *Wire {
	for i, v := range vs {
		n := name
		if i > 0 {
			n = ""
		}
		w.Attr(tag, n, binary.BigEndian.AppendUint32(nil, uint32(int32(v))))
	}
	return w
}

// Boolean appends a boolean attribute.
func (w *Wire) Boolean(name string, v bool) *Wire {
	b := byte(0)
	if v {
		b = 1
	}
	return w.Attr(ipp.TagBoolean, name, []byte{b})
}

// Bytes returns the encoded response, end tag included. The Wire stays usable.
func (w *Wire) Bytes() []byte { return append(append([]byte(nil), w.b...), byte(ipp.GroupEnd)) }
