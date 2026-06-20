package agg

import (
	"testing"

	"github.com/tamnd/doc/bson"
)

// benchInput builds n documents shaped {_id, a, b, items:[...]}.
func benchInput(n int) []bson.Raw {
	docs := make([]bson.Raw, n)
	items := mkArray([]bson.RawValue{mkInt32(1), mkInt32(2), mkInt32(3)})
	for i := 0; i < n; i++ {
		docs[i] = bson.NewBuilder().
			AppendInt32("_id", int32(i)).
			AppendInt32("a", int32(i)).
			AppendInt32("b", int32(i*2)).
			AppendValue("items", items).
			Build()
	}
	return docs
}

func BenchmarkExprArithmetic(b *testing.B) {
	e, err := compileExpr(op("$add", mkString("$a"), op("$multiply", mkString("$b"), mkInt32(2))))
	if err != nil {
		b.Fatal(err)
	}
	doc := bson.NewBuilder().AppendInt32("a", 3).AppendInt32("b", 4).Build()
	ctx := docCtx(doc, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.eval(ctx)
	}
}

func BenchmarkPipelineMatchProject(b *testing.B) {
	in := benchInput(1000)
	match := stageMatchGTE()
	project := bson.NewBuilder().AppendDocument("$project",
		bson.NewBuilder().AppendInt32("a", 1).Build()).Build()
	p, err := Compile([]bson.Raw{match, project})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Run(in, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPipelineUnwind(b *testing.B) {
	in := benchInput(1000)
	unwind := bson.NewBuilder().AppendString("$unwind", "$items").Build()
	p, err := Compile([]bson.Raw{unwind})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Run(in, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// stageMatchGTE builds {$match: {a: {$gte: 500}}}.
func stageMatchGTE() bson.Raw {
	return bson.NewBuilder().AppendDocument("$match",
		bson.NewBuilder().AppendDocument("a",
			bson.NewBuilder().AppendInt32("$gte", 500).Build()).Build()).Build()
}
