package doc

// The result types report the outcome of a write. They mirror mongo-go-driver
// field for field so existing code reads the same against doc (spec 2061 doc 14
// §5, §7, §9).

// InsertOneResult reports the _id assigned to or supplied for an inserted
// document. InsertedID is the natural Go form of the _id (ObjectID, string, and
// so on), decoded from the stored value.
type InsertOneResult struct {
	InsertedID any
}

// InsertManyResult reports the _id values of a batch insert, in the order the
// documents were supplied.
type InsertManyResult struct {
	InsertedIDs []any
}

// UpdateResult reports the outcome of an update, replace, or upsert: how many
// documents matched, how many actually changed, and the _id of any upserted
// document.
type UpdateResult struct {
	MatchedCount  int64
	ModifiedCount int64
	UpsertedCount int64
	UpsertedID    any
}

// DeleteResult reports how many documents a delete removed.
type DeleteResult struct {
	DeletedCount int64
}

// BulkWriteResult aggregates the per-category counts of a bulkWrite plus the
// _id values of inserted and upserted documents keyed by their position in the
// models slice.
type BulkWriteResult struct {
	InsertedCount int64
	MatchedCount  int64
	ModifiedCount int64
	DeletedCount  int64
	UpsertedCount int64
	UpsertedIDs   map[int64]any
}

// CollectionStats reports storage accounting for one collection (spec 2061 doc
// 14 §13.4).
type CollectionStats struct {
	Namespace     string
	DocumentCount int64
	StorageSize   int64
	IndexSize     int64
	TotalSize     int64
	IndexSizes    map[string]int64
	Capped        bool
	MaxDocuments  int64
}

// DatabaseStats reports storage accounting aggregated across a database's
// collections.
type DatabaseStats struct {
	Database      string
	Collections   int64
	Indexes       int64
	DocumentCount int64
	StorageSize   int64
	IndexSize     int64
	TotalSize     int64
}

// ListDatabasesResult is the structured form returned by ListDatabases.
type ListDatabasesResult struct {
	Databases []DatabaseSpecification
	TotalSize int64
}

// DatabaseSpecification is one entry of a ListDatabases result.
type DatabaseSpecification struct {
	Name       string
	SizeOnDisk int64
	Empty      bool
}
