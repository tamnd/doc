package update

import (
	"testing"
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/sys"
)

// updateSeeds returns valid update documents spanning the operator families: field updates,
// numeric and min/max, array push/pull/addToSet, rename and unset, current date, and a nested
// dotted path. They give the fuzzer real operator shapes to mutate.
func updateSeeds() [][]byte {
	var seeds [][]byte

	set := bson.NewBuilder().AppendInt32("a", 1).AppendString("name", "bob").Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("$set", set).Build())

	inc := bson.NewBuilder().AppendInt32("n", 5).AppendDouble("f", 1.5).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("$inc", inc).Build())

	mul := bson.NewBuilder().AppendInt32("n", 2).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("$mul", mul).Build())

	mn := bson.NewBuilder().AppendInt32("lo", 0).Build()
	mx := bson.NewBuilder().AppendInt32("hi", 100).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("$min", mn).AppendDocument("$max", mx).Build())

	unset := bson.NewBuilder().AppendString("gone", "").Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("$unset", unset).Build())

	rename := bson.NewBuilder().AppendString("old", "new").Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("$rename", rename).Build())

	push := bson.NewBuilder().AppendInt32("tags", 7).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("$push", push).Build())

	pull := bson.NewBuilder().AppendInt32("tags", 7).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("$pull", pull).Build())

	add := bson.NewBuilder().AppendInt32("set", 3).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("$addToSet", add).Build())

	cur := bson.NewBuilder().AppendBoolean("ts", true).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("$currentDate", cur).Build())

	dotted := bson.NewBuilder().AppendInt32("a.b.c", 9).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("$set", dotted).Build())

	// A plain replacement document (no operators) is also a valid update.
	seeds = append(seeds, bson.NewBuilder().AppendString("name", "replaced").AppendInt32("v", 1).Build())

	return seeds
}

// updateTargets returns documents to apply compiled updates to, overlapping with the fields the
// update seeds touch so the operators do real work.
func updateTargets() []bson.Raw {
	arr := bson.NewBuilder().AppendInt32("0", 7).AppendInt32("1", 8).Build()
	return []bson.Raw{
		bson.NewBuilder().AppendObjectID("_id", sys.ObjectID{1}).Build(),
		bson.NewBuilder().AppendObjectID("_id", sys.ObjectID{2}).AppendInt32("a", 1).AppendInt32("n", 10).AppendString("name", "x").Build(),
		bson.NewBuilder().AppendObjectID("_id", sys.ObjectID{3}).AppendArray("tags", arr).AppendDouble("f", 2.0).Build(),
		bson.NewBuilder().AppendObjectID("_id", sys.ObjectID{4}).AppendString("old", "v").AppendInt32("hi", 50).AppendInt32("lo", 50).Build(),
	}
}

// FuzzUpdateApply compiles arbitrary bytes as an update document and, when compilation succeeds,
// applies it to every target document under both the update and insert paths. The spec 19 §18
// contract: Compile and Apply must never panic, hang, or read out of bounds; a rejected update
// or an apply error is acceptable, a panic is not. The clock is fixed so a found crash replays
// deterministically.
func FuzzUpdateApply(f *testing.F) {
	for _, s := range updateSeeds() {
		f.Add(s)
	}
	targets := updateTargets()
	now := time.Unix(1_700_000_000, 0).UTC()
	f.Fuzz(func(t *testing.T, data []byte) {
		raw := bson.Raw(data)
		if raw.Validate() != nil {
			return // not a well formed BSON document; the BSON fuzzer owns that
		}
		u, err := Compile(raw)
		if err != nil {
			return // a rejected update is fine as long as it did not panic
		}
		for _, d := range targets {
			if out, changed, err := u.Apply(d, now); err == nil && changed {
				// A successful update must yield a document that is still well formed.
				if out.Validate() != nil {
					t.Fatalf("Apply produced an invalid document from update %x on %x", []byte(raw), []byte(d))
				}
			}
			if out, changed, err := u.ApplyForInsert(d, now); err == nil && changed {
				if out.Validate() != nil {
					t.Fatalf("ApplyForInsert produced an invalid document from update %x on %x", []byte(raw), []byte(d))
				}
			}
		}
	})
}
