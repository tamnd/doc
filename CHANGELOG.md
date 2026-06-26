# Changelog

All notable changes to doc are recorded here.
The format follows Keep a Changelog, and the project follows semantic versioning.

## [1.0.0]

The first stable release.
The engine is feature-complete across milestones M0 through M9, the library API is frozen for the 1.x line, the PRAGMA catalogue is stable, and the on-disk format is frozen at major version 1.

### Added

- Embedded document database in a single `.doc` file, with a driver-shaped Go API: `Open` returns a `DB`, and `Database`/`Collection` expose `InsertOne`, `InsertMany`, `Find`, `FindOne`, `UpdateOne`, `UpdateMany`, `ReplaceOne`, `DeleteOne`, `DeleteMany`, `BulkWrite`, the find-and-modify family, `Distinct`, `CountDocuments`, and `Aggregate`.
- The MongoDB Query Language: comparison, logical, element, and array operators, dotted-path access with array fan-out, MongoDB null and missing semantics, and the BSON cross-type total order.
- The aggregation pipeline: `$match`, `$project`, `$group` with accumulators, `$sort`, `$skip`, `$limit`, `$unwind`, and `$lookup` including the MongoDB 5.0 pipeline form.
- Indexes: single-field, compound, multikey, unique, sparse, partial, and TTL, maintained on commit, with a cost-based planner over collection, index, and covered scans, and `Explain`.
- Transactions through sessions, under snapshot isolation by default or serializable snapshot isolation on request, on an MVCC core where readers never block writers.
- Change streams on a collection or a whole database.
- Durability: a WAL-mode pager with a 2Q buffer pool and group commit, crash recovery to the last committed transaction, online backup, incremental backup, WAL archiving, and point-in-time restore.
- A MongoDB wire-protocol server (`doc <file> serve`) with SCRAM-SHA-256 authentication, role-based access control, TLS including x509, wire compression, and wire-level sessions and transactions, checked against the official Go, Node, and Python drivers and `mongosh`.
- At-rest page-level encryption with a passphrase or a raw key, and `rekey` to rotate it.
- A command-line tool with an interactive `mongosh`-style shell and subcommands for `validate`, `compact`, `checkpoint`, `vacuum`, `backup`, `restore`, `import`, `export`, `dump`, `load`, `stats`, `schema`, and `rekey`.
- The optional columnar projection store for accelerating `$group`, `$sum`, and `$avg` over many documents and few fields.
- Observability: the slow-operation profiler, metrics, and a Prometheus surface.
- A documentation site, a migration guide from the MongoDB Go driver, godoc comments on the public API, and published benchmark numbers with the setup disclosed.

### Stability

- The exported Go API is frozen at v1 under semantic versioning.
- The PRAGMA catalogue is stable; an unknown PRAGMA is rejected rather than silently accepted.
- The file format is frozen at major version 1; a file with a newer major version is rejected with a clear error instead of being misread.

[1.0.0]: https://github.com/tamnd/doc/releases/tag/v1.0.0
