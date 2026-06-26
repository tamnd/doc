package update

import (
	"testing"

	"github.com/tamnd/doc/bson"
)

// benchDoc is a representative document the update benchmarks transform.
func benchDoc() bson.Raw {
	return doc(
		fInt("_id", 1),
		fInt("n", 100),
		fLong("big", 1<<40),
		fDbl("ratio", 1.5),
		fStr("name", "widget"),
		fStr("city", "london"),
		fDoc("meta", doc(fInt("v", 1), fStr("k", "x"))),
	)
}

// setOne builds {$set:{name:"gadget"}}.
func setOne() bson.Raw {
	return bson.NewBuilder().AppendDocument("$set",
		bson.NewBuilder().AppendString("name", "gadget").Build()).Build()
}

// incOne builds {$inc:{n:1}}.
func incOne() bson.Raw {
	return bson.NewBuilder().AppendDocument("$inc",
		bson.NewBuilder().AppendInt32("n", 1).Build()).Build()
}

// mixed builds {$set:{name:"gadget"},$inc:{n:1},$min:{ratio:1.0}}.
func mixed() bson.Raw {
	return bson.NewBuilder().
		AppendDocument("$set", bson.NewBuilder().AppendString("name", "gadget").Build()).
		AppendDocument("$inc", bson.NewBuilder().AppendInt32("n", 1).Build()).
		AppendDocument("$min", bson.NewBuilder().AppendDouble("ratio", 1.0).Build()).
		Build()
}

func BenchmarkCompile(b *testing.B) {
	upd := mixed()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Compile(upd); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkApplySet(b *testing.B)   { benchApply(b, setOne()) }
func BenchmarkApplyInc(b *testing.B)   { benchApply(b, incOne()) }
func BenchmarkApplyMixed(b *testing.B) { benchApply(b, mixed()) }

// pushEach builds {$push:{tags:{$each:["b","c"],$sort:1,$slice:5}}}.
func pushEach() bson.Raw {
	mod := bson.NewBuilder().
		AppendArray("$each", bson.BuildArray(vStr("b"), vStr("c"))).
		AppendInt32("$sort", 1).
		AppendInt32("$slice", 5).
		Build()
	return bson.NewBuilder().AppendDocument("$push",
		bson.NewBuilder().AppendDocument("tags", mod).Build()).Build()
}

// pullScalar builds {$pull:{tags:"a"}}.
func pullScalar() bson.Raw {
	return bson.NewBuilder().AppendDocument("$pull",
		bson.NewBuilder().AppendValue("tags", vStr("a")).Build()).Build()
}

func BenchmarkApplyPushEach(b *testing.B)   { benchApplyArr(b, pushEach()) }
func BenchmarkApplyPullScalar(b *testing.B) { benchApplyArr(b, pullScalar()) }

// benchApplyArr measures an array operator over a document carrying a tags array.
func benchApplyArr(b *testing.B, upd bson.Raw) {
	b.Helper()
	u, err := Compile(upd)
	if err != nil {
		b.Fatal(err)
	}
	d := doc(fInt("_id", 1), fArr("tags", vStr("a"), vStr("d"), vStr("e")))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := u.Apply(d, epoch); err != nil {
			b.Fatal(err)
		}
	}
}

// benchApply measures one compiled update applied repeatedly to a fresh document.
func benchApply(b *testing.B, upd bson.Raw) {
	b.Helper()
	u, err := Compile(upd)
	if err != nil {
		b.Fatal(err)
	}
	d := benchDoc()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := u.Apply(d, epoch); err != nil {
			b.Fatal(err)
		}
	}
}
