package update

import (
	"errors"

	"github.com/tamnd/doc/bson"
)

// ErrBitType reports a $bit applied to a non-integer field (including a missing
// field, which $bit does not create), matching MongoDB's type error.
var ErrBitType = errors.New("update: $bit requires an integer field")

// bitOp identifies one bitwise sub-operator.
type bitOp int

const (
	bitAnd bitOp = iota
	bitOr
	bitXor
)

// bitSpec is a compiled $bit argument for one path: a sub-operator and an integer
// mask whose width (int32 or int64) is recorded so the result keeps the operand's
// integer type promotion rules out of the way.
type bitSpec struct {
	op   bitOp
	mask int64
}

// compileBit parses a $bit argument document {and|or|xor: <integer>}; exactly one
// sub-operator must be present and its operand must be an integer.
func compileBit(v bson.RawValue) (*bitSpec, error) {
	if v.Type != bson.TypeDocument {
		return nil, ErrBadUpdate
	}
	elems, err := v.Document().Elements()
	if err != nil {
		return nil, err
	}
	if len(elems) != 1 {
		return nil, ErrBadUpdate
	}
	mask, ok := intMask(elems[0].Value)
	if !ok {
		return nil, ErrBadUpdate
	}
	bs := &bitSpec{mask: mask}
	switch elems[0].Key {
	case "and":
		bs.op = bitAnd
	case "or":
		bs.op = bitOr
	case "xor":
		bs.op = bitXor
	default:
		return nil, ErrBadUpdate
	}
	return bs, nil
}

// applyBit applies the bitwise operation to the integer field at path. The field
// must already exist and be an int32 or int64; the result keeps the field's
// width.
func applyBit(root *container, path []string, bs *bitSpec) error {
	parent, leaf, ok, err := resolve(root, path, false, false)
	if err != nil || !ok || parent == nil {
		return err
	}
	cur, present := parent.leafValue(leaf)
	if !present {
		return ErrBitType
	}
	switch cur.Type {
	case bson.TypeInt32:
		r := bitApply(bs, int64(cur.Int32()))
		parent.setLeaf(leaf, int32Value(int32(r)))
	case bson.TypeInt64:
		r := bitApply(bs, cur.Int64())
		parent.setLeaf(leaf, int64Value(r))
	default:
		return ErrBitType
	}
	return nil
}

// bitApply combines cur with the mask under the sub-operator.
func bitApply(bs *bitSpec, cur int64) int64 {
	switch bs.op {
	case bitAnd:
		return cur & bs.mask
	case bitOr:
		return cur | bs.mask
	default:
		return cur ^ bs.mask
	}
}

// intMask reads an integer operand for a $bit mask (int32 or int64 only; a double
// is not a valid bitwise operand).
func intMask(v bson.RawValue) (int64, bool) {
	switch v.Type {
	case bson.TypeInt32:
		return int64(v.Int32()), true
	case bson.TypeInt64:
		return v.Int64(), true
	default:
		return 0, false
	}
}
