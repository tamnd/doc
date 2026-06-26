package collection

import (
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
)

// SweepTTL removes documents whose TTL-indexed date field is older than the index's
// expireAfterSeconds threshold relative to now (spec 2061 doc 04 §11.4, doc 09
// §5.4). It runs a read pass to collect the expired _ids, then deletes each in its
// own transaction so a partial sweep is always consistent. It returns the number of
// documents deleted. A capped collection or one with no TTL index is a no-op.
func (c *Collection) SweepTTL(now time.Time) (int, error) {
	if c.policy().Capped {
		return 0, nil
	}
	specs := c.ttlSpecs()
	if len(specs) == 0 {
		return 0, nil
	}
	expired := c.collectExpired(specs, now)
	deleted := 0
	for _, filter := range expired {
		tx := c.Begin()
		n, err := tx.deleteTTL(filter)
		if err != nil {
			_ = tx.Rollback()
			return deleted, err
		}
		if err := tx.Commit(); err != nil {
			// A concurrent writer changed the document since the read pass. Skip it;
			// the next sweep will reconsider it.
			if IsRetriable(err) {
				continue
			}
			return deleted, err
		}
		deleted += int(n)
	}
	return deleted, nil
}

// ttlSpecs returns the collection's TTL index specs: single-field indexes with a
// positive expireAfterSeconds.
func (c *Collection) ttlSpecs() []*catalog.IndexSpec {
	var out []*catalog.IndexSpec
	for _, sp := range c.cat.Specs() {
		if sp.ExpireAfterSeconds > 0 && len(sp.Key) == 1 {
			out = append(out, sp)
		}
	}
	return out
}

// collectExpired scans the collection at a read snapshot and returns an _id-equality
// filter for every document expired under any of the TTL specs. A document is
// expired when its indexed field holds a date at or before now minus the index's
// expireAfterSeconds.
func (c *Collection) collectExpired(specs []*catalog.IndexSpec, now time.Time) []bson.Raw {
	tx := c.BeginReadOnly()
	defer func() { _ = tx.Rollback() }()
	docs, err := tx.Find(nil)
	if err != nil {
		return nil
	}
	nowMillis := now.UnixMilli()
	var out []bson.Raw
	for _, d := range docs {
		if !expiredUnder(d, specs, nowMillis) {
			continue
		}
		id, ok := d.Lookup(idFieldName)
		if !ok {
			continue
		}
		out = append(out, bson.NewBuilder().AppendValue(idFieldName, id).Build())
	}
	return out
}

// expiredUnder reports whether doc is expired under any TTL spec. Only a date-typed
// field counts; MongoDB ignores TTL fields that are missing or not a date.
func expiredUnder(doc bson.Raw, specs []*catalog.IndexSpec, nowMillis int64) bool {
	for _, sp := range specs {
		v, ok := doc.Lookup(sp.Key[0].Field)
		if !ok || v.Type != bson.TypeDateTime {
			continue
		}
		if v.DateTime() <= nowMillis-sp.ExpireAfterSeconds*1000 {
			return true
		}
	}
	return false
}

// deleteTTL removes the document matching an _id filter without the capped-delete
// guard, since the TTL sweeper is the engine acting on its own behalf rather than a
// user delete. TTL collections are never capped, so this only ever runs on a regular
// collection, but the dedicated path keeps the guard's intent precise.
func (t *Txn) deleteTTL(filter bson.Raw) (int64, error) {
	if t.done {
		return 0, ErrTxnDone
	}
	key, doc, err := t.findMatch(filter)
	if err != nil {
		return 0, err
	}
	if doc == nil {
		return 0, nil
	}
	t.bufferDelete(key)
	return 1, nil
}
