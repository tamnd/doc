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
	ctx := docCtx(doc, &execCtx{})
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

// benchGroupInput builds n documents shaped {_id, g, n} with g spread over groups.
func benchGroupInput(n, groups int) []bson.Raw {
	docs := make([]bson.Raw, n)
	for i := 0; i < n; i++ {
		docs[i] = bson.NewBuilder().
			AppendInt32("_id", int32(i)).
			AppendInt32("g", int32(i%groups)).
			AppendInt32("n", int32(i)).
			Build()
	}
	return docs
}

func BenchmarkPipelineGroup(b *testing.B) {
	in := benchGroupInput(10000, 100)
	group := bson.NewBuilder().AppendDocument("$group", bson.NewBuilder().
		AppendString("_id", "$g").
		AppendDocument("total", bson.NewBuilder().AppendString("$sum", "$n").Build()).
		AppendDocument("avg", bson.NewBuilder().AppendString("$avg", "$n").Build()).
		Build()).Build()
	p, err := Compile([]bson.Raw{group})
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

func BenchmarkPipelineSort(b *testing.B) {
	in := benchGroupInput(10000, 10000)
	sort := bson.NewBuilder().AppendDocument("$sort",
		bson.NewBuilder().AppendInt32("n", -1).Build()).Build()
	p, err := Compile([]bson.Raw{sort})
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

func BenchmarkPipelineSortTopK(b *testing.B) {
	in := benchGroupInput(10000, 10000)
	sort := bson.NewBuilder().AppendDocument("$sort",
		bson.NewBuilder().AppendInt32("n", -1).Build()).Build()
	limit := bson.NewBuilder().AppendInt32("$limit", 10).Build()
	p, err := Compile([]bson.Raw{sort, limit})
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

func BenchmarkPipelineLookup(b *testing.B) {
	in := benchGroupInput(1000, 1000)
	foreign := benchGroupInput(1000, 1000)
	env := &Env{Read: func(string) ([]bson.Raw, error) { return foreign, nil }}
	lookup := bson.NewBuilder().AppendDocument("$lookup", bson.NewBuilder().
		AppendString("from", "f").
		AppendString("localField", "_id").
		AppendString("foreignField", "_id").
		AppendString("as", "j").
		Build()).Build()
	p, err := Compile([]bson.Raw{lookup})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.RunWith(in, 0, env); err != nil {
			b.Fatal(err)
		}
	}
}
