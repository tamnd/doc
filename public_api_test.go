package doc

import (
	"context"
	"errors"
	"testing"

	"github.com/tamnd/doc/options"
)

// upsertOpt builds an UpdateOptions with upsert enabled.
func upsertOpt() *options.UpdateOptions { return options.Update().SetUpsert(true) }

// returnAfter builds a FindOneAndUpdateOptions that returns the post-image.
func returnAfter() *options.FindOneAndUpdateOptions {
	return options.FindOneAndUpdate().SetReturnDocument(options.After)
}

// indexUnique builds an IndexOptions marking the index unique.
func indexUnique() *options.IndexOptions { return options.Index().SetUnique(true) }

// openTestDB opens a fresh in-memory database for one test and registers its
// close on cleanup.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(memoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

func TestHandleHierarchyNames(t *testing.T) {
	db := openTestDB(t)
	d := db.Database("shop")
	if d.Name() != "shop" {
		t.Fatalf("Database.Name = %q, want shop", d.Name())
	}
	c := d.Collection("orders")
	if c.Name() != "orders" {
		t.Fatalf("Collection.Name = %q, want orders", c.Name())
	}
	if c.Database().Name() != "shop" {
		t.Fatalf("Collection.Database.Name = %q, want shop", c.Database().Name())
	}
}

func TestInsertOneAndFindOne(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("users")

	res, err := c.InsertOne(ctx, M{"name": "ada", "age": 36})
	if err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	if _, ok := res.InsertedID.(ObjectID); !ok {
		t.Fatalf("InsertedID type = %T, want ObjectID", res.InsertedID)
	}

	var got struct {
		Name string `bson:"name"`
		Age  int    `bson:"age"`
	}
	if err := c.FindOne(ctx, M{"name": "ada"}).Decode(&got); err != nil {
		t.Fatalf("FindOne.Decode: %v", err)
	}
	if got.Name != "ada" || got.Age != 36 {
		t.Fatalf("decoded = %+v, want {ada 36}", got)
	}
}

func TestFindOneNoMatch(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("users")
	err := c.FindOne(ctx, M{"name": "missing"}).Err()
	if !errors.Is(err, ErrNoDocuments) {
		t.Fatalf("FindOne err = %v, want ErrNoDocuments", err)
	}
}

func TestInsertManyAndCursorAll(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("nums")

	docs := []any{M{"n": 1}, M{"n": 2}, M{"n": 3}}
	ins, err := c.InsertMany(ctx, docs)
	if err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	if len(ins.InsertedIDs) != 3 {
		t.Fatalf("InsertedIDs len = %d, want 3", len(ins.InsertedIDs))
	}

	cur, err := c.Find(ctx, M{}, nil)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	var out []struct {
		N int `bson:"n"`
	}
	if err := cur.All(ctx, &out); err != nil {
		t.Fatalf("cursor.All: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("All len = %d, want 3", len(out))
	}
	sum := 0
	for _, o := range out {
		sum += o.N
	}
	if sum != 6 {
		t.Fatalf("sum = %d, want 6", sum)
	}
}

func TestCursorStepwise(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("nums")
	if _, err := c.InsertMany(ctx, []any{M{"n": 10}, M{"n": 20}}); err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	cur, err := c.Find(ctx, M{})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	defer cur.Close(ctx)
	count := 0
	for cur.Next(ctx) {
		var doc M
		if err := cur.Decode(&doc); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if cur.Current() == nil {
			t.Fatalf("Current returned nil during iteration")
		}
		count++
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("cursor.Err: %v", err)
	}
	if count != 2 {
		t.Fatalf("iterated %d, want 2", count)
	}
}

func TestUpdateOneSetAndUpsert(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("users")
	if _, err := c.InsertOne(ctx, M{"name": "ada", "age": 36}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}

	res, err := c.UpdateOne(ctx, M{"name": "ada"}, M{"$set": M{"age": 37}})
	if err != nil {
		t.Fatalf("UpdateOne: %v", err)
	}
	if res.MatchedCount != 1 || res.ModifiedCount != 1 {
		t.Fatalf("UpdateOne result = %+v, want matched 1 modified 1", res)
	}
}

func TestUpsertCreatesDocument(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("users")

	res, err := c.UpdateOne(ctx, M{"name": "grace"}, M{"$set": M{"age": 45}}, upsertOpt())
	if err != nil {
		t.Fatalf("UpdateOne upsert: %v", err)
	}
	if res.UpsertedCount != 1 {
		t.Fatalf("UpsertedCount = %d, want 1", res.UpsertedCount)
	}
	if res.UpsertedID == nil {
		t.Fatalf("UpsertedID is nil after upsert")
	}
	n, err := c.CountDocuments(ctx, M{"name": "grace"})
	if err != nil {
		t.Fatalf("CountDocuments: %v", err)
	}
	if n != 1 {
		t.Fatalf("count after upsert = %d, want 1", n)
	}
}

func TestReplaceOne(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("users")
	if _, err := c.InsertOne(ctx, M{"name": "ada", "age": 36, "city": "london"}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	res, err := c.ReplaceOne(ctx, M{"name": "ada"}, M{"name": "ada", "age": 99})
	if err != nil {
		t.Fatalf("ReplaceOne: %v", err)
	}
	if res.ModifiedCount != 1 {
		t.Fatalf("ModifiedCount = %d, want 1", res.ModifiedCount)
	}
	var got M
	if err := c.FindOne(ctx, M{"name": "ada"}).Decode(&got); err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if _, ok := got["city"]; ok {
		t.Fatalf("city should be gone after replace, got %+v", got)
	}
}

func TestDeleteOneAndMany(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("nums")
	if _, err := c.InsertMany(ctx, []any{M{"g": "a"}, M{"g": "a"}, M{"g": "b"}}); err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	d1, err := c.DeleteOne(ctx, M{"g": "a"})
	if err != nil {
		t.Fatalf("DeleteOne: %v", err)
	}
	if d1.DeletedCount != 1 {
		t.Fatalf("DeleteOne count = %d, want 1", d1.DeletedCount)
	}
	dm, err := c.DeleteMany(ctx, M{"g": "a"})
	if err != nil {
		t.Fatalf("DeleteMany: %v", err)
	}
	if dm.DeletedCount != 1 {
		t.Fatalf("DeleteMany count = %d, want 1", dm.DeletedCount)
	}
}

func TestDistinct(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("nums")
	if _, err := c.InsertMany(ctx, []any{M{"g": "a"}, M{"g": "a"}, M{"g": "b"}}); err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	vals, err := c.Distinct(ctx, "g", M{})
	if err != nil {
		t.Fatalf("Distinct: %v", err)
	}
	if len(vals) != 2 {
		t.Fatalf("Distinct len = %d, want 2", len(vals))
	}
}

func TestFindOneAndUpdateReturnAfter(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("users")
	if _, err := c.InsertOne(ctx, M{"name": "ada", "age": 36}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	var got M
	err := c.FindOneAndUpdate(ctx, M{"name": "ada"}, M{"$set": M{"age": 40}}, returnAfter()).Decode(&got)
	if err != nil {
		t.Fatalf("FindOneAndUpdate: %v", err)
	}
	if toInt(got["age"]) != 40 {
		t.Fatalf("returned age = %v, want 40", got["age"])
	}
}

func TestBulkWriteMixed(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("users")
	if _, err := c.InsertOne(ctx, M{"name": "ada", "age": 36}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}

	models := []WriteModel{
		NewInsertOneModel().SetDocument(M{"name": "grace", "age": 45}),
		NewUpdateOneModel().SetFilter(M{"name": "ada"}).SetUpdate(M{"$set": M{"age": 37}}),
		NewDeleteOneModel().SetFilter(M{"name": "nobody"}),
	}
	res, err := c.BulkWrite(ctx, models)
	if err != nil {
		t.Fatalf("BulkWrite: %v", err)
	}
	if res.InsertedCount != 1 {
		t.Fatalf("InsertedCount = %d, want 1", res.InsertedCount)
	}
	if res.ModifiedCount != 1 {
		t.Fatalf("ModifiedCount = %d, want 1", res.ModifiedCount)
	}
}

func TestIndexLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("users")
	if _, err := c.InsertOne(ctx, M{"name": "ada"}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}

	name, err := c.Indexes().CreateOne(ctx, IndexModel{Keys: D{{Key: "name", Value: 1}}})
	if err != nil {
		t.Fatalf("CreateOne: %v", err)
	}
	if name == "" {
		t.Fatalf("CreateOne returned empty name")
	}

	specs, err := c.Indexes().ListSpecifications(ctx)
	if err != nil {
		t.Fatalf("ListSpecifications: %v", err)
	}
	found := false
	for _, s := range specs {
		if s.Name == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("created index %q not in specifications %+v", name, specs)
	}

	if _, err := c.Indexes().DropOne(ctx, name); err != nil {
		t.Fatalf("DropOne: %v", err)
	}
}

func TestUniqueIndexRejectsDuplicate(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("users")
	if _, err := c.Indexes().CreateOne(ctx, IndexModel{
		Keys:    D{{Key: "email", Value: 1}},
		Options: indexUnique(),
	}); err != nil {
		t.Fatalf("CreateOne unique: %v", err)
	}
	if _, err := c.InsertOne(ctx, M{"email": "a@b.com"}); err != nil {
		t.Fatalf("InsertOne first: %v", err)
	}
	_, err := c.InsertOne(ctx, M{"email": "a@b.com"})
	if err == nil {
		t.Fatalf("expected duplicate key error on second insert")
	}
}

func TestDropCollectionAndDatabase(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")
	c := d.Collection("users")
	if _, err := c.InsertOne(ctx, M{"name": "ada"}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	names, err := d.ListCollectionNames(ctx, M{})
	if err != nil {
		t.Fatalf("ListCollectionNames: %v", err)
	}
	if len(names) != 1 || names[0] != "users" {
		t.Fatalf("collections = %v, want [users]", names)
	}
	if err := c.Drop(ctx); err != nil {
		t.Fatalf("Drop collection: %v", err)
	}
	if err := c.Drop(ctx); err != nil {
		t.Fatalf("Drop on missing collection should be a no-op, got %v", err)
	}
	if err := d.Drop(ctx); err != nil {
		t.Fatalf("Drop database: %v", err)
	}
}

func TestClosedDBRejectsOps(t *testing.T) {
	ctx := context.Background()
	db, err := Open(memoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := db.Database("shop").Collection("users")
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.InsertOne(ctx, M{"name": "x"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("InsertOne after Close = %v, want ErrClosed", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close should be a no-op, got %v", err)
	}
}

func TestReadOnMissingNamespaceIsEmpty(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("nope").Collection("nothing")
	n, err := c.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("CountDocuments on missing namespace: %v", err)
	}
	if n != 0 {
		t.Fatalf("count = %d, want 0", n)
	}
	cur, err := c.Find(ctx, M{})
	if err != nil {
		t.Fatalf("Find on missing namespace: %v", err)
	}
	if cur.Next(ctx) {
		t.Fatalf("cursor over missing namespace yielded a document")
	}
}

// toInt coerces a decoded numeric value to int for assertions.
func toInt(v any) int {
	switch n := v.(type) {
	case int32:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	default:
		return 0
	}
}
