package catalog

import (
	"sort"

	"github.com/tamnd/doc/heap"
	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
)

// The master catalog is the file-wide registry of databases and collections that
// the multi-collection engine sits on (spec 2061 doc 09 §2, §3, §4). It persists
// two record sets in two dedicated, self-identifying heaps: database records under
// DatabasesCollID and collection records under CollectionsCollID. Each record is a
// BSON document rewritten in place on change (delete the old record, insert the
// new), and the engine drives the pager group commit so a DDL change lands as one
// WAL transaction alongside any page allocation it triggered (spec 2061 doc 09
// §8.1). In-memory maps mirror the heaps for lookup, and listing sorts keys on
// demand, which keeps the ordered-enumeration contract the spec's B-tree layout
// gives without a second on-disk index for the handful of catalog records a single
// file holds.
const (
	// DatabasesCollID and CollectionsCollID are the reserved heap ids the master
	// catalog occupies, distinct from the legacy single-collection ids (1 for the
	// document heap and _id index, 2 for that collection's index catalog).
	DatabasesCollID   uint32 = 3
	CollectionsCollID uint32 = 4

	// FirstUserCollID is the first heap id the engine hands to a user collection,
	// and CollIDStride is the gap between successive collections. A collection's
	// document heap and _id index use its base id and its secondary-index catalog
	// uses base+1, so the stride leaves room for both plus future per-collection
	// heaps without overlapping the next collection.
	FirstUserCollID uint32 = 16
	CollIDStride    uint32 = 16
)

// nsKey is the in-memory map key for a collection: database and name joined by a
// NUL, which cannot appear in either name.
func nsKey(db, name string) string { return db + "\x00" + name }

// dbEntry and collEntry pair a decoded record with the heap RID it occupies, so a
// rewrite can delete the prior record before inserting the new one.
type dbEntry struct {
	rec *DatabaseRecord
	rid storage.RID
}

type collEntry struct {
	rec *CollectionRecord
	rid storage.RID
}

// MasterStore is the persistent database and collection registry.
type MasterStore struct {
	pgr      *pager.Pager
	dbHeap   *heap.Heap
	collHeap *heap.Heap
	dbs      map[string]*dbEntry
	colls    map[string]*collEntry
}

// OpenMaster opens or initializes the master catalog over an open pager, loading
// every database and collection record into memory. Empty heaps (a fresh file)
// leave the registry empty.
func OpenMaster(pgr *pager.Pager) (*MasterStore, error) {
	dbHeap, err := heap.Open(pgr, DatabasesCollID)
	if err != nil {
		return nil, err
	}
	collHeap, err := heap.Open(pgr, CollectionsCollID)
	if err != nil {
		return nil, err
	}
	m := &MasterStore{
		pgr:      pgr,
		dbHeap:   dbHeap,
		collHeap: collHeap,
		dbs:      make(map[string]*dbEntry),
		colls:    make(map[string]*collEntry),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *MasterStore) load() error {
	dbRecs, err := m.dbHeap.Records()
	if err != nil {
		return err
	}
	for _, r := range dbRecs {
		rec := decodeDatabase(r.Doc)
		m.dbs[rec.Name] = &dbEntry{rec: rec, rid: r.RID}
	}
	collRecs, err := m.collHeap.Records()
	if err != nil {
		return err
	}
	for _, r := range collRecs {
		rec := decodeCollection(r.Doc)
		m.colls[nsKey(rec.DBName, rec.Name)] = &collEntry{rec: rec, rid: r.RID}
	}
	return nil
}

// MaxCollID returns the highest collection heap id any collection record uses, or
// 0 when none exist. The engine seeds its id allocator above this so a reopened
// file keeps handing out fresh ids (spec 2061 doc 09 §8.2).
func (m *MasterStore) MaxCollID() uint32 {
	var max uint32
	for _, e := range m.colls {
		if e.rec.CollID > max {
			max = e.rec.CollID
		}
	}
	return max
}

// GetDatabase returns the database record for name, or nil.
func (m *MasterStore) GetDatabase(name string) *DatabaseRecord {
	if e, ok := m.dbs[name]; ok {
		return e.rec
	}
	return nil
}

// GetCollection returns the collection record for db.name, or nil.
func (m *MasterStore) GetCollection(db, name string) *CollectionRecord {
	if e, ok := m.colls[nsKey(db, name)]; ok {
		return e.rec
	}
	return nil
}

// ListDatabases returns every database record sorted by name.
func (m *MasterStore) ListDatabases() []*DatabaseRecord {
	out := make([]*DatabaseRecord, 0, len(m.dbs))
	for _, e := range m.dbs {
		out = append(out, e.rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListCollections returns every collection record in db, sorted by name. A nil or
// empty db lists collections across all databases, sorted by (db, name).
func (m *MasterStore) ListCollections(db string) []*CollectionRecord {
	out := make([]*CollectionRecord, 0, len(m.colls))
	for _, e := range m.colls {
		if db == "" || e.rec.DBName == db {
			out = append(out, e.rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DBName != out[j].DBName {
			return out[i].DBName < out[j].DBName
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// stageDatabase rewrites a database record in its heap without committing.
func (m *MasterStore) stageDatabase(rec *DatabaseRecord) error {
	tx := catalogTxn{}
	e := m.dbs[rec.Name]
	if e != nil && !e.rid.IsNull() {
		if err := m.dbHeap.Delete(tx, e.rid); err != nil {
			return err
		}
	}
	rid, err := m.dbHeap.Insert(tx, encodeDatabase(rec))
	if err != nil {
		return err
	}
	m.dbs[rec.Name] = &dbEntry{rec: rec, rid: rid}
	return nil
}

// StageCollection rewrites a collection record in its heap without committing, so
// the engine can fold the catalog write into the same pager commit as the page
// allocation or data write it accompanies.
func (m *MasterStore) StageCollection(rec *CollectionRecord) error {
	tx := catalogTxn{}
	k := nsKey(rec.DBName, rec.Name)
	e := m.colls[k]
	if e != nil && !e.rid.IsNull() {
		if err := m.collHeap.Delete(tx, e.rid); err != nil {
			return err
		}
	}
	rid, err := m.collHeap.Insert(tx, encodeCollection(rec))
	if err != nil {
		return err
	}
	m.colls[k] = &collEntry{rec: rec, rid: rid}
	return nil
}

// PutDatabase stages a database record and commits the pager.
func (m *MasterStore) PutDatabase(rec *DatabaseRecord) error {
	if err := m.stageDatabase(rec); err != nil {
		return err
	}
	return m.pgr.Commit()
}

// RemoveDatabase deletes a database record and commits the pager.
func (m *MasterStore) RemoveDatabase(name string) error {
	e, ok := m.dbs[name]
	if !ok {
		return nil
	}
	if !e.rid.IsNull() {
		if err := m.dbHeap.Delete(catalogTxn{}, e.rid); err != nil {
			return err
		}
	}
	delete(m.dbs, name)
	return m.pgr.Commit()
}

// RemoveCollection deletes a collection record and commits the pager.
func (m *MasterStore) RemoveCollection(db, name string) error {
	k := nsKey(db, name)
	e, ok := m.colls[k]
	if !ok {
		return nil
	}
	if !e.rid.IsNull() {
		if err := m.collHeap.Delete(catalogTxn{}, e.rid); err != nil {
			return err
		}
	}
	delete(m.colls, k)
	return m.pgr.Commit()
}

// Commit flushes staged catalog writes through the pager group commit.
func (m *MasterStore) Commit() error { return m.pgr.Commit() }
