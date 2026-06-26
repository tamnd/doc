package doc

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/options"
)

func TestLoadJSON(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")

	var sb strings.Builder
	for i := range 100 {
		fmt.Fprintf(&sb, "{\"_id\": %d, \"sku\": \"s%d\"}\n", i, i%7)
	}
	res, err := c.LoadJSON(ctx, strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}
	if res.InsertedCount != 100 {
		t.Fatalf("InsertedCount = %d, want 100", res.InsertedCount)
	}
	if res.FailedCount != 0 {
		t.Fatalf("FailedCount = %d, want 0", res.FailedCount)
	}
	if res.BytesRead == 0 {
		t.Fatal("BytesRead = 0, want > 0")
	}
	n, err := c.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("CountDocuments: %v", err)
	}
	if n != 100 {
		t.Fatalf("stored count = %d, want 100", n)
	}
}

func TestLoadJSONSkipsBlankLines(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")
	src := "{\"_id\":1}\n\n  \n{\"_id\":2}\n"
	res, err := c.LoadJSON(ctx, strings.NewReader(src))
	if err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}
	if res.InsertedCount != 2 {
		t.Fatalf("InsertedCount = %d, want 2", res.InsertedCount)
	}
}

func TestLoadJSONUnorderedSkipsDuplicates(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")
	// Two documents share _id 1, so the second is a duplicate-key failure the
	// unordered load skips and records.
	src := "{\"_id\":1}\n{\"_id\":2}\n{\"_id\":1}\n{\"_id\":3}\n"
	res, err := c.LoadJSON(ctx, strings.NewReader(src), options.Load().SetBatchSize(10))
	if err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}
	if res.InsertedCount != 3 {
		t.Fatalf("InsertedCount = %d, want 3", res.InsertedCount)
	}
	if res.FailedCount != 1 {
		t.Fatalf("FailedCount = %d, want 1", res.FailedCount)
	}
	if len(res.Errors) != 1 || res.Errors[0].Index != 2 {
		t.Fatalf("Errors = %+v, want one at stream index 2", res.Errors)
	}
}

func TestLoadJSONOrderedStopsOnError(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")
	src := "{\"_id\":1}\n{\"_id\":1}\n{\"_id\":2}\n"
	_, err := c.LoadJSON(ctx, strings.NewReader(src), options.Load().SetOrdered(true).SetBatchSize(1))
	if err == nil {
		t.Fatal("ordered load with a duplicate should return an error")
	}
}

func TestLoadJSONBadLineUnordered(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")
	src := "{\"_id\":1}\n{not json}\n{\"_id\":2}\n"
	res, err := c.LoadJSON(ctx, strings.NewReader(src))
	if err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}
	if res.InsertedCount != 2 {
		t.Fatalf("InsertedCount = %d, want 2", res.InsertedCount)
	}
	if res.FailedCount != 1 {
		t.Fatalf("FailedCount = %d, want 1", res.FailedCount)
	}
}

func TestLoadBSON(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")

	var buf bytes.Buffer
	for i := range 50 {
		raw := bson.NewBuilder().AppendInt32("_id", int32(i)).AppendString("v", "x").Build()
		buf.Write(raw)
	}
	res, err := c.LoadBSON(ctx, &buf)
	if err != nil {
		t.Fatalf("LoadBSON: %v", err)
	}
	if res.InsertedCount != 50 {
		t.Fatalf("InsertedCount = %d, want 50", res.InsertedCount)
	}
	n, _ := c.CountDocuments(ctx, M{})
	if n != 50 {
		t.Fatalf("stored count = %d, want 50", n)
	}
}

func TestLoadBSONTruncatedStream(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")
	raw := bson.NewBuilder().AppendInt32("_id", 1).Build()
	// One whole document then a torn header: the loader inserts the good one and
	// reports the read error.
	buf := append([]byte{}, raw...)
	buf = append(buf, 0x10, 0x00) // two bytes of a four-byte length
	res, err := c.LoadBSON(ctx, bytes.NewReader(buf))
	if err == nil {
		t.Fatal("a truncated bson stream should return an error")
	}
	if res.InsertedCount != 1 {
		t.Fatalf("InsertedCount = %d, want 1", res.InsertedCount)
	}
}

// sliceIterator is a DocumentIterator over an in-memory slice, the kind of adapter a
// caller writes for a CSV file or an external cursor.
type sliceIterator struct {
	docs []any
	i    int
}

func (s *sliceIterator) Next(context.Context) bool {
	if s.i >= len(s.docs) {
		return false
	}
	s.i++
	return true
}
func (s *sliceIterator) Document() (any, error) { return s.docs[s.i-1], nil }
func (s *sliceIterator) Err() error             { return nil }
func (s *sliceIterator) Close() error           { return nil }

func TestImportGenericIterator(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("people")
	it := &sliceIterator{docs: []any{
		M{"_id": 1, "name": "a"},
		M{"_id": 2, "name": "b"},
		D{{"_id", 3}, {"name", "c"}},
	}}
	res, err := c.Import(ctx, it, options.Load().SetBatchSize(2))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.InsertedCount != 3 {
		t.Fatalf("InsertedCount = %d, want 3", res.InsertedCount)
	}
}

func TestLoadDropIndexesDuringLoad(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")
	if _, err := c.Indexes().CreateOne(ctx, IndexModel{Keys: M{"sku": 1}}); err != nil {
		t.Fatalf("CreateOne: %v", err)
	}

	var sb strings.Builder
	for i := range 200 {
		fmt.Fprintf(&sb, "{\"_id\": %d, \"sku\": \"s%d\"}\n", i, i)
	}
	res, err := c.LoadJSON(ctx, strings.NewReader(sb.String()),
		options.Load().SetDropIndexesDuringLoad(true).SetBatchSize(50))
	if err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}
	if res.InsertedCount != 200 {
		t.Fatalf("InsertedCount = %d, want 200", res.InsertedCount)
	}
	// The secondary index is back after the load, and it serves a lookup.
	specs, err := c.Indexes().ListSpecifications(ctx)
	if err != nil {
		t.Fatalf("ListSpecifications: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("index count = %d, want 2 (_id_ + sku_1)", len(specs))
	}
	n, err := c.CountDocuments(ctx, M{"sku": "s42"})
	if err != nil {
		t.Fatalf("CountDocuments: %v", err)
	}
	if n != 1 {
		t.Fatalf("count by rebuilt index = %d, want 1", n)
	}
}

func TestLoadBypassValidation(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")
	// A validator that requires an email field.
	if err := d.CreateCollection(ctx, "users",
		options.CreateCollection().SetValidator(M{"email": M{"$exists": true}})); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	c := d.Collection("users")
	src := "{\"_id\":1}\n{\"_id\":2}\n"
	// Without bypass the documents fail the validator.
	res, err := c.LoadJSON(ctx, strings.NewReader(src))
	if err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}
	if res.InsertedCount != 0 || res.FailedCount != 2 {
		t.Fatalf("without bypass: inserted=%d failed=%d, want 0 and 2", res.InsertedCount, res.FailedCount)
	}
	// With bypass they go in.
	res, err = c.LoadJSON(ctx, strings.NewReader(src), options.Load().SetBypassDocumentValidation(true))
	if err != nil {
		t.Fatalf("LoadJSON bypass: %v", err)
	}
	if res.InsertedCount != 2 {
		t.Fatalf("with bypass: InsertedCount = %d, want 2", res.InsertedCount)
	}
}

func BenchmarkLoadJSON(b *testing.B) {
	ctx := context.Background()
	var sb strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&sb, "{\"_id\": %d, \"v\": %d}\n", i, i)
	}
	payload := sb.String()
	b.ReportAllocs()
	for b.Loop() {
		db, err := Open(memoryPath)
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		c := db.Database("shop").Collection("orders")
		if _, err := c.LoadJSON(ctx, strings.NewReader(payload)); err != nil {
			b.Fatalf("LoadJSON: %v", err)
		}
		_ = db.Close()
	}
}
