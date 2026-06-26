package collection

import (
	"errors"
	"fmt"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/schema"
)

// ErrDocumentValidation reports a write rejected because the resulting document did
// not satisfy the collection's validator (spec 2061 doc 09 §10.4). It maps to
// MongoDB's DocumentValidationFailure, error code 121.
var ErrDocumentValidation = errors.New("collection: document failed validation")

// ErrCappedDelete reports a delete attempted on a capped collection, which only
// removes documents implicitly as the ring advances (spec 2061 doc 04 §11.2).
var ErrCappedDelete = errors.New("collection: cannot delete from a capped collection")

// ErrCappedGrow reports an update or replace on a capped collection that would make
// the document larger, which the ring layout forbids (spec 2061 doc 04 §11.2).
var ErrCappedGrow = errors.New("collection: cannot grow a document in a capped collection")

// Policy is the per-collection write policy the engine derives from the catalog
// record and installs on the open collection: the compiled validator with its level
// and action, and the capped-collection bounds. The zero Policy validates nothing
// and is not capped, which is the regular-collection default.
type Policy struct {
	Validator        *schema.Validator
	ValidationLevel  catalog.ValidationLevel
	ValidationAction catalog.ValidationAction

	Capped         bool
	CappedMaxDocs  int64
	CappedMaxBytes int64
}

// SetPolicy installs the write policy for the collection. The engine calls it after
// opening the collection and after any DDL that changes the validator or capped
// bounds (createCollection, collMod). It is safe to call before the collection takes
// any write.
func (c *Collection) SetPolicy(p Policy) {
	c.mu.Lock()
	c.pol = p
	c.mu.Unlock()
}

// policy returns a copy of the collection's current write policy under the lock.
func (c *Collection) policy() Policy {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pol
}

// validateWrite checks a candidate document against the collection's validator. For
// an insert it always validates; for an update or replace under the moderate level
// it skips validation when the pre-image was itself invalid, so a newly attached
// validator does not block edits to historical non-conforming data (spec 2061 doc 09
// §10.3). A failure under the error action returns ErrDocumentValidation; under the
// warn action the write is admitted.
func (t *Txn) validateWrite(newDoc, preImage bson.Raw, isInsert bool) error {
	pol := t.c.policy()
	if pol.Validator == nil || pol.Validator.Empty() || pol.ValidationLevel == catalog.ValidationOff {
		return nil
	}
	if t.bypassValidation {
		return nil
	}
	if !isInsert && pol.ValidationLevel == catalog.ValidationModerate {
		if preImage != nil && pol.Validator.Validate(preImage) != nil {
			return nil
		}
	}
	if err := pol.Validator.Validate(newDoc); err != nil {
		if pol.ValidationAction == catalog.ValidationWarn {
			return nil
		}
		return fmt.Errorf("%w: %v", ErrDocumentValidation, err)
	}
	return nil
}

// enforceCap brings a capped collection back within its document-count and size
// bounds after an insert by evicting the oldest live documents in insertion order,
// the logical form of the ring buffer advancing its head (spec 2061 doc 04 §11.2).
// Eviction is buffered into the same transaction, so the insert and the eviction
// commit atomically.
func (t *Txn) enforceCap() {
	pol := t.c.policy()
	if !pol.Capped {
		return
	}
	for {
		keys := t.liveKeysInOrder()
		if len(keys) == 0 {
			return
		}
		overCount := pol.CappedMaxDocs > 0 && int64(len(keys)) > pol.CappedMaxDocs
		overBytes := pol.CappedMaxBytes > 0 && t.liveBytes(keys) > pol.CappedMaxBytes
		if !overCount && !overBytes {
			return
		}
		t.bufferDelete(keys[0])
	}
}

// liveKeysInOrder returns the overlay keys this transaction sees as live, in
// insertion order: committed keys first (first-insert order) followed by keys this
// transaction inserted.
func (t *Txn) liveKeysInOrder() []string {
	out := make([]string, 0)
	for _, key := range t.scanKeys() {
		if t.currentDoc(key) != nil {
			out = append(out, key)
		}
	}
	return out
}

// checkCappedGrow rejects an update or replace on a capped collection whose new
// document is larger than the one it supersedes, which the fixed-size ring cannot
// hold (spec 2061 doc 04 §11.2). It is a no-op on a regular collection.
func (t *Txn) checkCappedGrow(oldDoc, newDoc bson.Raw) error {
	if !t.c.policy().Capped {
		return nil
	}
	if len(newDoc) > len(oldDoc) {
		return ErrCappedGrow
	}
	return nil
}

// liveBytes sums the BSON byte length of the live documents at the given keys.
func (t *Txn) liveBytes(keys []string) int64 {
	var total int64
	for _, key := range keys {
		if doc := t.currentDoc(key); doc != nil {
			total += int64(len(doc))
		}
	}
	return total
}
