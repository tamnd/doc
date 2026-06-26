---
title: "Release notes"
description: "What each doc release contains."
weight: 60
---

The authoritative, commit-level history lives in [`CHANGELOG.md`](https://github.com/tamnd/doc/blob/main/CHANGELOG.md) and on the [releases page](https://github.com/tamnd/doc/releases).
This page summarises each version.

## v1.0.0

The 1.0 release.
doc is feature-complete across milestones M0 through M9, the library API is frozen for the 1.x line, the PRAGMA catalogue is stable, and the file format is frozen at major version 1.
See the [stability](/reference/stability/) page for exactly what that covers.

What a 1.0 `.doc` file and binary give you:

- **The MongoDB document model and query language.** BSON documents in collections with ObjectId `_id`s, the MQL comparison, logical, element, and array operators, dotted-path access with array fan-out, MongoDB null and missing semantics, and the BSON cross-type total order.
- **A driver-shaped Go API.** `Open` returns a `DB`; `Database` and `Collection` walk down to the same `InsertOne`, `Find`, `UpdateMany`, `ReplaceOne`, `BulkWrite`, `FindOneAndUpdate`, `Distinct`, `Aggregate`, and index-view methods the official MongoDB Go driver exposes, with matching result types and cursors.
- **The aggregation pipeline.** `$match`, `$project`, `$group` with the accumulators, `$sort`, `$skip`, `$limit`, `$unwind`, and `$lookup` including the MongoDB 5.0 pipeline form.
- **Indexes and a planner.** Single-field, compound, multikey, unique, sparse, partial, and TTL indexes, maintained on the commit path, with order-preserving key encoding and a cost-based planner that chooses between collection, index, and covered scans, with `Explain`.
- **Transactions.** Multi-document, multi-collection transactions through sessions, under snapshot isolation by default or serializable (SSI) on request, on an MVCC core where readers never block writers.
- **Durability and recovery.** A WAL-mode pager with a 2Q buffer pool and group commit, crash recovery to the last committed transaction, online backup, incremental backup, WAL archiving, and point-in-time restore.
- **A MongoDB wire server.** `doc <file> serve` answers the wire protocol so `mongosh` and the official Go, Node, and Python drivers connect unchanged, with SCRAM-SHA-256 authentication, role-based access control, TLS including x509, wire compression, and wire-level sessions and transactions.
- **Operational tooling.** `validate`, `compact`, `checkpoint`, `vacuum`, `backup`, `restore`, `import`, `export`, `dump`, `load`, `stats`, `schema`, and `rekey`, plus an interactive `mongosh`-style shell.
- **At-rest encryption.** Page-level encryption with a passphrase or a raw key, and `rekey` to rotate it.
- **Hardening.** A fuzz suite over the BSON codec, the MQL parser, the update operators, and WAL replay; property tests for MVCC snapshot isolation and recovery; and a crash-injection recovery campaign.
- **The format is frozen at major version 1.** A file whose major version is newer than the build is rejected with a clear error instead of misread, and the pre-1.0 "format may change" notice is gone.

### The road to 1.0

The engine was built bottom-up across ten milestones, each compiled, tested, and benchmarked before the next:

- **M0** the file format, the storage SPI seam, the WAL substrate, and the WAL-mode pager with a 2Q buffer pool.
- **M1** the slotted-page record store with durable inserts and the `_id` B-tree.
- **M2** the BSON codec, the snapshot-isolation MVCC core, and the `Collection` layer, verified against live MongoDB.
- **M3** the read query path (MQL, projection, sort, skip, limit), the document-mutation write path (the update operators, `updateOne`/`updateMany`/`replaceOne`, the find-and-modify family, `distinct`), and secondary indexes with the cost-based planner.
- **M4** array updates and `$setOnInsert`, bulk writes and upsert, and the aggregation pipeline.
- **M5** sessions and multi-document transactions, serializable snapshot isolation, and the linearizability corpus.
- **M6** the multi-collection engine, the public library API, document validation, capped and TTL collections, and the CLI and shell.
- **M7** observability, online checkpoint and vacuum, online backup, WAL archiving and point-in-time recovery, and the bench and soak harnesses.
- **M8** the full MongoDB wire server: framing and handshake, the data commands and compression, authentication and RBAC, TLS and at-rest encryption, wire sessions and transactions, and driver-compatibility checks.
- **M9** hardened fuzz corpora, `$lookup` with a pipeline, the columnar projection store, crash and recovery at scale, the performance polish, and this release.
