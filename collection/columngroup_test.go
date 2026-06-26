package collection

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/tamnd/doc/agg"
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/colstore"
	"github.com/tamnd/doc/sys"
)

// This file pins the M9-e invariant: the vectorized $group executor returns exactly
// what the reconstruct path returns, value for value and type for type. Both run over
// the same column store, so both carry the store's int32-to-int64 widening identically;
// the comparison is vectorized-versus-reconstruct, not column-versus-heap. The test
// drives random documents through random eligible $group pipelines and asserts the two
// paths produce byte-identical result documents. Any divergence means the executor
// failed to reproduce an accumulator, which would be a correctness bug, not a perf one.

// reconstructGroup runs the pipeline through the column store's reconstruct path: it
// rebuilds the covered documents and replays the compiled pipeline, which is the
// established correct-by-construction baseline the vectorized executor must match.
func reconstructGroup(t *testing.T, c *Collection, pipeline []bson.Raw) []bson.Raw {
	t.Helper()
	tx := c.BeginReadOnly()
	defer func() { _ = tx.Rollback() }()
	docs, ok := tx.columnSource(pipeline)
	if !ok {
		t.Fatalf("reconstruct path declined an eligible pipeline")
	}
	p, err := agg.Compile(pipeline)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	out, err := p.RunWith(docs, tx.c.clk.Now().UnixMilli(), tx.aggEnv())
	if err != nil {
		t.Fatalf("reconstruct run: %v", err)
	}
	return out
}

// vectorizedGroup runs the pipeline through the vectorized executor, asserting the
// fast path actually fired (so the test never silently compares two reconstruct runs).
func vectorizedGroup(t *testing.T, c *Collection, pipeline []bson.Raw) []bson.Raw {
	t.Helper()
	tx := c.BeginReadOnly()
	defer func() { _ = tx.Rollback() }()
	out, ok := tx.columnGroup(pipeline)
	if !ok {
		t.Fatalf("vectorized path declined an eligible pipeline")
	}
	return out
}

// assertSameDocs compares two result sets for exact byte equality as ordered lists.
// Both paths emit groups in first-seen scan order, so the order must match too.
func assertSameDocs(t *testing.T, label string, want, got []bson.Raw) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: length differs: reconstruct=%d vectorized=%d", label, len(want), len(got))
	}
	for i := range want {
		if !bytesEqual(want[i], got[i]) {
			t.Fatalf("%s: doc %d differs\n reconstruct=%x\n vectorized =%x", label, i, want[i], got[i])
		}
	}
}

func bytesEqual(a, b bson.Raw) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// randDocValue appends a random value of a random type under key to the builder. The
// spread covers every kind the executor must handle: integers, doubles, strings,
// booleans, an occasional missing field, and an opaque type (ObjectId) for min/max.
func randDocValue(b *bson.Builder, rng *rand.Rand, key string) {
	switch rng.Intn(7) {
	case 0:
		b.AppendInt32(key, int32(rng.Intn(200)-100))
	case 1:
		b.AppendInt64(key, int64(rng.Intn(200)-100))
	case 2:
		b.AppendDouble(key, float64(rng.Intn(2000)-1000)/10)
	case 3:
		b.AppendString(key, fmt.Sprintf("s%d", rng.Intn(8)))
	case 4:
		b.AppendBoolean(key, rng.Intn(2) == 0)
	case 5:
		// leave the field missing
	default:
		var oid sys.ObjectID
		for i := range oid {
			oid[i] = byte(rng.Intn(256))
		}
		b.AppendObjectID(key, oid)
	}
}

// seedRandomColumn builds a collection of n random documents over fields g (the group
// key candidate) and v (the value candidate), enables a column store over both, and
// returns the collection. The dataset is large enough that the column path is the
// preferred plan.
func seedRandomColumn(t *testing.T, n int, seed int64) *Collection {
	t.Helper()
	c := newTestColl(t)
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < n; i++ {
		b := bson.NewBuilder().AppendInt32("_id", int32(i+1))
		randDocValue(b, rng, "g")
		randDocValue(b, rng, "v")
		if _, err := c.InsertOne(b.Build()); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := c.EnableColumnStore(colstore.ModeTransactional, []string{"g", "v"}); err != nil {
		t.Fatalf("enable column store: %v", err)
	}
	return c
}

// eligiblePipelines returns the set of vectorizable $group pipelines exercised by the
// differential test: every accumulator, both key shapes, and the constant-sum idiom.
func eligiblePipelines() []struct {
	name     string
	pipeline []bson.Raw
} {
	group := func(stage *bson.Builder) []bson.Raw {
		return []bson.Raw{bson.NewBuilder().AppendDocument("$group", stage.Build()).Build()}
	}
	acc := func(op, arg string) bson.Raw {
		return bson.NewBuilder().AppendString(op, arg).Build()
	}
	var out []struct {
		name     string
		pipeline []bson.Raw
	}
	add := func(name string, p []bson.Raw) {
		out = append(out, struct {
			name     string
			pipeline []bson.Raw
		}{name, p})
	}

	// Group by field g, every accumulator over v.
	add("by-field-all", group(bson.NewBuilder().
		AppendString("_id", "$g").
		AppendDocument("sum", acc("$sum", "$v")).
		AppendDocument("avg", acc("$avg", "$v")).
		AppendDocument("min", acc("$min", "$v")).
		AppendDocument("max", acc("$max", "$v")).
		AppendDocument("cnt", bson.NewBuilder().AppendDocument("$count", bson.NewBuilder().Build()).Build()).
		AppendDocument("n", bson.NewBuilder().AppendInt32("$sum", 1).Build())))

	// Whole-collection group (null _id).
	add("whole-collection", group(bson.NewBuilder().
		AppendNull("_id").
		AppendDocument("sum", acc("$sum", "$v")).
		AppendDocument("avg", acc("$avg", "$v")).
		AppendDocument("min", acc("$min", "$v")).
		AppendDocument("max", acc("$max", "$v")).
		AppendDocument("n", bson.NewBuilder().AppendInt64("$sum", 1).Build())))

	// Group by the value field itself, counting (a key that is also opaque/mixed).
	add("by-value-count", group(bson.NewBuilder().
		AppendString("_id", "$v").
		AppendDocument("cnt", bson.NewBuilder().AppendDocument("$count", bson.NewBuilder().Build()).Build())))

	// Min and max only, over a mixed-type field, to stress BSON-order comparison.
	add("minmax-mixed", group(bson.NewBuilder().
		AppendString("_id", "$g").
		AppendDocument("lo", acc("$min", "$v")).
		AppendDocument("hi", acc("$max", "$v"))))

	return out
}

// TestVectorGroupMatchesReconstruct is the differential test. For many random datasets
// and every eligible pipeline, the vectorized executor must return byte-identical
// documents to the reconstruct path.
func TestVectorGroupMatchesReconstruct(t *testing.T) {
	rounds := 25
	if testing.Short() {
		rounds = 5
	}
	for round := 0; round < rounds; round++ {
		seed := int64(round)*1000003 + 7
		c := seedRandomColumn(t, 1500, seed)
		for _, tc := range eligiblePipelines() {
			want := reconstructGroup(t, c, tc.pipeline)
			got := vectorizedGroup(t, c, tc.pipeline)
			assertSameDocs(t, fmt.Sprintf("round %d %s", round, tc.name), want, got)
		}
	}
}

// TestVectorGroupConstantSumTypes checks the $sum constant idiom reproduces the
// pipeline's integer width: an int32 constant stays int32 until it overflows to int64,
// and an int64 constant is int64 from the start.
func TestVectorGroupConstantSumTypes(t *testing.T) {
	c := seedRandomColumn(t, 1500, 999)
	pipeline := []bson.Raw{bson.NewBuilder().AppendDocument("$group", bson.NewBuilder().
		AppendString("_id", "$g").
		AppendDocument("c32", bson.NewBuilder().AppendInt32("$sum", 1).Build()).
		AppendDocument("c64", bson.NewBuilder().AppendInt64("$sum", 1).Build()).
		Build()).Build()}
	want := reconstructGroup(t, c, pipeline)
	got := vectorizedGroup(t, c, pipeline)
	assertSameDocs(t, "constant-sum", sortDocs(want), sortDocs(got))
}

// sortDocs returns the documents sorted by their bytes, so a comparison is independent
// of group order where the test does not care about it.
func sortDocs(docs []bson.Raw) []bson.Raw {
	out := make([]bson.Raw, len(docs))
	copy(out, docs)
	sort.Slice(out, func(i, j int) bool {
		return string(out[i]) < string(out[j])
	})
	return out
}
