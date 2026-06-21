package collection

import (
	"bytes"
	"errors"
	"slices"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/query"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/update"
)

// ErrImmutableField reports an update or replacement that would change a
// document's _id, which MongoDB forbids (error code 66, ImmutableField). The _id
// check lives here, at the write layer, because _id immutability is a collection
// invariant: the update engine itself is storage-agnostic and treats _id as an
// ordinary field (spec 2061 doc 13 §4.4).
var ErrImmutableField = errors.New("collection: _id is immutable")

// UpdateResult reports the outcome of an update or replace: Matched documents
// satisfied the filter, and Modified is the subset whose contents actually
// changed (a no-op update, such as $set to a field's current value, matches
// without modifying). When an upsert inserts a document, Matched and Modified are
// zero, Upserted is one, and UpsertedID holds the inserted _id (spec 2061 doc 13
// §2.3, §11.6).
type UpdateResult struct {
	Matched    int64
	Modified   int64
	Upserted   int64
	UpsertedID bson.RawValue
}

// ReturnDocument selects which version findOneAndUpdate / findOneAndReplace
// returns: the document before the change (MongoDB's default) or after it.
type ReturnDocument int

const (
	// ReturnBefore returns the matched document as it was before the change.
	ReturnBefore ReturnDocument = iota
	// ReturnAfter returns the document after the change is applied.
	ReturnAfter
)

// FindModifyOptions carries the optional stages of a findAndModify command: a
// sort that picks which document among several matches is acted on, a projection
// applied to the returned document, and which version to return.
type FindModifyOptions struct {
	Sort       bson.Raw
	Projection bson.Raw
	Return     ReturnDocument
	Upsert     bool
}

// UpdateOne applies an update-operator document to the first document matching
// filter. It returns the matched and modified counts (each 0 or 1).
func (t *Txn) UpdateOne(filter, updateDoc bson.Raw) (UpdateResult, error) {
	return t.updateOne(filter, updateDoc, UpdateOptions{})
}

// UpdateOneWith is UpdateOne with options, notably Upsert: when nothing matches
// and Upsert is set, a document is built from the filter and update and inserted.
func (t *Txn) UpdateOneWith(filter, updateDoc bson.Raw, opts UpdateOptions) (UpdateResult, error) {
	return t.updateOne(filter, updateDoc, opts)
}

// UpdateMany applies an update-operator document to every document matching
// filter, returning the total matched and modified counts.
func (t *Txn) UpdateMany(filter, updateDoc bson.Raw) (UpdateResult, error) {
	return t.UpdateManyWith(filter, updateDoc, UpdateOptions{})
}

// UpdateManyWith is UpdateMany with options. With Upsert set and no document
// matching, it inserts a single document (matching MongoDB's at-most-one upsert).
func (t *Txn) UpdateManyWith(filter, updateDoc bson.Raw, opts UpdateOptions) (UpdateResult, error) {
	if t.done {
		return UpdateResult{}, ErrTxnDone
	}
	if !t.writable {
		return UpdateResult{}, storage.ErrReadOnly
	}
	u, err := compileUpdate(updateDoc)
	if err != nil {
		return UpdateResult{}, err
	}
	m, err := compileFilter(filter)
	if err != nil {
		return UpdateResult{}, err
	}
	var res UpdateResult
	for _, key := range t.scanKeys() {
		doc := t.currentDoc(key)
		if doc == nil || !m.Match(doc) {
			continue
		}
		res.Matched++
		modified, err := t.applyOperatorUpdate(key, doc, u)
		if err != nil {
			return UpdateResult{}, err
		}
		if modified {
			res.Modified++
		}
	}
	if res.Matched == 0 && opts.Upsert {
		return t.upsertUpdate(filter, u)
	}
	return res, nil
}

// updateOne is the shared single-document update path used by UpdateOne and
// findOneAndUpdate's matched branch.
func (t *Txn) updateOne(filter, updateDoc bson.Raw, opts UpdateOptions) (UpdateResult, error) {
	if t.done {
		return UpdateResult{}, ErrTxnDone
	}
	if !t.writable {
		return UpdateResult{}, storage.ErrReadOnly
	}
	u, err := compileUpdate(updateDoc)
	if err != nil {
		return UpdateResult{}, err
	}
	key, doc, err := t.findMatch(filter)
	if err != nil {
		return UpdateResult{}, err
	}
	if doc == nil {
		if opts.Upsert {
			return t.upsertUpdate(filter, u)
		}
		return UpdateResult{}, nil
	}
	modified, err := t.applyOperatorUpdate(key, doc, u)
	if err != nil {
		return UpdateResult{}, err
	}
	res := UpdateResult{Matched: 1}
	if modified {
		res.Modified = 1
	}
	return res, nil
}

// ReplaceOne replaces the first document matching filter with replacement, a
// non-operator document. The original _id is preserved; a replacement that
// carries a different _id is rejected with ErrImmutableField.
func (t *Txn) ReplaceOne(filter, replacement bson.Raw) (UpdateResult, error) {
	return t.ReplaceOneWith(filter, replacement, UpdateOptions{})
}

// ReplaceOneWith is ReplaceOne with options. With Upsert set and no match, the
// replacement is inserted (the _id comes from the filter when it pins one).
func (t *Txn) ReplaceOneWith(filter, replacement bson.Raw, opts UpdateOptions) (UpdateResult, error) {
	if t.done {
		return UpdateResult{}, ErrTxnDone
	}
	if !t.writable {
		return UpdateResult{}, storage.ErrReadOnly
	}
	if err := validateReplacement(replacement); err != nil {
		return UpdateResult{}, err
	}
	key, doc, err := t.findMatch(filter)
	if err != nil {
		return UpdateResult{}, err
	}
	if doc == nil {
		if opts.Upsert {
			return t.upsertReplace(filter, replacement)
		}
		return UpdateResult{}, nil
	}
	modified, err := t.applyReplace(key, doc, replacement)
	if err != nil {
		return UpdateResult{}, err
	}
	res := UpdateResult{Matched: 1}
	if modified {
		res.Modified = 1
	}
	return res, nil
}

// upsertUpdate builds and inserts a document for an operator-update upsert and
// reports it as an UpdateResult.
func (t *Txn) upsertUpdate(filter bson.Raw, u *update.Update) (UpdateResult, error) {
	_, id, err := t.upsertWithUpdate(filter, u)
	if err != nil {
		return UpdateResult{}, err
	}
	return UpdateResult{Upserted: 1, UpsertedID: id}, nil
}

// upsertReplace builds and inserts a document for a replacement upsert and
// reports it as an UpdateResult.
func (t *Txn) upsertReplace(filter, replacement bson.Raw) (UpdateResult, error) {
	_, id, err := t.upsertWithReplacement(filter, replacement)
	if err != nil {
		return UpdateResult{}, err
	}
	return UpdateResult{Upserted: 1, UpsertedID: id}, nil
}

// upsertReturn is the findAndModify upsert branch: it inserts the constructed
// document (from u for an update or replacement for a replace) and returns it for
// ReturnAfter, or nil for ReturnBefore (MongoDB returns the pre-image, which does
// not exist on an insert). The returned document is projected per opts.
func (t *Txn) upsertReturn(filter bson.Raw, u *update.Update, replacement bson.Raw, opts FindModifyOptions) (bson.Raw, error) {
	var newDoc bson.Raw
	var err error
	if u != nil {
		newDoc, _, err = t.upsertWithUpdate(filter, u)
	} else {
		newDoc, _, err = t.upsertWithReplacement(filter, replacement)
	}
	if err != nil {
		return nil, err
	}
	if opts.Return != ReturnAfter {
		return nil, nil
	}
	return projectReturn(newDoc, opts.Projection)
}

// FindOneAndUpdate applies an update-operator document to the first document
// matching filter (under opts.Sort) and returns the before or after version,
// shaped by opts.Projection. It returns nil when nothing matches.
func (t *Txn) FindOneAndUpdate(filter, updateDoc bson.Raw, opts FindModifyOptions) (bson.Raw, error) {
	if t.done {
		return nil, ErrTxnDone
	}
	if !t.writable {
		return nil, storage.ErrReadOnly
	}
	u, err := compileUpdate(updateDoc)
	if err != nil {
		return nil, err
	}
	key, before, err := t.firstSorted(filter, opts.Sort)
	if err != nil {
		return nil, err
	}
	if before == nil {
		if opts.Upsert {
			return t.upsertReturn(filter, u, nil, opts)
		}
		return nil, nil
	}
	newDoc, modified, err := u.Apply(before, t.c.clk.Now())
	if err != nil {
		return nil, err
	}
	if modified {
		if err := checkIDPreserved(before, newDoc); err != nil {
			return nil, err
		}
		t.bufferReplace(key, newDoc)
	}
	return t.returnDoc(before, newDoc, opts)
}

// FindOneAndReplace replaces the first document matching filter (under opts.Sort)
// with replacement and returns the before or after version. It returns nil when
// nothing matches.
func (t *Txn) FindOneAndReplace(filter, replacement bson.Raw, opts FindModifyOptions) (bson.Raw, error) {
	if t.done {
		return nil, ErrTxnDone
	}
	if !t.writable {
		return nil, storage.ErrReadOnly
	}
	if err := validateReplacement(replacement); err != nil {
		return nil, err
	}
	key, before, err := t.firstSorted(filter, opts.Sort)
	if err != nil {
		return nil, err
	}
	if before == nil {
		if opts.Upsert {
			return t.upsertReturn(filter, nil, replacement, opts)
		}
		return nil, nil
	}
	newDoc, err := buildReplacement(before, replacement)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(before, newDoc) {
		t.bufferReplace(key, newDoc)
	}
	return t.returnDoc(before, newDoc, opts)
}

// FindOneAndDelete deletes the first document matching filter (under opts.Sort)
// and returns it, shaped by opts.Projection. It returns nil when nothing matches.
// opts.Return is ignored: the deleted document is always the before version.
func (t *Txn) FindOneAndDelete(filter bson.Raw, opts FindModifyOptions) (bson.Raw, error) {
	if t.done {
		return nil, ErrTxnDone
	}
	if !t.writable {
		return nil, storage.ErrReadOnly
	}
	key, before, err := t.firstSorted(filter, opts.Sort)
	if err != nil || before == nil {
		return nil, err
	}
	t.bufferDelete(key)
	return projectReturn(before, opts.Projection)
}

// Distinct returns the distinct values of field across the documents matching
// filter, deduplicated and ordered by the BSON total order. A field path is
// resolved with implicit array traversal, and a resolved array is unwound into
// its elements, matching MongoDB's distinct command (spec 2061 doc 11 §6).
func (t *Txn) Distinct(field string, filter bson.Raw) ([]bson.RawValue, error) {
	if t.done {
		return nil, ErrTxnDone
	}
	m, err := compileFilter(filter)
	if err != nil {
		return nil, err
	}
	var vals []bson.RawValue
	for _, key := range t.scanKeys() {
		doc := t.currentDoc(key)
		if doc == nil || !m.Match(doc) {
			continue
		}
		for _, v := range query.Distinct(doc, field) {
			vals = append(vals, cloneValue(v))
		}
	}
	return dedupSortValues(vals), nil
}

// ---- helpers -------------------------------------------------------------

// applyOperatorUpdate applies a compiled update to the document at key, checks
// _id immutability, and buffers the new version when the document changed. It
// returns whether the document was modified.
func (t *Txn) applyOperatorUpdate(key string, doc bson.Raw, u *update.Update) (bool, error) {
	newDoc, modified, err := u.Apply(doc, t.c.clk.Now())
	if err != nil {
		return false, err
	}
	if !modified {
		return false, nil
	}
	if err := checkIDPreserved(doc, newDoc); err != nil {
		return false, err
	}
	if err := t.checkCappedGrow(doc, newDoc); err != nil {
		return false, err
	}
	if err := t.validateWrite(newDoc, doc, false); err != nil {
		return false, err
	}
	t.bufferReplace(key, newDoc)
	return true, nil
}

// applyReplace replaces the document at key with the replacement document,
// preserving the original _id, and buffers the new version when it differs.
func (t *Txn) applyReplace(key string, doc, replacement bson.Raw) (bool, error) {
	newDoc, err := buildReplacement(doc, replacement)
	if err != nil {
		return false, err
	}
	if bytes.Equal(doc, newDoc) {
		return false, nil
	}
	if err := t.checkCappedGrow(doc, newDoc); err != nil {
		return false, err
	}
	if err := t.validateWrite(newDoc, doc, false); err != nil {
		return false, err
	}
	t.bufferReplace(key, newDoc)
	return true, nil
}

// bufferReplace buffers a new version of an existing overlay key: it tombstones
// the committed version (if any) and installs newDoc, matching the delete path's
// removeRID handling. A key first written in this transaction has no committed
// version, so only insertDoc is overwritten.
func (t *Txn) bufferReplace(key string, newDoc bson.Raw) {
	p := t.ensurePending(key)
	if rid, old, ok := t.committedVersion(key); ok {
		p.removeRID = rid
		p.removeDoc = old
		p.hasRemove = true
	}
	p.insertDoc = newDoc
}

// bufferDelete buffers a delete of an existing overlay key.
func (t *Txn) bufferDelete(key string) {
	p := t.ensurePending(key)
	if rid, old, ok := t.committedVersion(key); ok {
		p.removeRID = rid
		p.removeDoc = old
		p.hasRemove = true
	}
	p.insertDoc = nil
}

// firstSorted returns the overlay key and document of the first match for filter
// after applying sort: with no sort it is the natural-order first match; with a
// sort it is the smallest match under the sort, whose key is recovered from its
// _id. It returns a nil document when nothing matches.
func (t *Txn) firstSorted(filter, sortDoc bson.Raw) (string, bson.Raw, error) {
	if len(sortDoc) == 0 {
		return t.findMatch(filter)
	}
	m, err := compileFilter(filter)
	if err != nil {
		return "", nil, err
	}
	srt, err := query.CompileSort(sortDoc)
	if err != nil {
		return "", nil, err
	}
	var matches []bson.Raw
	for _, key := range t.scanKeys() {
		doc := t.currentDoc(key)
		if doc != nil && m.Match(doc) {
			matches = append(matches, doc)
		}
	}
	if len(matches) == 0 {
		return "", nil, nil
	}
	srt.Apply(matches)
	first := matches[0]
	id, ok := bson.IDOf(first)
	if !ok {
		return "", nil, errMissingID
	}
	key, err := overlayKey(id)
	if err != nil {
		return "", nil, err
	}
	return key, first, nil
}

// returnDoc selects the before or after version per opts.Return and projects it.
func (t *Txn) returnDoc(before, after bson.Raw, opts FindModifyOptions) (bson.Raw, error) {
	doc := before
	if opts.Return == ReturnAfter {
		doc = after
	}
	return projectReturn(doc, opts.Projection)
}

// projectReturn applies an optional projection to a returned document, cloning it
// so the caller owns the bytes.
func projectReturn(doc, projection bson.Raw) (bson.Raw, error) {
	proj, err := query.CompileProjection(projection)
	if err != nil {
		return nil, err
	}
	out, err := proj.Apply(doc.Clone())
	if err != nil {
		return nil, err
	}
	return out, nil
}

// compileUpdate compiles an update document, rejecting a replacement (non-
// operator) document where an operator document is required.
func compileUpdate(updateDoc bson.Raw) (*update.Update, error) {
	if len(updateDoc) > 0 {
		if err := updateDoc.WellFormed(); err != nil {
			return nil, err
		}
	}
	return update.Compile(updateDoc)
}

// validateReplacement rejects a replacement document that is actually an
// operator document; MongoDB requires the replacement form here.
func validateReplacement(replacement bson.Raw) error {
	if len(replacement) > 0 {
		if err := replacement.WellFormed(); err != nil {
			return err
		}
	}
	if update.IsOperatorDoc(replacement) {
		return update.ErrBadUpdate
	}
	return nil
}

// buildReplacement constructs the replacement document, preserving oldDoc's _id
// at the front and dropping any _id the caller supplied (after checking it
// matches). A differing _id is ErrImmutableField.
func buildReplacement(oldDoc, replacement bson.Raw) (bson.Raw, error) {
	oldID, ok := bson.IDOf(oldDoc)
	if !ok {
		return nil, errMissingID
	}
	if rid, ok := replacement.Lookup(idFieldName); ok {
		if !bson.Equal(rid, oldID) {
			return nil, ErrImmutableField
		}
	}
	elems, err := replacement.Elements()
	if err != nil {
		return nil, err
	}
	b := bson.NewBuilder().AppendValue(idFieldName, oldID)
	for _, e := range elems {
		if e.Key == idFieldName {
			continue
		}
		b.AppendValue(e.Key, e.Value)
	}
	return b.Build(), nil
}

// checkIDPreserved fails when an update changed a document's _id.
func checkIDPreserved(before, after bson.Raw) error {
	beforeID, ok := bson.IDOf(before)
	if !ok {
		return errMissingID
	}
	afterID, ok := bson.IDOf(after)
	if !ok || !bson.Equal(afterID, beforeID) {
		return ErrImmutableField
	}
	return nil
}

// cloneValue copies a RawValue's bytes so it survives the document it came from.
func cloneValue(v bson.RawValue) bson.RawValue {
	d := make([]byte, len(v.Data))
	copy(d, v.Data)
	return bson.RawValue{Type: v.Type, Data: d}
}

// dedupSortValues sorts values by the BSON total order and drops adjacent
// duplicates, yielding distinct values in a deterministic order.
func dedupSortValues(vals []bson.RawValue) []bson.RawValue {
	if len(vals) == 0 {
		return nil
	}
	slices.SortFunc(vals, func(a, b bson.RawValue) int { return bson.Compare(a, b) })
	out := vals[:1]
	for _, v := range vals[1:] {
		if bson.Compare(out[len(out)-1], v) != 0 {
			out = append(out, v)
		}
	}
	return out
}
