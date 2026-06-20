package bson

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/tamnd/doc/sys"
)

// ErrWrongType reports a typed accessor called on a RawValue of another type.
var ErrWrongType = errors.New("bson: value is not of the requested type")

// ErrCorruptValue reports a RawValue whose Data is too short for its type.
var ErrCorruptValue = errors.New("bson: value payload is truncated")

// RawValue is a single BSON value: its type byte and the raw payload bytes that
// follow the element name on the wire (spec 2061 doc 02 §4.3). It is a lazy view
// into a document buffer, so a RawValue read from a Raw aliases that Raw's bytes
// and is valid only as long as the Raw is. Clone the owning Raw to retain a value
// past its buffer's lifetime.
type RawValue struct {
	Type Type
	Data []byte
}

// Double returns the value of a Double. It panics on a type mismatch; use
// DoubleOK when the type is not known.
func (v RawValue) Double() float64 { f, _ := v.DoubleOK(); return f }

func (v RawValue) DoubleOK() (float64, bool) {
	if v.Type != TypeDouble || len(v.Data) < 8 {
		return 0, false
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(v.Data)), true
}

// Int32, Int64, DateTime, Timestamp, Boolean read their fixed-size payloads.
func (v RawValue) Int32() int32 { i, _ := v.Int32OK(); return i }

func (v RawValue) Int32OK() (int32, bool) {
	if v.Type != TypeInt32 || len(v.Data) < 4 {
		return 0, false
	}
	return int32(binary.LittleEndian.Uint32(v.Data)), true
}

func (v RawValue) Int64() int64 { i, _ := v.Int64OK(); return i }

func (v RawValue) Int64OK() (int64, bool) {
	if v.Type != TypeInt64 || len(v.Data) < 8 {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(v.Data)), true
}

func (v RawValue) DateTime() int64 {
	if v.Type != TypeDateTime || len(v.Data) < 8 {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(v.Data))
}

func (v RawValue) Timestamp() uint64 {
	if v.Type != TypeTimestamp || len(v.Data) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(v.Data)
}

func (v RawValue) Boolean() bool {
	return v.Type == TypeBoolean && len(v.Data) >= 1 && v.Data[0] != 0x00
}

// AsFloat64 promotes any numeric value to float64 for cross-type numeric
// comparison (spec 2061 doc 02 §7.3). Decimal128 is not promoted here (it needs a
// decimal library); it reports false.
func (v RawValue) AsFloat64() (float64, bool) {
	switch v.Type {
	case TypeDouble:
		return v.DoubleOK()
	case TypeInt32:
		i, ok := v.Int32OK()
		return float64(i), ok
	case TypeInt64:
		i, ok := v.Int64OK()
		return float64(i), ok
	default:
		return 0, false
	}
}

// StringValue returns a String/Symbol/JavaScript value's text (without the BSON
// length prefix or terminator).
func (v RawValue) StringValue() string { s, _ := v.StringValueOK(); return s }

func (v RawValue) StringValueOK() (string, bool) {
	switch v.Type {
	case TypeString, TypeSymbol, TypeJavaScript:
	default:
		return "", false
	}
	if len(v.Data) < 4 {
		return "", false
	}
	n := int(binary.LittleEndian.Uint32(v.Data))
	if n < 1 || 4+n > len(v.Data) {
		return "", false
	}
	return string(v.Data[4 : 4+n-1]), true // n includes the terminator
}

// ObjectID returns a 12-byte ObjectId value.
func (v RawValue) ObjectID() sys.ObjectID {
	var o sys.ObjectID
	if v.Type == TypeObjectID && len(v.Data) >= 12 {
		copy(o[:], v.Data[:12])
	}
	return o
}

// Binary returns a binary value's subtype and payload bytes.
func (v RawValue) Binary() (subtype byte, data []byte, ok bool) {
	if v.Type != TypeBinary || len(v.Data) < 5 {
		return 0, nil, false
	}
	n := int(binary.LittleEndian.Uint32(v.Data))
	if n < 0 || 5+n > len(v.Data) {
		return 0, nil, false
	}
	return v.Data[4], v.Data[5 : 5+n], true
}

// Decimal128 returns the raw 16-byte IEEE 754-2008 encoding.
func (v RawValue) Decimal128() [16]byte {
	var d [16]byte
	if v.Type == TypeDecimal128 && len(v.Data) >= 16 {
		copy(d[:], v.Data[:16])
	}
	return d
}

// Document returns an embedded document (or array) value as a Raw.
func (v RawValue) Document() Raw {
	if (v.Type == TypeDocument || v.Type == TypeArray) && len(v.Data) >= MinDocLen {
		return Raw(v.Data)
	}
	return nil
}

// Regex returns a regex value's pattern and options cstrings.
func (v RawValue) Regex() (pattern, options string, ok bool) {
	if v.Type != TypeRegex {
		return "", "", false
	}
	i := cstrEnd(v.Data, 0)
	if i < 0 {
		return "", "", false
	}
	j := cstrEnd(v.Data, i+1)
	if j < 0 {
		return "", "", false
	}
	return string(v.Data[:i]), string(v.Data[i+1 : j]), true
}

// cstrEnd returns the index of the terminating NUL at or after off, or -1.
func cstrEnd(b []byte, off int) int {
	for i := off; i < len(b); i++ {
		if b[i] == 0x00 {
			return i
		}
	}
	return -1
}
