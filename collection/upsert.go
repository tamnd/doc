package collection

import (
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/update"
)

// UpdateOptions carries the optional flags of an update or replace command. Upsert
// turns a zero-match update into an insert built from the filter and the update
// spec (spec 2061 doc 13 §11).
type UpdateOptions struct {
	Upsert bool
}

// insertBuffered mints an _id when absent, normalizes the document, and buffers it
// as an insert, rejecting a live duplicate _id. It is the shared body of InsertOne
// and the upsert insert branch, returning the normalized document and its _id.
func (t *Txn) insertBuffered(d bson.Raw) (bson.Raw, bson.RawValue, error) {
	out, idv, err := bson.EnsureID(d, t.c.gen)
	if err != nil {
		return nil, bson.RawValue{}, err
	}
	key, err := overlayKey(idv)
	if err != nil {
		return nil, bson.RawValue{}, err
	}
	if t.currentDoc(key) != nil {
		return nil, bson.RawValue{}, ErrDuplicateKey
	}
	if err := t.validateWrite(out, nil, true); err != nil {
		return nil, bson.RawValue{}, err
	}
	p := t.ensurePending(key)
	p.insertDoc = out
	t.enforceCap()
	return out, idv, nil
}

// upsertWithUpdate constructs and inserts a document for an operator-update upsert:
// the base document from the filter's equality predicates, then the update applied
// on its insert branch (so $setOnInsert participates). It returns the inserted
// document and its _id.
func (t *Txn) upsertWithUpdate(filter bson.Raw, u *update.Update) (bson.Raw, bson.RawValue, error) {
	base, err := upsertBase(filter, t.c.clk.Now())
	if err != nil {
		return nil, bson.RawValue{}, err
	}
	newDoc, _, err := u.ApplyForInsert(base, t.c.clk.Now())
	if err != nil {
		return nil, bson.RawValue{}, err
	}
	return t.insertBuffered(newDoc)
}

// upsertWithReplacement constructs and inserts a document for a replacement
// upsert: the replacement document carries the new shape, and the _id comes from
// the filter's _id equality when present (otherwise it is generated). It returns
// the inserted document and its _id.
func (t *Txn) upsertWithReplacement(filter, replacement bson.Raw) (bson.Raw, bson.RawValue, error) {
	base, err := upsertBase(filter, t.c.clk.Now())
	if err != nil {
		return nil, bson.RawValue{}, err
	}
	newDoc := replacement
	if id, ok := base.Lookup(idFieldName); ok {
		newDoc, err = withID(replacement, id)
		if err != nil {
			return nil, bson.RawValue{}, err
		}
	}
	return t.insertBuffered(newDoc)
}

// upsertBase builds the initial upsert document from a filter's equality
// predicates: top-level and dotted equalities and $eq conditions contribute their
// fields, $and equalities are merged in, and every other operator is ignored (spec
// 2061 doc 13 §11.4). Dotted paths build nested sub-documents; a path conflict
// (e.g. both "a" and "a.b") is an error.
func upsertBase(filter bson.Raw, now time.Time) (bson.Raw, error) {
	eqs, err := equalityFields(filter)
	if err != nil {
		return nil, err
	}
	if len(eqs) == 0 {
		return bson.NewBuilder().Build(), nil
	}
	inner := bson.NewBuilder()
	for _, e := range eqs {
		inner.AppendValue(e.path, e.val)
	}
	setDoc := bson.NewBuilder().AppendDocument("$set", inner.Build()).Build()
	u, err := update.Compile(setDoc)
	if err != nil {
		return nil, err
	}
	out, _, err := u.Apply(bson.NewBuilder().Build(), now)
	return out, err
}

// eqField is one extracted equality (dotted path, value) from a filter.
type eqField struct {
	path string
	val  bson.RawValue
}

// equalityFields walks a filter and returns its equality predicates: a plain value
// is an equality, {$eq: v} contributes v, $and merges nested equalities, and any
// other operator or logical combinator is ignored.
func equalityFields(filter bson.Raw) ([]eqField, error) {
	if len(filter) == 0 {
		return nil, nil
	}
	elems, err := filter.Elements()
	if err != nil {
		return nil, err
	}
	var out []eqField
	for _, e := range elems {
		if len(e.Key) > 0 && e.Key[0] == '$' {
			if e.Key == "$and" && e.Value.Type == bson.TypeArray {
				clauses, cerr := e.Value.Document().Elements()
				if cerr != nil {
					return nil, cerr
				}
				for _, c := range clauses {
					if c.Value.Type != bson.TypeDocument {
						continue
					}
					sub, serr := equalityFields(c.Value.Document())
					if serr != nil {
						return nil, serr
					}
					out = append(out, sub...)
				}
			}
			continue
		}
		v := e.Value
		if isOperatorValue(v) {
			if eq, ok := v.Document().Lookup("$eq"); ok {
				out = append(out, eqField{path: e.Key, val: eq})
			}
			continue
		}
		out = append(out, eqField{path: e.Key, val: v})
	}
	return out, nil
}

// isOperatorValue reports whether a filter value is an operator document (its
// first key begins with "$"), as opposed to a literal equality value.
func isOperatorValue(v bson.RawValue) bool {
	if v.Type != bson.TypeDocument {
		return false
	}
	elems, err := v.Document().Elements()
	if err != nil || len(elems) == 0 {
		return false
	}
	return len(elems[0].Key) > 0 && elems[0].Key[0] == '$'
}

// withID returns replacement with id placed first and any caller-supplied _id
// dropped after confirming it matches, mirroring buildReplacement's _id handling.
func withID(replacement bson.Raw, id bson.RawValue) (bson.Raw, error) {
	if rid, ok := replacement.Lookup(idFieldName); ok && !bson.Equal(rid, id) {
		return nil, ErrImmutableField
	}
	elems, err := replacement.Elements()
	if err != nil {
		return nil, err
	}
	b := bson.NewBuilder().AppendValue(idFieldName, id)
	for _, e := range elems {
		if e.Key == idFieldName {
			continue
		}
		b.AppendValue(e.Key, e.Value)
	}
	return b.Build(), nil
}
