package bson

import (
	"errors"

	"github.com/tamnd/doc/sys"
)

const idField = "_id"

var (
	// ErrInvalidIDType reports an _id whose BSON type is not allowed as an
	// identity (spec 2061 doc 02 §5.3): array, null, the deprecated types, the
	// sentinels, and types without well-defined equality.
	ErrInvalidIDType = errors.New("bson: value type is not a valid _id")
)

// ValidIDType reports whether t may be a document identity. The allowed types are
// the scalars plus embedded document (compound keys); arrays and the rejected
// list of doc 02 §5.3 are not.
func ValidIDType(t Type) bool {
	switch t {
	case TypeDouble, TypeString, TypeBinary, TypeObjectID, TypeBoolean,
		TypeDateTime, TypeInt32, TypeTimestamp, TypeInt64, TypeDecimal128, TypeDocument:
		return true
	default:
		// Array, Null, Undefined, Regex, DBPointer, JavaScript, Symbol,
		// CodeWithScope, MinKey, MaxKey are all rejected as identities.
		return false
	}
}

// EnsureID normalizes a document for storage: it guarantees an `_id` field, moves
// it to the first position (spec 2061 doc 02 §5.1), and returns the normalized
// document together with the identity value. If the input has no `_id`, a fresh
// ObjectId is generated with gen and prepended. An `_id` of a disallowed type is
// rejected with ErrInvalidIDType. The input is first deep-validated.
//
// The returned RawValue aliases the returned Raw, not the input, so it stays
// valid as long as the caller holds the normalized document.
func EnsureID(d Raw, gen sys.IDGenerator) (Raw, RawValue, error) {
	if err := d.Validate(); err != nil {
		return nil, RawValue{}, err
	}
	elems, err := d.Elements()
	if err != nil {
		return nil, RawValue{}, err
	}

	idIdx := -1
	for i, e := range elems {
		if e.Key == idField {
			idIdx = i
			break
		}
	}

	if idIdx < 0 {
		// No _id: mint one and prepend.
		oid := gen.NewID()
		b := NewBuilder()
		b.AppendObjectID(idField, oid)
		for _, e := range elems {
			b.AppendValue(e.Key, e.Value)
		}
		out := b.Build()
		idv, _ := out.Lookup(idField)
		return out, idv, nil
	}

	if !ValidIDType(elems[idIdx].Value.Type) {
		return nil, RawValue{}, ErrInvalidIDType
	}

	if idIdx == 0 {
		// Already first and valid: return the document unchanged, but re-fetch the
		// value against d so the RawValue aliases the document we return.
		idv, _ := d.Lookup(idField)
		return d, idv, nil
	}

	// Present but not first: re-emit with _id first.
	b := NewBuilder()
	b.AppendValue(idField, elems[idIdx].Value)
	for i, e := range elems {
		if i == idIdx {
			continue
		}
		b.AppendValue(e.Key, e.Value)
	}
	out := b.Build()
	idv, _ := out.Lookup(idField)
	return out, idv, nil
}

// IDOf returns the _id value of an already-normalized document. It is a thin
// Lookup that names the invariant: a stored document always has _id first.
func IDOf(d Raw) (RawValue, bool) { return d.Lookup(idField) }
