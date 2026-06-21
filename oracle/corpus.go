package oracle

import (
	"fmt"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/sys"
)

// coll is the single collection name the M2-c corpus drives.
const coll = "c"

// op constructors keep the case tables readable.
func insOp(doc bson.Raw) Op   { return Op{Kind: OpInsertOne, Collection: coll, Doc: doc} }
func findOneOp(f bson.Raw) Op { return Op{Kind: OpFindOne, Collection: coll, Filter: f} }
func findOp(f bson.Raw) Op    { return Op{Kind: OpFind, Collection: coll, Filter: f} }
func delOneOp(f bson.Raw) Op  { return Op{Kind: OpDeleteOne, Collection: coll, Filter: f} }
func countOp(f bson.Raw) Op   { return Op{Kind: OpCount, Collection: coll, Filter: f} }

// ---- document builders ---------------------------------------------------

func docI32(id, n int32) bson.Raw {
	return bson.NewBuilder().AppendInt32("_id", id).AppendInt32("n", n).Build()
}

func filtI32(id int32) bson.Raw {
	return bson.NewBuilder().AppendInt32("_id", id).Build()
}

func grpDoc(id, grp int32) bson.Raw {
	return bson.NewBuilder().AppendInt32("_id", id).AppendInt32("grp", grp).Build()
}

func grpFilter(grp int32) bson.Raw {
	return bson.NewBuilder().AppendInt32("grp", grp).Build()
}

// idSample is one _id type exercised across the standard operation matrix: a
// document carrying that _id, a filter selecting it, and a same-typed filter that
// selects nothing.
type idSample struct {
	name string
	doc  bson.Raw
	filt bson.Raw
	miss bson.Raw
}

// idSamples returns one sample per supported _id type. Every document carries an
// explicit _id (so doc and MongoDB never diverge on an auto-minted ObjectId) with
// _id first (so stored byte order matches), plus a tag field to make the body
// non-trivial.
func idSamples() []idSample {
	var oid sys.ObjectID
	for i := range oid {
		oid[i] = byte(i + 1)
	}
	var oid2 sys.ObjectID
	for i := range oid2 {
		oid2[i] = byte(0xF0 + i)
	}
	tag := func(b *bson.Builder) bson.Raw { return b.AppendString("tag", "v").Build() }

	return []idSample{
		{
			name: "int32",
			doc:  tag(bson.NewBuilder().AppendInt32("_id", 7)),
			filt: bson.NewBuilder().AppendInt32("_id", 7).Build(),
			miss: bson.NewBuilder().AppendInt32("_id", 8).Build(),
		},
		{
			name: "int64",
			doc:  tag(bson.NewBuilder().AppendInt64("_id", 1<<40)),
			filt: bson.NewBuilder().AppendInt64("_id", 1<<40).Build(),
			miss: bson.NewBuilder().AppendInt64("_id", 1<<41).Build(),
		},
		{
			name: "double",
			doc:  tag(bson.NewBuilder().AppendDouble("_id", 3.5)),
			filt: bson.NewBuilder().AppendDouble("_id", 3.5).Build(),
			miss: bson.NewBuilder().AppendDouble("_id", 4.5).Build(),
		},
		{
			name: "string",
			doc:  tag(bson.NewBuilder().AppendString("_id", "alpha")),
			filt: bson.NewBuilder().AppendString("_id", "alpha").Build(),
			miss: bson.NewBuilder().AppendString("_id", "beta").Build(),
		},
		{
			name: "bool",
			doc:  tag(bson.NewBuilder().AppendBoolean("_id", true)),
			filt: bson.NewBuilder().AppendBoolean("_id", true).Build(),
			miss: bson.NewBuilder().AppendBoolean("_id", false).Build(),
		},
		{
			name: "objectid",
			doc:  tag(bson.NewBuilder().AppendObjectID("_id", oid)),
			filt: bson.NewBuilder().AppendObjectID("_id", oid).Build(),
			miss: bson.NewBuilder().AppendObjectID("_id", oid2).Build(),
		},
	}
}

// Corpus returns the M2-c conformance corpus: a few hundred insert / find /
// delete / count cases whose probe result is compared between MongoDB and doc.
// Every inserted document carries an explicit, deterministic _id so the two
// engines agree byte for byte (spec 2061 doc 19 §17).
func Corpus() []Case {
	var cases []Case
	add := func(c Case) { cases = append(cases, c) }

	// Per-_id-type operation matrix.
	for _, s := range idSamples() {
		add(Case{
			Name:  s.name + "/insert-then-find",
			Setup: []Op{insOp(s.doc)},
			Probe: findOneOp(s.filt),
		})
		add(Case{
			Name:  s.name + "/find-missing",
			Setup: []Op{insOp(s.doc)},
			Probe: findOneOp(s.miss),
		})
		add(Case{
			Name:  s.name + "/count-after-insert",
			Setup: []Op{insOp(s.doc)},
			Probe: countOp(nil),
		})
		add(Case{
			Name:  s.name + "/delete-then-find",
			Setup: []Op{insOp(s.doc), delOneOp(s.filt)},
			Probe: findOneOp(s.filt),
		})
		add(Case{
			Name:  s.name + "/delete-then-count",
			Setup: []Op{insOp(s.doc), delOneOp(s.filt)},
			Probe: countOp(nil),
		})
		add(Case{
			Name:  s.name + "/duplicate-insert",
			Setup: []Op{insOp(s.doc)},
			Probe: insOp(s.doc),
		})
		add(Case{
			Name:  s.name + "/delete-returns-one",
			Setup: []Op{insOp(s.doc)},
			Probe: delOneOp(s.filt),
		})
		add(Case{
			Name:  s.name + "/delete-missing-returns-zero",
			Setup: []Op{insOp(s.doc)},
			Probe: delOneOp(s.miss),
		})
		add(Case{
			Name:  s.name + "/find-all",
			Setup: []Op{insOp(s.doc)},
			Probe: findOp(s.filt),
		})
		add(Case{
			Name:  s.name + "/reinsert-after-delete",
			Setup: []Op{insOp(s.doc), delOneOp(s.filt), insOp(s.doc)},
			Probe: findOneOp(s.filt),
		})
	}

	// Bulk count: insert n documents, probe the count.
	for n := 0; n <= 25; n++ {
		var setup []Op
		for i := int32(1); i <= int32(n); i++ {
			setup = append(setup, insOp(docI32(i, i*10)))
		}
		add(Case{
			Name:  fmt.Sprintf("bulk/count-%d", n),
			Setup: setup,
			Probe: countOp(nil),
		})
	}

	// Point lookup into a populated collection: insert ids 1..n, probe each id.
	const n = 50
	var bulk []Op
	for i := int32(1); i <= n; i++ {
		bulk = append(bulk, insOp(docI32(i, i*10)))
	}
	for i := int32(1); i <= n; i++ {
		add(Case{
			Name:  fmt.Sprintf("point/find-id-%d", i),
			Setup: bulk,
			Probe: findOneOp(filtI32(i)),
		})
	}
	add(Case{Name: "point/find-missing-high", Setup: bulk, Probe: findOneOp(filtI32(n + 1))})
	add(Case{Name: "point/find-missing-zero", Setup: bulk, Probe: findOneOp(filtI32(0))})

	// Delete from the middle, then verify count and absence.
	add(Case{
		Name:  "bulk/delete-middle-count",
		Setup: append(append([]Op{}, bulk...), delOneOp(filtI32(15))),
		Probe: countOp(nil),
	})
	add(Case{
		Name:  "bulk/delete-middle-find",
		Setup: append(append([]Op{}, bulk...), delOneOp(filtI32(15))),
		Probe: findOneOp(filtI32(15)),
	})

	// Field equality: a population split across two groups.
	var groups []Op
	for i := int32(1); i <= 12; i++ {
		groups = append(groups, insOp(grpDoc(i, i%3)))
	}
	for g := int32(0); g <= 2; g++ {
		add(Case{
			Name:  fmt.Sprintf("group/count-grp-%d", g),
			Setup: groups,
			Probe: countOp(grpFilter(g)),
		})
		add(Case{
			Name:  fmt.Sprintf("group/find-grp-%d", g),
			Setup: groups,
			Probe: findOp(grpFilter(g)),
		})
		add(Case{
			Name:  fmt.Sprintf("group/find-one-grp-%d", g),
			Setup: groups,
			Probe: findOneOp(grpFilter(g)),
		})
	}
	add(Case{Name: "group/count-no-match", Setup: groups, Probe: countOp(grpFilter(9))})
	add(Case{Name: "group/find-no-match", Setup: groups, Probe: findOp(grpFilter(9))})

	// Natural-order probes on an empty filter.
	order := []Op{insOp(docI32(5, 50)), insOp(docI32(3, 30)), insOp(docI32(9, 90)), insOp(docI32(1, 10))}
	add(Case{Name: "natural/find-one-empty", Setup: order, Probe: findOneOp(nil)})
	add(Case{Name: "natural/find-all-empty", Setup: order, Probe: findOp(nil)})
	add(Case{Name: "natural/count-empty", Setup: order, Probe: countOp(nil)})

	// Edge cases on an empty collection.
	add(Case{Name: "empty/find-one", Probe: findOneOp(filtI32(1))})
	add(Case{Name: "empty/find-all", Probe: findOp(nil)})
	add(Case{Name: "empty/count", Probe: countOp(nil)})
	add(Case{Name: "empty/delete", Probe: delOneOp(filtI32(1))})

	// M3-a find surface: operators, null/missing, sort, projection, paging.
	cases = append(cases, m3Cases()...)

	// M3-b write surface: update operators, update/replace, findAndModify,
	// distinct.
	cases = append(cases, m3bCases()...)

	// M4 aggregation pipeline: $group and its accumulators, $sort, $sortByCount,
	// $bucket, $bucketAuto, $facet, $lookup, $graphLookup, $unionWith, $redact,
	// and the expression language.
	cases = append(cases, m4Cases()...)

	return cases
}
