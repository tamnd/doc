---
title: "Guides"
linkTitle: "Guides"
description: "Task-oriented walkthroughs for the things people actually do with doc: CRUD and queries, indexes, transactions, the wire server, operations, and tuning."
weight: 20
featured: true
---

Each guide is built around a job rather than a flag: putting documents in and getting them back out, indexing them so the planner can find them quickly, running multi-document transactions, serving the file to existing drivers, keeping a `.doc` file healthy, and making it fast.
They assume you have worked through the [quick start](/getting-started/quick-start/).

- [CRUD and queries](/guides/crud-and-queries/): insert, find, update, delete, the MQL operators, and the aggregation pipeline.
- [Indexes and query planning](/guides/indexes-and-planning/): single-field, compound, multikey, unique, sparse, partial, and TTL indexes, and reading a plan with Explain.
- [Transactions and change streams](/guides/transactions-and-change-streams/): sessions, snapshot and serializable isolation, and tailing changes.
- [The wire-protocol server](/guides/the-wire-server/): serving a `.doc` file so `mongosh` and the official drivers connect unchanged.
- [Operations](/guides/operations/): integrity checks, compaction, checkpoints, vacuum, online backup, WAL archiving, point-in-time restore, and moving data.
- [Performance tuning](/guides/performance-tuning/): durability levels, the page cache, the columnar store, and the benchmark gate.
