package oracle

import "github.com/tamnd/doc/bson"

// This file holds the M4 conformance corpus: the aggregation pipeline. It
// exercises $group with its accumulators, $sort, $sortByCount, $bucket,
// $bucketAuto, $facet, $lookup, $graphLookup, $unionWith, $redact, and the
// expression language reached through $project and $addFields. Each probe runs an
// aggregate and is compared byte for byte against live MongoDB (spec 2061 doc 19
// §17, doc 12).
//
// Aggregation has two sources of legitimate nondeterminism that the corpus pins
// down so the engines cannot diverge for a reason that is not a bug:
//
//   - $group, $bucket output and the order MongoDB scans a collection are
//     unspecified, so every group probe sorts its input by _id first and its
//     output by the group key last.
//   - the array a $lookup or $graphLookup builds has no defined element order, so
//     those probes $unwind the array and sort the rows before projecting.
//
// $out, $merge, and $sample are deliberately absent. The writers need a writable
// transaction and a distinct target collection (the collection integration tests
// cover them), and $sample draws a random subset that no reference can match.

// aggOp builds an aggregate operation over the shared collection.
func aggOp(stages ...bson.Raw) Op {
	return Op{Kind: OpAggregate, Collection: coll, Pipeline: stages}
}

// st builds a stage document {name: <body>}.
func st(name string, body bson.Raw) bson.Raw {
	return bson.NewBuilder().AppendDocument(name, body).Build()
}

// stv builds a stage document {name: <value>}.
func stv(name string, v bson.RawValue) bson.Raw {
	return bson.NewBuilder().AppendValue(name, v).Build()
}

// accDoc builds a single-operator accumulator document {op: arg}.
func accDoc(op string, arg bson.RawValue) bson.Raw {
	return bson.NewBuilder().AppendValue(op, arg).Build()
}

// sortAsc builds {field: 1}; sortDesc builds {field: -1}.
func sortAsc(field string) bson.Raw  { return bson.NewBuilder().AppendInt32(field, 1).Build() }
func sortDesc(field string) bson.Raw { return bson.NewBuilder().AppendInt32(field, -1).Build() }

// m4SortID is the canonical {$sort: {_id: 1}} applied to pin output order.
func m4SortID() bson.Raw { return st("$sort", sortAsc("_id")) }

// ---- seed ----------------------------------------------------------------

// m4Seed inserts a small, deterministic document set the aggregation cases run
// over. Category a has three documents and b has one, so a $sortByCount has no
// tie. reportsTo forms a chain 4 -> 1, 3 -> 2 -> 1 -> 0 for $graphLookup. tags
// drive $unwind and the array expression operators.
func m4Seed() []Op {
	mk := func(id int32, cat string, n int32, reportsTo int32, tags bson.RawValue) bson.Raw {
		return bson.NewBuilder().
			AppendInt32("_id", id).
			AppendString("cat", cat).
			AppendInt32("n", n).
			AppendInt32("reportsTo", reportsTo).
			AppendValue("tags", tags).
			Build()
	}
	tagsOf := func(ss ...string) bson.RawValue {
		vals := make([]bson.RawValue, len(ss))
		for i, s := range ss {
			vals[i] = rawStr(s)
		}
		return mkArrayVal(vals)
	}
	return []Op{
		insOp(mk(1, "a", 10, 0, tagsOf("x", "y"))),
		insOp(mk(2, "a", 20, 1, tagsOf("y"))),
		insOp(mk(3, "a", 30, 2, tagsOf())),
		insOp(mk(4, "b", 40, 1, tagsOf("x", "z"))),
	}
}

// mkArrayVal wraps an array of element values as a single RawValue.
func mkArrayVal(vals []bson.RawValue) bson.RawValue {
	return rawVal(func(b *bson.Builder) { b.AppendArray("x", bson.BuildArray(vals...)) })
}

// ---- cases ---------------------------------------------------------------

// m4Cases returns the M4 aggregation conformance cases.
func m4Cases() []Case {
	var cases []Case
	seed := m4Seed()
	add := func(name string, probe Op) {
		cases = append(cases, Case{Name: "m4/" + name, Setup: seed, Probe: probe})
	}

	addGroupCases(add)
	addSortCases(add)
	addBucketCases(add)
	addJoinCases(add)
	addReshapeCases(add)
	addExprCases(add)

	return cases
}

// addGroupCases covers $group and its accumulators, each pinned deterministic by
// sorting the input by _id and the output by the group key.
func addGroupCases(add func(string, Op)) {
	// {$group: {_id: "$cat", <acc>: {...}}}
	groupBy := func(accField, op string, arg bson.RawValue) Op {
		g := bson.NewBuilder().
			AppendString("_id", "$cat").
			AppendDocument(accField, accDoc(op, arg)).
			Build()
		return aggOp(m4SortID(), st("$group", g), m4SortID())
	}

	add("group/sum-field", groupBy("v", "$sum", rawStr("$n")))
	add("group/sum-one", groupBy("v", "$sum", rawInt(1)))
	add("group/avg", groupBy("v", "$avg", rawStr("$n")))
	add("group/min", groupBy("v", "$min", rawStr("$n")))
	add("group/max", groupBy("v", "$max", rawStr("$n")))
	add("group/first", groupBy("v", "$first", rawStr("$n")))
	add("group/last", groupBy("v", "$last", rawStr("$n")))
	add("group/push", groupBy("v", "$push", rawStr("$n")))
	add("group/stddevpop", groupBy("v", "$stdDevPop", rawStr("$n")))
	add("group/stddevsamp", groupBy("v", "$stdDevSamp", rawStr("$n")))
	add("group/count-expr", groupBy("v", "$count", emptyDocVal()))

	// Group by a constant null to fold the whole collection into one row.
	allG := bson.NewBuilder().
		AppendNull("_id").
		AppendDocument("total", accDoc("$sum", rawStr("$n"))).
		AppendDocument("count", accDoc("$sum", rawInt(1))).
		Build()
	add("group/all", aggOp(st("$group", allG)))
}

// addSortCases covers $sort and $sortByCount.
func addSortCases(add func(string, Op)) {
	add("sort/asc", aggOp(st("$sort", sortAsc("n"))))
	add("sort/desc", aggOp(st("$sort", sortDesc("n"))))
	// Compound sort: cat ascending, n descending.
	compound := bson.NewBuilder().AppendInt32("cat", 1).AppendInt32("n", -1).Build()
	add("sort/compound", aggOp(st("$sort", compound)))
	add("sort/limit", aggOp(st("$sort", sortDesc("n")), stv("$limit", rawInt(2))))
	add("sort/skip-limit", aggOp(st("$sort", sortAsc("n")), stv("$skip", rawInt(1)), stv("$limit", rawInt(2))))
	add("sortbycount", aggOp(stv("$sortByCount", rawStr("$cat"))))
}

// addBucketCases covers $bucket and $bucketAuto. $bucket output is already sorted
// by the lower boundary; $bucketAuto by the bucket min.
func addBucketCases(add func(string, Op)) {
	boundaries := bson.BuildArray(rawInt(0), rawInt(15), rawInt(35), rawInt(100))
	output := bson.NewBuilder().AppendDocument("c", accDoc("$sum", rawInt(1))).Build()
	bucket := bson.NewBuilder().
		AppendString("groupBy", "$n").
		AppendArray("boundaries", boundaries).
		AppendString("default", "other").
		AppendDocument("output", output).
		Build()
	add("bucket", aggOp(st("$bucket", bucket)))

	auto := bson.NewBuilder().
		AppendString("groupBy", "$n").
		AppendInt32("buckets", 2).
		Build()
	add("bucketauto", aggOp(st("$bucketAuto", auto)))
}

// addJoinCases covers $lookup, $graphLookup, and $unionWith. The joined arrays are
// unwound and sorted so the row order is total.
func addJoinCases(add func(string, Op)) {
	// Self-join on cat, then flatten: each document lists the _ids sharing its cat.
	lookup := bson.NewBuilder().
		AppendString("from", coll).
		AppendString("localField", "cat").
		AppendString("foreignField", "cat").
		AppendString("as", "same").
		Build()
	lookupProj := bson.NewBuilder().AppendInt32("_id", 1).AppendString("sid", "$same._id").Build()
	lookupSort := bson.NewBuilder().AppendInt32("_id", 1).AppendInt32("sid", 1).Build()
	add("lookup/self", aggOp(
		st("$lookup", lookup),
		stv("$unwind", rawStr("$same")),
		st("$project", lookupProj),
		st("$sort", lookupSort),
	))

	// graphLookup the reporting chain from each document, count the reached nodes.
	graph := bson.NewBuilder().
		AppendString("from", coll).
		AppendString("startWith", "$reportsTo").
		AppendString("connectFromField", "reportsTo").
		AppendString("connectToField", "_id").
		AppendString("as", "chain").
		Build()
	graphProj := bson.NewBuilder().
		AppendInt32("_id", 1).
		AppendDocument("depth", accDoc("$size", rawStr("$chain"))).
		Build()
	add("graphlookup/depth", aggOp(st("$graphLookup", graph), st("$project", graphProj), m4SortID()))

	// unionWith the same collection doubles every document; group by _id to count.
	unionGroup := bson.NewBuilder().
		AppendString("_id", "$_id").
		AppendDocument("c", accDoc("$sum", rawInt(1))).
		Build()
	add("unionwith/self", aggOp(stv("$unionWith", rawStr(coll)), st("$group", unionGroup), m4SortID()))
}

// addReshapeCases covers $redact and $facet.
func addReshapeCases(add func(string, Op)) {
	// Keep documents unless cat is b.
	cond := bson.NewBuilder().
		AppendValue("if", opVal("$eq", rawStr("$cat"), rawStr("b"))).
		AppendString("then", "$$PRUNE").
		AppendString("else", "$$KEEP").
		Build()
	redact := bson.NewBuilder().AppendDocument("$cond", cond).Build()
	add("redact/prune-b", aggOp(stv("$redact", docVal(redact)), m4SortID()))

	// Two sub-pipelines over the same input: counts per cat, and the grand total.
	byCat := bson.BuildArray(docVal(stv("$sortByCount", rawStr("$cat"))))
	totalGroup := bson.NewBuilder().AppendNull("_id").AppendDocument("n", accDoc("$sum", rawStr("$n"))).Build()
	total := bson.BuildArray(docVal(st("$group", totalGroup)))
	facet := bson.NewBuilder().AppendArray("byCat", byCat).AppendArray("total", total).Build()
	add("facet", aggOp(st("$facet", facet)))
}

// addExprCases covers the expression language reached through $project and
// $addFields, one representative probe per operator family.
func addExprCases(add func(string, Op)) {
	// proj runs {$project: {_id: 1, r: <expr>}} then sorts by _id.
	proj := func(name string, expr bson.RawValue) {
		p := bson.NewBuilder().AppendInt32("_id", 1).AppendValue("r", expr).Build()
		add("expr/"+name, aggOp(st("$project", p), m4SortID()))
	}

	proj("add", opVal("$add", rawStr("$n"), rawInt(1)))
	proj("subtract", opVal("$subtract", rawStr("$n"), rawInt(5)))
	proj("multiply", opVal("$multiply", rawStr("$n"), rawInt(2)))
	proj("divide", opVal("$divide", rawStr("$n"), rawInt(10)))
	proj("mod", opVal("$mod", rawStr("$n"), rawInt(3)))
	proj("abs", op1Val("$abs", opVal("$subtract", rawInt(0), rawStr("$n"))))
	proj("ceil", op1Val("$ceil", rawDouble(2.3)))
	proj("floor", op1Val("$floor", rawDouble(2.7)))
	proj("round", op1Val("$round", rawDouble(2.5)))
	proj("pow", opVal("$pow", rawStr("$n"), rawInt(2)))

	proj("eq", opVal("$eq", rawStr("$cat"), rawStr("a")))
	proj("ne", opVal("$ne", rawStr("$cat"), rawStr("a")))
	proj("gt", opVal("$gt", rawStr("$n"), rawInt(20)))
	proj("lte", opVal("$lte", rawStr("$n"), rawInt(20)))
	proj("cmp", opVal("$cmp", rawStr("$n"), rawInt(20)))

	proj("concat", opVal("$concat", rawStr("$cat"), rawStr("-"), rawStr("$cat")))
	proj("toupper", op1Val("$toUpper", rawStr("$cat")))
	proj("tolower", op1Val("$toLower", rawStr("A")))
	proj("strlen", op1Val("$strLenCP", rawStr("$cat")))
	proj("substr", opVal("$substrCP", rawStr("hello"), rawInt(0), rawInt(2)))
	proj("split", opVal("$split", rawStr("a,b,c"), rawStr(",")))

	proj("size", op1Val("$size", rawStr("$tags")))
	proj("arrayelem", opVal("$arrayElemAt", rawStr("$tags"), rawInt(0)))
	proj("in", opVal("$in", rawStr("x"), rawStr("$tags")))
	proj("concatarrays", opVal("$concatArrays", rawStr("$tags"), valArr(rawStr("q"))))
	proj("reverse", op1Val("$reverseArray", rawStr("$tags")))

	proj("ifnull", opVal("$ifNull", rawStr("$missing"), rawInt(-1)))
	proj("cond", condVal(opVal("$gt", rawStr("$n"), rawInt(20)), rawStr("big"), rawStr("small")))

	proj("type", op1Val("$type", rawStr("$n")))
	proj("toString", op1Val("$toString", rawStr("$n")))
	proj("toInt", op1Val("$toInt", rawStr("$n")))

	// $addFields keeps every field and adds one.
	addF := bson.NewBuilder().AppendValue("doubled", opVal("$multiply", rawStr("$n"), rawInt(2))).Build()
	add("expr/addfields", aggOp(st("$addFields", addF), m4SortID()))
}

// ---- expression value builders ------------------------------------------

// opVal builds an operator expression value {op: [args...]}.
func opVal(op string, args ...bson.RawValue) bson.RawValue {
	d := bson.NewBuilder().AppendArray(op, bson.BuildArray(args...)).Build()
	return docVal(d)
}

// op1Val builds a single-argument operator expression {op: arg}.
func op1Val(op string, arg bson.RawValue) bson.RawValue {
	d := bson.NewBuilder().AppendValue(op, arg).Build()
	return docVal(d)
}

// condVal builds {$cond: [if, then, else]}.
func condVal(cond, then, els bson.RawValue) bson.RawValue {
	d := bson.NewBuilder().AppendArray("$cond", bson.BuildArray(cond, then, els)).Build()
	return docVal(d)
}

// valArr wraps element values as an array value.
func valArr(vals ...bson.RawValue) bson.RawValue {
	return rawVal(func(b *bson.Builder) { b.AppendArray("x", bson.BuildArray(vals...)) })
}

// emptyDocVal is the empty-document value, the argument shape of $count.
func emptyDocVal() bson.RawValue {
	return rawVal(func(b *bson.Builder) { b.AppendDocument("x", bson.NewBuilder().Build()) })
}
