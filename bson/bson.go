// Package bson holds the BSON value model and codec for doc (spec 2061 doc 02).
//
// Raw is the opaque, length-prefixed document the storage SPI and record store
// traffic in. On top of Raw, M2 adds the read side (Type, RawValue and its typed
// accessors, Elements, Lookup, deep Validate) and the write side (Builder), plus
// the `_id` normalization the write path needs (EnsureID, ValidIDType). The
// order-preserving key encoding that turns these values into B-tree keys lives in
// package index, driven by RawValue. Dotted-path access and the comparison engine
// arrive with the query layer in M3.
package bson

import "errors"

// Raw is an opaque, length-prefixed BSON document as it appears on the wire and
// on the page. The first four bytes are a little-endian int32 holding the total
// document length including those four bytes and the trailing NUL terminator, so
// a Raw is self-describing: Len reads the prefix without decoding the body.
//
// A nil Raw is the absence of a document; a zero-length Raw is invalid because a
// valid BSON document is at least five bytes (the length prefix plus the
// terminating NUL).
type Raw []byte

// MinDocLen is the smallest legal BSON document: a 4-byte length prefix followed
// by the single 0x00 terminator, encoding the empty document {}.
const MinDocLen = 5

// ErrTooShort reports a Raw that cannot hold even an empty document.
var ErrTooShort = errors.New("bson: document shorter than 5 bytes")

// ErrLengthMismatch reports a Raw whose length prefix disagrees with len(r).
var ErrLengthMismatch = errors.New("bson: length prefix does not match slice length")

// Len returns the document length encoded in the 4-byte little-endian prefix.
// It does not validate that the prefix matches len(r); use Validate for that.
// Len panics if r is shorter than four bytes, which is a programming error: a
// Raw that short is never produced by the codec or read back from a page.
func (r Raw) Len() int {
	_ = r[3] // bounds-check hint; panics with a clear index error if too short
	return int(uint32(r[0]) | uint32(r[1])<<8 | uint32(r[2])<<16 | uint32(r[3])<<24)
}

// Clone returns a deep copy of r. The record store hands callers Raw values that
// alias the buffer pool; a caller that retains a document past the lifetime of
// its transaction must Clone it first.
func (r Raw) Clone() Raw {
	if r == nil {
		return nil
	}
	out := make(Raw, len(r))
	copy(out, r)
	return out
}

// Empty is the canonical encoding of the empty document {}: a length prefix of 5
// followed by the terminator. It is used as a placeholder and in tests.
var Empty = Raw{0x05, 0x00, 0x00, 0x00, 0x00}
