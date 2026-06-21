package engine

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/collection"
)

// CollectionSnapshot is one collection's full logical contents: enough to recreate
// it byte-for-byte in a fresh file. It carries the create options, the secondary
// index models (the _id index is implicit), and every live document in natural
// order.
type CollectionSnapshot struct {
	DB        string
	Name      string
	Spec      CreateSpec
	Indexes   []collection.IndexModel
	Documents []bson.Raw
}

// Snapshot is the whole file's logical contents, the unit offline compaction
// rewrites into a fresh, hole-free file (spec 2061 doc 18 §15.2). It is a logical
// dump: physical layout, freelist, forwarding tombstones, and dead slots are all
// dropped on the way out and rebuilt clean on the way in.
type Snapshot struct {
	Collections []CollectionSnapshot
}

// Export reads the logical contents of every collection: its create options, its
// secondary indexes, and all of its live documents. It is the read half of
// offline compaction and of a logical dump.
func (e *Engine) Export() (*Snapshot, error) {
	e.mu.Lock()
	recs := e.mcat.ListCollections("")
	handles := make([]*collection.Collection, len(recs))
	for i, rec := range recs {
		handles[i] = e.colls[nsKey(rec.DBName, rec.Name)]
	}
	e.mu.Unlock()

	var snap Snapshot
	for i, rec := range recs {
		c := handles[i]
		if c == nil {
			continue
		}
		cs := CollectionSnapshot{
			DB:   rec.DBName,
			Name: rec.Name,
			Spec: specFromRecord(rec),
		}
		for _, ix := range c.ListIndexes() {
			if ix.Name == catalog.IDIndexName {
				continue
			}
			cs.Indexes = append(cs.Indexes, collection.IndexModel{
				Key:                ix.Key,
				Name:               ix.Name,
				Unique:             ix.Unique,
				Sparse:             ix.Sparse,
				PartialFilter:      ix.PartialFilter,
				ExpireAfterSeconds: ix.ExpireAfterSeconds,
			})
		}
		docs, err := c.Find(nil)
		if err != nil {
			return nil, err
		}
		cs.Documents = docs
		snap.Collections = append(snap.Collections, cs)
	}
	return &snap, nil
}

// Import recreates every collection in snap, loading its documents through the
// bulk path and rebuilding its secondary indexes afterward. It is the write half
// of offline compaction: run against a freshly created engine it produces a
// maximally compact file. Documents load with validation bypassed so a stricter
// validator added after the data was written cannot reject documents that are
// already stored.
func (e *Engine) Import(snap *Snapshot) error {
	const batch = 1000
	for _, cs := range snap.Collections {
		c, err := e.CreateCollectionWith(cs.DB, cs.Name, cs.Spec)
		if err != nil {
			return err
		}
		for start := 0; start < len(cs.Documents); start += batch {
			end := start + batch
			if end > len(cs.Documents) {
				end = len(cs.Documents)
			}
			if _, err := c.InsertManyBatch(cs.Documents[start:end], true, true); err != nil {
				return err
			}
		}
		if _, err := c.CreateIndexes(cs.Indexes); err != nil {
			return err
		}
	}
	return nil
}

// specFromRecord recovers the CreateSpec that would reproduce a collection from
// its catalog record: its capped bounds and its validator with level and action.
func specFromRecord(rec *catalog.CollectionRecord) CreateSpec {
	return CreateSpec{
		Capped:           rec.Kind == catalog.KindCapped,
		SizeBytes:        rec.Options.SizeBytes,
		MaxDocs:          rec.Options.MaxDocs,
		Validator:        rec.Validator,
		ValidationLevel:  rec.ValidationLevel,
		ValidationAction: rec.ValidationAction,
	}
}
