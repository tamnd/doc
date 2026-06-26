package compat

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// seedSales loads a small sales set used by the aggregation tests.
func seedSales(t *testing.T, c *mongo.Collection) {
	t.Helper()
	ctx := ctxFor(t)
	docs := []any{
		bson.D{{Key: "region", Value: "west"}, {Key: "amount", Value: 100}},
		bson.D{{Key: "region", Value: "west"}, {Key: "amount", Value: 50}},
		bson.D{{Key: "region", Value: "east"}, {Key: "amount", Value: 75}},
		bson.D{{Key: "region", Value: "east"}, {Key: "amount", Value: 25}},
		bson.D{{Key: "region", Value: "north"}, {Key: "amount", Value: 200}},
	}
	if _, err := c.InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed sales: %v", err)
	}
}

func TestAggregateGroupSum(t *testing.T) {
	c := coll(t, "agg_group")
	seedSales(t, c)
	ctx := ctxFor(t)

	pipeline := mongo.Pipeline{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$region"},
			{Key: "total", Value: bson.D{{Key: "$sum", Value: "$amount"}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}},
	}
	cur, err := c.Aggregate(ctx, pipeline)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	var out []bson.M
	if err := cur.All(ctx, &out); err != nil {
		t.Fatalf("cursor All: %v", err)
	}

	want := map[string]int32{"east": 100, "north": 200, "west": 150}
	if len(out) != len(want) {
		t.Fatalf("group count = %d, want %d (%v)", len(out), len(want), out)
	}
	for _, row := range out {
		id, _ := row["_id"].(string)
		if row["total"] != want[id] {
			t.Fatalf("region %q total = %v, want %d", id, row["total"], want[id])
		}
	}
}

func TestAggregateMatchProjectLimit(t *testing.T) {
	c := coll(t, "agg_pipeline")
	seedSales(t, c)
	ctx := ctxFor(t)

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "amount", Value: bson.D{{Key: "$gte", Value: 75}}}}}},
		{{Key: "$sort", Value: bson.D{{Key: "amount", Value: -1}}}},
		{{Key: "$project", Value: bson.D{{Key: "_id", Value: 0}, {Key: "amount", Value: 1}}}},
		{{Key: "$limit", Value: 2}},
	}
	cur, err := c.Aggregate(ctx, pipeline)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	var out []bson.M
	if err := cur.All(ctx, &out); err != nil {
		t.Fatalf("cursor All: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("limit gave %d rows, want 2", len(out))
	}
	if out[0]["amount"] != int32(200) || out[1]["amount"] != int32(100) {
		t.Fatalf("top amounts = %v, want 200 then 100", out)
	}
	if _, ok := out[0]["_id"]; ok {
		t.Fatalf("$project _id:0 should have dropped _id, got %v", out[0])
	}
}

func TestAggregateCountStage(t *testing.T) {
	c := coll(t, "agg_count")
	seedSales(t, c)
	ctx := ctxFor(t)

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "region", Value: "east"}}}},
		{{Key: "$count", Value: "n"}},
	}
	cur, err := c.Aggregate(ctx, pipeline)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	var out []bson.M
	if err := cur.All(ctx, &out); err != nil {
		t.Fatalf("cursor All: %v", err)
	}
	if len(out) != 1 || out[0]["n"] != int32(2) {
		t.Fatalf("$count = %v, want a single n=2", out)
	}
}
