---
title: "Migration from the MongoDB Go driver"
description: "How to move code from go.mongodb.org/mongo-driver to doc: what is identical, what changes, and what is not there."
weight: 30
---

doc's Go API is shaped like the official MongoDB Go driver on purpose.
The method names, the result types, the options builders, and the cursor model match, so most code moves over by changing imports and how you open the handle.
This page lists what is the same, what changes, and what doc does not have.

## The short version

Replace the connect call and the import paths.
Everything from `Database` and `Collection` down looks the same.

```go
// MongoDB Go driver
import (
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/bson"
)

client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
coll := client.Database("shop").Collection("users")
res, err := coll.InsertOne(ctx, bson.M{"name": "ada"})
```

```go
// doc
import (
	"github.com/tamnd/doc"
	"github.com/tamnd/doc/options"
)

db, err := doc.Open("app.doc")
coll := db.Database("shop").Collection("users")
res, err := coll.InsertOne(ctx, doc.M{"name": "ada"})
```

## Opening the database

This is the one real difference in everyday code.
There is no server and no connection string.
`doc.Open(path, opts...)` returns a `*doc.DB`, which plays the role of the driver's `*mongo.Client`.
Use `:memory:` for an in-memory database.
Tune it with functional options instead of a URI; see [configuration](/reference/configuration/).

| MongoDB driver | doc |
| -------------- | --- |
| `mongo.Connect(ctx, options.Client().ApplyURI(uri))` | `doc.Open(path, opts...)` |
| `client.Ping(ctx, nil)` | not needed; `Open` either succeeds or returns an error |
| `client.Disconnect(ctx)` | `db.Close()` |

`db.Close()` is idempotent, and any handle derived from a closed `DB` returns `doc.ErrClosed`.

## Types

The helper types have the same names and roles, under the `doc` package instead of `bson`.

| MongoDB driver | doc |
| -------------- | --- |
| `bson.M` | `doc.M` |
| `bson.D` | `doc.D` |
| `bson.A` | `doc.A` |
| `bson.E` | `doc.E` |
| `primitive.ObjectID` | `doc.ObjectID` |
| `mongo.ErrNoDocuments` | `doc.ErrNoDocuments` |

Struct tags are unchanged: doc reads the same `bson:"..."` tags when it decodes into a struct.

## Options

The options builders live in `github.com/tamnd/doc/options` and read the same.

```go
opts := options.Update().SetUpsert(true)
res, err := coll.UpdateOne(ctx, filter, update, opts)

after := options.FindOneAndUpdate().SetReturnDocument(options.After)
err := coll.FindOneAndUpdate(ctx, filter, update, after).Decode(&out)
```

## Operations that are identical

These work the same way, with the same signatures and result types:

- Reads: `FindOne`, `Find`, `CountDocuments`, `EstimatedDocumentCount`, `Distinct`, `Aggregate`.
- Writes: `InsertOne`, `InsertMany`, `UpdateOne`, `UpdateMany`, `ReplaceOne`, `DeleteOne`, `DeleteMany`, `BulkWrite`.
- Find-and-modify: `FindOneAndUpdate`, `FindOneAndReplace`, `FindOneAndDelete`.
- Cursors: `cur.All(ctx, &slice)`, `cur.Next(ctx)`, `cur.Decode(&v)`, `cur.Current()`, `cur.Err()`, `cur.Close(ctx)`.
- Indexes: `coll.Indexes().CreateOne`, `CreateMany`, `List`, `ListSpecifications`, `DropOne`, `DropAll`, with `IndexModel` and the index options.
- Bulk models: `doc.NewInsertOneModel`, `doc.NewUpdateOneModel`, `doc.NewDeleteOneModel`, and the rest, returning a `*BulkWriteResult`.
- Admin: `db.ListCollectionNames`, `db.ListDatabaseNames`, `coll.Drop`, `db.Drop`, `db.RunCommand`.

The MongoDB Query Language and the aggregation pipeline are the same language, so your filters, updates, and pipelines carry over unchanged.

## Transactions and change streams

Sessions and transactions follow the driver shape: `db.StartSession`, `sess.WithTransaction`, and the manual `sess.StartTransaction` / `CommitTransaction` / `AbortTransaction` with `sess.EndSession`.
For the manual form, bind a context to the session with `doc.NewSessionContext(ctx, sess)` so the operations you run join the transaction.
Isolation is snapshot by default and serializable on request through `PRAGMA default_isolation`, which replaces the driver's read-concern and write-concern knobs for a single-file database.
Change streams use `coll.Watch(ctx, pipeline)` and `db.Watch(ctx, pipeline)` with the same event shape.

See the [transactions guide](/guides/transactions-and-change-streams/).

## What is not there

doc is one file on one machine, so the parts of the driver that exist only because MongoDB is a distributed server have no equivalent:

- No connection strings, hosts, ports, or connection pools (until you run the [wire server](/guides/the-wire-server/), which a normal driver then connects to in the usual way).
- No replica sets, no read preferences, and no per-operation read or write concern across nodes. Durability is set once with `synchronous`, and isolation with `default_isolation`.
- No sharding, no `mongos`, and no zones.
- A `.doc` file is single-writer. Run one writer at a time, and put the wire server in front when several processes need to share one database.

If you reach for one of these, you are reaching past what an embedded single-file database does.
Everything else is the API you already know.

## Talking to doc with the real driver

You do not have to port at all if you would rather keep the official driver.
Run `doc <file> serve` and point `go.mongodb.org/mongo-driver` at `mongodb://localhost:27017`.
The wire server was checked against the official Go, Node, and Python drivers and `mongosh`.
See the [wire-server guide](/guides/the-wire-server/).
