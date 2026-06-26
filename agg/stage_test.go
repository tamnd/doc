package agg

import (
	"testing"

	"github.com/tamnd/doc/bson"
)

// stageV builds a stage document {name: v}.
func stageV(name string, v bson.RawValue) bson.Raw {
	return bson.NewBuilder().AppendValue(name, v).Build()
}

// stageD builds a stage document {name: <document>}.
func stageD(name string, body bson.Raw) bson.Raw {
	return bson.NewBuilder().AppendDocument(name, body).Build()
}

// runPipe compiles and runs a pipeline over input.
func runPipe(t *testing.T, input []bson.Raw, stages ...bson.Raw) []bson.Raw {
	t.Helper()
	p, err := Compile(stages)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	out, err := p.Run(input, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out
}

// abDoc builds {_id, a, b}.
func abDoc(id, a, b int32) bson.Raw {
	return bson.NewBuilder().AppendInt32("_id", id).AppendInt32("a", a).AppendInt32("b", b).Build()
}

func field32(d bson.Raw, key string) int32 {
	v, _ := d.Lookup(key)
	return v.Int32()
}

func TestMatchStage(t *testing.T) {
	in := []bson.Raw{abDoc(1, 5, 1), abDoc(2, 15, 2), abDoc(3, 25, 3)}
	gte := stageD("$match", bson.NewBuilder().
		AppendDocument("a", bson.NewBuilder().AppendInt32("$gte", 15).Build()).Build())
	out := runPipe(t, in, gte)
	if len(out) != 2 {
		t.Fatalf("matched %d, want 2", len(out))
	}
}

func TestMatchExpr(t *testing.T) {
	in := []bson.Raw{abDoc(1, 5, 10), abDoc(2, 20, 3)}
	// $expr: a > b
	m := stageD("$match", bson.NewBuilder().
		AppendValue("$expr", op("$gt", mkString("$a"), mkString("$b"))).Build())
	out := runPipe(t, in, m)
	if len(out) != 1 || field32(out[0], "_id") != 2 {
		t.Fatalf("expr match = %v", out)
	}
}

func TestProjectInclusion(t *testing.T) {
	in := []bson.Raw{abDoc(1, 10, 20)}
	out := runPipe(t, in, stageV("$project", mkInt32One("a")))
	d := out[0]
	if _, ok := d.Lookup("a"); !ok {
		t.Fatal("a should be present")
	}
	if _, ok := d.Lookup("b"); ok {
		t.Fatal("b should be excluded")
	}
	if _, ok := d.Lookup("_id"); !ok {
		t.Fatal("_id kept by default")
	}
}

// mkInt32One builds {field: 1}.
func mkInt32One(field string) bson.RawValue {
	return mkDoc(bson.NewBuilder().AppendInt32(field, 1).Build())
}

func TestProjectExclusion(t *testing.T) {
	in := []bson.Raw{abDoc(1, 10, 20)}
	out := runPipe(t, in, stageV("$project", mkDoc(bson.NewBuilder().AppendInt32("b", 0).Build())))
	d := out[0]
	if _, ok := d.Lookup("b"); ok {
		t.Fatal("b should be excluded")
	}
	if _, ok := d.Lookup("a"); !ok {
		t.Fatal("a should be kept")
	}
}

func TestProjectComputedAndIDExclude(t *testing.T) {
	in := []bson.Raw{abDoc(1, 10, 20)}
	spec := bson.NewBuilder().
		AppendInt32("_id", 0).
		AppendValue("sum", op("$add", mkString("$a"), mkString("$b"))).Build()
	out := runPipe(t, in, stageV("$project", mkDoc(spec)))
	d := out[0]
	if _, ok := d.Lookup("_id"); ok {
		t.Fatal("_id should be excluded")
	}
	if v, _ := d.Lookup("sum"); v.Int32() != 30 {
		t.Fatalf("sum = %d, want 30", v.Int32())
	}
}

func TestAddFieldsAndUnset(t *testing.T) {
	in := []bson.Raw{abDoc(1, 10, 20)}
	add := stageV("$addFields", mkDoc(bson.NewBuilder().
		AppendValue("c", op("$add", mkString("$a"), mkString("$b"))).Build()))
	out := runPipe(t, in, add)
	if v, _ := out[0].Lookup("c"); v.Int32() != 30 {
		t.Fatalf("addFields c = %d", v.Int32())
	}

	out2 := runPipe(t, in, stageV("$unset", mkString("a")))
	if _, ok := out2[0].Lookup("a"); ok {
		t.Fatal("unset a still present")
	}
}

func TestReplaceWith(t *testing.T) {
	in := []bson.Raw{bson.NewBuilder().
		AppendInt32("_id", 1).
		AppendDocument("sub", bson.NewBuilder().AppendInt32("z", 9).Build()).Build()}
	out := runPipe(t, in, stageV("$replaceWith", mkString("$sub")))
	if v, _ := out[0].Lookup("z"); v.Int32() != 9 {
		t.Fatalf("replaceWith z = %d", v.Int32())
	}
}

func TestLimitSkipCount(t *testing.T) {
	in := []bson.Raw{abDoc(1, 0, 0), abDoc(2, 0, 0), abDoc(3, 0, 0), abDoc(4, 0, 0)}
	out := runPipe(t, in, stageV("$skip", mkInt32(1)), stageV("$limit", mkInt32(2)))
	if len(out) != 2 || field32(out[0], "_id") != 2 {
		t.Fatalf("skip/limit = %v", out)
	}
	cnt := runPipe(t, in, stageV("$count", mkString("total")))
	if len(cnt) != 1 {
		t.Fatalf("count produced %d docs", len(cnt))
	}
	// $count emits a 32-bit integer when the value fits, matching MongoDB.
	if got := field32(cnt[0], "total"); got != 4 {
		t.Fatalf("count total = %d, want 4", got)
	}
}

func TestUnwind(t *testing.T) {
	items := mkArray([]bson.RawValue{mkInt32(7), mkInt32(8), mkInt32(9)})
	in := []bson.Raw{bson.NewBuilder().AppendInt32("_id", 1).AppendValue("items", items).Build()}
	out := runPipe(t, in, stageV("$unwind", mkString("$items")))
	if len(out) != 3 {
		t.Fatalf("unwind produced %d docs, want 3", len(out))
	}
	if v, _ := out[0].Lookup("items"); v.Int32() != 7 {
		t.Fatalf("first unwound items = %d", v.Int32())
	}
}

func TestUnwindPreserveEmpty(t *testing.T) {
	empty := mkArray(nil)
	in := []bson.Raw{
		bson.NewBuilder().AppendInt32("_id", 1).AppendValue("items", empty).Build(),
		bson.NewBuilder().AppendInt32("_id", 2).Build(),
	}
	arg := bson.NewBuilder().
		AppendString("path", "$items").
		AppendBoolean("preserveNullAndEmptyArrays", true).Build()
	out := runPipe(t, in, stageD("$unwind", arg))
	if len(out) != 2 {
		t.Fatalf("preserve produced %d docs, want 2", len(out))
	}
}

func TestUnwindWithIndex(t *testing.T) {
	items := mkArray([]bson.RawValue{mkString("x"), mkString("y")})
	in := []bson.Raw{bson.NewBuilder().AppendInt32("_id", 1).AppendValue("items", items).Build()}
	arg := bson.NewBuilder().
		AppendString("path", "$items").
		AppendString("includeArrayIndex", "i").Build()
	out := runPipe(t, in, stageD("$unwind", arg))
	if len(out) != 2 {
		t.Fatalf("unwind produced %d docs", len(out))
	}
	if v, _ := out[1].Lookup("i"); v.Int64() != 1 {
		t.Fatalf("array index = %d, want 1", v.Int64())
	}
}
