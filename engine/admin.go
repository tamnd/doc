package engine

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/schema"
)

// CollStats is one collection's storage accounting, the engine's raw form of the
// collStats command result (spec 2061 doc 14 §13.4). The public layer shapes it
// into a doc.CollectionStats.
type CollStats struct {
	Namespace     string
	DocumentCount int64
	StorageSize   int64
	IndexSize     int64
	IndexSizes    map[string]int64
	Capped        bool
	MaxDocuments  int64
}

// DBStats aggregates storage accounting across one database's collections.
type DBStats struct {
	Database      string
	Collections   int64
	Indexes       int64
	DocumentCount int64
	StorageSize   int64
	IndexSize     int64
	TotalSize     int64
}

// CollectionStats gathers storage accounting for db.name from the open collection
// handle and its catalog record (spec 2061 doc 14 §13.4). It returns
// ErrNamespaceNotFound when the collection does not exist.
func (e *Engine) CollectionStats(db, name string) (CollStats, error) {
	e.mu.Lock()
	rec := e.mcat.GetCollection(db, name)
	c := e.colls[nsKey(db, name)]
	e.mu.Unlock()
	if rec == nil || c == nil {
		return CollStats{}, ErrNamespaceNotFound
	}
	s := c.Stats()
	out := CollStats{
		Namespace:     db + "." + name,
		DocumentCount: s.Documents,
		StorageSize:   s.StorageBytes,
		IndexSizes:    make(map[string]int64, len(s.Indexes)),
		Capped:        rec.Kind == catalog.KindCapped,
		MaxDocuments:  rec.Options.MaxDocs,
	}
	for _, ix := range s.Indexes {
		out.IndexSizes[ix.Name] = ix.Bytes
		out.IndexSize += ix.Bytes
	}
	return out, nil
}

// DatabaseStats sums the storage accounting of every collection in db (spec 2061
// doc 14 §13.5). A database with no collections reports all zeros.
func (e *Engine) DatabaseStats(db string) DBStats {
	e.mu.Lock()
	recs := e.mcat.ListCollections(db)
	e.mu.Unlock()
	out := DBStats{Database: db}
	for _, rec := range recs {
		cs, err := e.CollectionStats(db, rec.Name)
		if err != nil {
			continue
		}
		out.Collections++
		out.Indexes += int64(len(cs.IndexSizes))
		out.DocumentCount += cs.DocumentCount
		out.StorageSize += cs.StorageSize
		out.IndexSize += cs.IndexSize
	}
	out.TotalSize = out.StorageSize + out.IndexSize
	return out
}

// CollModSpec carries the mutations collMod applies to a collection (spec 2061
// doc 09 §8.7): a replacement validator with its level and action, and a TTL
// change to one secondary index. A nil field leaves that property unchanged.
type CollModSpec struct {
	// SetValidator replaces the validator with Validator (which may be empty to
	// clear it). When false, Validator is ignored and the validator is untouched.
	SetValidator bool
	Validator    bson.Raw

	ValidationLevel  *catalog.ValidationLevel
	ValidationAction *catalog.ValidationAction

	// IndexName, when non-empty, names a secondary index whose TTL bound changes
	// to ExpireAfterSeconds.
	IndexName          string
	ExpireAfterSeconds *int64
}

// CollMod applies a collection modification in one WAL transaction (spec 2061 doc
// 09 §8.7). It rewrites the catalog record's validator, level, and action, stages
// any TTL change to a named index, commits both through the shared pager, then
// re-installs the collection's write policy so the new validator takes effect on
// the next write. It returns ErrNamespaceNotFound for an unknown collection and
// surfaces a validator that does not compile.
func (e *Engine) CollMod(db, name string, spec CollModSpec) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec := e.mcat.GetCollection(db, name)
	c := e.colls[nsKey(db, name)]
	if rec == nil || c == nil {
		return ErrNamespaceNotFound
	}
	if spec.SetValidator {
		if len(spec.Validator) > 0 {
			if _, err := schema.Compile(spec.Validator); err != nil {
				return err
			}
		}
		rec.Validator = spec.Validator
	}
	if spec.ValidationLevel != nil {
		rec.ValidationLevel = *spec.ValidationLevel
	}
	if spec.ValidationAction != nil {
		rec.ValidationAction = *spec.ValidationAction
	}
	rec.ModifiedAt = e.nowMillis()
	if err := e.mcat.StageCollection(rec); err != nil {
		return err
	}
	if spec.IndexName != "" && spec.ExpireAfterSeconds != nil {
		if err := c.StageIndexTTL(spec.IndexName, *spec.ExpireAfterSeconds); err != nil {
			return err
		}
	}
	if err := e.pgr.Commit(); err != nil {
		return err
	}
	pol, err := policyFromRecord(rec)
	if err != nil {
		return err
	}
	c.SetPolicy(pol)
	return nil
}

// PageSize returns the file's page size in bytes, the create-time geometry the
// page_size PRAGMA reports (spec 2061 doc 19 §21.1).
func (e *Engine) PageSize() int { return e.pgr.PageSize() }

// SyncLevel returns the pager's current commit durability level, the value the
// synchronous PRAGMA reads.
func (e *Engine) SyncLevel() pager.SyncLevel { return e.pgr.SyncLevel() }

// SetSyncLevel changes the pager's commit durability level at runtime, the write
// half of the synchronous PRAGMA.
func (e *Engine) SetSyncLevel(l pager.SyncLevel) { e.pgr.SetSync(l) }
