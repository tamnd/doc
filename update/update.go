// Package update applies MongoDB update-operator documents to BSON documents.
//
// An update document is either an operator document (every top-level key begins
// with "$", e.g. {$set:{a:1},$inc:{n:2}}) or a replacement document (no operator
// keys); this package compiles and applies the operator form. A compiled Update
// transforms one bson.Raw into a new bson.Raw and reports whether the document
// actually changed, so the write path can distinguish a matched-but-unmodified
// document from a modified one (spec 2061 doc 13 §4, §5).
//
// M3-b implements the field operators $set, $unset, $inc, $mul, $min, $max,
// $rename, and $currentDate over dotted paths through sub-documents (and existing
// array elements). M4 adds the array operators ($push with $each/$sort/$slice/
// $position, $addToSet, $pop, $pull, $pullAll), $bit, and $setOnInsert (applied
// only by ApplyForInsert, the upsert insert branch). The positional operators
// ($, $[], $[id]) and aggregation-pipeline updates remain (spec 2061 doc 19 §22).
package update

import (
	"bytes"
	"errors"
	"strings"
	"time"

	"github.com/tamnd/doc/bson"
)

// ErrBadUpdate reports a malformed update document: a non-operator (replacement)
// document, a mix of operator and plain keys, an unknown operator, or an operator
// whose argument has the wrong shape.
var ErrBadUpdate = errors.New("update: malformed update document")

// ErrConflict reports two operators in one update that target the same path or a
// path that is a prefix of another (MongoDB's "would create a conflict" error).
var ErrConflict = errors.New("update: conflicting update operators")

// ErrPathConflict reports an operator that tries to create a field inside a value
// that is not a document (e.g. $set "a.b" where "a" holds a scalar).
var ErrPathConflict = errors.New("update: cannot create field in non-document")

// ErrNotNumeric reports $inc or $mul applied to a non-numeric field.
var ErrNotNumeric = errors.New("update: operator requires a numeric field")

// ErrOverflow reports an int32 $inc or $mul whose result leaves the int32 range.
var ErrOverflow = errors.New("update: numeric overflow")

// ErrRenameArray reports a $rename whose source or destination path traverses an
// array, which MongoDB does not support.
var ErrRenameArray = errors.New("update: $rename may not traverse an array")

// opKind identifies one field update operator.
type opKind int

const (
	opSet opKind = iota
	opUnset
	opInc
	opMul
	opMin
	opMax
	opRename
	opCurrentDateDate
	opCurrentDateTimestamp
	opPush
	opAddToSet
	opPop
	opPull
	opPullAll
	opBit
)

// operation is one compiled (operator, path, operand) triple. dest is the
// destination path for $rename; arg is the operand for the value operators; the
// pointer fields carry the parsed argument for the array and bitwise operators.
// onlyInsert marks a $setOnInsert operation, applied only when an upsert inserts.
type operation struct {
	kind       opKind
	path       []string
	dest       []string // $rename destination
	arg        bson.RawValue
	push       *pushSpec       // $push
	vals       []bson.RawValue // $addToSet values, $pullAll values
	pull       *pullSpec       // $pull
	bit        *bitSpec        // $bit
	onlyInsert bool            // $setOnInsert
}

// Update is a compiled update-operator document.
type Update struct {
	ops     []operation
	usesNow bool
}

// IsOperatorDoc reports whether an update document is in operator form: its first
// top-level key begins with "$". An empty document is treated as a replacement
// document (false), matching MongoDB. A malformed document also reports false; the
// caller's compile step surfaces the real error.
func IsOperatorDoc(d bson.Raw) bool {
	elems, err := d.Elements()
	if err != nil || len(elems) == 0 {
		return false
	}
	return strings.HasPrefix(elems[0].Key, "$")
}

// Compile parses an operator-form update document into an Update. A replacement
// document (no operator keys) or a mixed document is ErrBadUpdate.
func Compile(d bson.Raw) (*Update, error) {
	elems, err := d.Elements()
	if err != nil {
		return nil, err
	}
	if len(elems) == 0 {
		return nil, ErrBadUpdate
	}
	u := &Update{}
	for _, e := range elems {
		if !strings.HasPrefix(e.Key, "$") {
			return nil, ErrBadUpdate
		}
		if err := u.compileOperator(e.Key, e.Value); err != nil {
			return nil, err
		}
	}
	if err := u.checkConflicts(); err != nil {
		return nil, err
	}
	return u, nil
}

// compileOperator expands one operator element ({$op:{path:operand,...}}) into the
// per-path operations it represents.
func (u *Update) compileOperator(op string, spec bson.RawValue) error {
	if spec.Type != bson.TypeDocument {
		return ErrBadUpdate
	}
	fields, err := spec.Document().Elements()
	if err != nil {
		return err
	}
	for _, f := range fields {
		path := splitPath(f.Key)
		if len(path) == 0 {
			return ErrBadUpdate
		}
		switch op {
		case "$set":
			u.ops = append(u.ops, operation{kind: opSet, path: path, arg: f.Value})
		case "$unset":
			u.ops = append(u.ops, operation{kind: opUnset, path: path})
		case "$inc":
			if !f.Value.Type.IsNumeric() {
				return ErrBadUpdate
			}
			u.ops = append(u.ops, operation{kind: opInc, path: path, arg: f.Value})
		case "$mul":
			if !f.Value.Type.IsNumeric() {
				return ErrBadUpdate
			}
			u.ops = append(u.ops, operation{kind: opMul, path: path, arg: f.Value})
		case "$min":
			u.ops = append(u.ops, operation{kind: opMin, path: path, arg: f.Value})
		case "$max":
			u.ops = append(u.ops, operation{kind: opMax, path: path, arg: f.Value})
		case "$rename":
			dest, derr := renameDest(f.Value)
			if derr != nil {
				return derr
			}
			u.ops = append(u.ops, operation{kind: opRename, path: path, dest: dest})
		case "$currentDate":
			kind, cerr := currentDateKind(f.Value)
			if cerr != nil {
				return cerr
			}
			u.usesNow = true
			u.ops = append(u.ops, operation{kind: kind, path: path})
		case "$setOnInsert":
			u.ops = append(u.ops, operation{kind: opSet, path: path, arg: f.Value, onlyInsert: true})
		case "$push":
			ps, perr := compilePush(f.Value)
			if perr != nil {
				return perr
			}
			u.ops = append(u.ops, operation{kind: opPush, path: path, push: ps})
		case "$addToSet":
			vals, aerr := compileAddToSet(f.Value)
			if aerr != nil {
				return aerr
			}
			u.ops = append(u.ops, operation{kind: opAddToSet, path: path, vals: vals})
		case "$pop":
			dir, perr := compilePop(f.Value)
			if perr != nil {
				return perr
			}
			u.ops = append(u.ops, operation{kind: opPop, path: path, arg: dir})
		case "$pull":
			pl, perr := compilePull(f.Value)
			if perr != nil {
				return perr
			}
			u.ops = append(u.ops, operation{kind: opPull, path: path, pull: pl})
		case "$pullAll":
			vals, perr := compilePullAll(f.Value)
			if perr != nil {
				return perr
			}
			u.ops = append(u.ops, operation{kind: opPullAll, path: path, vals: vals})
		case "$bit":
			bs, berr := compileBit(f.Value)
			if berr != nil {
				return berr
			}
			u.ops = append(u.ops, operation{kind: opBit, path: path, bit: bs})
		default:
			return ErrBadUpdate
		}
	}
	return nil
}

// renameDest reads a $rename destination, which must be a string path.
func renameDest(v bson.RawValue) ([]string, error) {
	s, ok := v.StringValueOK()
	if !ok {
		return nil, ErrBadUpdate
	}
	dest := splitPath(s)
	if len(dest) == 0 {
		return nil, ErrBadUpdate
	}
	return dest, nil
}

// currentDateKind reads a $currentDate spec: true or {$type:"date"} selects a
// Date, {$type:"timestamp"} selects a Timestamp.
func currentDateKind(v bson.RawValue) (opKind, error) {
	switch v.Type {
	case bson.TypeBoolean:
		return opCurrentDateDate, nil
	case bson.TypeDocument:
		t, ok := v.Document().Lookup("$type")
		if !ok {
			return 0, ErrBadUpdate
		}
		s, ok := t.StringValueOK()
		if !ok {
			return 0, ErrBadUpdate
		}
		switch s {
		case "date":
			return opCurrentDateDate, nil
		case "timestamp":
			return opCurrentDateTimestamp, nil
		}
	}
	return 0, ErrBadUpdate
}

// checkConflicts rejects an update whose operators target conflicting paths: the
// same path twice, or one path that is a prefix of another (componentwise).
func (u *Update) checkConflicts() error {
	var paths [][]string
	for _, op := range u.ops {
		paths = append(paths, op.path)
		if op.kind == opRename {
			paths = append(paths, op.dest)
		}
	}
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			if pathPrefixConflict(paths[i], paths[j]) {
				return ErrConflict
			}
		}
	}
	return nil
}

// pathPrefixConflict reports whether one path equals or is a prefix of the other.
func pathPrefixConflict(a, b []string) bool {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Apply applies the update to doc and returns the new document and whether it
// changed. now supplies the value for $currentDate operators (read once for the
// whole update, matching MongoDB). $setOnInsert operations are skipped: they take
// effect only on the insert branch of an upsert (see ApplyForInsert). The input
// document is never mutated.
func (u *Update) Apply(doc bson.Raw, now time.Time) (bson.Raw, bool, error) {
	return u.apply(doc, now, false)
}

// ApplyForInsert applies the update to a freshly constructed document on the
// insert branch of an upsert, where $setOnInsert operations DO take effect (spec
// 2061 doc 13 §5.3, §11.1).
func (u *Update) ApplyForInsert(doc bson.Raw, now time.Time) (bson.Raw, bool, error) {
	return u.apply(doc, now, true)
}

// apply runs every operation against a decoded copy of doc; insert selects
// whether $setOnInsert operations participate.
func (u *Update) apply(doc bson.Raw, now time.Time, insert bool) (bson.Raw, bool, error) {
	root, err := decodeDoc(doc)
	if err != nil {
		return nil, false, err
	}
	for i := range u.ops {
		if u.ops[i].onlyInsert && !insert {
			continue
		}
		if err := u.applyOp(root, &u.ops[i], now); err != nil {
			return nil, false, err
		}
	}
	out := root.encode()
	return out, !bytes.Equal(doc, out), nil
}

// splitPath splits a dotted update path into its components.
func splitPath(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, ".")
}
