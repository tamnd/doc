package query

import (
	"testing"

	"github.com/tamnd/doc/bson"
)

// keysOf returns the top-level field names of a document in order.
func keysOf(t *testing.T, d bson.Raw) []string {
	t.Helper()
	elems, err := d.Elements()
	if err != nil {
		t.Fatalf("Elements: %v", err)
	}
	out := make([]string, len(elems))
	for i, e := range elems {
		out[i] = e.Key
	}
	return out
}

func sample() bson.Raw {
	return doc(
		func(b *bson.Builder) { b.AppendInt32("_id", 1) },
		fStr("name", "ada"),
		fInt("age", 36),
		fStr("city", "london"),
	)
}

func projectAndKeys(t *testing.T, proj bson.Raw) []string {
	t.Helper()
	p, err := CompileProjection(proj)
	if err != nil {
		t.Fatalf("CompileProjection: %v", err)
	}
	out, err := p.Apply(sample())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return keysOf(t, out)
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProjectionInclusion(t *testing.T) {
	got := projectAndKeys(t, doc(fInt("name", 1), fInt("age", 1)))
	if want := []string{"_id", "name", "age"}; !eqStrings(got, want) {
		t.Errorf("inclusion keys = %v, want %v (_id kept by default)", got, want)
	}
}

func TestProjectionInclusionDropID(t *testing.T) {
	got := projectAndKeys(t, doc(fInt("name", 1), func(b *bson.Builder) { b.AppendInt32("_id", 0) }))
	if want := []string{"name"}; !eqStrings(got, want) {
		t.Errorf("inclusion with _id:0 keys = %v, want %v", got, want)
	}
}

func TestProjectionExclusion(t *testing.T) {
	got := projectAndKeys(t, doc(fInt("city", 0)))
	if want := []string{"_id", "name", "age"}; !eqStrings(got, want) {
		t.Errorf("exclusion keys = %v, want %v", got, want)
	}
}

func TestProjectionMixError(t *testing.T) {
	_, err := CompileProjection(doc(fInt("name", 1), fInt("city", 0)))
	if err == nil {
		t.Error("mixing inclusion and exclusion (other than _id) should error")
	}
}

func TestProjectionEmptyPassThrough(t *testing.T) {
	got := projectAndKeys(t, bson.NewBuilder().Build())
	if want := []string{"_id", "name", "age", "city"}; !eqStrings(got, want) {
		t.Errorf("empty projection keys = %v, want %v", got, want)
	}
}

func TestProjectionOrderFollowsDocument(t *testing.T) {
	// Projection requests age before name, but the stored order wins.
	got := projectAndKeys(t, doc(fInt("age", 1), fInt("name", 1)))
	if want := []string{"_id", "name", "age"}; !eqStrings(got, want) {
		t.Errorf("projection should preserve stored field order: got %v, want %v", got, want)
	}
}
