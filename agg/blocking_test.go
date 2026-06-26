package agg

import (
	"testing"

	"github.com/tamnd/doc/bson"
)

// runEnv compiles and runs a pipeline with an environment, used by the
// cross-collection stages ($lookup, $graphLookup, $unionWith, $out, $merge).
func runEnv(t *testing.T, env *Env, input []bson.Raw, stages ...bson.Raw) []bson.Raw {
	t.Helper()
	p, err := Compile(stages)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	out, err := p.RunWith(input, 0, env)
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	return out
}

// dbl reads a double field.
func dbl(d bson.Raw, key string) float64 {
	v, _ := d.Lookup(key)
	return v.Double()
}

// str reads a string field.
func str(d bson.Raw, key string) string {
	v, _ := d.Lookup(key)
	return v.StringValue()
}

// arrLen returns the length of an array field.
func arrLen(t *testing.T, d bson.Raw, key string) int {
	t.Helper()
	v, ok := d.Lookup(key)
	if !ok {
		t.Fatalf("field %q missing", key)
	}
	els, err := arrayElements(v)
	if err != nil {
		t.Fatalf("array %q: %v", key, err)
	}
	return len(els)
}

// catDoc builds {_id, cat, n}.
func catDoc(id int32, cat string, n int32) bson.Raw {
	return bson.NewBuilder().AppendInt32("_id", id).AppendString("cat", cat).AppendInt32("n", n).Build()
}

// ---- $group --------------------------------------------------------------

func TestGroupSumCount(t *testing.T) {
	in := []bson.Raw{
		catDoc(1, "a", 10), catDoc(2, "a", 20), catDoc(3, "b", 5),
	}
	// {$group: {_id: "$cat", total: {$sum: "$n"}, count: {$sum: 1}}}
	spec := bson.NewBuilder().
		AppendString("_id", "$cat").
		AppendDocument("total", op1("$sum", mkString("$n"))).
		AppendDocument("count", op1("$sum", mkInt32(1))).
		Build()
	out := runPipe(t, in, stageD("$group", spec))
	if len(out) != 2 {
		t.Fatalf("groups = %d, want 2", len(out))
	}
	got := map[string]int32{}
	for _, d := range out {
		got[str(d, "_id")] = field32(d, "total")
	}
	if got["a"] != 30 || got["b"] != 5 {
		t.Fatalf("sums = %v", got)
	}
}

func TestGroupAvgMinMax(t *testing.T) {
	in := []bson.Raw{catDoc(1, "a", 10), catDoc(2, "a", 20), catDoc(3, "a", 30)}
	spec := bson.NewBuilder().
		AppendString("_id", "$cat").
		AppendDocument("avg", op1("$avg", mkString("$n"))).
		AppendDocument("min", op1("$min", mkString("$n"))).
		AppendDocument("max", op1("$max", mkString("$n"))).
		Build()
	out := runPipe(t, in, stageD("$group", spec))
	if len(out) != 1 {
		t.Fatalf("groups = %d", len(out))
	}
	if dbl(out[0], "avg") != 20 {
		t.Fatalf("avg = %v", dbl(out[0], "avg"))
	}
	if field32(out[0], "min") != 10 || field32(out[0], "max") != 30 {
		t.Fatalf("min/max = %v/%v", field32(out[0], "min"), field32(out[0], "max"))
	}
}

func TestGroupPushAddToSet(t *testing.T) {
	in := []bson.Raw{catDoc(1, "a", 1), catDoc(2, "a", 1), catDoc(3, "a", 2)}
	spec := bson.NewBuilder().
		AppendString("_id", "$cat").
		AppendDocument("all", op1("$push", mkString("$n"))).
		AppendDocument("uniq", op1("$addToSet", mkString("$n"))).
		Build()
	out := runPipe(t, in, stageD("$group", spec))
	if arrLen(t, out[0], "all") != 3 {
		t.Fatalf("push len = %d", arrLen(t, out[0], "all"))
	}
	if arrLen(t, out[0], "uniq") != 2 {
		t.Fatalf("addToSet len = %d", arrLen(t, out[0], "uniq"))
	}
}

func TestGroupFirstLast(t *testing.T) {
	in := []bson.Raw{catDoc(1, "a", 10), catDoc(2, "a", 20), catDoc(3, "a", 30)}
	spec := bson.NewBuilder().
		AppendString("_id", "$cat").
		AppendDocument("first", op1("$first", mkString("$n"))).
		AppendDocument("last", op1("$last", mkString("$n"))).
		Build()
	out := runPipe(t, in, stageD("$group", spec))
	if field32(out[0], "first") != 10 || field32(out[0], "last") != 30 {
		t.Fatalf("first/last = %v/%v", field32(out[0], "first"), field32(out[0], "last"))
	}
}

func TestGroupNumericKeyEquality(t *testing.T) {
	// 1 (int32), 1.0 (double), NumberLong(1) all group together.
	in := []bson.Raw{
		bson.NewBuilder().AppendInt32("_id", 1).AppendInt32("k", 1).Build(),
		bson.NewBuilder().AppendInt32("_id", 2).AppendDouble("k", 1.0).Build(),
		bson.NewBuilder().AppendInt32("_id", 3).AppendInt64("k", 1).Build(),
	}
	spec := bson.NewBuilder().
		AppendString("_id", "$k").
		AppendDocument("c", op1("$sum", mkInt32(1))).
		Build()
	out := runPipe(t, in, stageD("$group", spec))
	if len(out) != 1 {
		t.Fatalf("numeric keys grouped into %d, want 1", len(out))
	}
	if field32(out[0], "c") != 3 {
		t.Fatalf("count = %d", field32(out[0], "c"))
	}
}

func TestGroupNullKey(t *testing.T) {
	in := []bson.Raw{
		bson.NewBuilder().AppendInt32("_id", 1).Build(), // missing k
		bson.NewBuilder().AppendInt32("_id", 2).AppendNull("k").Build(),
	}
	spec := bson.NewBuilder().
		AppendString("_id", "$k").
		AppendDocument("c", op1("$sum", mkInt32(1))).
		Build()
	out := runPipe(t, in, stageD("$group", spec))
	if len(out) != 1 || field32(out[0], "c") != 2 {
		t.Fatalf("missing and null should share a null key: %v", out)
	}
}

func TestGroupStdDev(t *testing.T) {
	in := []bson.Raw{catDoc(1, "a", 2), catDoc(2, "a", 4), catDoc(3, "a", 4),
		catDoc(4, "a", 4), catDoc(5, "a", 5), catDoc(6, "a", 5),
		catDoc(7, "a", 7), catDoc(8, "a", 9)}
	spec := bson.NewBuilder().
		AppendString("_id", "$cat").
		AppendDocument("pop", op1("$stdDevPop", mkString("$n"))).
		Build()
	out := runPipe(t, in, stageD("$group", spec))
	if got := dbl(out[0], "pop"); got < 1.99 || got > 2.01 {
		t.Fatalf("stdDevPop = %v, want 2", got)
	}
}

func TestGroupMergeObjects(t *testing.T) {
	d1 := bson.NewBuilder().AppendInt32("_id", 1).AppendString("g", "x").
		AppendDocument("o", bson.NewBuilder().AppendInt32("a", 1).Build()).Build()
	d2 := bson.NewBuilder().AppendInt32("_id", 2).AppendString("g", "x").
		AppendDocument("o", bson.NewBuilder().AppendInt32("b", 2).Build()).Build()
	spec := bson.NewBuilder().
		AppendString("_id", "$g").
		AppendDocument("m", op1("$mergeObjects", mkString("$o"))).
		Build()
	out := runPipe(t, []bson.Raw{d1, d2}, stageD("$group", spec))
	m, _ := out[0].Lookup("m")
	md := m.Document()
	if _, ok := md.Lookup("a"); !ok {
		t.Fatal("merged missing a")
	}
	if _, ok := md.Lookup("b"); !ok {
		t.Fatal("merged missing b")
	}
}

func TestGroupTopBottomN(t *testing.T) {
	in := []bson.Raw{catDoc(1, "a", 30), catDoc(2, "a", 10), catDoc(3, "a", 20)}
	// {$top: {sortBy: {n: -1}, output: "$n"}}
	topBody := bson.NewBuilder().
		AppendDocument("sortBy", bson.NewBuilder().AppendInt32("n", -1).Build()).
		AppendString("output", "$n").Build()
	// {$bottomN: {n: 2, sortBy: {n: -1}, output: "$n"}}
	botBody := bson.NewBuilder().
		AppendInt32("n", 2).
		AppendDocument("sortBy", bson.NewBuilder().AppendInt32("n", -1).Build()).
		AppendString("output", "$n").Build()
	spec := bson.NewBuilder().
		AppendString("_id", "$cat").
		AppendDocument("top", op1d("$top", topBody)).
		AppendDocument("bot", op1d("$bottomN", botBody)).
		Build()
	out := runPipe(t, in, stageD("$group", spec))
	if field32(out[0], "top") != 30 {
		t.Fatalf("$top = %d, want 30", field32(out[0], "top"))
	}
	if arrLen(t, out[0], "bot") != 2 {
		t.Fatalf("$bottomN len = %d", arrLen(t, out[0], "bot"))
	}
}

// op1 builds {name: arg} as a single-operator document.
func op1(name string, arg bson.RawValue) bson.Raw {
	return bson.NewBuilder().AppendValue(name, arg).Build()
}

// op1d builds {name: <document>}.
func op1d(name string, body bson.Raw) bson.Raw {
	return bson.NewBuilder().AppendDocument(name, body).Build()
}

// ---- $sort ---------------------------------------------------------------

func TestSortAscDesc(t *testing.T) {
	in := []bson.Raw{abDoc(1, 30, 0), abDoc(2, 10, 0), abDoc(3, 20, 0)}
	asc := stageD("$sort", bson.NewBuilder().AppendInt32("a", 1).Build())
	out := runPipe(t, in, asc)
	if field32(out[0], "a") != 10 || field32(out[2], "a") != 30 {
		t.Fatalf("asc order wrong: %v", out)
	}
	desc := stageD("$sort", bson.NewBuilder().AppendInt32("a", -1).Build())
	out = runPipe(t, in, desc)
	if field32(out[0], "a") != 30 {
		t.Fatalf("desc order wrong: %v", out)
	}
}

func TestSortStable(t *testing.T) {
	// Equal keys keep input order.
	in := []bson.Raw{abDoc(1, 5, 0), abDoc(2, 5, 0), abDoc(3, 5, 0)}
	out := runPipe(t, in, stageD("$sort", bson.NewBuilder().AppendInt32("a", 1).Build()))
	if field32(out[0], "_id") != 1 || field32(out[2], "_id") != 3 {
		t.Fatalf("sort not stable: %v", out)
	}
}

func TestSortLimitTopK(t *testing.T) {
	in := []bson.Raw{abDoc(1, 30, 0), abDoc(2, 10, 0), abDoc(3, 20, 0), abDoc(4, 40, 0)}
	out := runPipe(t, in,
		stageD("$sort", bson.NewBuilder().AppendInt32("a", -1).Build()),
		stageV("$limit", mkInt32(2)))
	if len(out) != 2 || field32(out[0], "a") != 40 || field32(out[1], "a") != 30 {
		t.Fatalf("top-K wrong: %v", out)
	}
}

// ---- $sortByCount --------------------------------------------------------

func TestSortByCount(t *testing.T) {
	in := []bson.Raw{catDoc(1, "a", 0), catDoc(2, "b", 0), catDoc(3, "a", 0), catDoc(4, "a", 0)}
	out := runPipe(t, in, stageV("$sortByCount", mkString("$cat")))
	if len(out) != 2 {
		t.Fatalf("groups = %d", len(out))
	}
	if str(out[0], "_id") != "a" || field32(out[0], "count") != 3 {
		t.Fatalf("most frequent first: %v", out[0])
	}
}

// ---- $sample -------------------------------------------------------------

func TestSample(t *testing.T) {
	in := []bson.Raw{abDoc(1, 0, 0), abDoc(2, 0, 0), abDoc(3, 0, 0), abDoc(4, 0, 0), abDoc(5, 0, 0)}
	out := runPipe(t, in, stageD("$sample", bson.NewBuilder().AppendInt32("size", 3).Build()))
	if len(out) != 3 {
		t.Fatalf("sample size = %d, want 3", len(out))
	}
}

func TestSampleLargerThanInput(t *testing.T) {
	in := []bson.Raw{abDoc(1, 0, 0), abDoc(2, 0, 0)}
	out := runPipe(t, in, stageD("$sample", bson.NewBuilder().AppendInt32("size", 10).Build()))
	if len(out) != 2 {
		t.Fatalf("sample = %d, want 2 (clamped to input)", len(out))
	}
}

// ---- $bucket / $bucketAuto ----------------------------------------------

func TestBucket(t *testing.T) {
	in := []bson.Raw{
		valDoc(1, 1), valDoc(2, 5), valDoc(3, 12), valDoc(4, 22), valDoc(5, 99),
	}
	body := bson.NewBuilder().
		AppendString("groupBy", "$v").
		AppendArray("boundaries", bson.BuildArray(mkInt32(0), mkInt32(10), mkInt32(20), mkInt32(30))).
		AppendString("default", "other").
		AppendDocument("output", bson.NewBuilder().
			AppendDocument("c", op1("$sum", mkInt32(1))).Build()).
		Build()
	out := runPipe(t, in, stageD("$bucket", body))
	// buckets: [0,10)=2, [10,20)=1, [20,30)=1, default=1
	if len(out) != 4 {
		t.Fatalf("buckets = %d, want 4", len(out))
	}
	if field32(out[0], "c") != 2 {
		t.Fatalf("first bucket count = %d", field32(out[0], "c"))
	}
}

func TestBucketAuto(t *testing.T) {
	in := []bson.Raw{valDoc(1, 1), valDoc(2, 2), valDoc(3, 3), valDoc(4, 4),
		valDoc(5, 5), valDoc(6, 6), valDoc(7, 7), valDoc(8, 8)}
	body := bson.NewBuilder().
		AppendString("groupBy", "$v").
		AppendInt32("buckets", 4).
		Build()
	out := runPipe(t, in, stageD("$bucketAuto", body))
	if len(out) != 4 {
		t.Fatalf("auto buckets = %d, want 4", len(out))
	}
}

// valDoc builds {_id, v}.
func valDoc(id, v int32) bson.Raw {
	return bson.NewBuilder().AppendInt32("_id", id).AppendInt32("v", v).Build()
}

// ---- $facet --------------------------------------------------------------

func TestFacet(t *testing.T) {
	in := []bson.Raw{catDoc(1, "a", 10), catDoc(2, "a", 20), catDoc(3, "b", 5)}
	byCat := bson.BuildArray(mkDoc(stageV("$sortByCount", mkString("$cat"))))
	total := bson.BuildArray(mkDoc(stageD("$group", bson.NewBuilder().
		AppendNull("_id").
		AppendDocument("sum", op1("$sum", mkString("$n"))).Build())))
	body := bson.NewBuilder().
		AppendArray("byCat", byCat).
		AppendArray("total", total).
		Build()
	out := runPipe(t, in, stageD("$facet", body))
	if len(out) != 1 {
		t.Fatalf("facet emits one doc, got %d", len(out))
	}
	if arrLen(t, out[0], "byCat") != 2 {
		t.Fatalf("byCat groups = %d", arrLen(t, out[0], "byCat"))
	}
	if arrLen(t, out[0], "total") != 1 {
		t.Fatalf("total = %d", arrLen(t, out[0], "total"))
	}
}

// ---- $lookup -------------------------------------------------------------

func TestLookupEquality(t *testing.T) {
	orders := []bson.Raw{
		bson.NewBuilder().AppendInt32("_id", 1).AppendInt32("item", 100).Build(),
		bson.NewBuilder().AppendInt32("_id", 2).AppendInt32("item", 200).Build(),
	}
	inventory := []bson.Raw{
		bson.NewBuilder().AppendInt32("_id", 100).AppendString("sku", "x").Build(),
		bson.NewBuilder().AppendInt32("_id", 200).AppendString("sku", "y").Build(),
	}
	env := &Env{Read: func(string) ([]bson.Raw, error) { return inventory, nil }}
	body := bson.NewBuilder().
		AppendString("from", "inventory").
		AppendString("localField", "item").
		AppendString("foreignField", "_id").
		AppendString("as", "docs").
		Build()
	out := runEnv(t, env, orders, stageD("$lookup", body))
	if arrLen(t, out[0], "docs") != 1 {
		t.Fatalf("lookup join len = %d", arrLen(t, out[0], "docs"))
	}
}

func TestLookupPipelineWithLet(t *testing.T) {
	orders := []bson.Raw{
		bson.NewBuilder().AppendInt32("_id", 1).AppendInt32("qty", 5).Build(),
	}
	stock := []bson.Raw{
		bson.NewBuilder().AppendInt32("_id", 100).AppendInt32("avail", 3).Build(),
		bson.NewBuilder().AppendInt32("_id", 101).AppendInt32("avail", 9).Build(),
	}
	env := &Env{Read: func(string) ([]bson.Raw, error) { return stock, nil }}
	// pipeline: [{$match: {$expr: {$gte: ["$avail", "$$q"]}}}]
	sub := bson.BuildArray(mkDoc(stageD("$match", bson.NewBuilder().
		AppendValue("$expr", op("$gte", mkString("$avail"), mkString("$$q"))).Build())))
	body := bson.NewBuilder().
		AppendString("from", "stock").
		AppendDocument("let", bson.NewBuilder().AppendString("q", "$qty").Build()).
		AppendArray("pipeline", sub).
		AppendString("as", "ok").
		Build()
	out := runEnv(t, env, orders, stageD("$lookup", body))
	if arrLen(t, out[0], "ok") != 1 {
		t.Fatalf("pipeline lookup len = %d, want 1 (only avail>=5)", arrLen(t, out[0], "ok"))
	}
}

func TestLookupNoEnv(t *testing.T) {
	orders := []bson.Raw{bson.NewBuilder().AppendInt32("_id", 1).AppendInt32("item", 1).Build()}
	body := bson.NewBuilder().
		AppendString("from", "x").AppendString("localField", "item").
		AppendString("foreignField", "_id").AppendString("as", "d").Build()
	p, err := Compile([]bson.Raw{stageD("$lookup", body)})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, err := p.Run(orders, 0); err == nil {
		t.Fatal("lookup without env should error")
	}
}

// ---- $graphLookup --------------------------------------------------------

func TestGraphLookup(t *testing.T) {
	// chain: 1 -> 2 -> 3
	people := []bson.Raw{
		person(1, "a", 0),
		person(2, "b", 1),
		person(3, "c", 2),
	}
	env := &Env{Read: func(string) ([]bson.Raw, error) { return people, nil }}
	body := bson.NewBuilder().
		AppendString("from", "people").
		AppendString("startWith", "$reportsTo").
		AppendString("connectFromField", "reportsTo").
		AppendString("connectToField", "_id").
		AppendString("as", "chain").
		Build()
	// start from person 3, should reach 2 and 1
	out := runEnv(t, env, []bson.Raw{person(3, "c", 2)}, stageD("$graphLookup", body))
	if arrLen(t, out[0], "chain") != 2 {
		t.Fatalf("graph chain len = %d, want 2", arrLen(t, out[0], "chain"))
	}
}

func TestGraphLookupMaxDepth(t *testing.T) {
	people := []bson.Raw{person(1, "a", 0), person(2, "b", 1), person(3, "c", 2)}
	env := &Env{Read: func(string) ([]bson.Raw, error) { return people, nil }}
	body := bson.NewBuilder().
		AppendString("from", "people").
		AppendString("startWith", "$reportsTo").
		AppendString("connectFromField", "reportsTo").
		AppendString("connectToField", "_id").
		AppendInt32("maxDepth", 0).
		AppendString("as", "chain").
		Build()
	out := runEnv(t, env, []bson.Raw{person(3, "c", 2)}, stageD("$graphLookup", body))
	if arrLen(t, out[0], "chain") != 1 {
		t.Fatalf("maxDepth 0 chain len = %d, want 1", arrLen(t, out[0], "chain"))
	}
}

// person builds {_id, name, reportsTo}.
func person(id int32, name string, reportsTo int32) bson.Raw {
	return bson.NewBuilder().AppendInt32("_id", id).AppendString("name", name).
		AppendInt32("reportsTo", reportsTo).Build()
}

// ---- $unionWith ----------------------------------------------------------

func TestUnionWith(t *testing.T) {
	a := []bson.Raw{abDoc(1, 0, 0), abDoc(2, 0, 0)}
	b := []bson.Raw{abDoc(3, 0, 0)}
	env := &Env{Read: func(string) ([]bson.Raw, error) { return b, nil }}
	out := runEnv(t, env, a, stageV("$unionWith", mkString("other")))
	if len(out) != 3 {
		t.Fatalf("union len = %d, want 3", len(out))
	}
}

func TestUnionWithPipeline(t *testing.T) {
	a := []bson.Raw{abDoc(1, 0, 0)}
	b := []bson.Raw{abDoc(2, 5, 0), abDoc(3, 50, 0)}
	env := &Env{Read: func(string) ([]bson.Raw, error) { return b, nil }}
	sub := bson.BuildArray(mkDoc(stageD("$match",
		bson.NewBuilder().AppendDocument("a", bson.NewBuilder().AppendInt32("$gte", 10).Build()).Build())))
	body := bson.NewBuilder().
		AppendString("coll", "other").
		AppendArray("pipeline", sub).
		Build()
	out := runEnv(t, env, a, stageD("$unionWith", body))
	if len(out) != 2 {
		t.Fatalf("union+pipeline len = %d, want 2", len(out))
	}
}

// ---- $redact -------------------------------------------------------------

func TestRedactPrune(t *testing.T) {
	in := []bson.Raw{
		bson.NewBuilder().AppendInt32("_id", 1).AppendString("level", "public").Build(),
		bson.NewBuilder().AppendInt32("_id", 2).AppendString("level", "secret").Build(),
	}
	// $cond: level == "secret" -> PRUNE else KEEP
	expr := op1d("$cond", bson.NewBuilder().
		AppendValue("if", op("$eq", mkString("$level"), mkString("secret"))).
		AppendString("then", "$$PRUNE").
		AppendString("else", "$$KEEP").Build())
	out := runPipe(t, in, stageV("$redact", mkDoc(expr)))
	if len(out) != 1 || field32(out[0], "_id") != 1 {
		t.Fatalf("redact should drop the secret doc: %v", out)
	}
}

func TestRedactDescend(t *testing.T) {
	// Nested subdoc with a secret field that gets pruned while the parent descends.
	inner := bson.NewBuilder().AppendString("level", "secret").AppendInt32("v", 9).Build()
	doc := bson.NewBuilder().AppendInt32("_id", 1).AppendString("level", "public").
		AppendDocument("sub", inner).Build()
	expr := op1d("$cond", bson.NewBuilder().
		AppendValue("if", op("$eq", mkString("$level"), mkString("secret"))).
		AppendString("then", "$$PRUNE").
		AppendString("else", "$$DESCEND").Build())
	out := runPipe(t, []bson.Raw{doc}, stageV("$redact", mkDoc(expr)))
	if len(out) != 1 {
		t.Fatalf("parent should survive: %v", out)
	}
	if _, ok := out[0].Lookup("sub"); ok {
		t.Fatal("secret subdoc should be pruned")
	}
}

// ---- $out / $merge -------------------------------------------------------

func TestOutWrite(t *testing.T) {
	in := []bson.Raw{abDoc(1, 10, 0), abDoc(2, 20, 0)}
	var written WriteRequest
	env := &Env{Write: func(req WriteRequest) error { written = req; return nil }}
	out := runEnv(t, env, in, stageV("$out", mkString("target")))
	if len(out) != 0 {
		t.Fatalf("$out emits nothing downstream, got %d", len(out))
	}
	if !written.Replace || written.Coll != "target" || len(written.Docs) != 2 {
		t.Fatalf("write request = %+v", written)
	}
}

func TestMergeWrite(t *testing.T) {
	in := []bson.Raw{abDoc(1, 10, 0)}
	var written WriteRequest
	env := &Env{Write: func(req WriteRequest) error { written = req; return nil }}
	body := bson.NewBuilder().
		AppendString("into", "target").
		AppendString("whenMatched", "replace").
		AppendString("whenNotMatched", "insert").
		Build()
	runEnv(t, env, in, stageD("$merge", body))
	if written.Replace {
		t.Fatal("$merge is not a replace")
	}
	if written.WhenMatched != "replace" || written.WhenNotMatched != "insert" {
		t.Fatalf("merge actions = %+v", written)
	}
	if len(written.On) != 1 || written.On[0] != "_id" {
		t.Fatalf("default on = %v", written.On)
	}
}

// ---- optimizer -----------------------------------------------------------

func TestOptimizeCoalesceLimits(t *testing.T) {
	in := []bson.Raw{abDoc(1, 0, 0), abDoc(2, 0, 0), abDoc(3, 0, 0)}
	out := runPipe(t, in, stageV("$limit", mkInt32(2)), stageV("$limit", mkInt32(1)))
	if len(out) != 1 {
		t.Fatalf("coalesced limit = %d, want 1", len(out))
	}
}

func TestOptimizeMergeReshapes(t *testing.T) {
	in := []bson.Raw{abDoc(1, 0, 0)}
	s1 := stageD("$addFields", bson.NewBuilder().AppendInt32("x", 1).Build())
	s2 := stageD("$set", bson.NewBuilder().AppendInt32("y", 2).Build())
	out := runPipe(t, in, s1, s2)
	if field32(out[0], "x") != 1 || field32(out[0], "y") != 2 {
		t.Fatalf("merged reshape lost a field: %v", out[0])
	}
}

func TestOptimizePushMatch(t *testing.T) {
	// $match on a (not introduced by $addFields) should still produce correct results
	// after being pushed before the reshape.
	in := []bson.Raw{abDoc(1, 5, 0), abDoc(2, 15, 0)}
	add := stageD("$addFields", bson.NewBuilder().AppendInt32("z", 7).Build())
	m := stageD("$match", bson.NewBuilder().
		AppendDocument("a", bson.NewBuilder().AppendInt32("$gte", 10).Build()).Build())
	out := runPipe(t, in, add, m)
	if len(out) != 1 || field32(out[0], "_id") != 2 {
		t.Fatalf("push-match changed results: %v", out)
	}
	if field32(out[0], "z") != 7 {
		t.Fatal("reshape field lost after push")
	}
}
