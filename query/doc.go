// Package query is doc's MQL match, projection, and sort layer: the part of the
// find path that turns a filter, a projection, and a sort document into functions
// over stored BSON documents (spec 2061 doc 08, doc 11). It is storage-agnostic -
// it operates on bson.Raw documents a caller supplies, so the collection layer
// can drive it over either an in-memory overlay scan or, later, an index scan.
//
// The match layer implements the M3-a filter surface: implicit equality and
// implicit AND, the comparison operators ($eq/$ne/$gt/$gte/$lt/$lte/$in/$nin),
// the logical operators ($and/$or/$nor/$not), the element operators
// ($exists/$type), and the array operators ($elemMatch/$size/$all), with dotted
// path resolution, array fan-out, the BSON type-bracket comparison rules, and
// MongoDB's null/missing conflation. The evaluation/aggregation operators
// ($regex, $mod, $expr, $where, $text) and geospatial operators arrive later.
package query
