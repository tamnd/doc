package collection

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/index"
	"github.com/tamnd/doc/query"
	"github.com/tamnd/doc/storage"
)

// IndexModel describes a secondary index to create: its ordered key, an optional
// explicit name (the MongoDB default name is generated when empty), and its
// options. It mirrors the mongo-driver IndexModel so the surface is familiar
// (spec 2061 doc 09 §8.5).
type IndexModel struct {
	Key                []catalog.KeyPart
	Name               string
	Unique             bool
	Sparse             bool
	PartialFilter      bson.Raw
	ExpireAfterSeconds int64
}

// IndexInfo is one entry of ListIndexes: a created index's name, key, and the
// options that distinguish it. The _id index is always reported first.
type IndexInfo struct {
	Name               string
	Key                []catalog.KeyPart
	Unique             bool
	Sparse             bool
	Multikey           bool
	PartialFilter      bson.Raw
	ExpireAfterSeconds int64
}

// CreateIndex builds a single secondary index over the collection's current
// committed documents and persists it. It returns the index name. A foreground
// build is used: the collection is scanned once, each document's keys inserted,
// and the catalog committed atomically with the new B-tree pages. A duplicate name
// or key spec is rejected, and a unique index whose existing data already holds a
// repeated key fails with ErrDuplicateKey (spec 2061 doc 09 §8.5, doc 07 §10).
func (c *Collection) CreateIndex(m IndexModel) (string, error) {
	sp := &catalog.IndexSpec{
		Name:               m.Name,
		Key:                m.Key,
		Unique:             m.Unique,
		Sparse:             m.Sparse,
		PartialFilter:      m.PartialFilter,
		ExpireAfterSeconds: m.ExpireAfterSeconds,
		Root:               format.NullPage,
	}
	if err := c.cat.Add(sp); err != nil {
		return "", err
	}
	treeCollID := c.secondaryBase + uint32(len(c.cat.Specs())-1)
	bt, err := c.openIndexTree(treeCollID, sp)
	if err != nil {
		_, _ = c.cat.Remove(sp.Name)
		return "", err
	}
	if err := c.buildIndex(sp, bt); err != nil {
		_, _ = c.cat.Remove(sp.Name)
		return "", err
	}
	c.sidx[sp.Name] = bt
	if err := c.cat.Stage(); err != nil {
		return "", err
	}
	c.catalogDirty = false
	if err := c.pgr.Commit(); err != nil {
		return "", err
	}
	return sp.Name, nil
}

// CreateIndexes creates several indexes in order and returns their names. It stops
// at the first failure; indexes created before the failure remain.
func (c *Collection) CreateIndexes(models []IndexModel) ([]string, error) {
	names := make([]string, 0, len(models))
	for _, m := range models {
		name, err := c.CreateIndex(m)
		if err != nil {
			return names, err
		}
		names = append(names, name)
	}
	return names, nil
}

// buildIndex scans the collection's latest committed documents and inserts each
// one's index keys into bt, learning the multikey flag along the way. It runs
// before the catalog is persisted, so a unique-violation failure leaves the
// catalog without the spec.
func (c *Collection) buildIndex(sp *catalog.IndexSpec, bt *index.BTree) error {
	rows := c.committedRows()
	wt := writeTxn{version: 0}
	seen := make(map[string]struct{})
	for _, r := range rows {
		keys, multikey, err := c.indexableKeys(sp, r.doc)
		if err != nil {
			return err
		}
		if multikey {
			sp.Multikey = true
		}
		for _, k := range keys {
			if sp.Unique {
				if _, dup := seen[string(k)]; dup {
					return ErrDuplicateKey
				}
				seen[string(k)] = struct{}{}
			}
			if err := bt.Put(wt, storage.IndexKey(k), r.rid); err != nil {
				return err
			}
		}
	}
	return nil
}

// committedRow is one latest committed document and its heap RID, the unit the
// index builder and the index fetch path operate on.
type committedRow struct {
	rid storage.RID
	doc bson.Raw
}

// committedRows returns the latest committed version of every live document, in
// natural order. It reads the overlay under the collection mutex.
func (c *Collection) committedRows() []committedRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows := make([]committedRow, 0, len(c.order))
	for _, key := range c.order {
		ch := c.byID[key]
		if ch == nil || len(ch.versions) == 0 {
			continue
		}
		v := ch.versions[0]
		if v.doc == nil {
			continue
		}
		rows = append(rows, committedRow{rid: v.rid, doc: v.doc})
	}
	return rows
}

// DropIndex drops a secondary index by name. Dropping the _id index is rejected.
// The spec is removed from the catalog and the catalog is committed; the dropped
// B-tree's pages are not reclaimed yet (deferred freelist reclaim, spec 2061 doc
// 09 §8.6).
func (c *Collection) DropIndex(name string) error {
	if _, err := c.cat.Remove(name); err != nil {
		return err
	}
	delete(c.sidx, name)
	if err := c.cat.Stage(); err != nil {
		return err
	}
	return c.pgr.Commit()
}

// DropAllIndexes drops every secondary index, leaving the _id index.
func (c *Collection) DropAllIndexes() error {
	for _, sp := range append([]*catalog.IndexSpec(nil), c.cat.Specs()...) {
		if _, err := c.cat.Remove(sp.Name); err != nil {
			return err
		}
		delete(c.sidx, sp.Name)
	}
	if err := c.cat.Stage(); err != nil {
		return err
	}
	return c.pgr.Commit()
}

// ListIndexes returns the collection's indexes, the implicit _id index first,
// then each secondary index in creation order.
func (c *Collection) ListIndexes() []IndexInfo {
	out := []IndexInfo{{
		Name: catalog.IDIndexName,
		Key:  []catalog.KeyPart{{Field: idFieldName}},
	}}
	for _, sp := range c.cat.Specs() {
		out = append(out, IndexInfo{
			Name:               sp.Name,
			Key:                append([]catalog.KeyPart(nil), sp.Key...),
			Unique:             sp.Unique,
			Sparse:             sp.Sparse,
			Multikey:           sp.Multikey,
			PartialFilter:      sp.PartialFilter,
			ExpireAfterSeconds: sp.ExpireAfterSeconds,
		})
	}
	return out
}

// ---- maintenance ---------------------------------------------------------

// indexableKeys returns the index keys a document contributes to sp, honoring the
// partial filter (a document not matching it contributes nothing) and sparse rule.
// The second return reports whether an array forced sp to multikey. An empty key
// list means the document is not indexed by sp.
func (c *Collection) indexableKeys(sp *catalog.IndexSpec, doc bson.Raw) ([][]byte, bool, error) {
	if sp.PartialFilter != nil {
		m, err := query.Compile(sp.PartialFilter)
		if err != nil {
			return nil, false, err
		}
		if !m.Match(doc) {
			return nil, false, nil
		}
	}
	keys, indexed, multikey, err := sp.Keys(doc)
	if err != nil {
		return nil, false, err
	}
	if !indexed {
		return nil, false, nil
	}
	return keys, multikey, nil
}

// insertSecondary inserts a freshly stored document's keys into every secondary
// index, learning the multikey flag (which is persisted via the catalog on the
// same commit).
func (c *Collection) insertSecondary(wt writeTxn, doc bson.Raw, rid storage.RID) error {
	for _, sp := range c.cat.Specs() {
		bt := c.sidx[sp.Name]
		if bt == nil {
			continue
		}
		keys, multikey, err := c.indexableKeys(sp, doc)
		if err != nil {
			return err
		}
		if multikey && !sp.Multikey {
			sp.Multikey = true
			c.catalogDirty = true
		}
		for _, k := range keys {
			if err := bt.Put(wt, storage.IndexKey(k), rid); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteSecondary removes a superseded document's keys from every secondary index.
// A missing entry is tolerated: it means the document did not contribute that key
// (for example a partial index the document never qualified for).
func (c *Collection) deleteSecondary(wt writeTxn, doc bson.Raw, rid storage.RID) error {
	if doc == nil {
		return nil
	}
	for _, sp := range c.cat.Specs() {
		bt := c.sidx[sp.Name]
		if bt == nil {
			continue
		}
		keys, _, err := c.indexableKeys(sp, doc)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := bt.Delete(wt, storage.IndexKey(k), rid); err != nil && err != storage.ErrNotFound {
				return err
			}
		}
	}
	return nil
}

// checkUniqueSecondary verifies that the transaction's inserts do not collide with
// any unique secondary index, reading the live committed index state. RIDs the
// transaction is removing in the same commit are exempt (an in-place update keeps
// its own slot), and two inserts in one transaction that share a unique key
// collide. It runs before any page mutation so a violation aborts cleanly.
func (c *Collection) checkUniqueSecondary(pending map[string]*pendingOp, order []string) error {
	var uniques []*catalog.IndexSpec
	for _, sp := range c.cat.Specs() {
		if sp.Unique {
			uniques = append(uniques, sp)
		}
	}
	if len(uniques) == 0 {
		return nil
	}
	removing := make(map[storage.RID]struct{})
	for _, key := range order {
		if p := pending[key]; p.hasRemove {
			removing[p.removeRID] = struct{}{}
		}
	}
	rt := writeTxn{}
	for _, sp := range uniques {
		bt := c.sidx[sp.Name]
		if bt == nil {
			continue
		}
		seen := make(map[string]struct{})
		for _, key := range order {
			p := pending[key]
			if p.insertDoc == nil {
				continue
			}
			keys, _, err := c.indexableKeys(sp, p.insertDoc)
			if err != nil {
				return err
			}
			for _, k := range keys {
				ks := string(k)
				if _, dup := seen[ks]; dup {
					return ErrDuplicateKey
				}
				seen[ks] = struct{}{}
				rid, gerr := bt.Get(rt, storage.IndexKey(k))
				if gerr == nil {
					if _, exempt := removing[rid]; !exempt {
						return ErrDuplicateKey
					}
				}
			}
		}
	}
	return nil
}
