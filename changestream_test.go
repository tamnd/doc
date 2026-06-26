package doc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/options"
)

// watchCtx returns a context that bounds a blocking Next so a stuck test fails fast
// rather than hanging.
func watchCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// asInt coerces a decoded BSON number, which may arrive as int32 or int64, to int.
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func TestWatchInsertDeliversEvent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")

	cs, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close(ctx)

	if _, err := c.InsertOne(ctx, M{"_id": 1, "item": "book"}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}

	if !cs.Next(watchCtx(t)) {
		t.Fatalf("Next returned false, err=%v", cs.Err())
	}
	ev := cs.Current()
	if ev.OperationType != OperationInsert {
		t.Fatalf("OperationType = %q, want insert", ev.OperationType)
	}
	if ev.Ns.DB != "shop" || ev.Ns.Collection != "orders" {
		t.Fatalf("Ns = %+v, want shop.orders", ev.Ns)
	}
	if ev.FullDocument == nil {
		t.Fatal("insert event missing fullDocument")
	}
	if (*ev.FullDocument)["item"] != "book" {
		t.Fatalf("fullDocument.item = %v, want book", (*ev.FullDocument)["item"])
	}
	if len(cs.ResumeToken()) == 0 {
		t.Fatal("ResumeToken empty after first event")
	}
}

func TestWatchUpdateDescribesFields(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")
	if _, err := c.InsertOne(ctx, M{"_id": 1, "qty": 1, "note": "x"}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}

	cs, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close(ctx)

	if _, err := c.UpdateOne(ctx, M{"_id": 1}, M{"$set": M{"qty": 5}, "$unset": M{"note": ""}}); err != nil {
		t.Fatalf("UpdateOne: %v", err)
	}

	if !cs.Next(watchCtx(t)) {
		t.Fatalf("Next returned false, err=%v", cs.Err())
	}
	ev := cs.Current()
	if ev.OperationType != OperationUpdate {
		t.Fatalf("OperationType = %q, want update", ev.OperationType)
	}
	if ev.UpdateDescription == nil {
		t.Fatal("UpdateDescription nil on update event")
	}
	qty, ok := asInt(ev.UpdateDescription.UpdatedFields["qty"])
	if !ok || qty != 5 {
		t.Fatalf("updatedFields.qty = %v, want 5", ev.UpdateDescription.UpdatedFields["qty"])
	}
	if len(ev.UpdateDescription.RemovedFields) != 1 || ev.UpdateDescription.RemovedFields[0] != "note" {
		t.Fatalf("removedFields = %v, want [note]", ev.UpdateDescription.RemovedFields)
	}
	if ev.FullDocument != nil {
		t.Fatal("default mode should omit fullDocument on update")
	}
}

func TestWatchUpdateLookupIncludesDocument(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")
	if _, err := c.InsertOne(ctx, M{"_id": 1, "qty": 1}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}

	cs, err := c.Watch(ctx, nil, options.ChangeStream().SetFullDocument(options.UpdateLookup))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close(ctx)

	if _, err := c.UpdateOne(ctx, M{"_id": 1}, M{"$set": M{"qty": 9}}); err != nil {
		t.Fatalf("UpdateOne: %v", err)
	}
	if !cs.Next(watchCtx(t)) {
		t.Fatalf("Next returned false, err=%v", cs.Err())
	}
	ev := cs.Current()
	if ev.FullDocument == nil {
		t.Fatal("updateLookup should include fullDocument")
	}
	qty, _ := asInt((*ev.FullDocument)["qty"])
	if qty != 9 {
		t.Fatalf("fullDocument.qty = %v, want 9", (*ev.FullDocument)["qty"])
	}
}

func TestWatchDeleteCarriesDocumentKey(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")
	if _, err := c.InsertOne(ctx, M{"_id": 7}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}

	cs, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close(ctx)

	if _, err := c.DeleteOne(ctx, M{"_id": 7}); err != nil {
		t.Fatalf("DeleteOne: %v", err)
	}
	if !cs.Next(watchCtx(t)) {
		t.Fatalf("Next returned false, err=%v", cs.Err())
	}
	ev := cs.Current()
	if ev.OperationType != OperationDelete {
		t.Fatalf("OperationType = %q, want delete", ev.OperationType)
	}
	if ev.FullDocument != nil {
		t.Fatal("delete event should have no fullDocument")
	}
	id, ok := asInt(ev.DocumentKey["_id"])
	if !ok || id != 7 {
		t.Fatalf("documentKey._id = %v, want 7", ev.DocumentKey["_id"])
	}
}

func TestWatchReplaceUsesPostImage(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")
	if _, err := c.InsertOne(ctx, M{"_id": 1, "a": 1}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}

	cs, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close(ctx)

	if _, err := c.ReplaceOne(ctx, M{"_id": 1}, M{"_id": 1, "b": 2}); err != nil {
		t.Fatalf("ReplaceOne: %v", err)
	}
	if !cs.Next(watchCtx(t)) {
		t.Fatalf("Next returned false, err=%v", cs.Err())
	}
	ev := cs.Current()
	if ev.OperationType != OperationReplace {
		t.Fatalf("OperationType = %q, want replace", ev.OperationType)
	}
	if ev.FullDocument == nil {
		t.Fatal("replace event missing fullDocument")
	}
	b, _ := asInt((*ev.FullDocument)["b"])
	if b != 2 {
		t.Fatalf("fullDocument.b = %v, want 2", (*ev.FullDocument)["b"])
	}
}

func TestWatchPipelineMatchFilters(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")

	cs, err := c.Watch(ctx, A{M{"$match": M{"operationType": "delete"}}})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close(ctx)

	if _, err := c.InsertOne(ctx, M{"_id": 1}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	if _, err := c.DeleteOne(ctx, M{"_id": 1}); err != nil {
		t.Fatalf("DeleteOne: %v", err)
	}

	if !cs.Next(watchCtx(t)) {
		t.Fatalf("Next returned false, err=%v", cs.Err())
	}
	if cs.Current().OperationType != OperationDelete {
		t.Fatalf("filtered stream delivered %q, want delete", cs.Current().OperationType)
	}
}

func TestWatchResumeAfterToken(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")

	cs, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i}); err != nil {
			t.Fatalf("InsertOne %d: %v", i, err)
		}
	}
	if !cs.Next(watchCtx(t)) {
		t.Fatalf("Next 1 returned false, err=%v", cs.Err())
	}
	token := cs.ResumeToken()
	_ = cs.Close(ctx)

	resumed, err := c.Watch(ctx, nil, options.ChangeStream().SetResumeAfter(token))
	if err != nil {
		t.Fatalf("Watch resume: %v", err)
	}
	defer resumed.Close(ctx)

	if !resumed.Next(watchCtx(t)) {
		t.Fatalf("resumed Next returned false, err=%v", resumed.Err())
	}
	id, _ := asInt(resumed.Current().DocumentKey["_id"])
	if id != 2 {
		t.Fatalf("resumed first event _id = %d, want 2", id)
	}
}

func TestWatchDatabaseScopeSeesAllCollections(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	shop := db.Database("shop")

	cs, err := shop.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close(ctx)

	if _, err := shop.Collection("a").InsertOne(ctx, M{"_id": 1}); err != nil {
		t.Fatalf("InsertOne a: %v", err)
	}
	if _, err := shop.Collection("b").InsertOne(ctx, M{"_id": 2}); err != nil {
		t.Fatalf("InsertOne b: %v", err)
	}

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		if !cs.Next(watchCtx(t)) {
			t.Fatalf("Next %d returned false, err=%v", i, cs.Err())
		}
		seen[cs.Current().Ns.Collection] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("database stream saw %v, want both a and b", seen)
	}
}

func TestWatchDeploymentScopeSeesAllDatabases(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	cs, err := db.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close(ctx)

	if _, err := db.Database("d1").Collection("c").InsertOne(ctx, M{"_id": 1}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	if _, err := db.Database("d2").Collection("c").InsertOne(ctx, M{"_id": 2}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	dbs := map[string]bool{}
	for i := 0; i < 2; i++ {
		if !cs.Next(watchCtx(t)) {
			t.Fatalf("Next %d returned false, err=%v", i, cs.Err())
		}
		dbs[cs.Current().Ns.DB] = true
	}
	if !dbs["d1"] || !dbs["d2"] {
		t.Fatalf("deployment stream saw %v, want both d1 and d2", dbs)
	}
}

func TestWatchDropInvalidates(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")
	if _, err := c.InsertOne(ctx, M{"_id": 1}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}

	cs, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close(ctx)

	if err := c.Drop(ctx); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if !cs.Next(watchCtx(t)) {
		t.Fatalf("Next drop returned false, err=%v", cs.Err())
	}
	if cs.Current().OperationType != OperationDrop {
		t.Fatalf("first post-drop event = %q, want drop", cs.Current().OperationType)
	}
	if !cs.Next(watchCtx(t)) {
		t.Fatalf("Next invalidate returned false, err=%v", cs.Err())
	}
	if cs.Current().OperationType != OperationInvalidate {
		t.Fatalf("second post-drop event = %q, want invalidate", cs.Current().OperationType)
	}
	if cs.Next(watchCtx(t)) {
		t.Fatal("stream delivered an event after invalidate")
	}
	if !errors.Is(cs.Err(), ErrChangeStreamInvalidated) {
		t.Fatalf("Err after invalidate = %v, want ErrChangeStreamInvalidated", cs.Err())
	}
}

func TestWatchTryNextNonBlocking(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")

	cs, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close(ctx)

	if cs.TryNext(ctx) {
		t.Fatal("TryNext true with no events")
	}
	if cs.Err() != nil {
		t.Fatalf("TryNext set err with no events: %v", cs.Err())
	}
	if _, err := c.InsertOne(ctx, M{"_id": 1}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	if !cs.TryNext(ctx) {
		t.Fatalf("TryNext false after insert, err=%v", cs.Err())
	}
}

func TestWatchDecodeWholeEvent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")

	cs, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close(ctx)

	if _, err := c.InsertOne(ctx, M{"_id": 1, "item": "pen"}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	if !cs.Next(watchCtx(t)) {
		t.Fatalf("Next returned false, err=%v", cs.Err())
	}
	var ev ChangeEvent
	if err := cs.Decode(&ev); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ev.OperationType != OperationInsert || ev.Ns.Collection != "orders" {
		t.Fatalf("decoded event = %+v", ev)
	}
}

func TestWatchClosedDB(t *testing.T) {
	db := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := db.Database("shop").Collection("orders").Watch(context.Background(), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Watch after Close = %v, want ErrClosed", err)
	}
}

func TestResumeTokenRoundTrip(t *testing.T) {
	for _, seq := range []uint64{0, 1, 42, 1 << 20, 1<<63 + 5} {
		tok := encodeResumeToken(seq)
		got, ok := decodeResumeToken(tok)
		if !ok || got != seq {
			t.Fatalf("round trip seq=%d got=%d ok=%v", seq, got, ok)
		}
	}
	bad := bson.NewBuilder().AppendString("_data", "not-base64!!").Build()
	if _, ok := decodeResumeToken(bad); ok {
		t.Fatal("decoded a malformed token")
	}
}

func BenchmarkWatchInsert(b *testing.B) {
	ctx := context.Background()
	db, err := Open(memoryPath)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer db.Close()
	c := db.Database("shop").Collection("orders")
	cs, err := c.Watch(ctx, nil)
	if err != nil {
		b.Fatalf("Watch: %v", err)
	}
	defer cs.Close(ctx)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i}); err != nil {
			b.Fatalf("InsertOne: %v", err)
		}
		if !cs.Next(ctx) {
			b.Fatalf("Next: %v", cs.Err())
		}
	}
}
