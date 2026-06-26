// Package options carries the per-operation option structs of the public doc
// API. Each operation has a matching XxxOptions struct built through a fluent
// constructor (options.Find().SetLimit(10)), mirroring mongo-go-driver so the
// surface is familiar. The package is deliberately free of any dependency on
// the doc package: option fields that hold documents (sort, projection, filter)
// are typed as any and marshaled by doc when the operation runs, which keeps
// the import graph one-way (spec 2061 doc 14 §18).
package options

import "time"

// ReturnDocument selects which version findOneAndUpdate and findOneAndReplace
// hand back: the document as it was before the change, or after it.
type ReturnDocument int8

const (
	// Before returns the matched document as it stood before the modification.
	// This is the MongoDB default.
	Before ReturnDocument = iota
	// After returns the document as it stands after the modification.
	After
)

// CursorType selects ordinary versus tailable iteration over a collection.
type CursorType int8

const (
	// NonTailable is an ordinary cursor that ends when the result set is
	// exhausted.
	NonTailable CursorType = iota
	// Tailable keeps the cursor open at the end of a capped collection and
	// resumes when new documents arrive, returning false in the meantime.
	Tailable
	// TailableAwait is Tailable, but Next blocks inside the engine waiting for
	// new documents rather than returning false immediately.
	TailableAwait
)

// Collation describes language-aware string comparison rules. It is accepted
// across the API; the planner applies it where a collation-aware comparison is
// needed (spec 2061 doc 09 §8.7).
type Collation struct {
	Locale          string
	CaseLevel       bool
	CaseFirst       string
	Strength        int
	NumericOrdering bool
	Alternate       string
	MaxVariable     string
	Normalization   bool
	Backwards       bool
}

// ArrayFilters identifies which elements of an array an update touches through
// the $[<identifier>] positional operator.
type ArrayFilters struct {
	Filters []any
}

// InsertOneOptions configures a single-document insert.
type InsertOneOptions struct {
	BypassDocumentValidation *bool
	Comment                  any
}

// InsertOne returns a fresh InsertOneOptions builder.
func InsertOne() *InsertOneOptions { return &InsertOneOptions{} }

// SetBypassDocumentValidation skips schema validation for this insert.
func (o *InsertOneOptions) SetBypassDocumentValidation(b bool) *InsertOneOptions {
	o.BypassDocumentValidation = &b
	return o
}

// SetComment attaches a comment visible in logs and currentOp.
func (o *InsertOneOptions) SetComment(c any) *InsertOneOptions {
	o.Comment = c
	return o
}

// InsertManyOptions configures a batch insert.
type InsertManyOptions struct {
	Ordered                  *bool
	BypassDocumentValidation *bool
	Comment                  any
}

// InsertMany returns a fresh InsertManyOptions builder. The default is ordered.
func InsertMany() *InsertManyOptions { return &InsertManyOptions{} }

// SetOrdered selects ordered (stop on first error) or unordered insertion.
func (o *InsertManyOptions) SetOrdered(b bool) *InsertManyOptions {
	o.Ordered = &b
	return o
}

// SetBypassDocumentValidation skips schema validation for the batch.
func (o *InsertManyOptions) SetBypassDocumentValidation(b bool) *InsertManyOptions {
	o.BypassDocumentValidation = &b
	return o
}

// SetComment attaches a comment.
func (o *InsertManyOptions) SetComment(c any) *InsertManyOptions {
	o.Comment = c
	return o
}

// FindOptions shapes a find: projection, sort, skip/limit, and cursor behavior.
type FindOptions struct {
	Projection          any
	Sort                any
	Skip                *int64
	Limit               *int64
	Hint                any
	BatchSize           *int32
	Collation           *Collation
	AllowPartialResults *bool
	NoCursorTimeout     *bool
	Comment             any
	MaxTime             *time.Duration
	ShowRecordID        *bool
	CursorType          *CursorType
}

// Find returns a fresh FindOptions builder.
func Find() *FindOptions { return &FindOptions{} }

// SetProjection selects the fields to include (1) or exclude (0).
func (o *FindOptions) SetProjection(p any) *FindOptions { o.Projection = p; return o }

// SetSort sets the sort key document; use a doc.D to preserve key order.
func (o *FindOptions) SetSort(s any) *FindOptions { o.Sort = s; return o }

// SetSkip skips the first n matching documents.
func (o *FindOptions) SetSkip(n int64) *FindOptions { o.Skip = &n; return o }

// SetLimit caps the result at n documents; 0 means unlimited.
func (o *FindOptions) SetLimit(n int64) *FindOptions { o.Limit = &n; return o }

// SetHint forces a specific index for the scan.
func (o *FindOptions) SetHint(h any) *FindOptions { o.Hint = h; return o }

// SetBatchSize sets the cursor fetch batch size.
func (o *FindOptions) SetBatchSize(n int32) *FindOptions { o.BatchSize = &n; return o }

// SetCollation sets language-aware string comparison rules.
func (o *FindOptions) SetCollation(c *Collation) *FindOptions { o.Collation = c; return o }

// SetAllowPartialResults is reserved for a future sharded mode.
func (o *FindOptions) SetAllowPartialResults(b bool) *FindOptions {
	o.AllowPartialResults = &b
	return o
}

// SetNoCursorTimeout disables the idle cursor timeout.
func (o *FindOptions) SetNoCursorTimeout(b bool) *FindOptions { o.NoCursorTimeout = &b; return o }

// SetComment attaches a query comment.
func (o *FindOptions) SetComment(c any) *FindOptions { o.Comment = c; return o }

// SetMaxTime sets a server-side time limit per batch.
func (o *FindOptions) SetMaxTime(d time.Duration) *FindOptions { o.MaxTime = &d; return o }

// SetShowRecordID adds the internal $recordId to each result.
func (o *FindOptions) SetShowRecordID(b bool) *FindOptions { o.ShowRecordID = &b; return o }

// SetCursorType selects ordinary or tailable iteration.
func (o *FindOptions) SetCursorType(t CursorType) *FindOptions { o.CursorType = &t; return o }

// FindOneOptions shapes a single-document find.
type FindOneOptions struct {
	Projection   any
	Sort         any
	Skip         *int64
	Hint         any
	Collation    *Collation
	Comment      any
	MaxTime      *time.Duration
	ShowRecordID *bool
}

// FindOne returns a fresh FindOneOptions builder.
func FindOne() *FindOneOptions { return &FindOneOptions{} }

// SetProjection selects the fields to include or exclude.
func (o *FindOneOptions) SetProjection(p any) *FindOneOptions { o.Projection = p; return o }

// SetSort sets the sort that picks which match is returned.
func (o *FindOneOptions) SetSort(s any) *FindOneOptions { o.Sort = s; return o }

// SetSkip skips the first n matching documents.
func (o *FindOneOptions) SetSkip(n int64) *FindOneOptions { o.Skip = &n; return o }

// SetHint forces a specific index.
func (o *FindOneOptions) SetHint(h any) *FindOneOptions { o.Hint = h; return o }

// SetCollation sets string comparison rules.
func (o *FindOneOptions) SetCollation(c *Collation) *FindOneOptions { o.Collation = c; return o }

// SetComment attaches a query comment.
func (o *FindOneOptions) SetComment(c any) *FindOneOptions { o.Comment = c; return o }

// SetMaxTime sets a server-side time limit.
func (o *FindOneOptions) SetMaxTime(d time.Duration) *FindOneOptions { o.MaxTime = &d; return o }

// SetShowRecordID adds the internal $recordId to the result.
func (o *FindOneOptions) SetShowRecordID(b bool) *FindOneOptions { o.ShowRecordID = &b; return o }

// UpdateOptions configures update and replace operations.
type UpdateOptions struct {
	Upsert                   *bool
	ArrayFilters             *ArrayFilters
	Hint                     any
	Collation                *Collation
	BypassDocumentValidation *bool
	Comment                  any
}

// Update returns a fresh UpdateOptions builder.
func Update() *UpdateOptions { return &UpdateOptions{} }

// SetUpsert inserts a new document when the filter matches nothing.
func (o *UpdateOptions) SetUpsert(b bool) *UpdateOptions { o.Upsert = &b; return o }

// SetArrayFilters identifies array elements for positional updates.
func (o *UpdateOptions) SetArrayFilters(f ArrayFilters) *UpdateOptions { o.ArrayFilters = &f; return o }

// SetHint forces an index for the filter scan.
func (o *UpdateOptions) SetHint(h any) *UpdateOptions { o.Hint = h; return o }

// SetCollation sets string comparison rules.
func (o *UpdateOptions) SetCollation(c *Collation) *UpdateOptions { o.Collation = c; return o }

// SetBypassDocumentValidation skips schema validation.
func (o *UpdateOptions) SetBypassDocumentValidation(b bool) *UpdateOptions {
	o.BypassDocumentValidation = &b
	return o
}

// SetComment attaches a comment.
func (o *UpdateOptions) SetComment(c any) *UpdateOptions { o.Comment = c; return o }

// ReplaceOptions configures a whole-document replace.
type ReplaceOptions struct {
	Upsert                   *bool
	Hint                     any
	Collation                *Collation
	BypassDocumentValidation *bool
	Comment                  any
}

// Replace returns a fresh ReplaceOptions builder.
func Replace() *ReplaceOptions { return &ReplaceOptions{} }

// SetUpsert inserts the replacement when the filter matches nothing.
func (o *ReplaceOptions) SetUpsert(b bool) *ReplaceOptions { o.Upsert = &b; return o }

// SetHint forces an index for the filter scan.
func (o *ReplaceOptions) SetHint(h any) *ReplaceOptions { o.Hint = h; return o }

// SetCollation sets string comparison rules.
func (o *ReplaceOptions) SetCollation(c *Collation) *ReplaceOptions { o.Collation = c; return o }

// SetBypassDocumentValidation skips schema validation.
func (o *ReplaceOptions) SetBypassDocumentValidation(b bool) *ReplaceOptions {
	o.BypassDocumentValidation = &b
	return o
}

// SetComment attaches a comment.
func (o *ReplaceOptions) SetComment(c any) *ReplaceOptions { o.Comment = c; return o }

// DeleteOptions configures a delete.
type DeleteOptions struct {
	Hint      any
	Collation *Collation
	Comment   any
}

// Delete returns a fresh DeleteOptions builder.
func Delete() *DeleteOptions { return &DeleteOptions{} }

// SetHint forces an index for the filter scan.
func (o *DeleteOptions) SetHint(h any) *DeleteOptions { o.Hint = h; return o }

// SetCollation sets string comparison rules.
func (o *DeleteOptions) SetCollation(c *Collation) *DeleteOptions { o.Collation = c; return o }

// SetComment attaches a comment.
func (o *DeleteOptions) SetComment(c any) *DeleteOptions { o.Comment = c; return o }

// FindOneAndUpdateOptions configures an atomic find-and-update.
type FindOneAndUpdateOptions struct {
	Projection               any
	Sort                     any
	Upsert                   *bool
	ReturnDocument           *ReturnDocument
	ArrayFilters             *ArrayFilters
	Hint                     any
	Collation                *Collation
	BypassDocumentValidation *bool
	Comment                  any
}

// FindOneAndUpdate returns a fresh FindOneAndUpdateOptions builder.
func FindOneAndUpdate() *FindOneAndUpdateOptions { return &FindOneAndUpdateOptions{} }

// SetProjection shapes the returned document.
func (o *FindOneAndUpdateOptions) SetProjection(p any) *FindOneAndUpdateOptions {
	o.Projection = p
	return o
}

// SetSort picks which match is acted on.
func (o *FindOneAndUpdateOptions) SetSort(s any) *FindOneAndUpdateOptions { o.Sort = s; return o }

// SetUpsert inserts when nothing matches.
func (o *FindOneAndUpdateOptions) SetUpsert(b bool) *FindOneAndUpdateOptions { o.Upsert = &b; return o }

// SetReturnDocument selects the before or after version.
func (o *FindOneAndUpdateOptions) SetReturnDocument(r ReturnDocument) *FindOneAndUpdateOptions {
	o.ReturnDocument = &r
	return o
}

// SetArrayFilters identifies array elements for positional updates.
func (o *FindOneAndUpdateOptions) SetArrayFilters(f ArrayFilters) *FindOneAndUpdateOptions {
	o.ArrayFilters = &f
	return o
}

// SetHint forces an index.
func (o *FindOneAndUpdateOptions) SetHint(h any) *FindOneAndUpdateOptions { o.Hint = h; return o }

// SetCollation sets string comparison rules.
func (o *FindOneAndUpdateOptions) SetCollation(c *Collation) *FindOneAndUpdateOptions {
	o.Collation = c
	return o
}

// SetBypassDocumentValidation skips schema validation.
func (o *FindOneAndUpdateOptions) SetBypassDocumentValidation(b bool) *FindOneAndUpdateOptions {
	o.BypassDocumentValidation = &b
	return o
}

// SetComment attaches a comment.
func (o *FindOneAndUpdateOptions) SetComment(c any) *FindOneAndUpdateOptions { o.Comment = c; return o }

// FindOneAndReplaceOptions configures an atomic find-and-replace.
type FindOneAndReplaceOptions struct {
	Projection               any
	Sort                     any
	Upsert                   *bool
	ReturnDocument           *ReturnDocument
	Hint                     any
	Collation                *Collation
	BypassDocumentValidation *bool
	Comment                  any
}

// FindOneAndReplace returns a fresh FindOneAndReplaceOptions builder.
func FindOneAndReplace() *FindOneAndReplaceOptions { return &FindOneAndReplaceOptions{} }

// SetProjection shapes the returned document.
func (o *FindOneAndReplaceOptions) SetProjection(p any) *FindOneAndReplaceOptions {
	o.Projection = p
	return o
}

// SetSort picks which match is acted on.
func (o *FindOneAndReplaceOptions) SetSort(s any) *FindOneAndReplaceOptions { o.Sort = s; return o }

// SetUpsert inserts when nothing matches.
func (o *FindOneAndReplaceOptions) SetUpsert(b bool) *FindOneAndReplaceOptions {
	o.Upsert = &b
	return o
}

// SetReturnDocument selects the before or after version.
func (o *FindOneAndReplaceOptions) SetReturnDocument(r ReturnDocument) *FindOneAndReplaceOptions {
	o.ReturnDocument = &r
	return o
}

// SetHint forces an index.
func (o *FindOneAndReplaceOptions) SetHint(h any) *FindOneAndReplaceOptions { o.Hint = h; return o }

// SetCollation sets string comparison rules.
func (o *FindOneAndReplaceOptions) SetCollation(c *Collation) *FindOneAndReplaceOptions {
	o.Collation = c
	return o
}

// SetBypassDocumentValidation skips schema validation.
func (o *FindOneAndReplaceOptions) SetBypassDocumentValidation(b bool) *FindOneAndReplaceOptions {
	o.BypassDocumentValidation = &b
	return o
}

// SetComment attaches a comment.
func (o *FindOneAndReplaceOptions) SetComment(c any) *FindOneAndReplaceOptions {
	o.Comment = c
	return o
}

// FindOneAndDeleteOptions configures an atomic find-and-delete.
type FindOneAndDeleteOptions struct {
	Projection any
	Sort       any
	Hint       any
	Collation  *Collation
	Comment    any
}

// FindOneAndDelete returns a fresh FindOneAndDeleteOptions builder.
func FindOneAndDelete() *FindOneAndDeleteOptions { return &FindOneAndDeleteOptions{} }

// SetProjection shapes the returned document.
func (o *FindOneAndDeleteOptions) SetProjection(p any) *FindOneAndDeleteOptions {
	o.Projection = p
	return o
}

// SetSort picks which match is deleted.
func (o *FindOneAndDeleteOptions) SetSort(s any) *FindOneAndDeleteOptions { o.Sort = s; return o }

// SetHint forces an index.
func (o *FindOneAndDeleteOptions) SetHint(h any) *FindOneAndDeleteOptions { o.Hint = h; return o }

// SetCollation sets string comparison rules.
func (o *FindOneAndDeleteOptions) SetCollation(c *Collation) *FindOneAndDeleteOptions {
	o.Collation = c
	return o
}

// SetComment attaches a comment.
func (o *FindOneAndDeleteOptions) SetComment(c any) *FindOneAndDeleteOptions { o.Comment = c; return o }

// BulkWriteOptions configures a bulkWrite batch.
type BulkWriteOptions struct {
	Ordered                  *bool
	BypassDocumentValidation *bool
	Comment                  any
}

// BulkWrite returns a fresh BulkWriteOptions builder. The default is ordered.
func BulkWrite() *BulkWriteOptions { return &BulkWriteOptions{} }

// SetOrdered selects ordered or unordered execution.
func (o *BulkWriteOptions) SetOrdered(b bool) *BulkWriteOptions { o.Ordered = &b; return o }

// SetBypassDocumentValidation skips schema validation.
func (o *BulkWriteOptions) SetBypassDocumentValidation(b bool) *BulkWriteOptions {
	o.BypassDocumentValidation = &b
	return o
}

// SetComment attaches a comment.
func (o *BulkWriteOptions) SetComment(c any) *BulkWriteOptions { o.Comment = c; return o }

// AggregateOptions configures an aggregation.
type AggregateOptions struct {
	AllowDiskUse             *bool
	BatchSize                *int32
	BypassDocumentValidation *bool
	Collation                *Collation
	MaxTime                  *time.Duration
	Hint                     any
	Comment                  any
}

// Aggregate returns a fresh AggregateOptions builder.
func Aggregate() *AggregateOptions { return &AggregateOptions{} }

// SetAllowDiskUse permits spilling large pipeline stages to disk.
func (o *AggregateOptions) SetAllowDiskUse(b bool) *AggregateOptions { o.AllowDiskUse = &b; return o }

// SetBatchSize sets the cursor batch size.
func (o *AggregateOptions) SetBatchSize(n int32) *AggregateOptions { o.BatchSize = &n; return o }

// SetBypassDocumentValidation skips validation on a $out or $merge stage.
func (o *AggregateOptions) SetBypassDocumentValidation(b bool) *AggregateOptions {
	o.BypassDocumentValidation = &b
	return o
}

// SetCollation sets string comparison rules.
func (o *AggregateOptions) SetCollation(c *Collation) *AggregateOptions { o.Collation = c; return o }

// SetMaxTime sets a server-side time limit.
func (o *AggregateOptions) SetMaxTime(d time.Duration) *AggregateOptions { o.MaxTime = &d; return o }

// SetHint forces an index for the first $match stage.
func (o *AggregateOptions) SetHint(h any) *AggregateOptions { o.Hint = h; return o }

// SetComment attaches a comment.
func (o *AggregateOptions) SetComment(c any) *AggregateOptions { o.Comment = c; return o }

// CountOptions configures CountDocuments.
type CountOptions struct {
	Limit     *int64
	Skip      *int64
	Hint      any
	Collation *Collation
	MaxTime   *time.Duration
	Comment   any
}

// Count returns a fresh CountOptions builder.
func Count() *CountOptions { return &CountOptions{} }

// SetLimit caps the count.
func (o *CountOptions) SetLimit(n int64) *CountOptions { o.Limit = &n; return o }

// SetSkip skips the first n matches before counting.
func (o *CountOptions) SetSkip(n int64) *CountOptions { o.Skip = &n; return o }

// SetHint forces an index.
func (o *CountOptions) SetHint(h any) *CountOptions { o.Hint = h; return o }

// SetCollation sets string comparison rules.
func (o *CountOptions) SetCollation(c *Collation) *CountOptions { o.Collation = c; return o }

// SetMaxTime sets a server-side time limit.
func (o *CountOptions) SetMaxTime(d time.Duration) *CountOptions { o.MaxTime = &d; return o }

// SetComment attaches a comment.
func (o *CountOptions) SetComment(c any) *CountOptions { o.Comment = c; return o }

// EstimatedDocumentCountOptions configures EstimatedDocumentCount.
type EstimatedDocumentCountOptions struct {
	MaxTime *time.Duration
	Comment any
}

// EstimatedDocumentCount returns a fresh builder.
func EstimatedDocumentCount() *EstimatedDocumentCountOptions {
	return &EstimatedDocumentCountOptions{}
}

// SetMaxTime sets a server-side time limit.
func (o *EstimatedDocumentCountOptions) SetMaxTime(d time.Duration) *EstimatedDocumentCountOptions {
	o.MaxTime = &d
	return o
}

// SetComment attaches a comment.
func (o *EstimatedDocumentCountOptions) SetComment(c any) *EstimatedDocumentCountOptions {
	o.Comment = c
	return o
}

// DistinctOptions configures Distinct.
type DistinctOptions struct {
	Collation *Collation
	MaxTime   *time.Duration
	Comment   any
}

// Distinct returns a fresh DistinctOptions builder.
func Distinct() *DistinctOptions { return &DistinctOptions{} }

// SetCollation sets string comparison rules.
func (o *DistinctOptions) SetCollation(c *Collation) *DistinctOptions { o.Collation = c; return o }

// SetMaxTime sets a server-side time limit.
func (o *DistinctOptions) SetMaxTime(d time.Duration) *DistinctOptions { o.MaxTime = &d; return o }

// SetComment attaches a comment.
func (o *DistinctOptions) SetComment(c any) *DistinctOptions { o.Comment = c; return o }

// IndexOptions configures a single index.
type IndexOptions struct {
	Name                    *string
	Unique                  *bool
	Sparse                  *bool
	Background              *bool
	ExpireAfterSeconds      *int32
	PartialFilterExpression any
	Collation               *Collation
	Weights                 any
	DefaultLanguage         *string
	LanguageOverride        *string
	WildcardProjection      any
	Hidden                  *bool
	Version                 *int32
}

// Index returns a fresh IndexOptions builder.
func Index() *IndexOptions { return &IndexOptions{} }

// SetName sets an explicit index name.
func (o *IndexOptions) SetName(n string) *IndexOptions { o.Name = &n; return o }

// SetUnique enforces uniqueness of the indexed key.
func (o *IndexOptions) SetUnique(b bool) *IndexOptions { o.Unique = &b; return o }

// SetSparse indexes only documents where the key exists.
func (o *IndexOptions) SetSparse(b bool) *IndexOptions { o.Sparse = &b; return o }

// SetBackground is accepted for compatibility; builds are always foreground.
func (o *IndexOptions) SetBackground(b bool) *IndexOptions { o.Background = &b; return o }

// SetExpireAfterSeconds turns the index into a TTL index.
func (o *IndexOptions) SetExpireAfterSeconds(n int32) *IndexOptions {
	o.ExpireAfterSeconds = &n
	return o
}

// SetPartialFilterExpression indexes only documents matching the expression.
func (o *IndexOptions) SetPartialFilterExpression(f any) *IndexOptions {
	o.PartialFilterExpression = f
	return o
}

// SetCollation sets string comparison rules for the index.
func (o *IndexOptions) SetCollation(c *Collation) *IndexOptions { o.Collation = c; return o }

// SetWeights sets text-index field weights.
func (o *IndexOptions) SetWeights(w any) *IndexOptions { o.Weights = w; return o }

// SetDefaultLanguage sets the default text-index language.
func (o *IndexOptions) SetDefaultLanguage(l string) *IndexOptions { o.DefaultLanguage = &l; return o }

// SetLanguageOverride sets the field naming a per-document language.
func (o *IndexOptions) SetLanguageOverride(l string) *IndexOptions {
	o.LanguageOverride = &l
	return o
}

// SetWildcardProjection restricts a wildcard index to a subset of fields.
func (o *IndexOptions) SetWildcardProjection(p any) *IndexOptions {
	o.WildcardProjection = p
	return o
}

// SetHidden keeps the index maintained but invisible to the planner.
func (o *IndexOptions) SetHidden(b bool) *IndexOptions { o.Hidden = &b; return o }

// SetVersion sets the index format version.
func (o *IndexOptions) SetVersion(v int32) *IndexOptions { o.Version = &v; return o }

// CreateIndexesOptions configures a CreateOne or CreateMany call.
type CreateIndexesOptions struct {
	MaxTime         *time.Duration
	Comment         any
	CommitQuorumInt *int
}

// CreateIndexes returns a fresh builder.
func CreateIndexes() *CreateIndexesOptions { return &CreateIndexesOptions{} }

// SetMaxTime sets a server-side time limit for the build.
func (o *CreateIndexesOptions) SetMaxTime(d time.Duration) *CreateIndexesOptions {
	o.MaxTime = &d
	return o
}

// SetComment attaches a comment.
func (o *CreateIndexesOptions) SetComment(c any) *CreateIndexesOptions { o.Comment = c; return o }

// ListIndexesOptions configures listing of a collection's indexes.
type ListIndexesOptions struct {
	BatchSize *int32
	MaxTime   *time.Duration
}

// ListIndexes returns a fresh builder.
func ListIndexes() *ListIndexesOptions { return &ListIndexesOptions{} }

// SetBatchSize sets the cursor batch size.
func (o *ListIndexesOptions) SetBatchSize(n int32) *ListIndexesOptions { o.BatchSize = &n; return o }

// SetMaxTime sets a server-side time limit.
func (o *ListIndexesOptions) SetMaxTime(d time.Duration) *ListIndexesOptions {
	o.MaxTime = &d
	return o
}

// TimeSeriesOptions configures a time-series collection.
type TimeSeriesOptions struct {
	TimeField   string
	MetaField   *string
	Granularity *string
}

// TimeSeries returns a fresh TimeSeriesOptions builder.
func TimeSeries() *TimeSeriesOptions { return &TimeSeriesOptions{} }

// SetTimeField names the required time field.
func (o *TimeSeriesOptions) SetTimeField(f string) *TimeSeriesOptions { o.TimeField = f; return o }

// SetMetaField names the optional metadata field.
func (o *TimeSeriesOptions) SetMetaField(f string) *TimeSeriesOptions { o.MetaField = &f; return o }

// SetGranularity sets the bucketing granularity.
func (o *TimeSeriesOptions) SetGranularity(g string) *TimeSeriesOptions { o.Granularity = &g; return o }

// CreateCollectionOptions configures an explicit CreateCollection.
type CreateCollectionOptions struct {
	Capped             *bool
	SizeInBytes        *int64
	MaxDocuments       *int64
	Validator          any
	ValidationLevel    *string
	ValidationAction   *string
	Collation          *Collation
	TimeSeries         *TimeSeriesOptions
	ExpireAfterSeconds *int64
}

// CreateCollection returns a fresh builder.
func CreateCollection() *CreateCollectionOptions { return &CreateCollectionOptions{} }

// SetCapped makes the collection a fixed-size ring buffer.
func (o *CreateCollectionOptions) SetCapped(b bool) *CreateCollectionOptions { o.Capped = &b; return o }

// SetSizeInBytes caps a capped collection's total size.
func (o *CreateCollectionOptions) SetSizeInBytes(n int64) *CreateCollectionOptions {
	o.SizeInBytes = &n
	return o
}

// SetMaxDocuments caps a capped collection's document count.
func (o *CreateCollectionOptions) SetMaxDocuments(n int64) *CreateCollectionOptions {
	o.MaxDocuments = &n
	return o
}

// SetValidator sets a $jsonSchema or query validator.
func (o *CreateCollectionOptions) SetValidator(v any) *CreateCollectionOptions {
	o.Validator = v
	return o
}

// SetValidationLevel sets strict or moderate validation.
func (o *CreateCollectionOptions) SetValidationLevel(l string) *CreateCollectionOptions {
	o.ValidationLevel = &l
	return o
}

// SetValidationAction sets error or warn on validation failure.
func (o *CreateCollectionOptions) SetValidationAction(a string) *CreateCollectionOptions {
	o.ValidationAction = &a
	return o
}

// SetCollation sets the collection's default collation.
func (o *CreateCollectionOptions) SetCollation(c *Collation) *CreateCollectionOptions {
	o.Collation = c
	return o
}

// SetTimeSeriesOptions configures a time-series collection.
func (o *CreateCollectionOptions) SetTimeSeriesOptions(t *TimeSeriesOptions) *CreateCollectionOptions {
	o.TimeSeries = t
	return o
}

// SetExpireAfterSeconds sets a collection-level TTL for time-series data.
func (o *CreateCollectionOptions) SetExpireAfterSeconds(n int64) *CreateCollectionOptions {
	o.ExpireAfterSeconds = &n
	return o
}

// RunCmdOptions configures RunCommand.
type RunCmdOptions struct {
	ReadPreference any
}

// RunCmd returns a fresh builder.
func RunCmd() *RunCmdOptions { return &RunCmdOptions{} }

// SetReadPreference sets the read preference for the command.
func (o *RunCmdOptions) SetReadPreference(p any) *RunCmdOptions { o.ReadPreference = p; return o }

// SessionOptions configures a client session.
type SessionOptions struct {
	CausalConsistency     *bool
	DefaultReadConcern    any
	DefaultWriteConcern   any
	DefaultReadPreference any
	Snapshot              *bool
}

// Session returns a fresh SessionOptions builder.
func Session() *SessionOptions { return &SessionOptions{} }

// SetCausalConsistency toggles causal consistency for the session.
func (o *SessionOptions) SetCausalConsistency(b bool) *SessionOptions {
	o.CausalConsistency = &b
	return o
}

// SetDefaultReadConcern sets the session default read concern.
func (o *SessionOptions) SetDefaultReadConcern(c any) *SessionOptions {
	o.DefaultReadConcern = c
	return o
}

// SetDefaultWriteConcern sets the session default write concern.
func (o *SessionOptions) SetDefaultWriteConcern(c any) *SessionOptions {
	o.DefaultWriteConcern = c
	return o
}

// SetDefaultReadPreference sets the session default read preference.
func (o *SessionOptions) SetDefaultReadPreference(p any) *SessionOptions {
	o.DefaultReadPreference = p
	return o
}

// SetSnapshot toggles snapshot reads for the session.
func (o *SessionOptions) SetSnapshot(b bool) *SessionOptions { o.Snapshot = &b; return o }

// TransactionOptions configures a transaction's concerns and timing.
type TransactionOptions struct {
	ReadConcern    any
	WriteConcern   any
	ReadPreference any
	MaxCommitTime  *time.Duration
	MaxRetries     *int
}

// Transaction returns a fresh TransactionOptions builder.
func Transaction() *TransactionOptions { return &TransactionOptions{} }

// SetReadConcern sets the transaction read concern.
func (o *TransactionOptions) SetReadConcern(c any) *TransactionOptions { o.ReadConcern = c; return o }

// SetWriteConcern sets the transaction write concern.
func (o *TransactionOptions) SetWriteConcern(c any) *TransactionOptions { o.WriteConcern = c; return o }

// SetReadPreference sets the transaction read preference.
func (o *TransactionOptions) SetReadPreference(p any) *TransactionOptions {
	o.ReadPreference = p
	return o
}

// SetMaxCommitTime caps how long the commit may take.
func (o *TransactionOptions) SetMaxCommitTime(d time.Duration) *TransactionOptions {
	o.MaxCommitTime = &d
	return o
}

// SetMaxRetries bounds the WithTransaction retry loop.
func (o *TransactionOptions) SetMaxRetries(n int) *TransactionOptions { o.MaxRetries = &n; return o }

// LoadOptions configures the bulk loader.
type LoadOptions struct {
	BatchSize                *int
	Ordered                  *bool
	DropIndexesDuringLoad    *bool
	BypassDocumentValidation *bool
}

// Load returns a fresh LoadOptions builder.
func Load() *LoadOptions { return &LoadOptions{} }

// SetBatchSize sets documents per write batch.
func (o *LoadOptions) SetBatchSize(n int) *LoadOptions { o.BatchSize = &n; return o }

// SetOrdered continues on individual document error when false.
func (o *LoadOptions) SetOrdered(b bool) *LoadOptions { o.Ordered = &b; return o }

// SetDropIndexesDuringLoad drops secondary indexes for the load and rebuilds after.
func (o *LoadOptions) SetDropIndexesDuringLoad(b bool) *LoadOptions {
	o.DropIndexesDuringLoad = &b
	return o
}

// SetBypassDocumentValidation skips schema validation during load.
func (o *LoadOptions) SetBypassDocumentValidation(b bool) *LoadOptions {
	o.BypassDocumentValidation = &b
	return o
}
