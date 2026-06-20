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
