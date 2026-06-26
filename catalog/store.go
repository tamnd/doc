package catalog

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/heap"
	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
)

// CatalogCollID is the reserved heap collection id under which the index catalog
// is persisted. It is distinct from the document collection's id (1) so the
// catalog's pages self-identify on reopen via the heap's collID scan (spec 2061
// doc 09 §7.2). The catalog is a single BSON record rewritten in place on every
// DDL: delete the prior record, insert the new one, and group-commit the pager.
const CatalogCollID = 2

// Store is the persistent registry of a collection's secondary index specs. It
// owns a dedicated heap keyed by CatalogCollID holding one BSON record: the list
// of IndexSpec documents. The _id index is implicit and never stored here.
type Store struct {
	pgr   *pager.Pager
	hp    *heap.Heap
	specs []*IndexSpec
	rid   storage.RID
}

// OpenStoreWithCollID opens the secondary-index catalog over a caller-chosen heap
// collection id, so a multi-collection file can give each collection its own index
// registry on a distinct, self-identifying heap (spec 2061 doc 09 §2.5). OpenStore
// is the single-collection case pinned to CatalogCollID.
func OpenStoreWithCollID(pgr *pager.Pager, collID uint32) (*Store, error) {
	hp, err := heap.Open(pgr, collID)
	if err != nil {
		return nil, err
	}
	s := &Store{pgr: pgr, hp: hp, rid: storage.NullRID}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// catalogTxn is the trivial storage.Txn the catalog hands to its heap. Catalog
// records are not MVCC-versioned (the catalog keeps exactly one latest record),
// so the version is a fixed sentinel and the transaction methods are inert; the
// pager group commit in Save provides durability.
type catalogTxn struct{}

func (catalogTxn) Snapshot() uint64     { return 0 }
func (catalogTxn) WriteVersion() uint64 { return 0 }
func (catalogTxn) IsReadOnly() bool     { return false }
func (catalogTxn) LogRecord(pageNo uint32, offset uint16, before, after []byte) error {
	return nil
}
func (catalogTxn) Commit() error   { return nil }
func (catalogTxn) Rollback() error { return nil }

// OpenStore opens (or initializes) the catalog over an already-open pager,
// loading the persisted index specs into memory.
func OpenStore(pgr *pager.Pager) (*Store, error) {
	return OpenStoreWithCollID(pgr, CatalogCollID)
}

// load reads the persisted catalog record and decodes its spec list. An empty
// heap (a fresh collection, or one with no secondary indexes) leaves the spec
// list empty.
func (s *Store) load() error {
	recs, err := s.hp.Records()
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return nil
	}
	// The catalog keeps exactly one record; if recovery ever surfaced more than
	// one, the last written wins.
	last := recs[len(recs)-1]
	specs, err := decodeSpecs(last.Doc)
	if err != nil {
		return err
	}
	s.rid = last.RID
	s.specs = specs
	return nil
}

// Specs returns the in-memory spec list. The caller must not mutate the returned
// slice or its elements; it is the live catalog state.
func (s *Store) Specs() []*IndexSpec { return s.specs }

// Find returns the spec with the given index name, or nil.
func (s *Store) Find(name string) *IndexSpec {
	for _, sp := range s.specs {
		if sp.Name == name {
			return sp
		}
	}
	return nil
}

// Add appends a validated spec to the in-memory list. It does not persist; the
// caller drives Save once the index build has settled its root page. A spec whose
// name or normalized key already exists is rejected.
func (s *Store) Add(sp *IndexSpec) error {
	if err := sp.validate(); err != nil {
		return err
	}
	if sp.Name == "" {
		sp.Name = DefaultName(sp.Key)
	}
	for _, ex := range s.specs {
		if ex.Name == sp.Name || sameKey(ex.Key, sp.Key) {
			return ErrIndexExists
		}
	}
	s.specs = append(s.specs, sp)
	return nil
}

// Remove drops the named spec from the in-memory list, returning the removed spec
// so the caller can reclaim its B-tree. Dropping the _id index is rejected.
func (s *Store) Remove(name string) (*IndexSpec, error) {
	if name == IDIndexName {
		return nil, ErrCannotDropID
	}
	for i, sp := range s.specs {
		if sp.Name == name {
			s.specs = append(s.specs[:i], s.specs[i+1:]...)
			return sp, nil
		}
	}
	return nil, ErrIndexNotFound
}

// Stage writes the current spec list into the catalog heap as the single catalog
// record without committing the pager: the prior record is deleted and the new
// one inserted, leaving the pages dirty. The caller drives the pager group commit,
// so the catalog write can land atomically with index-build or maintenance page
// writes buffered on the same pager (spec 2061 doc 09 §8.5).
func (s *Store) Stage() error {
	doc := encodeSpecs(s.specs)
	tx := catalogTxn{}
	if !s.rid.IsNull() {
		if err := s.hp.Delete(tx, s.rid); err != nil {
			return err
		}
		s.rid = storage.NullRID
	}
	rid, err := s.hp.Insert(tx, doc)
	if err != nil {
		return err
	}
	s.rid = rid
	return nil
}

// Save stages the spec list and group-commits the pager. It is the standalone
// persistence path for tests and any caller not folding the catalog write into a
// larger commit.
func (s *Store) Save() error {
	if err := s.Stage(); err != nil {
		return err
	}
	return s.pgr.Commit()
}

// sameKey reports whether two key specs are field-for-field identical, which
// MongoDB treats as the same index regardless of name.
func sameKey(a, b []KeyPart) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- BSON codec ----------------------------------------------------------

// encodeSpecs serializes the spec list to a single BSON document with an
// "indexes" array, each element a self-describing index document.
func encodeSpecs(specs []*IndexSpec) bson.Raw {
	var elems []bson.RawValue
	for _, sp := range specs {
		elems = append(elems, bson.RawValue{Type: bson.TypeDocument, Data: encodeSpec(sp)})
	}
	b := bson.NewBuilder()
	b.AppendArray("indexes", bson.BuildArray(elems...))
	return b.Build()
}

// encodeSpec serializes one index spec. The key is stored as an ordered array of
// {f, d} documents so multi-field key order survives a round trip.
func encodeSpec(sp *IndexSpec) bson.Raw {
	var keyElems []bson.RawValue
	for _, p := range sp.Key {
		kb := bson.NewBuilder()
		kb.AppendString("f", p.Field)
		dir := int32(1)
		if p.Desc {
			dir = -1
		}
		kb.AppendInt32("d", dir)
		keyElems = append(keyElems, bson.RawValue{Type: bson.TypeDocument, Data: kb.Build()})
	}
	b := bson.NewBuilder()
	b.AppendString("name", sp.Name)
	b.AppendArray("key", bson.BuildArray(keyElems...))
	b.AppendBoolean("unique", sp.Unique)
	b.AppendBoolean("sparse", sp.Sparse)
	if sp.PartialFilter != nil {
		b.AppendDocument("partialFilterExpression", sp.PartialFilter)
	}
	b.AppendInt64("expireAfterSeconds", sp.ExpireAfterSeconds)
	b.AppendBoolean("multikey", sp.Multikey)
	b.AppendInt64("root", int64(sp.Root))
	return b.Build()
}

// decodeSpecs parses the catalog record back into a spec list.
func decodeSpecs(doc bson.Raw) ([]*IndexSpec, error) {
	arr, ok := doc.Lookup("indexes")
	if !ok || arr.Type != bson.TypeArray {
		return nil, nil
	}
	elems, err := arr.Document().Elements()
	if err != nil {
		return nil, err
	}
	out := make([]*IndexSpec, 0, len(elems))
	for _, e := range elems {
		if e.Value.Type != bson.TypeDocument {
			continue
		}
		sp, derr := decodeSpec(e.Value.Document())
		if derr != nil {
			return nil, derr
		}
		out = append(out, sp)
	}
	return out, nil
}

func decodeSpec(d bson.Raw) (*IndexSpec, error) {
	sp := &IndexSpec{Root: format.NullPage}
	if v, ok := d.Lookup("name"); ok {
		sp.Name = v.StringValue()
	}
	if v, ok := d.Lookup("key"); ok && v.Type == bson.TypeArray {
		parts, err := v.Document().Elements()
		if err != nil {
			return nil, err
		}
		for _, pe := range parts {
			if pe.Value.Type != bson.TypeDocument {
				continue
			}
			pd := pe.Value.Document()
			kp := KeyPart{}
			if fv, ok := pd.Lookup("f"); ok {
				kp.Field = fv.StringValue()
			}
			if dv, ok := pd.Lookup("d"); ok {
				kp.Desc = dv.Int32() < 0
			}
			sp.Key = append(sp.Key, kp)
		}
	}
	if v, ok := d.Lookup("unique"); ok {
		sp.Unique = v.Boolean()
	}
	if v, ok := d.Lookup("sparse"); ok {
		sp.Sparse = v.Boolean()
	}
	if v, ok := d.Lookup("partialFilterExpression"); ok && v.Type == bson.TypeDocument {
		sp.PartialFilter = v.Document().Clone()
	}
	if v, ok := d.Lookup("expireAfterSeconds"); ok {
		sp.ExpireAfterSeconds = v.Int64()
	}
	if v, ok := d.Lookup("multikey"); ok {
		sp.Multikey = v.Boolean()
	}
	if v, ok := d.Lookup("root"); ok {
		sp.Root = uint32(v.Int64())
	}
	return sp, nil
}
