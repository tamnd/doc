//go:build mongo

package conformance

import (
	"github.com/tamnd/doc/bson"

	mbson "go.mongodb.org/mongo-driver/v2/bson"
)

// mongoDoc presents a doc BSON document to the driver. The driver marshals a
// bson.Raw (valid BSON bytes) directly, preserving field order, so the stored
// document matches what doc stores byte for byte.
func mongoDoc(d bson.Raw) mbson.Raw { return mbson.Raw(d) }

// mongoFilter presents a filter to the driver, mapping a nil filter (match all)
// to an empty document so MongoDB and doc agree on the "no filter" case.
func mongoFilter(f bson.Raw) any {
	if f == nil {
		return mbson.D{}
	}
	return mbson.Raw(f)
}

// toRaw copies the driver's BSON bytes into a doc bson.Raw. cur.Current and a
// decoded mbson.Raw both alias buffers the driver may reuse, so the bytes are
// copied before the harness retains them.
func toRaw(r mbson.Raw) bson.Raw {
	out := make(bson.Raw, len(r))
	copy(out, r)
	return out
}
