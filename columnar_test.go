package doc

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tamnd/doc/options"
)

// seedSales inserts a deterministic spread of {region, units} documents and returns
// the per-region unit totals the aggregation must reproduce.
func seedSales(ctx context.Context, t *testing.T, c *Collection, n int) map[string]int64 {
	t.Helper()
	regions := []string{"north", "south", "east", "west"}
	want := map[string]int64{}
	docs := make([]any, n)
	for i := 0; i < n; i++ {
		r := regions[i%len(regions)]
		u := int64(i % 13)
		docs[i] = M{"region": r, "units": u}
		want[r] += u
	}
	if _, err := c.InsertMany(ctx, docs); err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	return want
}

// regionTotals runs {$group: {_id: "$region", total: {$sum: "$units"}}} and returns
// the region->total map it produced.
func regionTotals(ctx context.Context, t *testing.T, c *Collection) map[string]int64 {
	t.Helper()
	pipeline := []M{{"$group": M{"_id": "$region", "total": M{"$sum": "$units"}}}}
	cur, err := c.Aggregate(ctx, pipeline)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	var rows []struct {
		ID    string `bson:"_id"`
		Total int64  `bson:"total"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		t.Fatalf("cursor.All: %v", err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.ID] = r.Total
	}
	return got
}

func sameTotals(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestCreateCollectionColumnarStore drives the columnar projection store through the
// public CreateCollection option surface (spec 2061 doc 19 §21.4): a collection
// created with columnar_store transactional answers a covered $group with the same
// totals a heap collection would, and the answer keeps tracking the heap as rows are
// inserted after creation.
func TestCreateCollectionColumnarStore(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")

	opt := options.CreateCollection().
		SetColumnarStore("transactional").
		SetColumnarFields([]string{"region", "units"})
	if err := d.CreateCollection(ctx, "sales", opt); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	c := d.Collection("sales")

	want := seedSales(ctx, t, c, 400)
	if got := regionTotals(ctx, t, c); !sameTotals(want, got) {
		t.Fatalf("columnar totals differ\n want = %v\n got  = %v", want, got)
	}

	// More rows after enable must be maintained into the store synchronously.
	if _, err := c.InsertMany(ctx, []any{M{"region": "north", "units": int64(100)}}); err != nil {
		t.Fatalf("InsertMany follow-up: %v", err)
	}
	want["north"] += 100
	if got := regionTotals(ctx, t, c); !sameTotals(want, got) {
		t.Fatalf("columnar totals after insert differ\n want = %v\n got  = %v", want, got)
	}
}

// TestColumnarStorePersistsAcrossReopen verifies the columnar_store option is part of
// the persisted catalog record: a collection created with it, then closed and
// reopened from disk, rebuilds the store at open and still answers covered
// aggregations correctly (spec 2061 doc 09 §6).
func TestColumnarStorePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "columnar.doc")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d := db.Database("shop")
	opt := options.CreateCollection().SetColumnarStore("transactional")
	if err := d.CreateCollection(ctx, "sales", opt); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	want := seedSales(ctx, t, d.Collection("sales"), 300)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	c := db2.Database("shop").Collection("sales")
	if got := regionTotals(ctx, t, c); !sameTotals(want, got) {
		t.Fatalf("columnar totals after reopen differ\n want = %v\n got  = %v", want, got)
	}
}
