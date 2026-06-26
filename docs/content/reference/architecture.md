---
title: "Architecture"
description: "How a .doc file is put together, from the page on disk up to the wire server, and where each piece lives in the source tree."
weight: 35
---

This page is a map of the engine.
It is not needed to use doc, but it helps if you want to read the source, reason about durability, or understand why a guarantee holds.
The design is built bottom-up: every layer sits on a tested layer below it, and the seam between the storage substrate and the document model is explicit.

## The layers

From the disk up:

1. **The file and the pages.** A `.doc` file is a header followed by fixed-size pages (4K, 8K by default, or 16K). Every page is checksummed. This is package `format`.
2. **The VFS.** All file access goes through a small filesystem seam so the same engine runs on a real disk, in memory (`:memory:`), or under a fault-injecting test harness. This is package `vfs`.
3. **The write-ahead log and the pager.** Writes go to the WAL first, then to the main file at a checkpoint. The pager owns a buffer pool (a 2Q cache) over the pages and gives out group commit. A crash recovers to the last committed transaction by replaying the WAL. These are packages `wal` and `pager`.
4. **MVCC.** Multi-version concurrency control gives every transaction a consistent snapshot. Readers never block writers, and a write conflict is detected at commit. This is package `mvcc`, driven through a small oracle that hands out versions (package `oracle`).
5. **The storage SPI.** Everything above the substrate talks to it through one interface: record stores, index stores, and transactions. This is package `storage`, and it is the clean seam that keeps the document model independent of the byte layout.
6. **Records and the _id index.** Documents live in slotted pages with overflow chains for large values (package `heap`), keyed by an order-preserving B-tree on `_id` (package `index`).
7. **BSON and the document model.** The BSON codec and the cross-type comparison order live in package `bson`. A single collection, with its MVCC overlay and its `_id` index, is package `collection`.
8. **Queries and writes.** Query matching, projection, and sort are package `query`; the update operators are package `update`; the aggregation pipeline is package `agg`. The cost-based planner that chooses a scan is package `plan`.
9. **Catalog and engine.** Many collections across many databases in one file are tracked by a catalog (package `catalog`) and multiplexed over the shared pager and oracle by the engine (package `engine`).
10. **The public API.** The root package `doc` is the surface you import: `Open`, `DB`, `Database`, `Collection`, sessions, change streams, and the options.

## Supporting pieces

- **Columnar store.** An optional projected column store accelerates `$group`, `$sum`, and `$avg` over many documents and few fields, by reading encoded segments instead of scanning the heap. It is package `colstore`, and it is derived from the heap, so it never holds the only copy of anything.
- **Schema validation.** JSON Schema and query-expression validators are compiled and enforced inside the write transaction. This is package `schema`.
- **Encryption.** At-rest page-level encryption wraps the VFS beneath the pager, with a two-tier key scheme and a verify step before any page is read. This is package `crypto`.
- **The wire server.** The MongoDB wire protocol, the handshake, the data commands, SCRAM authentication, RBAC, TLS, and wire-level sessions and transactions are package `wire`. It sits behind the same command layer the CLI uses, so it adds no behaviour the library does not already have.
- **Observability.** Metrics, the slow-operation profiler, and a Prometheus surface are package `metrics`.
- **JSON bridge.** The canonical and relaxed Extended JSON codec the CLI and import/export use is package `extjson`.
- **Clocks and ids.** The `ObjectID` generator and the clock seam are package `sys`.

## Why the seams matter

Two seams do most of the work of keeping the design honest.

The storage SPI (`storage`) means the document model never reaches past it to a raw byte offset.
A bug in the query engine cannot corrupt a page, because it has no way to address one; it asks the record store for a document and gets one back.

The VFS seam (`vfs`) means durability is testable.
The crash and recovery suite drives a fault-injecting filesystem that can tear a write at any fsync boundary, then reopens the file and checks that recovery lands on a clean commit-order prefix.
That is how doc can claim crash safety without hand-waving: the claim is a test that runs on every change.

## Reading the source

The whole design is written up in the spec under `notes/Spec/2061`, and the per-milestone implementation notes are under `notes/Spec/2061/implementation`.
If you want to follow the build order rather than the layer order, the [release notes](/reference/release-notes/) list the ten milestones in the sequence they were built, from the file format up to the wire server.
