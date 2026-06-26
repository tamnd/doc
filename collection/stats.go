package collection

import "github.com/tamnd/doc/catalog"

// IndexStat is the per-index slice of a collection's storage accounting: the
// index name, its live entry count, and the bytes its B-tree pages occupy.
type IndexStat struct {
	Name    string
	Entries int64
	Bytes   int64
}

// Stats is a collection's storage accounting, the raw numbers the public
// collStats command shapes into a result document (spec 2061 doc 14 §13.4). The
// document count and storage bytes come from the heap's occupancy; the per-index
// byte sizes come from each B-tree's node-page count times the page size.
type Stats struct {
	Documents    int64
	StorageBytes int64
	Indexes      []IndexStat
}

// Stats gathers the collection's storage accounting from the heap and every open
// index. The _id index is reported first, then each secondary index in creation
// order, matching ListIndexes.
func (c *Collection) Stats() Stats {
	fs := c.hp.FreeSpaceStats()
	pageSize := int64(c.pgr.PageSize())
	out := Stats{
		Documents:    int64(fs.LiveRecords),
		StorageBytes: int64(fs.PageCount+fs.OverflowPages) * pageSize,
	}
	idStats := c.idx.Stats()
	out.Indexes = append(out.Indexes, IndexStat{
		Name:    catalog.IDIndexName,
		Entries: int64(idStats.Entries),
		Bytes:   int64(idStats.Pages) * pageSize,
	})
	for _, sp := range c.cat.Specs() {
		bt := c.sidx[sp.Name]
		if bt == nil {
			continue
		}
		s := bt.Stats()
		out.Indexes = append(out.Indexes, IndexStat{
			Name:    sp.Name,
			Entries: int64(s.Entries),
			Bytes:   int64(s.Pages) * pageSize,
		})
	}
	return out
}

// StageIndexTTL changes the expireAfterSeconds of an existing secondary index and
// stages the catalog without committing, so collMod can fold it into the same WAL
// transaction as a validator change (spec 2061 doc 09 §8.7). It rejects the _id
// index and an unknown name. A seconds value of zero turns the index back into a
// plain (non-expiring) index.
func (c *Collection) StageIndexTTL(name string, seconds int64) error {
	if name == catalog.IDIndexName {
		return catalog.ErrIndexNotFound
	}
	sp := c.cat.Find(name)
	if sp == nil {
		return catalog.ErrIndexNotFound
	}
	sp.ExpireAfterSeconds = seconds
	return c.cat.Stage()
}

// SetIndexTTL stages a TTL change and commits it in its own transaction.
func (c *Collection) SetIndexTTL(name string, seconds int64) error {
	if err := c.StageIndexTTL(name, seconds); err != nil {
		return err
	}
	return c.pgr.Commit()
}
