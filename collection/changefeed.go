package collection

import "github.com/tamnd/doc/bson"

// ChangeRecord is one document change a committed transaction produced, the unit the
// engine forwards to the change feed (spec 2061 doc 18 §8). It is deliberately small:
// the operation kind, the document key, and the post- and pre-images the public layer
// turns into a ChangeEvent. The collection layer does not know namespaces; the engine
// pairs each record with the database and collection it came from.
type ChangeRecord struct {
	// Op is one of "insert", "update", "replace", "delete".
	Op string
	// ID is the affected document's _id, the documentKey of the event.
	ID bson.RawValue
	// Doc is the post-image: the document after the write, for insert/update/replace.
	// It is nil for a delete.
	Doc bson.Raw
	// Before is the pre-image: the document before the write, for update/replace/delete.
	// It is nil for an insert.
	Before bson.Raw
}

// EmitFunc receives the change records of one committed transaction along with the
// commit version that orders them. The engine installs it; a collection with no hook
// produces no feed, so the change-stream machinery costs nothing when unused.
type EmitFunc func(recs []ChangeRecord, commitVersion uint64)

// SetChangeHook installs the change-feed emitter on the collection. The engine calls
// it once per collection at open, binding the hook that carries the namespace.
func (c *Collection) SetChangeHook(fn EmitFunc) { c.emit = fn }

// fireChange forwards a committed transaction's records to the change hook, if one is
// installed and there is anything to report.
func (c *Collection) fireChange(recs []ChangeRecord, cv uint64) {
	if c.emit == nil || len(recs) == 0 {
		return
	}
	c.emit(recs, cv)
}

// changeRecords derives the change records of a committed transaction from its buffered
// operations. It runs only when a change hook is installed, so the common no-watcher
// path pays nothing. The operation kind comes from the net intent of each buffered op:
// a fresh insert has no superseded version, a delete has no post-image, and a write
// that supersedes a committed version is a replace or an update depending on how it was
// buffered. An op that collapsed to a no-op in the transaction (insert then delete of
// the same _id) produces no record.
func (t *Txn) changeRecords() []ChangeRecord {
	if t.c.emit == nil && t.c.cstore == nil {
		return nil
	}
	recs := make([]ChangeRecord, 0, len(t.order))
	for _, key := range t.order {
		p := t.pending[key]
		if p.noop() {
			continue
		}
		var op string
		switch {
		case p.insertDoc != nil && !p.hasRemove:
			op = "insert"
		case p.insertDoc != nil && p.hasRemove:
			if p.replaced {
				op = "replace"
			} else {
				op = "update"
			}
		default:
			op = "delete"
		}
		src := p.insertDoc
		if src == nil {
			src = p.removeDoc
		}
		idv, _ := src.Lookup(idFieldName)
		recs = append(recs, ChangeRecord{
			Op:     op,
			ID:     cloneValue(idv),
			Doc:    cloneRaw(p.insertDoc),
			Before: cloneRaw(p.removeDoc),
		})
	}
	return recs
}

// cloneRaw returns an independent copy of raw, or nil for an empty input, so a record
// handed to the feed does not alias the transaction's buffers.
func cloneRaw(raw bson.Raw) bson.Raw {
	if len(raw) == 0 {
		return nil
	}
	return raw.Clone()
}
