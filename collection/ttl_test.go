package collection

import (
	"testing"
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
)

// docWithDate builds {_id: id, createdAt: <millis>}.
func docWithDate(id int32, millis int64) bson.Raw {
	return bson.NewBuilder().
		AppendInt32("_id", id).
		AppendDateTime("createdAt", millis).
		Build()
}

func TestSweepTTLDeletesExpired(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.CreateIndex(IndexModel{
		Key:                []catalog.KeyPart{{Field: "createdAt"}},
		ExpireAfterSeconds: 100,
	}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	now := time.UnixMilli(1_000_000)
	// id 1 is well past its TTL, id 2 is fresh.
	if _, err := c.InsertOne(docWithDate(1, now.UnixMilli()-200_000)); err != nil {
		t.Fatalf("insert expired: %v", err)
	}
	if _, err := c.InsertOne(docWithDate(2, now.UnixMilli()-1_000)); err != nil {
		t.Fatalf("insert fresh: %v", err)
	}

	n, err := c.SweepTTL(now)
	if err != nil {
		t.Fatalf("SweepTTL: %v", err)
	}
	if n != 1 {
		t.Fatalf("SweepTTL deleted %d, want 1", n)
	}
	if got, _ := c.FindOne(filterID(1)); got != nil {
		t.Fatal("expired _id 1 should have been swept")
	}
	if got, _ := c.FindOne(filterID(2)); got == nil {
		t.Fatal("fresh _id 2 should remain")
	}
}

func TestSweepTTLNoIndexIsNoop(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.InsertOne(docWithDate(1, 0)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	n, err := c.SweepTTL(time.UnixMilli(10_000_000))
	if err != nil {
		t.Fatalf("SweepTTL: %v", err)
	}
	if n != 0 {
		t.Fatalf("SweepTTL with no TTL index deleted %d, want 0", n)
	}
}

func TestSweepTTLIgnoresNonDateField(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.CreateIndex(IndexModel{
		Key:                []catalog.KeyPart{{Field: "createdAt"}},
		ExpireAfterSeconds: 1,
	}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	// createdAt is a string, not a date: MongoDB never expires it.
	d := bson.NewBuilder().AppendInt32("_id", 1).AppendString("createdAt", "soon").Build()
	if _, err := c.InsertOne(d); err != nil {
		t.Fatalf("insert: %v", err)
	}
	n, err := c.SweepTTL(time.UnixMilli(10_000_000))
	if err != nil {
		t.Fatalf("SweepTTL: %v", err)
	}
	if n != 0 {
		t.Fatalf("SweepTTL on non-date field deleted %d, want 0", n)
	}
}

func TestSweepTTLSkipsCapped(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.CreateIndex(IndexModel{
		Key:                []catalog.KeyPart{{Field: "createdAt"}},
		ExpireAfterSeconds: 1,
	}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	c.SetPolicy(Policy{Capped: true, CappedMaxDocs: 100})
	if _, err := c.InsertOne(docWithDate(1, 0)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	n, err := c.SweepTTL(time.UnixMilli(10_000_000))
	if err != nil {
		t.Fatalf("SweepTTL: %v", err)
	}
	if n != 0 {
		t.Fatalf("SweepTTL on capped collection deleted %d, want 0", n)
	}
}
