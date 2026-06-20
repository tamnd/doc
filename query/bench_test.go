package query

import (
	"testing"

	"github.com/tamnd/doc/bson"
)

// benchDoc builds a representative document with a scalar, a nested document, and
// an array, so a matcher exercises field lookup, dotted descent, and array
// fan-out.
func benchDoc(n int32) bson.Raw {
	return bson.NewBuilder().
		AppendInt32("_id", n).
		AppendString("name", "benchmark").
		AppendInt32("age", n%80).
		AppendDocument("addr", bson.NewBuilder().AppendString("city", "london").Build()).
		AppendArray("scores", bson.BuildArray(vInt(n%10), vInt(n%20), vInt(n%30))).
		Build()
}

func BenchmarkCompileFilter(b *testing.B) {
	filter := cmpDoc()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Compile(filter); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMatchEquality(b *testing.B) {
	m, _ := Compile(doc(fInt("age", 5)))
	d := benchDoc(5)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Match(d)
	}
}

func BenchmarkMatchComparison(b *testing.B) {
	m, _ := Compile(cmpDoc())
	d := benchDoc(40)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Match(d)
	}
}

func BenchmarkMatchDottedPath(b *testing.B) {
	m, _ := Compile(doc(fStr("addr.city", "london")))
	d := benchDoc(7)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Match(d)
	}
}

func BenchmarkMatchArrayFanOut(b *testing.B) {
	m, _ := Compile(doc(fInt("scores", 5)))
	d := benchDoc(5)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Match(d)
	}
}

func BenchmarkSortThousand(b *testing.B) {
	docs := make([]bson.Raw, 1000)
	for i := range docs {
		docs[i] = benchDoc(int32((i*7919)%1000) + 1)
	}
	s, _ := CompileSort(doc(fInt("age", 1), fInt("_id", 1)))
	scratch := make([]bson.Raw, len(docs))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(scratch, docs)
		s.Apply(scratch)
	}
}

// cmpDoc builds {age:{$gte:20}} for the comparison benchmarks.
func cmpDoc() bson.Raw {
	return doc(func(b *bson.Builder) { b.AppendDocument("age", doc(fInt("$gte", 20))) })
}
