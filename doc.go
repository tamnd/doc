// Package doc is a modern, high-performance, low-latency embedded document
// database for Go that looks and feels like SQLite: the whole database is a
// single self-describing .doc file, durability comes from a write-ahead log, and
// you open it with a path and a line of code. It speaks the MongoDB document
// model (BSON documents in collections, ObjectId _ids) and a subset of the
// MongoDB Query Language, and a future server mode answers the MongoDB wire
// protocol so existing drivers connect unchanged.
//
// The durable substrate (pager, buffer pool, write-ahead log, group commit,
// crash recovery, and MVCC) lives in this module behind the storage SPI in
// package storage, in packages pager, wal, and mvcc. Above that verified seam doc
// builds the document-specific machinery: the slotted-page record store, the BSON
// codec, MQL evaluation, the aggregation pipeline, indexes, and the wire protocol.
// The full design is spec 2061 under notes/Spec/2061; the implementation notes
// are under notes/Spec/2061/implementation.
//
// This is the module root. In M0 it carries only version and build identity; the
// embedded Open/DB/Collection API (spec 2061 doc 14) lands as the milestones
// fill in the layers beneath it.
package doc

// Version is the semantic version of the doc module. It is pre-1.0 during the
// milestone build-out (spec 2061 doc 19 §22); the library API is frozen at the
// first v1.0.0 release.
const Version = "0.0.0-m0"

// Magic mirrors the file-format magic prefix bytes "doc\0" for callers that want
// to sniff a file without importing package format. The authoritative magic is
// format.Magic.
var Magic = [4]byte{'d', 'o', 'c', 0x00}
