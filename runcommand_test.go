package doc

import (
	"context"
	"errors"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/options"
)

// asCommandError reports whether err is a CommandError and binds it.
func asCommandError(err error, ce *CommandError) bool { return errors.As(err, ce) }

// indexTTL builds an IndexOptions with a TTL of n seconds.
func indexTTL(n int32) *options.IndexOptions { return options.Index().SetExpireAfterSeconds(n) }

// runCmd runs a command and fails the test on error, returning the reply document.
func runCmd(t *testing.T, d *Database, cmd any) bson.Raw {
	t.Helper()
	res := d.RunCommand(context.Background(), cmd)
	raw, err := res.Raw()
	if err != nil {
		t.Fatalf("RunCommand(%v): %v", cmd, err)
	}
	return bson.Raw(raw)
}

func okOf(t *testing.T, reply bson.Raw) {
	t.Helper()
	v, ok := reply.Lookup("ok")
	if !ok {
		t.Fatalf("reply has no ok field: %v", reply)
	}
	if f, _ := v.AsFloat64(); f != 1 {
		t.Fatalf("ok = %v, want 1", f)
	}
}

func TestRunCommandPing(t *testing.T) {
	db := openTestDB(t)
	okOf(t, runCmd(t, db.Database("admin"), M{"ping": 1}))
}

func TestRunCommandBuildInfo(t *testing.T) {
	db := openTestDB(t)
	reply := runCmd(t, db.Database("admin"), M{"buildInfo": 1})
	okOf(t, reply)
	v, ok := reply.Lookup("version")
	if !ok || v.StringValue() != Version {
		t.Fatalf("version = %v, want %s", v, Version)
	}
}

func TestRunCommandCollStats(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")
	for i := range 12 {
		if _, err := d.Collection("orders").InsertOne(ctx, M{"_id": i}); err != nil {
			t.Fatalf("InsertOne: %v", err)
		}
	}
	reply := runCmd(t, d, M{"collStats": "orders"})
	okOf(t, reply)
	if v, _ := reply.Lookup("count"); v.Int64() != 12 {
		t.Fatalf("count = %d, want 12", v.Int64())
	}
	if v, _ := reply.Lookup("ns"); v.StringValue() != "shop.orders" {
		t.Fatalf("ns = %q, want shop.orders", v.StringValue())
	}
	if _, ok := reply.Lookup("indexSizes"); !ok {
		t.Fatal("collStats reply missing indexSizes")
	}
}

func TestRunCommandCollStatsMissing(t *testing.T) {
	db := openTestDB(t)
	res := db.Database("shop").RunCommand(context.Background(), M{"collStats": "ghost"})
	var ce CommandError
	if err := res.Err(); err == nil || !asCommandError(err, &ce) {
		t.Fatalf("want CommandError, got %v", res.Err())
	}
	if ce.Code != 26 {
		t.Fatalf("code = %d, want 26", ce.Code)
	}
}

func TestRunCommandDBStats(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")
	for _, n := range []string{"a", "b"} {
		if _, err := d.Collection(n).InsertOne(ctx, M{"_id": 1}); err != nil {
			t.Fatalf("InsertOne: %v", err)
		}
	}
	reply := runCmd(t, d, M{"dbStats": 1})
	okOf(t, reply)
	if v, _ := reply.Lookup("collections"); v.Int64() != 2 {
		t.Fatalf("collections = %d, want 2", v.Int64())
	}
	if v, _ := reply.Lookup("objects"); v.Int64() != 2 {
		t.Fatalf("objects = %d, want 2", v.Int64())
	}
}

func TestRunCommandListCollections(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")
	for _, n := range []string{"alpha", "beta"} {
		if _, err := d.Collection(n).InsertOne(ctx, M{"_id": 1}); err != nil {
			t.Fatalf("InsertOne: %v", err)
		}
	}
	reply := runCmd(t, d, M{"listCollections": 1})
	okOf(t, reply)
	names := firstBatchField(t, reply, "name")
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("collection names = %v, want [alpha beta]", names)
	}
}

func TestRunCommandCreateAndDrop(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")
	okOf(t, runCmd(t, d, D{{"create", "events"}, {"capped", true}, {"size", 1 << 16}, {"max", 100}}))
	names, err := d.ListCollectionNames(ctx, nil)
	if err != nil {
		t.Fatalf("ListCollectionNames: %v", err)
	}
	if len(names) != 1 || names[0] != "events" {
		t.Fatalf("after create, names = %v", names)
	}
	st, err := d.Collection("events").Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !st.Capped {
		t.Fatal("created collection is not capped")
	}
	okOf(t, runCmd(t, d, M{"drop": "events"}))
	names, _ = d.ListCollectionNames(ctx, nil)
	if len(names) != 0 {
		t.Fatalf("after drop, names = %v", names)
	}
}

func TestRunCommandCreateIndexesAndList(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")
	if _, err := d.Collection("orders").InsertOne(ctx, M{"_id": 1, "sku": "a"}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	reply := runCmd(t, d, D{
		{"createIndexes", "orders"},
		{"indexes", A{
			D{{"key", M{"sku": 1}}, {"name", "sku_1"}, {"unique", true}},
		}},
	})
	okOf(t, reply)
	if v, _ := reply.Lookup("numIndexesAfter"); v.Int32() != 2 {
		t.Fatalf("numIndexesAfter = %d, want 2", v.Int32())
	}
	list := runCmd(t, d, M{"listIndexes": "orders"})
	names := firstBatchField(t, list, "name")
	if len(names) != 2 {
		t.Fatalf("index names = %v, want 2", names)
	}
	okOf(t, runCmd(t, d, D{{"dropIndexes", "orders"}, {"index", "sku_1"}}))
	list = runCmd(t, d, M{"listIndexes": "orders"})
	if names := firstBatchField(t, list, "name"); len(names) != 1 {
		t.Fatalf("after drop, index names = %v, want 1", names)
	}
}

func TestRunCommandCollModValidator(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")
	if err := d.CreateCollection(ctx, "users"); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	// Attach a validator that requires an email field, then confirm it bites.
	okOf(t, runCmd(t, d, D{
		{"collMod", "users"},
		{"validator", M{"email": M{"$exists": true}}},
	}))
	if _, err := d.Collection("users").InsertOne(ctx, M{"_id": 1}); err == nil {
		t.Fatal("insert without email should fail the new validator")
	}
	if _, err := d.Collection("users").InsertOne(ctx, M{"_id": 2, "email": "a@b.c"}); err != nil {
		t.Fatalf("insert with email should pass: %v", err)
	}
}

func TestRunCommandCollModIndexTTL(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")
	if _, err := d.Collection("sessions").InsertOne(ctx, M{"_id": 1}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	if _, err := d.Collection("sessions").Indexes().CreateOne(ctx, IndexModel{
		Keys:    M{"created": 1},
		Options: indexTTL(60),
	}); err != nil {
		t.Fatalf("CreateOne: %v", err)
	}
	okOf(t, runCmd(t, d, D{
		{"collMod", "sessions"},
		{"index", D{{"name", "created_1"}, {"expireAfterSeconds", 120}}},
	}))
	specs, err := d.Collection("sessions").Indexes().ListSpecifications(ctx)
	if err != nil {
		t.Fatalf("ListSpecifications: %v", err)
	}
	found := false
	for _, s := range specs {
		if s.Name == "created_1" {
			found = true
			if s.ExpireAfterSeconds == nil || *s.ExpireAfterSeconds != 120 {
				t.Fatalf("expireAfterSeconds = %v, want 120", s.ExpireAfterSeconds)
			}
		}
	}
	if !found {
		t.Fatal("created_1 index not found after collMod")
	}
}

func TestRunCommandCountDistinct(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")
	for i := range 6 {
		if _, err := d.Collection("orders").InsertOne(ctx, M{"_id": i, "color": colorOf(i)}); err != nil {
			t.Fatalf("InsertOne: %v", err)
		}
	}
	reply := runCmd(t, d, D{{"count", "orders"}, {"query", M{"color": "red"}}})
	if v, _ := reply.Lookup("n"); v.Int64() != 3 {
		t.Fatalf("count n = %d, want 3", v.Int64())
	}
	dreply := runCmd(t, d, D{{"distinct", "orders"}, {"key", "color"}})
	v, ok := dreply.Lookup("values")
	if !ok || v.Type != bson.TypeArray {
		t.Fatalf("distinct values missing: %v", dreply)
	}
	elems, _ := v.Document().Elements()
	if len(elems) != 2 {
		t.Fatalf("distinct values = %d, want 2", len(elems))
	}
}

func TestRunCommandUnknown(t *testing.T) {
	db := openTestDB(t)
	res := db.Database("shop").RunCommand(context.Background(), M{"frobnicate": 1})
	var ce CommandError
	if err := res.Err(); err == nil || !asCommandError(err, &ce) {
		t.Fatalf("want CommandError, got %v", res.Err())
	}
	if ce.Code != codeCommandNotFound {
		t.Fatalf("code = %d, want %d", ce.Code, codeCommandNotFound)
	}
}

// colorOf returns red for even indices and blue for odd, giving three of each
// across six documents.
func colorOf(i int) string {
	if i%2 == 0 {
		return "red"
	}
	return "blue"
}

// firstBatchField pulls one string field out of every document in a cursor
// reply's firstBatch array, in order.
func firstBatchField(t *testing.T, reply bson.Raw, field string) []string {
	t.Helper()
	cv, ok := reply.Lookup("cursor")
	if !ok || cv.Type != bson.TypeDocument {
		t.Fatalf("reply missing cursor: %v", reply)
	}
	fb, ok := cv.Document().Lookup("firstBatch")
	if !ok || fb.Type != bson.TypeArray {
		t.Fatalf("cursor missing firstBatch")
	}
	elems, _ := fb.Document().Elements()
	out := make([]string, 0, len(elems))
	for _, e := range elems {
		if e.Value.Type != bson.TypeDocument {
			continue
		}
		if v, ok := e.Value.Document().Lookup(field); ok {
			out = append(out, v.StringValue())
		}
	}
	return out
}
