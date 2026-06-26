package doc

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/collection"
	"github.com/tamnd/doc/extjson"
	"github.com/tamnd/doc/options"
)

// DocumentIterator is any source of documents the loader can drain: a parsed JSON
// stream, a raw BSON stream, or a caller-supplied adapter over a CSV file, an ORM
// result, or an external cursor (spec 2061 doc 14 §19.3). Next advances and reports
// whether a document is ready; Document returns the current one; Err reports a
// terminal read error; Close releases the source.
type DocumentIterator interface {
	Next(ctx context.Context) bool
	Document() (any, error)
	Err() error
	Close() error
}

// LoadError records one document the loader could not insert, identified by its
// zero-based position in the source stream.
type LoadError struct {
	Index int64
	Err   error
}

// Error renders the position and cause.
func (e LoadError) Error() string { return fmt.Sprintf("document %d: %v", e.Index, e.Err) }

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e LoadError) Unwrap() error { return e.Err }

// LoadResult reports the outcome of a bulk load (spec 2061 doc 14 §19.5). Errors is
// populated only for an unordered load; an ordered load stops at the first failure
// and returns that error directly.
type LoadResult struct {
	InsertedCount int64
	FailedCount   int64
	Errors        []LoadError
	Duration      time.Duration
	BytesRead     int64
}

// loaderDefaults resolves the LoadOptions a load runs with. The defaults match the
// spec: a batch of 1000, unordered so one bad document does not sink the load, no
// index dropping, and validation on.
type loaderDefaults struct {
	batchSize int
	ordered   bool
	dropIdx   bool
	bypass    bool
}

func resolveLoad(opts []*options.LoadOptions) loaderDefaults {
	d := loaderDefaults{batchSize: 1000}
	for _, o := range opts {
		if o == nil {
			continue
		}
		if o.BatchSize != nil && *o.BatchSize > 0 {
			d.batchSize = *o.BatchSize
		}
		if o.Ordered != nil {
			d.ordered = *o.Ordered
		}
		if o.DropIndexesDuringLoad != nil {
			d.dropIdx = *o.DropIndexesDuringLoad
		}
		if o.BypassDocumentValidation != nil {
			d.bypass = *o.BypassDocumentValidation
		}
	}
	return d
}

// LoadJSON reads a newline-delimited JSON (NDJSON) stream and inserts every line as
// a document. Each non-empty line must be one JSON object; blank lines are skipped.
// Extended JSON is accepted, so a line may carry typed values such as {"$oid":...}
// (spec 2061 doc 14 §19.1).
func (c *Collection) LoadJSON(ctx context.Context, r io.Reader, opts ...*options.LoadOptions) (*LoadResult, error) {
	cr := &countingReader{r: r}
	return c.runLoad(ctx, newJSONIterator(cr), cr, opts)
}

// LoadBSON reads a stream of concatenated raw BSON documents, the format mongodump
// writes, and inserts each one. No JSON parsing happens, so this is the fastest
// loader path (spec 2061 doc 14 §19.2).
func (c *Collection) LoadBSON(ctx context.Context, r io.Reader, opts ...*options.LoadOptions) (*LoadResult, error) {
	cr := &countingReader{r: r}
	return c.runLoad(ctx, newBSONIterator(cr), cr, opts)
}

// Import drains any DocumentIterator into the collection, the generic loader entry
// for a caller-supplied source (spec 2061 doc 14 §19.3). The BytesRead field of the
// result is zero for a generic import, since the iterator owns its own reader.
func (c *Collection) Import(ctx context.Context, iter DocumentIterator, opts ...*options.LoadOptions) (*LoadResult, error) {
	return c.runLoad(ctx, iter, nil, opts)
}

// runLoad is the shared engine behind LoadJSON, LoadBSON, and Import. It optionally
// drops secondary indexes up front, drains the iterator in batches, and rebuilds the
// indexes at the end. The counting reader, when present, supplies BytesRead.
func (c *Collection) runLoad(ctx context.Context, iter DocumentIterator, cr *countingReader, opts []*options.LoadOptions) (*LoadResult, error) {
	if err := c.db.check(ctx); err != nil {
		return nil, err
	}
	cfg := resolveLoad(opts)
	start := time.Now()
	defer func() { _ = iter.Close() }()

	var dropped []IndexModel
	if cfg.dropIdx {
		var err error
		dropped, err = c.dropSecondaryIndexes(ctx)
		if err != nil {
			return nil, err
		}
	}

	col, err := c.writable()
	if err != nil {
		return nil, err
	}

	res := &LoadResult{}
	batch := make([]bson.Raw, 0, cfg.batchSize)
	var index int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		start := index - int64(len(batch))
		r, bwErr := col.InsertManyBatch(batch, cfg.ordered, cfg.bypass)
		res.InsertedCount += int64(len(r.InsertedIDs))
		batch = batch[:0]
		if bwErr != nil {
			return c.recordBatchErr(res, bwErr, start, cfg.ordered)
		}
		return nil
	}

	for iter.Next(ctx) {
		dv, derr := iter.Document()
		if derr != nil {
			if cfg.ordered {
				return c.finishLoad(ctx, res, start, cr, dropped, derr)
			}
			res.FailedCount++
			res.Errors = append(res.Errors, LoadError{Index: index, Err: derr})
			index++
			continue
		}
		raw, merr := toDoc(dv)
		if merr != nil {
			if cfg.ordered {
				return c.finishLoad(ctx, res, start, cr, dropped, merr)
			}
			res.FailedCount++
			res.Errors = append(res.Errors, LoadError{Index: index, Err: merr})
			index++
			continue
		}
		batch = append(batch, raw)
		index++
		if len(batch) >= cfg.batchSize {
			if err := flush(); err != nil {
				return c.finishLoad(ctx, res, start, cr, dropped, err)
			}
		}
	}
	if err := flush(); err != nil {
		return c.finishLoad(ctx, res, start, cr, dropped, err)
	}
	if err := iter.Err(); err != nil {
		return c.finishLoad(ctx, res, start, cr, dropped, err)
	}
	return c.finishLoad(ctx, res, start, cr, dropped, nil)
}

// recordBatchErr folds a batch insert error into the result. For an unordered load
// a *BulkWriteException carries one entry per failed document, which becomes one
// LoadError each with its position translated from batch-relative to stream-absolute.
// For an ordered load the error stops the whole load and is returned.
func (c *Collection) recordBatchErr(res *LoadResult, bwErr error, batchStart int64, ordered bool) error {
	if ordered {
		return bwErr
	}
	var bwe *collection.BulkWriteException
	if errors.As(bwErr, &bwe) {
		for _, we := range bwe.WriteErrors {
			res.FailedCount++
			res.Errors = append(res.Errors, LoadError{Index: batchStart + int64(we.Index), Err: we.Err})
		}
		return nil
	}
	// An error the batch could not attribute to a single document (a commit
	// failure, say) ends the load even when unordered.
	return bwErr
}

// finishLoad rebuilds any dropped indexes, stamps the duration and bytes read, and
// returns the result. A rebuild error replaces a nil load error; it never masks a
// load error that already stopped the run.
func (c *Collection) finishLoad(ctx context.Context, res *LoadResult, start time.Time, cr *countingReader, dropped []IndexModel, loadErr error) (*LoadResult, error) {
	if len(dropped) > 0 {
		if _, err := c.Indexes().CreateMany(ctx, dropped); err != nil && loadErr == nil {
			loadErr = err
		}
	}
	res.Duration = time.Since(start)
	if cr != nil {
		res.BytesRead = cr.n
	}
	return res, loadErr
}

// dropSecondaryIndexes captures the model for every secondary index, drops them all,
// and returns the models so the load can rebuild them when it finishes. The _id index
// is primary and is never dropped.
func (c *Collection) dropSecondaryIndexes(ctx context.Context) ([]IndexModel, error) {
	specs, err := c.Indexes().ListSpecifications(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]IndexModel, 0, len(specs))
	for _, s := range specs {
		if s.Name == "_id_" {
			continue
		}
		io := options.Index().SetName(s.Name)
		if s.Unique {
			io.SetUnique(true)
		}
		if s.Sparse {
			io.SetSparse(true)
		}
		if s.ExpireAfterSeconds != nil {
			io.SetExpireAfterSeconds(*s.ExpireAfterSeconds)
		}
		models = append(models, IndexModel{Keys: s.KeysDocument, Options: io})
	}
	if len(models) == 0 {
		return nil, nil
	}
	if _, err := c.Indexes().DropAll(ctx); err != nil {
		return nil, err
	}
	return models, nil
}

// countingReader wraps a reader and tallies the bytes pulled through it, so a load
// can report how much of the source it consumed.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// jsonIterator turns an NDJSON stream into a DocumentIterator. Each non-empty line is
// parsed as Extended JSON when Document is called, so a malformed line surfaces as a
// per-document error the loader can skip rather than a fatal scan error.
type jsonIterator struct {
	sc   *bufio.Scanner
	line []byte
	err  error
}

func newJSONIterator(r io.Reader) *jsonIterator {
	sc := bufio.NewScanner(r)
	// NDJSON lines can be large; lift the token cap to 16 MiB, one BSON document.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return &jsonIterator{sc: sc}
}

func (it *jsonIterator) Next(context.Context) bool {
	for it.sc.Scan() {
		line := it.sc.Bytes()
		if len(trimSpace(line)) == 0 {
			continue
		}
		// Hold a copy: the scanner reuses its buffer on the next Scan.
		it.line = append(it.line[:0], line...)
		return true
	}
	it.err = it.sc.Err()
	return false
}

func (it *jsonIterator) Document() (any, error) {
	raw, err := extjson.Parse(it.line)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (it *jsonIterator) Err() error   { return it.err }
func (it *jsonIterator) Close() error { return nil }

// bsonIterator turns a concatenated raw-BSON stream into a DocumentIterator. It reads
// one document at a time: a four-byte little-endian length, then the rest of that many
// bytes, validating the framing as it goes.
type bsonIterator struct {
	r    io.Reader
	cur  bson.Raw
	err  error
	done bool
}

func newBSONIterator(r io.Reader) *bsonIterator { return &bsonIterator{r: r} }

func (it *bsonIterator) Next(context.Context) bool {
	if it.done {
		return false
	}
	var hdr [4]byte
	_, err := io.ReadFull(it.r, hdr[:])
	if err == io.EOF {
		it.done = true
		return false
	}
	if err != nil {
		it.err = errLoad("read bson length", err)
		it.done = true
		return false
	}
	size := int64(binary.LittleEndian.Uint32(hdr[:]))
	if size < 5 || size > 16*1024*1024 {
		it.err = fmt.Errorf("doc: bson document length %d out of range", size)
		it.done = true
		return false
	}
	buf := make([]byte, size)
	copy(buf, hdr[:])
	if _, err := io.ReadFull(it.r, buf[4:]); err != nil {
		it.err = errLoad("read bson body", err)
		it.done = true
		return false
	}
	raw := bson.Raw(buf)
	if verr := raw.Validate(); verr != nil {
		it.err = errLoad("validate bson document", verr)
		it.done = true
		return false
	}
	it.cur = raw
	return true
}

func (it *bsonIterator) Document() (any, error) { return it.cur, nil }
func (it *bsonIterator) Err() error             { return it.err }
func (it *bsonIterator) Close() error           { return nil }

func errLoad(what string, err error) error { return fmt.Errorf("doc: %s: %w", what, err) }

// trimSpace strips ASCII leading and trailing whitespace without allocating, used to
// recognize a blank NDJSON line.
func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isASCIISpace(b[i]) {
		i++
	}
	for j > i && isASCIISpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isASCIISpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
