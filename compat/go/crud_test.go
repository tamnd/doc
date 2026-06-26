package compat

import (
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestInsertAndFindOne(t *testing.T) {
	c := coll(t, "crud_insert")
	ctx := ctxFor(t)

	res, err := c.InsertOne(ctx, bson.D{{Key: "name", Value: "ada"}, {Key: "age", Value: 36}})
	if err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	if res.InsertedID == nil {
		t.Fatal("InsertOne returned a nil InsertedID")
	}

	var got bson.M
	if err := c.FindOne(ctx, bson.D{{Key: "name", Value: "ada"}}).Decode(&got); err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got["age"] != int32(36) {
		t.Fatalf("age = %v (%T), want int32 36", got["age"], got["age"])
	}
}

func TestFindOneNoMatchReturnsErrNoDocuments(t *testing.T) {
	c := coll(t, "crud_nomatch")
	ctx := ctxFor(t)
	err := c.FindOne(ctx, bson.D{{Key: "missing", Value: true}}).Decode(&bson.M{})
	if !errors.Is(err, mongo.ErrNoDocuments) {
		t.Fatalf("FindOne on no match = %v, want ErrNoDocuments", err)
	}
}

func TestInsertManyAndFindAll(t *testing.T) {
	c := coll(t, "crud_many")
	ctx := ctxFor(t)

	docs := []any{
		bson.D{{Key: "n", Value: 1}},
		bson.D{{Key: "n", Value: 2}},
		bson.D{{Key: "n", Value: 3}},
	}
	if _, err := c.InsertMany(ctx, docs); err != nil {
		t.Fatalf("InsertMany: %v", err)
	}

	n, err := c.CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("CountDocuments: %v", err)
	}
	if n != 3 {
		t.Fatalf("CountDocuments = %d, want 3", n)
	}

	cur, err := c.Find(ctx, bson.D{{Key: "n", Value: bson.D{{Key: "$gte", Value: 2}}}},
		options.Find().SetSort(bson.D{{Key: "n", Value: 1}}))
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	var out []bson.M
	if err := cur.All(ctx, &out); err != nil {
		t.Fatalf("cursor All: %v", err)
	}
	if len(out) != 2 || out[0]["n"] != int32(2) || out[1]["n"] != int32(3) {
		t.Fatalf("Find($gte:2) sorted = %v, want n=2 then n=3", out)
	}
}

func TestUpdateOneSetAndInc(t *testing.T) {
	c := coll(t, "crud_update")
	ctx := ctxFor(t)
	if _, err := c.InsertOne(ctx, bson.D{{Key: "k", Value: "a"}, {Key: "hits", Value: 1}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	upd := bson.D{
		{Key: "$set", Value: bson.D{{Key: "label", Value: "first"}}},
		{Key: "$inc", Value: bson.D{{Key: "hits", Value: 4}}},
	}
	res, err := c.UpdateOne(ctx, bson.D{{Key: "k", Value: "a"}}, upd)
	if err != nil {
		t.Fatalf("UpdateOne: %v", err)
	}
	if res.MatchedCount != 1 || res.ModifiedCount != 1 {
		t.Fatalf("UpdateOne counts = matched %d modified %d, want 1/1", res.MatchedCount, res.ModifiedCount)
	}

	var got bson.M
	if err := c.FindOne(ctx, bson.D{{Key: "k", Value: "a"}}).Decode(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got["hits"] != int32(5) || got["label"] != "first" {
		t.Fatalf("after update = %v, want hits 5 and label first", got)
	}
}

func TestUpsertCreatesDocument(t *testing.T) {
	c := coll(t, "crud_upsert")
	ctx := ctxFor(t)
	res, err := c.UpdateOne(ctx,
		bson.D{{Key: "k", Value: "new"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 9}}}},
		options.UpdateOne().SetUpsert(true))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if res.UpsertedCount != 1 || res.UpsertedID == nil {
		t.Fatalf("upsert result = %+v, want one upserted id", res)
	}
}

func TestDeleteOneAndMany(t *testing.T) {
	c := coll(t, "crud_delete")
	ctx := ctxFor(t)
	docs := []any{
		bson.D{{Key: "g", Value: "x"}},
		bson.D{{Key: "g", Value: "x"}},
		bson.D{{Key: "g", Value: "y"}},
	}
	if _, err := c.InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	one, err := c.DeleteOne(ctx, bson.D{{Key: "g", Value: "x"}})
	if err != nil || one.DeletedCount != 1 {
		t.Fatalf("DeleteOne = %+v err %v, want 1 deleted", one, err)
	}
	many, err := c.DeleteMany(ctx, bson.D{{Key: "g", Value: "x"}})
	if err != nil || many.DeletedCount != 1 {
		t.Fatalf("DeleteMany = %+v err %v, want 1 deleted", many, err)
	}
	left, err := c.CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 1 {
		t.Fatalf("remaining = %d, want 1", left)
	}
}

func TestFindOneAndUpdateReturnsNew(t *testing.T) {
	c := coll(t, "crud_findupdate")
	ctx := ctxFor(t)
	if _, err := c.InsertOne(ctx, bson.D{{Key: "id", Value: 1}, {Key: "score", Value: 10}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var got bson.M
	err := c.FindOneAndUpdate(ctx,
		bson.D{{Key: "id", Value: 1}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "score", Value: 5}}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&got)
	if err != nil {
		t.Fatalf("FindOneAndUpdate: %v", err)
	}
	if got["score"] != int32(15) {
		t.Fatalf("returned score = %v, want 15 (the post-update value)", got["score"])
	}
}
