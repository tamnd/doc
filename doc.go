// Package doc is a modern, high-performance, low-latency embedded document
// database for Go that looks and feels like SQLite: the whole database is a
// single self-describing .doc file, durability comes from a write-ahead log, and
// you open it with a path and a line of code. It speaks the MongoDB document
// model (BSON documents in collections, ObjectId _ids) and the MongoDB Query
// Language, and a server mode answers the MongoDB wire protocol so existing
// drivers connect unchanged.
//
// The durable substrate (pager, buffer pool, write-ahead log, group commit,
// crash recovery, and MVCC) lives in this module behind the storage SPI in
// package storage, in packages pager, wal, and mvcc. Above that verified seam doc
// builds the document-specific machinery: the slotted-page record store, the BSON
// codec, MQL evaluation, the aggregation pipeline, indexes, and the wire protocol.
// The full design is spec 2061 under notes/Spec/2061; the implementation notes
// are under notes/Spec/2061/implementation.
//
// The entry point is Open, which returns a *DB. From there Database and
// Collection mirror the MongoDB Go driver's shape (spec 2061 doc 14), so code
// written against go.mongodb.org/mongo-driver moves over with little change. The
// migration guide in the docs site covers the differences.
//
// # API stability
//
// doc is pre-1.0. The whole engine is built and tested, but the exported library
// API is still settling, so a 0.x minor bump may rename or reshape an exported
// name as the surface is finalized toward 1.0. Pin a version if you depend on it.
// The on-disk file format is more conservative: it carries its own major and minor
// version, a build opens any file whose format major it understands, and it rejects
// a newer major with a clear error rather than misreading it.
package doc

// Version is the semantic version of the doc module. doc is pre-1.0, so the API may
// still change across 0.x minor releases; release builds of the doc binary stamp the
// exact tag over this default through the linker.
const Version = "0.1.0"

// Magic mirrors the file-format magic prefix bytes "doc\0" for callers that want
// to sniff a file without importing package format. The authoritative magic is
// format.Magic.
var Magic = [4]byte{'d', 'o', 'c', 0x00}
