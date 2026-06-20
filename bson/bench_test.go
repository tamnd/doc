package bson

import (
	"testing"

	"github.com/tamnd/doc/sys"
)

func sampleDoc() Raw {
	return NewBuilder().
		AppendObjectID("_id", sys.ObjectID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}).
		AppendString("name", "Ada Lovelace").
		AppendInt32("age", 36).
		AppendDouble("score", 99.5).
		AppendBoolean("active", true).
		Build()
}

func BenchmarkBuild(b *testing.B) {
	oid := sys.ObjectID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	bld := NewBuilder()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bld.Reset()
		bld.AppendObjectID("_id", oid).
			AppendString("name", "Ada Lovelace").
			AppendInt32("age", 36).
			Build()
	}
}

func BenchmarkLookup(b *testing.B) {
	doc := sampleDoc()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := doc.Lookup("score"); !ok {
			b.Fatal("missing")
		}
	}
}

func BenchmarkValidate(b *testing.B) {
	doc := sampleDoc()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := doc.Validate(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkElements(b *testing.B) {
	doc := sampleDoc()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := doc.Elements(); err != nil {
			b.Fatal(err)
		}
	}
}
