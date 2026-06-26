package doc

import (
	"context"
	"strings"
	"testing"
)

func TestMetricsOpCounters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	coll := db.Database("app").Collection("users")

	for i := 0; i < 5; i++ {
		if _, err := coll.InsertOne(ctx, map[string]any{"i": i}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if _, err := coll.Find(ctx, map[string]any{}); err != nil {
		t.Fatalf("find: %v", err)
	}

	v := db.met.opsTotal.With("insert", "app.users").Value()
	if v != 5 {
		t.Fatalf("insert ops = %d, want 5", v)
	}
	if got := db.met.opsTotal.With("find", "app.users").Value(); got != 1 {
		t.Fatalf("find ops = %d, want 1", got)
	}
	if db.met.totalOps.Value() < 6 {
		t.Fatalf("totalOps = %d, want >= 6", db.met.totalOps.Value())
	}
}

func TestMetricsDocsReturned(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	coll := db.Database("app").Collection("nums")
	for i := 0; i < 3; i++ {
		if _, err := coll.InsertOne(ctx, map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	cur, err := coll.Find(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	_ = cur.All(ctx, &[]map[string]any{})

	if got := db.met.docsReturned.With("find", "app.nums").Value(); got != 3 {
		t.Fatalf("docsReturned = %d, want 3", got)
	}
}

func TestMetricsSnapshotReflectsStorage(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	coll := db.Database("app").Collection("c")
	for i := 0; i < 50; i++ {
		if _, err := coll.InsertOne(ctx, map[string]any{"i": i, "pad": strings.Repeat("x", 200)}); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := db.Metrics(ctx)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if snap.OpsTotal < 50 {
		t.Fatalf("OpsTotal = %d, want >= 50", snap.OpsTotal)
	}
	if snap.DocumentCount != 50 {
		t.Fatalf("DocumentCount = %d, want 50", snap.DocumentCount)
	}
	if snap.Collections < 1 {
		t.Fatalf("Collections = %d, want >= 1", snap.Collections)
	}
	if snap.FileSizeBytes <= 0 {
		t.Fatalf("FileSizeBytes = %d, want > 0", snap.FileSizeBytes)
	}
}

func TestMetricsPrometheusOutput(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	coll := db.Database("app").Collection("c")
	if _, err := coll.InsertOne(ctx, map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := db.WritePrometheus(ctx, &b); err != nil {
		t.Fatalf("write prometheus: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"# TYPE doc_ops_total counter",
		`doc_ops_total{op="insert",collection="app.c"} 1`,
		"# TYPE doc_op_duration_seconds histogram",
		"# TYPE doc_collection_count gauge",
		"# TYPE doc_file_size_bytes gauge",
		"# TYPE doc_page_writes_total counter",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("prometheus output missing %q\n%s", want, out)
		}
	}
}

func TestMetricsAfterClose(t *testing.T) {
	db, err := Open(memoryPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	// RefreshMetrics must be a no-op, not a panic, after close.
	db.RefreshMetrics()
	if _, err := db.Metrics(context.Background()); err == nil {
		t.Fatal("Metrics on a closed db should error")
	}
}

func TestServerStatusCarriesDocMetrics(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	d := db.Database("app")
	for i := 0; i < 4; i++ {
		if _, err := d.Collection("c").InsertOne(ctx, map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}

	var res map[string]any
	if err := d.RunCommand(ctx, map[string]any{"serverStatus": 1}).Decode(&res); err != nil {
		t.Fatalf("serverStatus: %v", err)
	}
	if res["process"] != "doc" {
		t.Fatalf("process = %v, want doc", res["process"])
	}
	sub, ok := res["doc"].(M)
	if !ok {
		t.Fatalf("doc sub-document missing or wrong type: %T", res["doc"])
	}
	ops, _ := sub["opsTotal"].(int64)
	if ops < 4 {
		t.Fatalf("doc.opsTotal = %v, want >= 4", sub["opsTotal"])
	}
	dc, _ := sub["documentCount"].(int64)
	if dc != 4 {
		t.Fatalf("doc.documentCount = %v, want 4", sub["documentCount"])
	}
}

func TestMetricsChangefeedCounter(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	coll := db.Database("app").Collection("c")
	if _, err := coll.InsertOne(ctx, map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if got := db.met.changefeedEvt.With("insert").Value(); got != 1 {
		t.Fatalf("changefeed insert events = %d, want 1", got)
	}
}
