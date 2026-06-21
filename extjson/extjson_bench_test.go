package extjson

import "testing"

// a representative document mixing the common types a CLI round-trips.
const benchDoc = `{"_id":{"$oid":"0123456789abcdef01234567"},` +
	`"name":"widget","qty":42,"price":9.99,"active":true,` +
	`"tags":["a","b","c"],"meta":{"created":{"$date":"2021-01-01T00:00:00Z"},"n":3}}`

func BenchmarkParse(b *testing.B) {
	data := []byte(benchDoc)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshalRelaxed(b *testing.B) {
	raw, err := Parse([]byte(benchDoc))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := MarshalRelaxed(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshalCanonical(b *testing.B) {
	raw, err := Parse([]byte(benchDoc))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Marshal(raw, Options{Canonical: true}); err != nil {
			b.Fatal(err)
		}
	}
}
