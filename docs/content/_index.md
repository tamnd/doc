---
title: "doc"
description: "An embedded, single-file, MongoDB-compatible document database for Go. It is to MongoDB what SQLite is to relational data: a library you link into your process, one ordinary file on disk, no server, no daemon. The binary is doc, the file extension is .doc."
heroTitle: "MongoDB that lives in one file"
heroLead: "Open a .doc file, insert documents, and run MongoDB queries and aggregations against a durable store with sub-millisecond reads. The API mirrors the MongoDB Go driver, writes are transactional through a write-ahead log, and the whole database is one ordinary file you can copy. Pure Go, no server, no cluster, no daemon, with an optional wire-protocol server when you want one."
heroPrimaryURL: "/getting-started/quick-start/"
heroPrimaryText: "Get started"
---

Most document databases need a service: a server to run, a replica set to manage, a process to keep alive.
That is a lot of moving parts for data that often fits on one machine.
doc takes the SQLite approach instead.
It is a library you link into your Go process and one file on disk.
You open a `.doc` file, insert documents, query them, and run aggregations, all in the same process, with no network in the path.

The Go API is shaped like the official MongoDB driver, so code written against `go.mongodb.org/mongo-driver` moves over with little change:

```go
db, err := doc.Open("app.doc")
if err != nil {
	log.Fatal(err)
}
defer db.Close()

users := db.Database("shop").Collection("users")
if _, err := users.InsertOne(ctx, doc.M{"name": "ada", "age": 36}); err != nil {
	log.Fatal(err)
}

var u doc.M
if err := users.FindOne(ctx, doc.M{"name": "ada"}).Decode(&u); err != nil {
	log.Fatal(err)
}
fmt.Println(u)
```

The same engine ships as a command-line tool, `doc`, so you can work with a file without writing any Go:

```sh
doc app.doc --eval 'db.users.insertOne({name: "ada", age: 36})'
doc app.doc --eval 'db.users.find({age: {$gte: 18}})'
```

And when you want an existing driver or `mongosh` to connect, the same file serves the MongoDB wire protocol:

```sh
doc app.doc serve --port 27017
mongosh "mongodb://localhost:27017"
```

## What you get

- **The MongoDB document model.** BSON documents in collections, ObjectId `_id`s, and the MongoDB Query Language: comparison, logical, element, and array operators, dotted paths, and array fan-out.
- **The aggregation pipeline.** `$match`, `$project`, `$group` with the usual accumulators, `$sort`, `$skip`, `$limit`, `$unwind`, and `$lookup` including the MongoDB 5.0 pipeline form.
- **A driver-shaped Go API.** `InsertOne`, `Find`, `UpdateMany`, `BulkWrite`, `FindOneAndUpdate`, `Aggregate`, cursors, and the index view, all named and shaped like the official driver.
- **Real transactions.** Multi-document, multi-collection transactions under snapshot or serializable isolation, with MVCC so readers never block writers.
- **Indexes and a planner.** Single-field, compound, multikey, unique, sparse, partial, and TTL indexes, maintained on commit, with a cost-based planner and `Explain`.
- **Durability you can trust.** A write-ahead log, group commit, crash recovery to the last committed transaction, online backup, WAL archiving, and point-in-time restore.
- **A wire server.** Serve a `.doc` file over the MongoDB wire protocol so `mongosh` and the official drivers connect unchanged, with SCRAM authentication, RBAC, and TLS.
- **One static binary.** Pure Go, `CGO_ENABLED=0`, no third-party dependencies in the core, cross-compiled to every platform Go targets.

## Where to go next

- New here? Start with the [introduction](/getting-started/introduction/), then the [quick start](/getting-started/quick-start/).
- Want to install it? See [installation](/getting-started/installation/).
- Coming from the MongoDB Go driver? The [migration guide](/reference/migration-from-the-mongodb-go-driver/) covers what is the same and what differs.
- Looking for a specific task? The [guides](/guides/) cover CRUD and queries, indexes, transactions, the wire server, operations, and tuning.
- Need every flag? The [CLI reference](/reference/cli/) is the full surface.
