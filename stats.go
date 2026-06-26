package doc

import "context"

// Stats returns storage accounting for the collection: its document count, the
// bytes its heap occupies, and the bytes each index occupies (spec 2061 doc 14
// §13.4). It returns ErrNamespaceNotFound when the collection has never been
// created (no implicit creation happens on a stats read).
func (c *Collection) Stats(ctx context.Context) (*CollectionStats, error) {
	if err := c.db.check(ctx); err != nil {
		return nil, err
	}
	cs, err := c.db.eng.CollectionStats(c.dbName, c.name)
	if err != nil {
		return nil, mapEngineErr(err)
	}
	return &CollectionStats{
		Namespace:     cs.Namespace,
		DocumentCount: cs.DocumentCount,
		StorageSize:   cs.StorageSize,
		IndexSize:     cs.IndexSize,
		TotalSize:     cs.StorageSize + cs.IndexSize,
		IndexSizes:    cs.IndexSizes,
		Capped:        cs.Capped,
		MaxDocuments:  cs.MaxDocuments,
	}, nil
}

// Stats returns storage accounting aggregated across the database's collections
// (spec 2061 doc 14 §13.5). A database with no collections reports all zeros.
func (d *Database) Stats(ctx context.Context) (*DatabaseStats, error) {
	if err := d.db.check(ctx); err != nil {
		return nil, err
	}
	ds := d.db.eng.DatabaseStats(d.name)
	return &DatabaseStats{
		Database:      ds.Database,
		Collections:   ds.Collections,
		Indexes:       ds.Indexes,
		DocumentCount: ds.DocumentCount,
		StorageSize:   ds.StorageSize,
		IndexSize:     ds.IndexSize,
		TotalSize:     ds.TotalSize,
	}, nil
}
