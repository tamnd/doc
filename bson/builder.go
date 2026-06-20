package bson

import (
	"encoding/binary"
	"math"

	"github.com/tamnd/doc/sys"
)

// Builder encodes a BSON document field by field. It appends elements to an
// internal body buffer and frames it with the length prefix and terminator on
// Build. The zero Builder is ready to use; reuse one across documents with Reset.
//
// Builder is the write-side counterpart to the reader: where Elements/Lookup walk
// stored bytes, Builder produces them. It is deliberately low-level (one method
// per BSON type) so the higher layers control field order, which BSON makes
// significant (spec 2061 doc 02 §2.3).
type Builder struct {
	body []byte
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder { return &Builder{} }

// Reset clears the Builder for reuse, keeping the backing array.
func (b *Builder) Reset() { b.body = b.body[:0] }

func (b *Builder) elem(t Type, key string) {
	b.body = append(b.body, byte(t))
	b.body = append(b.body, key...)
	b.body = append(b.body, 0x00)
}

// AppendValue appends an element carrying a pre-encoded RawValue verbatim. This
// is how _id normalization re-emits existing fields without re-decoding them.
func (b *Builder) AppendValue(key string, v RawValue) *Builder {
	b.elem(v.Type, key)
	b.body = append(b.body, v.Data...)
	return b
}

func (b *Builder) AppendDouble(key string, v float64) *Builder {
	b.elem(TypeDouble, key)
	b.body = binary.LittleEndian.AppendUint64(b.body, math.Float64bits(v))
	return b
}

func (b *Builder) AppendString(key, v string) *Builder {
	b.elem(TypeString, key)
	b.body = binary.LittleEndian.AppendUint32(b.body, uint32(len(v)+1))
	b.body = append(b.body, v...)
	b.body = append(b.body, 0x00)
	return b
}

func (b *Builder) AppendInt32(key string, v int32) *Builder {
	b.elem(TypeInt32, key)
	b.body = binary.LittleEndian.AppendUint32(b.body, uint32(v))
	return b
}

func (b *Builder) AppendInt64(key string, v int64) *Builder {
	b.elem(TypeInt64, key)
	b.body = binary.LittleEndian.AppendUint64(b.body, uint64(v))
	return b
}

func (b *Builder) AppendObjectID(key string, v sys.ObjectID) *Builder {
	b.elem(TypeObjectID, key)
	b.body = append(b.body, v[:]...)
	return b
}

func (b *Builder) AppendBoolean(key string, v bool) *Builder {
	b.elem(TypeBoolean, key)
	if v {
		b.body = append(b.body, 0x01)
	} else {
		b.body = append(b.body, 0x00)
	}
	return b
}

func (b *Builder) AppendDateTime(key string, msec int64) *Builder {
	b.elem(TypeDateTime, key)
	b.body = binary.LittleEndian.AppendUint64(b.body, uint64(msec))
	return b
}

func (b *Builder) AppendTimestamp(key string, v uint64) *Builder {
	b.elem(TypeTimestamp, key)
	b.body = binary.LittleEndian.AppendUint64(b.body, v)
	return b
}

func (b *Builder) AppendNull(key string) *Builder {
	b.elem(TypeNull, key)
	return b
}

func (b *Builder) AppendBinary(key string, subtype byte, data []byte) *Builder {
	b.elem(TypeBinary, key)
	b.body = binary.LittleEndian.AppendUint32(b.body, uint32(len(data)))
	b.body = append(b.body, subtype)
	b.body = append(b.body, data...)
	return b
}

// AppendDocument appends a nested document (or array) value already encoded as a
// Raw. The caller guarantees it is well-formed; Validate on the outer document
// re-checks it.
func (b *Builder) AppendDocument(key string, doc Raw) *Builder {
	b.elem(TypeDocument, key)
	b.body = append(b.body, doc...)
	return b
}

// AppendArray appends an array value already encoded as a Raw whose keys are the
// ascending decimal indices "0", "1", ... (the BSON array representation). Build
// the array body with BuildArray.
func (b *Builder) AppendArray(key string, arr Raw) *Builder {
	b.elem(TypeArray, key)
	b.body = append(b.body, arr...)
	return b
}

// BuildArray frames a sequence of values into a BSON array: a document whose keys
// are the ascending decimal indices of the values. Append it with AppendArray.
func BuildArray(vals ...RawValue) Raw {
	b := NewBuilder()
	for i, v := range vals {
		b.AppendValue(itoa(i), v)
	}
	return b.Build()
}

// itoa renders a small non-negative int as a decimal string without importing
// strconv into the hot builder path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Build frames the accumulated body into a finished document and returns it. The
// returned Raw is a fresh allocation independent of the Builder's buffer.
func (b *Builder) Build() Raw {
	total := 4 + len(b.body) + 1
	out := make(Raw, total)
	binary.LittleEndian.PutUint32(out, uint32(total))
	copy(out[4:], b.body)
	out[total-1] = 0x00
	return out
}
