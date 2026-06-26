---
title: "Introduction"
description: "What doc is, how it stores data, and when to reach for it instead of a MongoDB server or a SQL database."
weight: 10
---

doc is an embedded document database.
You link it into your Go program as a library, and your data lives in a single `.doc` file next to your binary.
There is no server to start, no port to open, and no cluster to operate.
This is the same trade SQLite makes for relational data, applied to the MongoDB document model.

## The model

A database holds named collections.
A collection holds BSON documents, each with an `_id`.
You query with the MongoDB Query Language and reshape with the aggregation pipeline.
If you have used MongoDB, the data model and the query surface are the ones you already know.

The Go API is shaped like the official MongoDB Go driver on purpose.
`Open` returns a handle, `Database` and `Collection` walk down to a collection, and the method names (`InsertOne`, `Find`, `UpdateMany`, `BulkWrite`, `Aggregate`, `FindOneAndUpdate`) match.
Code written against `go.mongodb.org/mongo-driver` moves over with small changes, which the [migration guide](/reference/migration-from-the-mongodb-go-driver/) covers in detail.

## How it stores data

The whole database is one file.
The file format is self-describing, with a magic header, a page layout, and a free list, the same shape SQLite uses.
Writes go through a write-ahead log first, then fold into the main file at a checkpoint, so a crash at any point recovers to the last committed transaction.

Concurrency uses multi-version concurrency control.
Each transaction reads a consistent snapshot, readers never block writers, and writers never block readers.
The default isolation is snapshot; serializable is one PRAGMA away when you need it.

Reads are fast because the working set lives in a page cache (a 2Q buffer pool) in your process, with no network and no serialization across a socket in the path.
A point lookup by `_id` resolves through the `_id` index and returns in well under a microsecond on a normal laptop.

## When to use it

doc fits when:

- You want MongoDB's document model and query language without running a MongoDB server.
- Your data fits on one machine, which covers a large share of real applications.
- You want a database you can embed, copy as a file, and ship inside a single static binary.
- You are building a CLI, a desktop app, a test suite, an edge service, or anything that should not depend on a separate database process.

It is not a fit when you need horizontal sharding across many machines, or many independent processes writing the same file at once.
A `.doc` file is single-writer.
When you do need several processes to share one database, run the [wire server](/guides/the-wire-server/) in front of the file and point your drivers at it.

## What is next

Install the binary or the library on the [installation](/getting-started/installation/) page, then run your first query on the [quick start](/getting-started/quick-start/).
