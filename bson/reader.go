package bson

import (
	"bytes"
	"encoding/binary"
	"errors"
	"unicode/utf8"
)

// MaxDepth is the maximum nesting depth of embedded documents and arrays (spec
// 2061 doc 02 §4.7); deeper documents are rejected on validation.
const MaxDepth = 100

// MaxDocSize is the 16 MiB ceiling on a single document (spec 2061 doc 02 §4.2).
const MaxDocSize = 16 * 1024 * 1024

var (
	// ErrMalformed reports a structurally invalid document: a truncated element,
	// a bad length, or a missing terminator surfaced while walking the body.
	ErrMalformed = errors.New("bson: malformed document")
	// ErrInvalidUTF8 reports a field name or string value with invalid UTF-8.
	ErrInvalidUTF8 = errors.New("bson: invalid UTF-8")
	// ErrEmbeddedNUL reports a string value containing a NUL byte.
	ErrEmbeddedNUL = errors.New("bson: embedded NUL in string")
	// ErrTooDeep reports nesting beyond MaxDepth.
	ErrTooDeep = errors.New("bson: document nesting too deep")
	// ErrTooLarge reports a document exceeding MaxDocSize.
	ErrTooLarge = errors.New("bson: document exceeds 16 MiB")
)

// Element is one decoded field: its key and a lazy view of its value.
type Element struct {
	Key   string
	Value RawValue
}

// Elements walks the document and returns its elements in stored order. It does a
// structural walk only (lengths and terminators line up); it does not deep
// validate UTF-8 or nesting. Use Validate for that. The returned RawValues alias
// r's bytes.
func (r Raw) Elements() ([]Element, error) {
	if len(r) < MinDocLen {
		return nil, ErrTooShort
	}
	if r.Len() != len(r) {
		return nil, ErrLengthMismatch
	}
	var out []Element
	off := 4
	for {
		if off >= len(r) {
			return nil, ErrMalformed
		}
		t := Type(r[off])
		if t == 0x00 {
			if off != len(r)-1 {
				return nil, ErrMalformed
			}
			return out, nil
		}
		off++
		ke := cstrEnd(r, off)
		if ke < 0 {
			return nil, ErrMalformed
		}
		key := string(r[off:ke])
		off = ke + 1
		n, err := scanValue(r[off:], t)
		if err != nil {
			return nil, err
		}
		out = append(out, Element{Key: key, Value: RawValue{Type: t, Data: r[off : off+n]}})
		off += n
	}
}

// Lookup returns the first value with the given key, reporting whether it was
// found. It stops at the first match without allocating an element slice.
func (r Raw) Lookup(key string) (RawValue, bool) {
	if len(r) < MinDocLen || r.Len() != len(r) {
		return RawValue{}, false
	}
	off := 4
	for off < len(r) {
		t := Type(r[off])
		if t == 0x00 {
			return RawValue{}, false
		}
		off++
		ke := cstrEnd(r, off)
		if ke < 0 {
			return RawValue{}, false
		}
		k := string(r[off:ke])
		off = ke + 1
		n, err := scanValue(r[off:], t)
		if err != nil {
			return RawValue{}, false
		}
		if k == key {
			return RawValue{Type: t, Data: r[off : off+n]}, true
		}
		off += n
	}
	return RawValue{}, false
}

// scanValue returns the number of payload bytes the value of type t occupies at
// the start of b. It is the single source of truth for BSON value sizing.
func scanValue(b []byte, t Type) (int, error) {
	switch t {
	case TypeDouble, TypeDateTime, TypeTimestamp, TypeInt64:
		return need(b, 8)
	case TypeInt32:
		return need(b, 4)
	case TypeObjectID:
		return need(b, 12)
	case TypeBoolean:
		return need(b, 1)
	case TypeDecimal128:
		return need(b, 16)
	case TypeNull, TypeUndefined, TypeMinKey, TypeMaxKey:
		return 0, nil
	case TypeString, TypeJavaScript, TypeSymbol:
		if len(b) < 4 {
			return 0, ErrMalformed
		}
		n := int(binary.LittleEndian.Uint32(b))
		if n < 1 || 4+n > len(b) || b[4+n-1] != 0x00 {
			return 0, ErrMalformed
		}
		return 4 + n, nil
	case TypeDocument, TypeArray:
		if len(b) < 4 {
			return 0, ErrMalformed
		}
		n := int(binary.LittleEndian.Uint32(b))
		if n < MinDocLen || n > len(b) || b[n-1] != 0x00 {
			return 0, ErrMalformed
		}
		return n, nil
	case TypeBinary:
		if len(b) < 5 {
			return 0, ErrMalformed
		}
		n := int(binary.LittleEndian.Uint32(b))
		if n < 0 || 5+n > len(b) {
			return 0, ErrMalformed
		}
		return 5 + n, nil
	case TypeRegex:
		i := cstrEnd(b, 0)
		if i < 0 {
			return 0, ErrMalformed
		}
		j := cstrEnd(b, i+1)
		if j < 0 {
			return 0, ErrMalformed
		}
		return j + 1, nil
	case TypeDBPointer:
		if len(b) < 4 {
			return 0, ErrMalformed
		}
		n := int(binary.LittleEndian.Uint32(b))
		if n < 1 || 4+n+12 > len(b) {
			return 0, ErrMalformed
		}
		return 4 + n + 12, nil
	case TypeCodeWithScope:
		if len(b) < 4 {
			return 0, ErrMalformed
		}
		n := int(binary.LittleEndian.Uint32(b))
		if n < 4 || n > len(b) {
			return 0, ErrMalformed
		}
		return n, nil
	default:
		return 0, ErrMalformed
	}
}

func need(b []byte, n int) (int, error) {
	if len(b) < n {
		return 0, ErrMalformed
	}
	return n, nil
}

// Validate performs a deep structural and semantic check of the document: every
// element sizes correctly, field names and string values are valid UTF-8 without
// embedded NULs, nesting is within MaxDepth, and the whole document is within
// MaxDocSize (spec 2061 doc 02 §4, §11.1). It is the gate every document passes
// before it is stored.
func (r Raw) Validate() error {
	if err := r.WellFormed(); err != nil {
		return err
	}
	if len(r) > MaxDocSize {
		return ErrTooLarge
	}
	return validateDoc(r, 1)
}

// WellFormed performs only the cheap framing check the storage layer depends on:
// the slice is at least an empty document, the length prefix matches len(r), and
// the trailing byte is the NUL terminator. It does not walk elements or check
// UTF-8 and nesting; the collection layer applies the deep Validate before a
// document reaches storage, while the heap treats a document as opaque bytes and
// only needs the framing to be intact (spec 2061 doc 02 §11.1, doc 19 §22 M1).
func (r Raw) WellFormed() error {
	if len(r) < MinDocLen {
		return ErrTooShort
	}
	if r.Len() != len(r) {
		return ErrLengthMismatch
	}
	if r[len(r)-1] != 0x00 {
		return ErrLengthMismatch
	}
	return nil
}

func validateDoc(b []byte, depth int) error {
	if depth > MaxDepth {
		return ErrTooDeep
	}
	if len(b) < MinDocLen || int(binary.LittleEndian.Uint32(b)) != len(b) || b[len(b)-1] != 0x00 {
		return ErrMalformed
	}
	off := 4
	for {
		t := Type(b[off])
		if t == 0x00 {
			if off != len(b)-1 {
				return ErrMalformed
			}
			return nil
		}
		off++
		ke := cstrEnd(b, off)
		if ke < 0 {
			return ErrMalformed
		}
		name := b[off:ke]
		if !utf8.Valid(name) {
			return ErrInvalidUTF8
		}
		off = ke + 1
		n, err := scanValue(b[off:], t)
		if err != nil {
			return err
		}
		val := b[off : off+n]
		switch t {
		case TypeString, TypeJavaScript, TypeSymbol:
			s := val[4 : 4+int(binary.LittleEndian.Uint32(val))-1]
			if !utf8.Valid(s) {
				return ErrInvalidUTF8
			}
			if bytes.IndexByte(s, 0x00) >= 0 {
				return ErrEmbeddedNUL
			}
		case TypeDocument, TypeArray:
			if err := validateDoc(val, depth+1); err != nil {
				return err
			}
		}
		off += n
	}
}
