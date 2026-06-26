package update

import (
	"errors"
	"slices"
	"strconv"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/query"
)

// ErrBadArrayOperand reports an array operator applied to a field that is neither
// an array nor absent (MongoDB's "$op requires an array" type error).
var ErrBadArrayOperand = errors.New("update: array operator requires an array field")

// pushSpec is a compiled $push argument: the values to append (each), an optional
// sort direction or sub-field sort spec, an optional slice bound, and an optional
// insert position. simple marks the bare {field: value} form, which appends value
// as a single element with no modifiers.
type pushSpec struct {
	each     []bson.RawValue
	sortBy   *pushSort
	slice    *int
	position *int
	simple   *bson.RawValue
}

// pushSort is a parsed $sort modifier: dir is +1 or -1; field is empty for a
// scalar sort and a single field path for a {field: dir} document sort.
type pushSort struct {
	dir   int
	field string
}

// pullSpec is a compiled $pull argument: when matcher is set the element is kept
// unless it matches the query, otherwise an element is removed when it equals val.
// wrapped marks an operator-form matcher (e.g. {$gt: 5}) that runs against the
// element wrapped under pullField; a field-form matcher runs against a sub-
// document element directly.
type pullSpec struct {
	matcher *query.Matcher
	wrapped bool
	val     bson.RawValue
	hasVal  bool
}

// compilePush parses a $push argument, which is either a bare value (simple
// append) or a modifier document containing $each and optional $sort/$slice/
// $position.
func compilePush(v bson.RawValue) (*pushSpec, error) {
	if !isPushModifierDoc(v) {
		val := v
		return &pushSpec{simple: &val}, nil
	}
	elems, err := v.Document().Elements()
	if err != nil {
		return nil, err
	}
	ps := &pushSpec{}
	sawEach := false
	for _, e := range elems {
		switch e.Key {
		case "$each":
			vals, verr := arrayValues(e.Value)
			if verr != nil {
				return nil, ErrBadUpdate
			}
			ps.each = vals
			sawEach = true
		case "$sort":
			s, serr := compilePushSort(e.Value)
			if serr != nil {
				return nil, serr
			}
			ps.sortBy = s
		case "$slice":
			n, ok := intOperand(e.Value)
			if !ok {
				return nil, ErrBadUpdate
			}
			ps.slice = &n
		case "$position":
			n, ok := intOperand(e.Value)
			if !ok || n < 0 {
				return nil, ErrBadUpdate
			}
			ps.position = &n
		default:
			return nil, ErrBadUpdate
		}
	}
	if !sawEach {
		return nil, ErrBadUpdate
	}
	return ps, nil
}

// isPushModifierDoc reports whether a $push argument is a modifier document (a
// document whose first key is $each), as opposed to a bare value to append.
func isPushModifierDoc(v bson.RawValue) bool {
	if v.Type != bson.TypeDocument {
		return false
	}
	elems, err := v.Document().Elements()
	if err != nil || len(elems) == 0 {
		return false
	}
	return elems[0].Key == "$each"
}

// compilePushSort parses a $sort modifier: a bare 1 or -1 sorts scalars; a
// {field: 1|-1} document sorts sub-documents by that field.
func compilePushSort(v bson.RawValue) (*pushSort, error) {
	if n, ok := intOperand(v); ok {
		if n != 1 && n != -1 {
			return nil, ErrBadUpdate
		}
		return &pushSort{dir: n}, nil
	}
	if v.Type == bson.TypeDocument {
		elems, err := v.Document().Elements()
		if err != nil || len(elems) != 1 {
			return nil, ErrBadUpdate
		}
		n, ok := intOperand(elems[0].Value)
		if !ok || (n != 1 && n != -1) {
			return nil, ErrBadUpdate
		}
		return &pushSort{dir: n, field: elems[0].Key}, nil
	}
	return nil, ErrBadUpdate
}

// compileAddToSet parses a $addToSet argument: a bare value adds one element; a
// {$each: [...]} document adds each.
func compileAddToSet(v bson.RawValue) ([]bson.RawValue, error) {
	if v.Type == bson.TypeDocument {
		if each, ok := v.Document().Lookup("$each"); ok {
			elems, err := v.Document().Elements()
			if err != nil {
				return nil, err
			}
			if len(elems) != 1 {
				return nil, ErrBadUpdate
			}
			return arrayValues(each)
		}
	}
	return []bson.RawValue{v}, nil
}

// compilePop parses a $pop argument, which must be 1 (remove last) or -1 (remove
// first).
func compilePop(v bson.RawValue) (bson.RawValue, error) {
	n, ok := intOperand(v)
	if !ok || (n != 1 && n != -1) {
		return bson.RawValue{}, ErrBadUpdate
	}
	return v, nil
}

// compilePull parses a $pull argument: a query document (operators or field
// conditions) compiles to a matcher; any other value is removed by equality.
func compilePull(v bson.RawValue) (*pullSpec, error) {
	if v.Type == bson.TypeDocument {
		m, wrapped, err := pullMatcher(v)
		if err != nil {
			return nil, err
		}
		return &pullSpec{matcher: m, wrapped: wrapped}, nil
	}
	return &pullSpec{val: v, hasVal: true}, nil
}

// compilePullAll parses a $pullAll argument, which must be an array of values.
func compilePullAll(v bson.RawValue) ([]bson.RawValue, error) {
	if v.Type != bson.TypeArray {
		return nil, ErrBadUpdate
	}
	return arrayValues(v)
}

// applyPush appends to (or inserts into, sorts, and trims) the array at path,
// creating an empty array when the field is absent.
func applyPush(root *container, path []string, ps *pushSpec) error {
	parent, leaf, ok, err := resolve(root, path, true, false)
	if err != nil || !ok {
		return err
	}
	arr, err := currentArray(parent, leaf)
	if err != nil {
		return err
	}
	add := ps.each
	if ps.simple != nil {
		add = []bson.RawValue{*ps.simple}
	}
	arr = insertAt(arr, add, ps.position)
	if ps.sortBy != nil {
		sortArray(arr, ps.sortBy)
	}
	if ps.slice != nil {
		arr = sliceArray(arr, *ps.slice)
	}
	parent.setLeaf(leaf, buildArray(arr))
	return nil
}

// applyAddToSet adds each value not already present (by BSON value equality),
// creating an empty array when the field is absent.
func applyAddToSet(root *container, path []string, vals []bson.RawValue) error {
	parent, leaf, ok, err := resolve(root, path, true, false)
	if err != nil || !ok {
		return err
	}
	arr, err := currentArray(parent, leaf)
	if err != nil {
		return err
	}
	for _, v := range vals {
		if !containsValue(arr, v) {
			arr = append(arr, v)
		}
	}
	parent.setLeaf(leaf, buildArray(arr))
	return nil
}

// applyPop removes the first (dir -1) or last (dir 1) element of the array at
// path. A missing field is a no-op; a non-array field is an error.
func applyPop(root *container, path []string, dir bson.RawValue) error {
	parent, leaf, ok, err := resolve(root, path, false, false)
	if err != nil || !ok || parent == nil {
		return err
	}
	cur, present := parent.leafValue(leaf)
	if !present {
		return nil
	}
	if cur.Type != bson.TypeArray {
		return ErrBadArrayOperand
	}
	arr, err := arrayValues(cur)
	if err != nil {
		return err
	}
	if len(arr) == 0 {
		return nil
	}
	n, _ := intOperand(dir)
	if n == 1 {
		arr = arr[:len(arr)-1]
	} else {
		arr = arr[1:]
	}
	parent.setLeaf(leaf, buildArray(arr))
	return nil
}

// applyPull removes every element matching the $pull condition. A missing field
// is a no-op; a non-array field is an error.
func applyPull(root *container, path []string, pl *pullSpec) error {
	parent, leaf, ok, err := resolve(root, path, false, false)
	if err != nil || !ok || parent == nil {
		return err
	}
	cur, present := parent.leafValue(leaf)
	if !present {
		return nil
	}
	if cur.Type != bson.TypeArray {
		return ErrBadArrayOperand
	}
	arr, err := arrayValues(cur)
	if err != nil {
		return err
	}
	kept := arr[:0:0]
	for _, e := range arr {
		if pullMatches(pl, e) {
			continue
		}
		kept = append(kept, e)
	}
	parent.setLeaf(leaf, buildArray(kept))
	return nil
}

// applyPullAll removes every element equal (by BSON value equality) to any listed
// value. A missing field is a no-op; a non-array field is an error.
func applyPullAll(root *container, path []string, vals []bson.RawValue) error {
	parent, leaf, ok, err := resolve(root, path, false, false)
	if err != nil || !ok || parent == nil {
		return err
	}
	cur, present := parent.leafValue(leaf)
	if !present {
		return nil
	}
	if cur.Type != bson.TypeArray {
		return ErrBadArrayOperand
	}
	arr, err := arrayValues(cur)
	if err != nil {
		return err
	}
	kept := arr[:0:0]
	for _, e := range arr {
		if !containsValue(vals, e) {
			kept = append(kept, e)
		}
	}
	parent.setLeaf(leaf, buildArray(kept))
	return nil
}

// ---- array helpers -------------------------------------------------------

// currentArray returns the array values at leaf, the empty slice for an absent
// field, or an error when the field holds a non-array value.
func currentArray(parent *container, leaf string) ([]bson.RawValue, error) {
	cur, present := parent.leafValue(leaf)
	if !present {
		return nil, nil
	}
	if cur.Type != bson.TypeArray {
		return nil, ErrBadArrayOperand
	}
	return arrayValues(cur)
}

// arrayValues decodes an array RawValue into its element values.
func arrayValues(v bson.RawValue) ([]bson.RawValue, error) {
	if v.Type != bson.TypeArray {
		return nil, ErrBadUpdate
	}
	elems, err := v.Document().Elements()
	if err != nil {
		return nil, err
	}
	vals := make([]bson.RawValue, len(elems))
	for i, e := range elems {
		vals[i] = e.Value
	}
	return vals, nil
}

// buildArray frames element values into an array RawValue with the positional
// keys "0", "1", ... that BSON arrays use.
func buildArray(vals []bson.RawValue) bson.RawValue {
	b := bson.NewBuilder()
	for i, v := range vals {
		b.AppendValue(arrayKey(i), v)
	}
	body := b.Build()
	return bson.RawValue{Type: bson.TypeArray, Data: body}
}

// arrayKey returns the decimal string key for array index i.
func arrayKey(i int) string {
	return strconv.Itoa(i)
}

// insertAt inserts add into arr at position pos (clamped to the array length),
// appending at the end when pos is nil.
func insertAt(arr, add []bson.RawValue, pos *int) []bson.RawValue {
	if pos == nil || *pos >= len(arr) {
		return append(arr, add...)
	}
	at := *pos
	out := make([]bson.RawValue, 0, len(arr)+len(add))
	out = append(out, arr[:at]...)
	out = append(out, add...)
	out = append(out, arr[at:]...)
	return out
}

// sortArray sorts arr in place by the $sort modifier: a scalar sort compares the
// whole element, a field sort compares the element's sub-field.
func sortArray(arr []bson.RawValue, s *pushSort) {
	slices.SortStableFunc(arr, func(a, b bson.RawValue) int {
		av, bv := a, b
		if s.field != "" {
			av = subField(a, s.field)
			bv = subField(b, s.field)
		}
		return s.dir * bson.Compare(av, bv)
	})
}

// subField returns the value of field within a sub-document element, or the BSON
// null-ordered missing value when the element is not a document or lacks it.
func subField(v bson.RawValue, field string) bson.RawValue {
	if v.Type != bson.TypeDocument {
		return bson.RawValue{}
	}
	sv, ok := v.Document().Lookup(field)
	if !ok {
		return bson.RawValue{}
	}
	return sv
}

// sliceArray trims arr to the $slice bound: n>0 keeps the first n, n<0 keeps the
// last -n, n==0 empties it.
func sliceArray(arr []bson.RawValue, n int) []bson.RawValue {
	switch {
	case n == 0:
		return arr[:0]
	case n > 0:
		if n >= len(arr) {
			return arr
		}
		return arr[:n]
	default:
		if -n >= len(arr) {
			return arr
		}
		return arr[len(arr)+n:]
	}
}

// containsValue reports whether vals holds a value equal to v by BSON value
// equality.
func containsValue(vals []bson.RawValue, v bson.RawValue) bool {
	for _, e := range vals {
		if bson.Equal(e, v) {
			return true
		}
	}
	return false
}

// intOperand reads an integer-valued numeric operand (int32, int64, or a double
// with no fractional part).
func intOperand(v bson.RawValue) (int, bool) {
	switch v.Type {
	case bson.TypeInt32:
		return int(v.Int32()), true
	case bson.TypeInt64:
		return int(v.Int64()), true
	case bson.TypeDouble:
		f := v.Double()
		if f == float64(int64(f)) {
			return int(int64(f)), true
		}
	}
	return 0, false
}

// ---- $pull matching ------------------------------------------------------

// pullMatcher compiles a $pull query document into a matcher that runs against a
// single array element. A document of operator keys ($gt, $lt, ...) applies to
// the element value directly; a document of field keys applies to a sub-document
// element.
func pullMatcher(v bson.RawValue) (m *query.Matcher, wrapped bool, err error) {
	if isOperatorOnlyDoc(v) {
		filter := bson.NewBuilder().AppendDocument(pullField, v.Document()).Build()
		m, err = query.Compile(filter)
		return m, true, err
	}
	m, err = query.Compile(v.Document())
	return m, false, err
}

// pullField is the synthetic field name used to evaluate operator-form $pull
// conditions against a wrapped element value.
const pullField = "__pull_elem__"

// isOperatorOnlyDoc reports whether every key of a document begins with "$".
func isOperatorOnlyDoc(v bson.RawValue) bool {
	if v.Type != bson.TypeDocument {
		return false
	}
	elems, err := v.Document().Elements()
	if err != nil || len(elems) == 0 {
		return false
	}
	for _, e := range elems {
		if len(e.Key) == 0 || e.Key[0] != '$' {
			return false
		}
	}
	return true
}

// pullMatches reports whether array element e is removed by the $pull condition.
func pullMatches(pl *pullSpec, e bson.RawValue) bool {
	if pl.hasVal {
		return bson.Equal(e, pl.val)
	}
	if pl.wrapped {
		wrapped := bson.NewBuilder().AppendValue(pullField, e).Build()
		return pl.matcher.Match(wrapped)
	}
	// A field-form condition only matches sub-document elements.
	if e.Type != bson.TypeDocument {
		return false
	}
	return pl.matcher.Match(e.Document())
}
