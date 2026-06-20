package update

import (
	"time"

	"github.com/tamnd/doc/bson"
)

// apply dispatches one compiled operation against the document tree.
func (u *Update) apply(root *container, op *operation, now time.Time) error {
	switch op.kind {
	case opSet:
		return applySet(root, op.path, op.arg)
	case opUnset:
		return applyUnset(root, op.path)
	case opInc:
		return applyArith(root, op.path, op.arg, addNumeric)
	case opMul:
		return applyArith(root, op.path, op.arg, mulNumeric)
	case opMin:
		return applyMinMax(root, op.path, op.arg, true)
	case opMax:
		return applyMinMax(root, op.path, op.arg, false)
	case opRename:
		return applyRename(root, op.path, op.dest)
	case opCurrentDateDate:
		return applySet(root, op.path, dateValue(now))
	case opCurrentDateTimestamp:
		return applySet(root, op.path, timestampValue(now))
	default:
		return ErrBadUpdate
	}
}

// applySet sets the leaf at path, creating intermediate documents as needed.
func applySet(root *container, path []string, v bson.RawValue) error {
	parent, leaf, ok, err := resolve(root, path, true, false)
	if err != nil || !ok {
		return err
	}
	parent.setLeaf(leaf, v)
	return nil
}

// applyUnset removes the leaf at path. A missing path is a no-op. Removing an
// element of an array sets it to null (preserving the array's length), matching
// MongoDB's positional-unset semantics.
func applyUnset(root *container, path []string) error {
	parent, leaf, ok, err := resolve(root, path, false, false)
	if err != nil || !ok || parent == nil {
		return err
	}
	if parent.array {
		if parent.find(leaf) >= 0 {
			parent.setLeaf(leaf, nullValue())
		}
		return nil
	}
	parent.remove(leaf)
	return nil
}

// applyArith applies an arithmetic operator ($inc/$mul). A missing field is
// created from the operand (zero base for $inc, so the result is the operand; the
// $mul base of zero yields zero, matching MongoDB).
func applyArith(root *container, path []string, arg bson.RawValue, combine func(cur, arg bson.RawValue) (bson.RawValue, error)) error {
	parent, leaf, ok, err := resolve(root, path, true, false)
	if err != nil || !ok {
		return err
	}
	cur, present := parent.leafValue(leaf)
	if !present {
		cur = zeroLike(arg)
	}
	res, cerr := combine(cur, arg)
	if cerr != nil {
		return cerr
	}
	parent.setLeaf(leaf, res)
	return nil
}

// applyMinMax sets the leaf to arg when arg orders before (min) or after (max) the
// current value by BSON comparison; a missing field is set unconditionally.
func applyMinMax(root *container, path []string, arg bson.RawValue, isMin bool) error {
	parent, leaf, ok, err := resolve(root, path, true, false)
	if err != nil || !ok {
		return err
	}
	cur, present := parent.leafValue(leaf)
	if !present {
		parent.setLeaf(leaf, arg)
		return nil
	}
	cmp := bson.Compare(arg, cur)
	if (isMin && cmp < 0) || (!isMin && cmp > 0) {
		parent.setLeaf(leaf, arg)
	}
	return nil
}

// applyRename moves the value at src to dest. A missing source is a no-op. Neither
// path may traverse an array.
func applyRename(root *container, src, dest []string) error {
	parent, leaf, ok, err := resolve(root, src, false, true)
	if err != nil || !ok || parent == nil {
		return err
	}
	moved, present := parent.takeNode(leaf)
	if !present {
		return nil
	}
	dparent, dleaf, ok, err := resolve(root, dest, true, true)
	if err != nil {
		return err
	}
	if !ok {
		return ErrPathConflict
	}
	dparent.setNode(dleaf, moved)
	return nil
}
