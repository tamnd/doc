package collection

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/index"
	"github.com/tamnd/doc/plan"
	"github.com/tamnd/doc/storage"
)

// planSource adapts a transaction's snapshot to plan.Source so the query planner
// and the execution engine read every document through the same overlay the rest
// of the collection uses. It is created per find and lives only for that read, so
// a collection scan and an index fetch resolve to the same document versions.
type planSource struct {
	t *Txn
}

// ScanDocuments returns the documents the transaction sees in natural order, each
// with the RID of the committed version it reads. A document the transaction
// buffered but has not committed carries a null RID, which the collection scan
// never needs because it never fetches by RID.
func (s planSource) ScanDocuments() ([]plan.Row, error) {
	t := s.t
	keys := t.scanKeys()
	out := make([]plan.Row, 0, len(keys))
	for _, key := range keys {
		doc := t.currentDoc(key)
		if doc == nil {
			continue
		}
		rid, _, _ := t.committedVersion(key)
		out = append(out, plan.Row{RID: rid, Doc: doc})
	}
	return out, nil
}

// OpenIndexCursor opens a forward cursor over the named index between the encoded
// field-key bounds. The reserved _id name maps to the primary index; every other
// name maps to a secondary B-tree. The cursor reads at the transaction's snapshot
// version, which the planner only allows when that snapshot is the latest
// committed state.
func (s planSource) OpenIndexCursor(name string, lo, hi storage.IndexKey, opts storage.ScanOpts) (storage.IndexCursor, error) {
	c := s.t.c
	var bt *index.BTree
	if name == catalog.IDIndexName {
		bt = c.idx
	} else {
		bt = c.sidx[name]
	}
	if bt == nil {
		return nil, catalog.ErrIndexNotFound
	}
	return bt.Scan(writeTxn{version: s.t.startVer}, lo, hi, opts)
}

// LookupRID resolves an index entry's RID to the document the transaction sees,
// returning false if no visible document owns that RID. The ridOwner map records
// the latest committed RID for each live document, which is what the index entries
// point to on the snapshot the planner allows an index scan for.
func (s planSource) LookupRID(rid storage.RID) (bson.Raw, bool) {
	t := s.t
	t.c.mu.Lock()
	key, ok := t.c.ridOwner[rid]
	t.c.mu.Unlock()
	if !ok {
		return nil, false
	}
	doc := t.currentDoc(key)
	if doc == nil {
		return nil, false
	}
	return doc, true
}

// indexDescs builds the planner's view of the available indexes: the implicit _id
// index first, then each secondary index in creation order. The _id index is
// unique and single-field over _id, so an _id equality filter can plan as a point
// index scan.
func (c *Collection) indexDescs() []plan.IndexDesc {
	descs := []plan.IndexDesc{{
		Name:   catalog.IDIndexName,
		Key:    []catalog.KeyPart{{Field: idFieldName}},
		Unique: true,
	}}
	for _, sp := range c.cat.Specs() {
		descs = append(descs, plan.IndexDesc{
			Name:     sp.Name,
			Key:      sp.Key,
			Unique:   sp.Unique,
			Sparse:   sp.Sparse,
			Multikey: sp.Multikey,
			Partial:  sp.PartialFilter != nil,
		})
	}
	return descs
}

// allowIndex reports whether an index access path is safe for this read. It is not
// when the transaction has its own buffered writes (an index scan would miss them)
// or when its snapshot predates the latest commit (the live index reflects the
// latest state, not the snapshot). In those cases the planner falls back to a
// collection scan over the snapshot-correct overlay.
func (t *Txn) allowIndex() bool {
	return !t.hasWrites() && t.startVer == t.c.orc.CommitVersion()
}

// buildPlan validates the filter and compiles the find request into a plan over
// this transaction's snapshot.
func (t *Txn) buildPlan(filter bson.Raw, opts FindOptions) (*plan.Plan, error) {
	if len(filter) > 0 {
		if err := filter.WellFormed(); err != nil {
			return nil, err
		}
	}
	req := plan.Request{
		Filter:     filter,
		Projection: opts.Projection,
		Sort:       opts.Sort,
		Skip:       opts.Skip,
		Limit:      opts.Limit,
		Indexes:    t.c.indexDescs(),
		AllowIndex: t.allowIndex(),
	}
	return plan.New(req, planSource{t: t})
}
