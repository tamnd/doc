package oracle

import (
	"fmt"

	"github.com/tamnd/doc/bson"
)

// This file holds the M3-a conformance corpus: the find surface beyond _id point
// lookups. It exercises the comparison, logical, element, and array operators, the
// null/missing rules, dotted-path resolution, and the sort/projection/skip/limit
// shaping stages, each probe compared byte for byte against live MongoDB (spec
// 2061 doc 19 §17).
//
// Every find probe carries an explicit {_id:1} sort so the result order is total
// and deterministic: MongoDB leaves the order of a collection scan unspecified, so
// pinning a sort on the unique _id removes the only source of legitimate
// divergence between the two engines.

// rawVal mints a standalone RawValue of a given type through a one-field document.
func rawVal(build func(*bson.Builder)) bson.RawValue {
	b := bson.NewBuilder()
	build(b)
	d := b.Build()
	v, _ := d.Lookup("x")
	return v
}

func rawInt(n int32) bson.RawValue  { return rawVal(func(b *bson.Builder) { b.AppendInt32("x", n) }) }
func rawStr(s string) bson.RawValue { return rawVal(func(b *bson.Builder) { b.AppendString("x", s) }) }

// docVal wraps a document as a RawValue, for building an array of sub-filters.
func docVal(d bson.Raw) bson.RawValue {
	return rawVal(func(b *bson.Builder) { b.AppendDocument("x", d) })
}

// sortByID is the canonical deterministic sort applied to every find probe.
var sortByID = bson.NewBuilder().AppendInt32("_id", 1).Build()

func findSorted(f bson.Raw) Op {
	return Op{Kind: OpFind, Collection: coll, Filter: f, Sort: sortByID}
}

// cmpFilter builds {field: {op: n}} for a comparison operator.
func cmpFilter(field, op string, n int32) bson.Raw {
	sub := bson.NewBuilder().AppendInt32(op, n).Build()
	return bson.NewBuilder().AppendDocument(field, sub).Build()
}

// listFilter builds {field: {op: [vals...]}} for $in / $nin / $all.
func listFilter(field, op string, vals ...bson.RawValue) bson.Raw {
	arr := bson.BuildArray(vals...)
	sub := bson.NewBuilder().AppendArray(op, arr).Build()
	return bson.NewBuilder().AppendDocument(field, sub).Build()
}

// existsFilter builds {field: {$exists: want}}.
func existsFilter(field string, want bool) bson.Raw {
	sub := bson.NewBuilder().AppendBoolean("$exists", want).Build()
	return bson.NewBuilder().AppendDocument(field, sub).Build()
}

// typeFilter builds {field: {$type: alias}}.
func typeFilter(field, alias string) bson.Raw {
	sub := bson.NewBuilder().AppendString("$type", alias).Build()
	return bson.NewBuilder().AppendDocument(field, sub).Build()
}

// sizeFilter builds {field: {$size: n}}.
func sizeFilter(field string, n int32) bson.Raw {
	sub := bson.NewBuilder().AppendInt32("$size", n).Build()
	return bson.NewBuilder().AppendDocument(field, sub).Build()
}

// people is the M3-a dataset, inserted in _id order. The values are chosen so age
// has a missing case (id 8) and a null case (id 7), scores spans empty and
// multi-element arrays, and the string fields are all distinct.
func people() []Op {
	type p struct {
		id     int32
		name   string
		city   string
		age    int32
		hasAge bool
		nilAge bool
		scores []int32
		hasSc  bool
		tags   []string
	}
	rows := []p{
		{1, "ada", "london", 36, true, false, []int32{10, 20, 30}, true, []string{"a", "b"}},
		{2, "bob", "paris", 42, true, false, []int32{40}, true, []string{"b", "c"}},
		{3, "cy", "london", 29, true, false, []int32{15, 25}, true, []string{"a"}},
		{4, "dee", "berlin", 51, true, false, []int32{}, true, []string{"c", "d"}},
		{5, "eve", "paris", 36, true, false, []int32{5, 55}, true, []string{}},
		{6, "fay", "berlin", 23, true, false, []int32{60, 70}, true, []string{"a", "b", "c"}},
		{7, "guy", "london", 0, false, true, []int32{99}, true, []string{"z"}},
		{8, "hal", "tokyo", 0, false, false, nil, false, []string{"a", "z"}},
	}
	var ops []Op
	for _, r := range rows {
		b := bson.NewBuilder().AppendInt32("_id", r.id).
			AppendString("name", r.name).
			AppendString("city", r.city)
		switch {
		case r.hasAge:
			b.AppendInt32("age", r.age)
		case r.nilAge:
			b.AppendNull("age")
		}
		if r.hasSc {
			vals := make([]bson.RawValue, len(r.scores))
			for i, s := range r.scores {
				vals[i] = rawInt(s)
			}
			b.AppendArray("scores", bson.BuildArray(vals...))
		}
		tagVals := make([]bson.RawValue, len(r.tags))
		for i, t := range r.tags {
			tagVals[i] = rawStr(t)
		}
		b.AppendArray("tags", bson.BuildArray(tagVals...))
		ops = append(ops, insOp(b.Build()))
	}
	return ops
}

// m3Cases returns the M3-a find conformance cases.
func m3Cases() []Case {
	var cases []Case
	add := func(c Case) { cases = append(cases, c) }
	setup := people()

	probe := func(name string, p Op) {
		add(Case{Name: name, Setup: setup, Probe: p})
	}

	// Comparison operators on age across the bracket, as both find and count.
	for _, op := range []string{"$gt", "$gte", "$lt", "$lte", "$ne", "$eq"} {
		for _, n := range []int32{23, 30, 36, 42, 51, 100} {
			f := cmpFilter("age", op, n)
			probe(fmt.Sprintf("cmp/age%s%d/find", op, n), findSorted(f))
			probe(fmt.Sprintf("cmp/age%s%d/count", op, n), countOp(f))
		}
	}

	// Comparison against a string field (binary collation).
	for _, op := range []string{"$gt", "$lt", "$gte", "$lte"} {
		f := bson.NewBuilder().AppendDocument("name",
			bson.NewBuilder().AppendString(op, "cy").Build()).Build()
		probe(fmt.Sprintf("cmp/name%s/find", op), findSorted(f))
	}

	// Implicit equality on a present field, on city.
	for _, c := range []string{"london", "paris", "berlin", "tokyo", "rome"} {
		f := bson.NewBuilder().AppendString("city", c).Build()
		probe("eq/city-"+c+"/find", findSorted(f))
		probe("eq/city-"+c+"/count", countOp(f))
	}

	// $in / $nin over ages and cities.
	probe("in/age/find", findSorted(listFilter("age", "$in", rawInt(23), rawInt(36), rawInt(51))))
	probe("nin/age/find", findSorted(listFilter("age", "$nin", rawInt(23), rawInt(36), rawInt(51))))
	probe("in/city/find", findSorted(listFilter("city", "$in", rawStr("london"), rawStr("tokyo"))))
	probe("in/with-null/find", findSorted(listFilter("age", "$in", rawInt(42), rawVal(func(b *bson.Builder) { b.AppendNull("x") }))))

	// null vs missing: {age:null} matches the explicit null and the missing field.
	probe("null/age-find", findSorted(bson.NewBuilder().AppendNull("age").Build()))
	probe("null/age-count", countOp(bson.NewBuilder().AppendNull("age").Build()))
	// {age:{$ne:null}} excludes both.
	neNull := bson.NewBuilder().AppendDocument("age",
		bson.NewBuilder().AppendNull("$ne").Build()).Build()
	probe("ne-null/age-find", findSorted(neNull))

	// $exists true/false on age (present, null, missing).
	probe("exists/age-true/find", findSorted(existsFilter("age", true)))
	probe("exists/age-false/find", findSorted(existsFilter("age", false)))
	probe("exists/scores-false/find", findSorted(existsFilter("scores", false)))

	// $type on several fields.
	for _, alias := range []string{"int", "string", "array", "null", "number", "double"} {
		probe("type/age-"+alias+"/find", findSorted(typeFilter("age", alias)))
	}
	probe("type/tags-array/find", findSorted(typeFilter("tags", "array")))

	// Array operators: equality fan-out, $size, $all, $elemMatch.
	probe("array/tag-eq-a/find", findSorted(bson.NewBuilder().AppendString("tags", "a").Build()))
	probe("array/score-eq-40/find", findSorted(bson.NewBuilder().AppendInt32("scores", 40).Build()))
	probe("array/size-tags-2/find", findSorted(sizeFilter("tags", 2)))
	probe("array/size-scores-0/find", findSorted(sizeFilter("scores", 0)))
	probe("array/all-tags-ab/find", findSorted(listFilter("tags", "$all", rawStr("a"), rawStr("b"))))
	probe("array/all-tags-ac/find", findSorted(listFilter("tags", "$all", rawStr("a"), rawStr("c"))))

	// $elemMatch operator form on scores: an element strictly between lo and hi.
	elemMatch := func(lo, hi int32) bson.Raw {
		inner := bson.NewBuilder().AppendInt32("$gt", lo).AppendInt32("$lt", hi).Build()
		sub := bson.NewBuilder().AppendDocument("$elemMatch", inner).Build()
		return bson.NewBuilder().AppendDocument("scores", sub).Build()
	}
	probe("array/elemmatch-20-50/find", findSorted(elemMatch(20, 50)))
	probe("array/elemmatch-90-100/find", findSorted(elemMatch(90, 100)))

	// Logical operators.
	orF := bson.NewBuilder().AppendArray("$or", bson.BuildArray(
		docVal(bson.NewBuilder().AppendString("city", "berlin").Build()),
		docVal(cmpFilter("age", "$gt", 40)),
	)).Build()
	probe("logical/or/find", findSorted(orF))

	andF := bson.NewBuilder().AppendArray("$and", bson.BuildArray(
		docVal(bson.NewBuilder().AppendString("city", "london").Build()),
		docVal(cmpFilter("age", "$lt", 35)),
	)).Build()
	probe("logical/and/find", findSorted(andF))

	norF := bson.NewBuilder().AppendArray("$nor", bson.BuildArray(
		docVal(bson.NewBuilder().AppendString("city", "paris").Build()),
	)).Build()
	probe("logical/nor/find", findSorted(norF))

	// $not negating a comparison: its argument is the operator document itself.
	notGt := bson.NewBuilder().AppendDocument("age",
		bson.NewBuilder().AppendDocument("$not",
			bson.NewBuilder().AppendInt32("$gt", 40).Build()).Build()).Build()
	probe("logical/not-gt40/find", findSorted(notGt))

	// Implicit AND of two fields.
	twoField := bson.NewBuilder().AppendString("city", "london").AppendInt32("age", 36).Build()
	probe("and/two-field/find", findSorted(twoField))

	// Sort variants (each tie-broken by the unique fields used).
	probe("sort/name-asc", Op{Kind: OpFind, Collection: coll, Sort: bson.NewBuilder().AppendInt32("name", 1).Build()})
	probe("sort/name-desc", Op{Kind: OpFind, Collection: coll, Sort: bson.NewBuilder().AppendInt32("name", -1).Build()})
	probe("sort/city-then-id", Op{Kind: OpFind, Collection: coll,
		Sort: bson.NewBuilder().AppendInt32("city", 1).AppendInt32("_id", 1).Build()})
	probe("sort/age-then-id", Op{Kind: OpFind, Collection: coll,
		Sort: bson.NewBuilder().AppendInt32("age", 1).AppendInt32("_id", 1).Build()})

	// Projection (top-level only, to stay within the documented sub-document ceiling).
	inclNameAge := bson.NewBuilder().AppendInt32("name", 1).AppendInt32("age", 1).Build()
	exclTags := bson.NewBuilder().AppendInt32("tags", 0).AppendInt32("scores", 0).Build()
	inclNoID := bson.NewBuilder().AppendInt32("name", 1).AppendInt32("_id", 0).Build()
	probe("project/include-name-age", Op{Kind: OpFind, Collection: coll, Sort: sortByID, Projection: inclNameAge})
	probe("project/exclude-tags-scores", Op{Kind: OpFind, Collection: coll, Sort: sortByID, Projection: exclTags})
	probe("project/include-no-id", Op{Kind: OpFind, Collection: coll, Sort: sortByID, Projection: inclNoID})

	// Skip and limit over the sorted stream.
	for _, sk := range []int64{0, 2, 5, 8, 20} {
		probe(fmt.Sprintf("page/skip-%d", sk), Op{Kind: OpFind, Collection: coll, Sort: sortByID, Skip: sk})
	}
	for _, lim := range []int64{1, 3, 8, 20} {
		probe(fmt.Sprintf("page/limit-%d", lim), Op{Kind: OpFind, Collection: coll, Sort: sortByID, Limit: lim})
	}
	probe("page/skip2-limit3", Op{Kind: OpFind, Collection: coll, Sort: sortByID, Skip: 2, Limit: 3})

	// findOne honoring sort.
	probe("findone/sort-age-asc", Op{Kind: OpFindOne, Collection: coll,
		Sort: bson.NewBuilder().AppendInt32("age", 1).AppendInt32("_id", 1).Build()})
	probe("findone/sort-age-desc", Op{Kind: OpFindOne, Collection: coll,
		Sort: bson.NewBuilder().AppendInt32("age", -1).AppendInt32("_id", 1).Build()})

	return cases
}
