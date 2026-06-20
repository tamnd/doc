package oracle

import "github.com/tamnd/doc/bson"

func rawLong(n int64) bson.RawValue {
	return rawVal(func(b *bson.Builder) { b.AppendInt64("x", n) })
}

func rawDouble(n float64) bson.RawValue {
	return rawVal(func(b *bson.Builder) { b.AppendDouble("x", n) })
}

// This file holds the M3-b conformance corpus: the write surface beyond insert and
// delete. It exercises the field update operators ($set, $unset, $inc, $mul, $min,
// $max, $rename, $currentDate), updateOne / updateMany / replaceOne, the
// findAndModify family, and distinct, each probe compared against live MongoDB
// (spec 2061 doc 19 §17, doc 13).
//
// Two probe shapes appear. A "state" case puts the write in Setup and probes a
// deterministic find, so the resulting collection is compared byte for byte; the
// {_id:1} sort makes the order total. A "count" case probes the write itself, so
// the matched and modified counts (or the returned document, or an error code)
// are compared. $currentDate stamps a wall-clock value that the two engines can
// never agree on byte for byte, so those cases probe a $type count instead.

// ---- op constructors -----------------------------------------------------

func updOneOp(f, u bson.Raw) Op {
	return Op{Kind: OpUpdateOne, Collection: coll, Filter: f, Update: u}
}

func updManyOp(f, u bson.Raw) Op {
	return Op{Kind: OpUpdateMany, Collection: coll, Filter: f, Update: u}
}

func replaceOp(f, r bson.Raw) Op {
	return Op{Kind: OpReplaceOne, Collection: coll, Filter: f, Replacement: r}
}

func distinctOp(field string, f bson.Raw) Op {
	return Op{Kind: OpDistinct, Collection: coll, Field: field, Filter: f}
}

func faUpdateOp(f, u bson.Raw, after bool) Op {
	return Op{Kind: OpFindOneAndUpdate, Collection: coll, Filter: f, Update: u, Sort: sortByID, ReturnAfter: after}
}

func faReplaceOp(f, r bson.Raw, after bool) Op {
	return Op{Kind: OpFindOneAndReplace, Collection: coll, Filter: f, Replacement: r, Sort: sortByID, ReturnAfter: after}
}

func faDeleteOp(f bson.Raw) Op {
	return Op{Kind: OpFindOneAndDelete, Collection: coll, Filter: f, Sort: sortByID}
}

// ---- update-document builders --------------------------------------------

// opDoc builds {$op:{field:value}} for a single-field operator.
func opDoc(op, field string, v bson.RawValue) bson.Raw {
	inner := bson.NewBuilder().AppendValue(field, v).Build()
	return bson.NewBuilder().AppendDocument(op, inner).Build()
}

// unsetDoc builds {$unset:{field:""}}; the value is ignored by $unset.
func unsetDoc(field string) bson.Raw {
	inner := bson.NewBuilder().AppendString(field, "").Build()
	return bson.NewBuilder().AppendDocument("$unset", inner).Build()
}

// renameDoc builds {$rename:{from:to}}.
func renameDoc(from, to string) bson.Raw {
	inner := bson.NewBuilder().AppendString(from, to).Build()
	return bson.NewBuilder().AppendDocument("$rename", inner).Build()
}

// currentDateDoc builds {$currentDate:{field:true}} (a Date) or
// {$currentDate:{field:{$type:"timestamp"}}}.
func currentDateDoc(field string, timestamp bool) bson.Raw {
	var inner bson.Raw
	if timestamp {
		spec := bson.NewBuilder().AppendString("$type", "timestamp").Build()
		inner = bson.NewBuilder().AppendDocument(field, spec).Build()
	} else {
		inner = bson.NewBuilder().AppendBoolean(field, true).Build()
	}
	return bson.NewBuilder().AppendDocument("$currentDate", inner).Build()
}

// typeCount builds {field:{$type:alias}} for probing a stamped field's type.
func typeCount(field, alias string) bson.Raw {
	sub := bson.NewBuilder().AppendString("$type", alias).Build()
	return bson.NewBuilder().AppendDocument(field, sub).Build()
}

// ---- seed ----------------------------------------------------------------

// m3bSeed inserts a small, deterministic document set the write cases run over.
// _ids are explicit so the two engines never diverge on an auto-minted id.
func m3bSeed() []Op {
	mk := func(id, n int32, s string, g int32) bson.Raw {
		return bson.NewBuilder().
			AppendInt32("_id", id).
			AppendInt32("n", n).
			AppendString("s", s).
			AppendInt32("g", g).
			Build()
	}
	return []Op{
		insOp(mk(1, 10, "a", 1)),
		insOp(mk(2, 20, "b", 1)),
		insOp(mk(3, 30, "c", 2)),
		// A document missing n and s, for missing-field operator behavior.
		insOp(bson.NewBuilder().AppendInt32("_id", 4).AppendInt32("g", 2).Build()),
	}
}

// findAll probes every document in deterministic _id order.
func findAll() Op { return findSorted(nil) }

// ---- cases ---------------------------------------------------------------

// m3bCases returns the M3-b write conformance cases.
func m3bCases() []Case {
	var cases []Case
	seed := m3bSeed()
	add := func(name string, setup []Op, probe Op) {
		cases = append(cases, Case{Name: name, Setup: setup, Probe: probe})
	}
	// state runs an update over the seed, then compares the whole collection.
	state := func(name string, write Op) {
		add("m3b/"+name+"/state", append(append([]Op{}, seed...), write), findAll())
	}
	// counts runs an update as the probe, comparing matched/modified.
	counts := func(name string, write Op) {
		add("m3b/"+name+"/counts", seed, write)
	}
	// both compares state and counts for one write.
	both := func(name string, write Op) {
		state(name, write)
		counts(name, write)
	}

	// $set on an existing field, a new field, and a nested path.
	both("set-existing", updOneOp(filtI32(1), opDoc("$set", "n", rawInt(99))))
	both("set-new", updOneOp(filtI32(1), opDoc("$set", "z", rawInt(7))))
	both("set-string", updOneOp(filtI32(2), opDoc("$set", "s", rawStr("zzz"))))
	both("set-nested", updOneOp(filtI32(1), opDoc("$set", "a.b", rawInt(5))))
	both("set-noop", updOneOp(filtI32(1), opDoc("$set", "n", rawInt(10))))

	// $unset existing, missing, and a whole field.
	both("unset-existing", updOneOp(filtI32(1), unsetDoc("n")))
	both("unset-missing", updOneOp(filtI32(4), unsetDoc("n")))
	both("unset-string", updOneOp(filtI32(2), unsetDoc("s")))

	// $inc: existing, missing (base 0), widening to int64, double promotion.
	both("inc-existing", updOneOp(filtI32(1), opDoc("$inc", "n", rawInt(5))))
	both("inc-missing", updOneOp(filtI32(4), opDoc("$inc", "n", rawInt(3))))
	both("inc-negative", updOneOp(filtI32(2), opDoc("$inc", "n", rawInt(-25))))
	both("inc-long", updOneOp(filtI32(1), opDoc("$inc", "n", rawLong(1<<40))))
	both("inc-double", updOneOp(filtI32(1), opDoc("$inc", "n", rawDouble(0.5))))

	// $mul: existing, missing (yields 0).
	both("mul-existing", updOneOp(filtI32(3), opDoc("$mul", "n", rawInt(2))))
	both("mul-missing", updOneOp(filtI32(4), opDoc("$mul", "n", rawInt(9))))
	both("mul-double", updOneOp(filtI32(2), opDoc("$mul", "n", rawDouble(1.5))))

	// $min / $max: applies, skips, and creates a missing field.
	both("min-lower", updOneOp(filtI32(3), opDoc("$min", "n", rawInt(5))))
	both("min-higher", updOneOp(filtI32(1), opDoc("$min", "n", rawInt(50))))
	both("min-missing", updOneOp(filtI32(4), opDoc("$min", "n", rawInt(5))))
	both("max-higher", updOneOp(filtI32(1), opDoc("$max", "n", rawInt(50))))
	both("max-lower", updOneOp(filtI32(3), opDoc("$max", "n", rawInt(5))))
	both("max-missing", updOneOp(filtI32(4), opDoc("$max", "n", rawInt(5))))

	// $rename: existing and missing source (a no-op).
	both("rename-existing", updOneOp(filtI32(1), renameDoc("n", "m")))
	both("rename-missing", updOneOp(filtI32(4), renameDoc("n", "m")))

	// Multi-operator updates.
	multi := bson.NewBuilder().
		AppendDocument("$set", bson.NewBuilder().AppendInt32("z", 1).Build()).
		AppendDocument("$inc", bson.NewBuilder().AppendInt32("n", 100).Build()).
		Build()
	both("multi-set-inc", updOneOp(filtI32(1), multi))

	// updateMany across a group.
	both("many-set", updManyOp(grpFilter(1), opDoc("$set", "tag", rawStr("x"))))
	both("many-inc", updManyOp(grpFilter(2), opDoc("$inc", "n", rawInt(1))))
	both("many-all", updManyOp(nil, opDoc("$set", "k", rawInt(1))))

	// replaceOne: same _id preserved, fields swapped.
	repl := bson.NewBuilder().AppendInt32("only", 1).Build()
	both("replace", replaceOp(filtI32(2), repl))
	both("replace-noop", replaceOp(filtI32(1),
		bson.NewBuilder().AppendInt32("n", 10).AppendString("s", "a").AppendInt32("g", 1).Build()))

	// findAndModify: returned document, before and after.
	add("m3b/fa-update-before", seed, faUpdateOp(filtI32(1), opDoc("$set", "n", rawInt(77)), false))
	add("m3b/fa-update-after", seed, faUpdateOp(filtI32(1), opDoc("$set", "n", rawInt(77)), true))
	add("m3b/fa-update-nomatch", seed, faUpdateOp(filtI32(99), opDoc("$set", "n", rawInt(1)), true))
	add("m3b/fa-update-sort", seed, faUpdateOp(grpFilter(1), opDoc("$set", "n", rawInt(0)), true))
	add("m3b/fa-replace-before", seed, faReplaceOp(filtI32(2), repl, false))
	add("m3b/fa-replace-after", seed, faReplaceOp(filtI32(2), repl, true))
	add("m3b/fa-delete", seed, faDeleteOp(filtI32(3)))
	add("m3b/fa-delete-nomatch", seed, faDeleteOp(filtI32(99)))

	// findAndModify state: the collection after the modify.
	state("fa-update", faUpdateOp(filtI32(1), opDoc("$set", "n", rawInt(77)), false))
	state("fa-delete", faDeleteOp(filtI32(3)))
	state("fa-replace", faReplaceOp(filtI32(2), repl, false))

	// distinct over the seed and a filtered subset.
	add("m3b/distinct-g", seed, distinctOp("g", nil))
	add("m3b/distinct-n", seed, distinctOp("n", nil))
	add("m3b/distinct-s", seed, distinctOp("s", nil))
	add("m3b/distinct-filtered", seed, distinctOp("n", grpFilter(1)))
	add("m3b/distinct-missing", seed, distinctOp("nope", nil))

	// distinct array unwind: a field holding arrays yields element values.
	arrSeed := []Op{
		insOp(bson.NewBuilder().AppendInt32("_id", 1).
			AppendArray("tags", bson.BuildArray(rawStr("x"), rawStr("y"))).Build()),
		insOp(bson.NewBuilder().AppendInt32("_id", 2).
			AppendArray("tags", bson.BuildArray(rawStr("y"), rawStr("z"))).Build()),
	}
	add("m3b/distinct-array", arrSeed, distinctOp("tags", nil))

	// $currentDate stamps a non-comparable value, so probe its type with a count.
	add("m3b/currentdate-date",
		append(append([]Op{}, seed...), updOneOp(filtI32(1), currentDateDoc("d", false))),
		countOp(typeCount("d", "date")))
	add("m3b/currentdate-timestamp",
		append(append([]Op{}, seed...), updOneOp(filtI32(1), currentDateDoc("t", true))),
		countOp(typeCount("t", "timestamp")))

	// Error cases: _id immutability, conflicting operators, $inc on a string.
	add("m3b/err-immutable-id", seed,
		updOneOp(filtI32(1), opDoc("$set", "_id", rawInt(9))))
	conflict := bson.NewBuilder().
		AppendDocument("$set", bson.NewBuilder().AppendInt32("a", 1).Build()).
		AppendDocument("$inc", bson.NewBuilder().AppendInt32("a", 1).Build()).
		Build()
	add("m3b/err-conflict", seed, updOneOp(filtI32(1), conflict))
	add("m3b/err-inc-string", seed,
		updOneOp(filtI32(1), opDoc("$inc", "s", rawInt(1))))

	return cases
}
