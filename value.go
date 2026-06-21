package doc

import (
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/sys"
)

// The document model mirrors mongo-go-driver's primitive types so code written
// against the official driver reads the same here (spec 2061 doc 14 §4.2). D and
// M are the two document forms: D is ordered, M is a map. A is an array, E a
// single ordered key-value pair.

// D is an ordered BSON document: a slice of key-value pairs that preserves
// insertion order. Use it for update specs, sort keys, and pipeline stages where
// order is significant.
type D []E

// E is a single element of a D.
type E struct {
	Key   string
	Value any
}

// M is an unordered BSON document. Use it for filters, where key order does not
// matter, and for schema-agnostic decoding.
type M map[string]any

// A is a BSON array.
type A []any

// Map collapses a D into an M, keeping the last value for any duplicated key.
func (d D) Map() M {
	m := make(M, len(d))
	for _, e := range d {
		m[e.Key] = e.Value
	}
	return m
}

// Type re-exports the BSON element type byte so callers need not import the
// internal bson package to switch on a RawValue's type.
type Type = bson.Type

// The BSON element type bytes, re-exported for use with RawValue.Type.
const (
	TypeDouble     = bson.TypeDouble
	TypeString     = bson.TypeString
	TypeDocument   = bson.TypeDocument
	TypeArray      = bson.TypeArray
	TypeBinary     = bson.TypeBinary
	TypeObjectID   = bson.TypeObjectID
	TypeBoolean    = bson.TypeBoolean
	TypeDateTime   = bson.TypeDateTime
	TypeNull       = bson.TypeNull
	TypeRegex      = bson.TypeRegex
	TypeJavaScript = bson.TypeJavaScript
	TypeInt32      = bson.TypeInt32
	TypeTimestamp  = bson.TypeTimestamp
	TypeInt64      = bson.TypeInt64
	TypeDecimal128 = bson.TypeDecimal128
	TypeMinKey     = bson.TypeMinKey
	TypeMaxKey     = bson.TypeMaxKey
)

// ObjectID is the 12-byte MongoDB ObjectId. It aliases the engine's identity type
// so a value generated here is the same value the storage layer stamps.
type ObjectID = sys.ObjectID

// objectIDGen is the process-wide generator behind NewObjectID. It is safe for
// concurrent use; sys.ObjectIDGenerator advances an atomic counter.
var (
	objectIDGenOnce sync.Once
	objectIDGen     sys.IDGenerator
)

func defaultObjectIDGen() sys.IDGenerator {
	objectIDGenOnce.Do(func() {
		objectIDGen = sys.NewObjectIDGenerator(sys.SystemClock{})
	})
	return objectIDGen
}

// NewObjectID returns a fresh ObjectID stamped with the current time.
func NewObjectID() ObjectID { return defaultObjectIDGen().NewID() }

// ErrInvalidHex reports a hex string that is not a valid 24-character ObjectID.
var ErrInvalidHex = errors.New("doc: invalid ObjectID hex string")

// ObjectIDFromHex parses a 24-character hexadecimal string into an ObjectID.
func ObjectIDFromHex(s string) (ObjectID, error) {
	if len(s) != 24 {
		return ObjectID{}, ErrInvalidHex
	}
	var oid ObjectID
	if _, err := hex.Decode(oid[:], []byte(s)); err != nil {
		return ObjectID{}, ErrInvalidHex
	}
	return oid, nil
}

// DateTime is a BSON UTC datetime: milliseconds since the Unix epoch.
type DateTime int64

// NewDateTime returns the current time as a DateTime.
func NewDateTime() DateTime { return DateTime(time.Now().UnixMilli()) }

// NewDateTimeFromTime converts a time.Time to a DateTime (UTC milliseconds).
func NewDateTimeFromTime(t time.Time) DateTime { return DateTime(t.UnixMilli()) }

// Time converts a DateTime back to a time.Time in UTC.
func (d DateTime) Time() time.Time { return time.UnixMilli(int64(d)).UTC() }

// Binary is a BSON binary value: a subtype byte and an opaque payload.
type Binary struct {
	Subtype byte
	Data    []byte
}

// Regex is a BSON regular expression. It is a value, not a compiled matcher; the
// query engine compiles Pattern with Options when it is used in a filter.
type Regex struct {
	Pattern string
	Options string
}

// JavaScript is a BSON JavaScript-code value (without scope).
type JavaScript string

// Timestamp is the internal BSON timestamp type used for replication and change
// streams: a seconds value T and an ordinal I within that second.
type Timestamp struct {
	T uint32
	I uint32
}

// uint64 packs a Timestamp into its wire representation: the increment in the low
// 32 bits, the seconds in the high 32 bits.
func (ts Timestamp) uint64() uint64 { return uint64(ts.T)<<32 | uint64(ts.I) }

func timestampFromUint64(v uint64) Timestamp {
	return Timestamp{T: uint32(v >> 32), I: uint32(v)}
}

// Decimal128 is a 128-bit IEEE 754 decimal value held in its raw little-endian
// wire bytes. Full decimal arithmetic is out of scope for the library API; the
// type round-trips through storage byte-for-byte and compares by the engine's
// canonical decimal order.
type Decimal128 [16]byte

// MinKey sorts before every other BSON value.
type MinKey struct{}

// MaxKey sorts after every other BSON value.
type MaxKey struct{}

// Null is the BSON null value. A nil interface also encodes as null; this named
// type exists so a struct field can carry an explicit null distinct from a zero
// value.
type Null struct{}
