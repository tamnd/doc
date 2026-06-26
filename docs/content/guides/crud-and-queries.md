---
title: "CRUD and queries"
description: "Insert, find, update, and delete documents, then query them with the MongoDB Query Language and the aggregation pipeline, all through the same API shape as the MongoDB Go driver."
weight: 10
---

This guide walks through the day-to-day work of storing and reading documents with `doc`.
The API mirrors the official MongoDB Go driver, so if you have used `go.mongodb.org/mongo-driver` the method names and return shapes will already be familiar.
The difference is that there is no server: `doc` is a library you link into your process, and your data lives in a single file on disk.

## Opening a database and getting a collection

You start by opening a database file.
A collection is reached through a database, the same two-step path you would use against MongoDB.

```go
package main

import (
	"context"
	"log"

	"github.com/tamnd/doc"
)

func main() {
	db, err := doc.Open("app.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	users := db.Database("app").Collection("users")
	_ = users
}
```

Pass `:memory:` as the path when you want an ephemeral database that never touches disk, which is handy for tests.

```go
db, err := doc.Open(":memory:")
```

All examples below assume you already have a `*doc.Collection` and a context.
Use `ctx := context.Background()` unless you have a real deadline to pass.

## Document helper types

Package `doc` gives you a few aliases for building documents and filters.

`doc.M` is `map[string]any`, the quickest way to write a document when field order does not matter.
`doc.D` is an ordered document, a slice of `doc.E` pairs where each `E` is `{Key string; Value any}`.
Reach for `doc.D` when order matters, for example in a multi-key sort or anywhere MongoDB itself is order-sensitive.
`doc.A` is `[]any`, used for array values and for aggregation pipelines.
`doc.ObjectID` is the 12-byte id type, the value you get back when you insert a document without your own `_id`.

```go
filter := doc.M{"status": "active"}

sort := doc.D{
	{Key: "age", Value: -1},
	{Key: "name", Value: 1},
}
```

## Inserting one and many

`InsertOne` writes a single document and returns the id it stored.
When you do not supply an `_id`, `doc` generates an `ObjectID` and hands it back on `InsertedID`.

```go
ctx := context.Background()

res, err := users.InsertOne(ctx, doc.M{
	"name":  "Ada",
	"age":   36,
	"tags":  doc.A{"founder", "engineer"},
})
if err != nil {
	log.Fatal(err)
}
id := res.InsertedID.(doc.ObjectID)
log.Printf("inserted %s", id.Hex())
```

`InsertMany` takes a slice of documents and returns the ids in order on `InsertedIDs`.

```go
res, err := users.InsertMany(ctx, []any{
	doc.M{"name": "Grace", "age": 41},
	doc.M{"name": "Linus", "age": 29},
})
if err != nil {
	log.Fatal(err)
}
log.Printf("inserted %d documents", len(res.InsertedIDs))
```

## Finding one document

`FindOne` returns a `*SingleResult`.
Call `Decode` to unmarshal the match into a struct or a map, and tag your struct fields with `bson` so they line up with the stored keys.

```go
type User struct {
	ID   doc.ObjectID `bson:"_id"`
	Name string       `bson:"name"`
	Age  int          `bson:"age"`
}

var u User
err := users.FindOne(ctx, doc.M{"name": "Ada"}).Decode(&u)
if errors.Is(err, doc.ErrNoDocuments) {
	log.Println("no such user")
} else if err != nil {
	log.Fatal(err)
}
```

When nothing matches, the error is `doc.ErrNoDocuments`.
Always test for it with `errors.Is`, because that is the one outcome that is not really a failure.

## Finding many documents

`Find` returns a cursor.
The simplest path is `All`, which decodes every matching document into a slice in one call.

```go
cur, err := users.Find(ctx, doc.M{"age": doc.M{"$gte": 30}})
if err != nil {
	log.Fatal(err)
}

var results []User
if err := cur.All(ctx, &results); err != nil {
	log.Fatal(err)
}
```

For large result sets, step through the cursor one document at a time so you never hold them all in memory at once.
Close the cursor when you are done, and check `Err` after the loop to catch an iteration error.

```go
cur, err := users.Find(ctx, doc.M{})
if err != nil {
	log.Fatal(err)
}
defer cur.Close(ctx)

for cur.Next(ctx) {
	var u User
	if err := cur.Decode(&u); err != nil {
		log.Fatal(err)
	}
	log.Println(u.Name)
}
if err := cur.Err(); err != nil {
	log.Fatal(err)
}
```

`Current` gives you the raw bytes of the document the cursor is parked on if you would rather not decode into a type.

## Counting and distinct values

`CountDocuments` returns how many documents match a filter.

```go
n, err := users.CountDocuments(ctx, doc.M{"status": "active"})
```

`Distinct` returns the distinct values of one field across the matching documents, as a `[]any`.

```go
vals, err := users.Distinct(ctx, "age", doc.M{})
```

The `errors` package from the standard library is what you import for `errors.Is`.

## Filtering with the MongoDB Query Language

A filter is just a document.
`doc.M{"age": 36}` matches documents whose `age` equals 36.
To go beyond equality you use operators, written as nested documents keyed by `$`.

Comparison operators cover the obvious cases.

```go
// age greater than or equal to 30 and less than 65
users.Find(ctx, doc.M{"age": doc.M{"$gte": 30, "$lt": 65}})

// status is one of a set
users.Find(ctx, doc.M{"status": doc.M{"$in": doc.A{"active", "trial"}}})

// name is not "Ada"
users.Find(ctx, doc.M{"name": doc.M{"$ne": "Ada"}})
```

The full comparison set is `$eq`, `$ne`, `$gt`, `$gte`, `$lt`, `$lte`, `$in`, and `$nin`.

Logical operators combine clauses.

```go
users.Find(ctx, doc.M{
	"$or": doc.A{
		doc.M{"age": doc.M{"$lt": 18}},
		doc.M{"age": doc.M{"$gte": 65}},
	},
})
```

`$and`, `$or`, `$not`, and `$nor` are all supported.

Element operators ask about the field itself rather than its value.

```go
// the field exists
users.Find(ctx, doc.M{"email": doc.M{"$exists": true}})

// the value is stored as a string
users.Find(ctx, doc.M{"age": doc.M{"$type": "string"}})
```

Array operators match against the contents of array fields.

```go
// tags contains both values
users.Find(ctx, doc.M{"tags": doc.M{"$all": doc.A{"founder", "engineer"}}})

// at least one scores element is above 90
users.Find(ctx, doc.M{"scores": doc.M{"$elemMatch": doc.M{"$gt": 90}}})

// exactly three tags
users.Find(ctx, doc.M{"tags": doc.M{"$size": 3}})
```

`$regex` matches strings against a pattern.

```go
users.Find(ctx, doc.M{"name": doc.M{"$regex": "^A"}})
```

You can reach into nested documents and arrays with dotted paths, and a path that crosses an array fans out across its elements.

```go
// any address in the addresses array has city "Oslo"
users.Find(ctx, doc.M{"addresses.city": "Oslo"})
```

Matching follows MongoDB semantics throughout: the null-versus-missing distinction, type bracketing within comparison operators, and a total ordering across BSON types so that mixed-type fields sort and compare the way they do on a real server.

## Updating documents

An update names a filter and an update document built from operators.
`UpdateOne` touches the first match, `UpdateMany` touches all of them.
Both return an `*UpdateResult` carrying `MatchedCount`, `ModifiedCount`, `UpsertedCount`, and `UpsertedID`.

```go
res, err := users.UpdateOne(ctx,
	doc.M{"name": "Ada"},
	doc.M{"$set": doc.M{"status": "active"}},
)
if err != nil {
	log.Fatal(err)
}
log.Printf("matched %d, modified %d", res.MatchedCount, res.ModifiedCount)
```

The field operators are `$set`, `$unset`, `$inc`, `$mul`, `$min`, `$max`, `$rename`, and `$currentDate`.

```go
users.UpdateOne(ctx,
	doc.M{"name": "Ada"},
	doc.M{
		"$inc":   doc.M{"logins": 1},
		"$unset": doc.M{"temp": ""},
		"$currentDate": doc.M{"lastSeen": true},
	},
)
```

Array updates have their own operators: `$push`, `$pull`, `$addToSet`, `$pop`, the positional `$`, and the all-positional `$[]`.

```go
// append a tag
users.UpdateOne(ctx,
	doc.M{"name": "Ada"},
	doc.M{"$push": doc.M{"tags": "investor"}},
)

// add only if not already present
users.UpdateOne(ctx,
	doc.M{"name": "Ada"},
	doc.M{"$addToSet": doc.M{"tags": "founder"}},
)

// update the matched array element in place
users.UpdateOne(ctx,
	doc.M{"scores.subject": "math"},
	doc.M{"$set": doc.M{"scores.$.value": 100}},
)
```

To insert a document when the filter matches nothing, set upsert.
`$setOnInsert` lets you supply fields that apply only on the insert branch.

```go
import "github.com/tamnd/doc/options"

users.UpdateOne(ctx,
	doc.M{"name": "Margaret"},
	doc.M{
		"$set":         doc.M{"status": "active"},
		"$setOnInsert": doc.M{"createdAt": time.Now()},
	},
	options.Update().SetUpsert(true),
)
```

`ReplaceOne` swaps the whole document, keeping the `_id`, instead of applying operators.

```go
users.ReplaceOne(ctx,
	doc.M{"name": "Ada"},
	doc.M{"name": "Ada", "age": 37, "status": "active"},
)
```

## Find and modify in one step

When you want the document back as well as the change applied, use the find-and-modify methods.
`FindOneAndUpdate`, `FindOneAndReplace`, and `FindOneAndDelete` all return a `*SingleResult` you decode.

By default you get the document as it was before the change.
Ask for `options.After` to get the post-image instead.

```go
import "github.com/tamnd/doc/options"

var updated User
err := users.FindOneAndUpdate(ctx,
	doc.M{"name": "Ada"},
	doc.M{"$inc": doc.M{"age": 1}},
	options.FindOneAndUpdate().SetReturnDocument(options.After),
).Decode(&updated)
if err != nil {
	log.Fatal(err)
}
log.Printf("age is now %d", updated.Age)
```

## Deleting documents

`DeleteOne` removes the first match, `DeleteMany` removes all matches.
Both return a `*DeleteResult` with `DeletedCount`.

```go
res, err := users.DeleteMany(ctx, doc.M{"status": "inactive"})
if err != nil {
	log.Fatal(err)
}
log.Printf("deleted %d", res.DeletedCount)
```

## Bulk writes

`BulkWrite` runs a batch of mixed operations in one call.
You build the operations from write models, then read the totals off the `*BulkWriteResult`.

```go
res, err := users.BulkWrite(ctx, []doc.WriteModel{
	doc.NewInsertOneModel().SetDocument(doc.M{"name": "Edsger"}),
	doc.NewUpdateOneModel().
		SetFilter(doc.M{"name": "Ada"}).
		SetUpdate(doc.M{"$set": doc.M{"status": "active"}}),
	doc.NewDeleteOneModel().SetFilter(doc.M{"name": "Linus"}),
})
if err != nil {
	log.Fatal(err)
}
log.Printf("inserted %d, modified %d", res.InsertedCount, res.ModifiedCount)
```

## Aggregation pipelines

`Aggregate` runs a pipeline, a slice of stage documents, and returns a cursor.
This example filters with `$match`, then groups by status and sums the ages in each group.

```go
pipeline := doc.A{
	doc.M{"$match": doc.M{"age": doc.M{"$gte": 18}}},
	doc.M{"$group": doc.M{
		"_id":     "$status",
		"total":   doc.M{"$sum": "$age"},
		"members": doc.M{"$sum": 1},
	}},
}

cur, err := users.Aggregate(ctx, pipeline)
if err != nil {
	log.Fatal(err)
}

var groups []doc.M
if err := cur.All(ctx, &groups); err != nil {
	log.Fatal(err)
}
```

The pipeline stages cover `$match`, `$project`, `$group`, `$sort`, `$skip`, `$limit`, `$unwind`, `$lookup` (including the MongoDB 5.0 `let`/`pipeline` form), `$count`, `$unset`, and `$addFields`/`$set`.
Inside `$group` you have the accumulators `$sum`, `$avg`, `$min`, `$max`, `$push`, `$addToSet`, `$first`, `$last`, and `$count`.
A columnar projection store sits underneath, so `$group` work built on `$sum` and `$avg` over many documents runs against packed columns rather than reparsing every document.

## Next

Once you are storing and querying documents, look at [indexes and planning](/guides/indexes-and-planning/) to keep those queries fast as the file grows.
For multi-document atomicity and watching for changes, see [transactions and change streams](/guides/transactions-and-change-streams/).
If you are moving an existing codebase over, [migration from the MongoDB Go driver](/reference/migration-from-the-mongodb-go-driver/) lays out what carries across unchanged and what does not.
