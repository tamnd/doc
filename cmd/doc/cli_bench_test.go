package main

import (
	"context"
	"io"
	"testing"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
)

// benchApp builds an app on a fresh in-memory database, rendering to io.Discard so a
// benchmark measures the CLI path rather than terminal output.
func benchApp(b *testing.B) *app {
	b.Helper()
	cfg := &config{
		db:         "default",
		cacheBytes: 64 << 20,
		sync:       doc.SyncOff,
		mode:       modeJSONL,
	}
	a, err := newApp(cfg, io.Discard)
	if err != nil {
		b.Fatalf("newApp: %v", err)
	}
	return a
}

func BenchmarkParseHelper(b *testing.B) {
	line := `db.users.find({"age":{"$gt":21}}).sort({"name":1}).limit(10)`
	b.ReportAllocs()
	for b.Loop() {
		if _, ok, err := parseHelper(line); err != nil || !ok {
			b.Fatalf("parseHelper: ok=%v err=%v", ok, err)
		}
	}
}

func BenchmarkDispatchInsert(b *testing.B) {
	a := benchApp(b)
	defer a.close()
	line := `db.c.insertOne({"name":"sample","value":42,"tags":["x","y"]})`
	b.ReportAllocs()
	for b.Loop() {
		if err := a.dispatch(line); err != nil {
			b.Fatalf("dispatch insert: %v", err)
		}
	}
}

func BenchmarkDispatchFind(b *testing.B) {
	a := benchApp(b)
	defer a.close()
	coll := a.collection("c")
	for i := range 1000 {
		doc := bson.NewBuilder().
			AppendInt32("_id", int32(i)).
			AppendString("name", "row").
			AppendInt32("value", int32(i*2)).
			Build()
		if _, err := coll.InsertOne(context.Background(), doc); err != nil {
			b.Fatalf("seed insert: %v", err)
		}
	}
	line := `db.c.find({"value":{"$gte":0}})`
	b.ReportAllocs()
	for b.Loop() {
		if err := a.dispatch(line); err != nil {
			b.Fatalf("dispatch find: %v", err)
		}
	}
}

func BenchmarkRenderDocs(b *testing.B) {
	a := benchApp(b)
	defer a.close()
	docs := make([]bson.Raw, 0, 200)
	for i := range 200 {
		docs = append(docs, bson.NewBuilder().
			AppendInt32("_id", int32(i)).
			AppendString("name", "benchmark row").
			AppendDouble("score", float64(i)+0.5).
			AppendBoolean("active", i%2 == 0).
			Build())
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := a.rend.renderDocs(docs); err != nil {
			b.Fatalf("renderDocs: %v", err)
		}
	}
}
