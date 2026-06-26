---
title: "Indexes and query planning"
description: "Create single-field, compound, multikey, unique, sparse, partial, and TTL indexes, and read the plan the cost-based planner picks with Explain."
weight: 20
---

## Why indexes matter here

The query planner can only beat a full collection scan when there is an index it can use.
Without one, every query reads each document in the collection and tests it against the filter.
That is fine for a few hundred documents and gets slow as the collection grows.

An index gives the planner a sorted structure it can seek into and scan in order.
The field key encoding is order-preserving, so a range query like `{age: {$gte: 18, $lt: 65}}` turns into a single contiguous scan of the index instead of a scan of the whole collection.

The planner is cost-based with a pull-based execution engine.
For each query it estimates the cost of the available plans and picks the cheapest one.
The plans it chooses between are a collection scan, an index scan, and a covered scan (an index scan that answers the query entirely from the index, without fetching the documents).

The `_id` index always exists, so lookups by `_id` are already indexed on every collection.

## The index view

All index management goes through the collection's index view.
`c.Indexes()` returns an `*IndexView`, and you create, list, and drop indexes through it.

```go
iv := c.Indexes()
```

## Single-field index

Pass an `IndexModel` with the keys you want to index.
A value of `1` means ascending, `-1` means descending.
`CreateOne` returns the generated index name and an error.

```go
name, err := c.Indexes().CreateOne(ctx, doc.IndexModel{
    Keys: doc.D{{Key: "email", Value: 1}},
})
if err != nil {
    return err
}
// name is something like "email_1"
```

After this, a query like `c.Find(ctx, doc.M{"email": "a@b.com"})` can use the index instead of scanning the collection.

## Compound index and the prefix rule

A compound index lists more than one key in the `Keys` document.
Order matters: the index is sorted by the first key, then by the second within each value of the first, and so on.

```go
_, err := c.Indexes().CreateOne(ctx, doc.IndexModel{
    Keys: doc.D{
        {Key: "status", Value: 1},
        {Key: "createdAt", Value: -1},
    },
})
```

A compound index serves a query when the query's fields form a prefix of the index keys.
The index above on `{status, createdAt}` can serve:

- a filter on `status` alone,
- a filter on `status` plus a filter or sort on `createdAt`.

It cannot serve a query that only filters on `createdAt`, because `createdAt` is not a prefix of the index.
If you need that query indexed too, add a separate index keyed on `createdAt`.

## Multikey index over array fields

When an indexed field holds an array, the index stores one entry per array element.
This is a multikey index, and it is created automatically: you do not ask for it, the commit path detects the array values and maintains the extra entries.

```go
// documents like {tags: ["go", "db", "embedded"]}
_, err := c.Indexes().CreateOne(ctx, doc.IndexModel{
    Keys: doc.D{{Key: "tags", Value: 1}},
})
```

A query such as `c.Find(ctx, doc.M{"tags": "go"})` then uses the index to find every document whose `tags` array contains `"go"`.

## Unique index and the duplicate-key error

A unique index rejects any insert or update that would create a second document with the same indexed value.
Set it with the index options.

```go
_, err := c.Indexes().CreateOne(ctx, doc.IndexModel{
    Keys:    doc.D{{Key: "email", Value: 1}},
    Options: options.Index().SetUnique(true),
})
```

When a write would violate the constraint, the insert or update fails with a duplicate-key error.
Check for it on the write call and handle it (for example, treat it as "already exists"):

```go
_, err := c.InsertOne(ctx, doc.M{"email": "a@b.com"})
if err != nil {
    // err is a duplicate-key error if the email already exists
    return err
}
```

A compound index can be unique too, in which case the combination of the keyed fields must be unique rather than each field on its own.

## Sparse and partial indexes

A sparse index only holds entries for documents that contain the indexed field.
Documents missing the field are left out of the index entirely.

```go
_, err := c.Indexes().CreateOne(ctx, doc.IndexModel{
    Keys:    doc.D{{Key: "phone", Value: 1}},
    Options: options.Index().SetSparse(true),
})
```

A partial index goes further: you give it a filter, and only documents matching that filter are indexed.

```go
_, err := c.Indexes().CreateOne(ctx, doc.IndexModel{
    Keys: doc.D{{Key: "lastLogin", Value: 1}},
    Options: options.Index().
        SetPartialFilterExpression(doc.M{"active": true}),
})
```

Prefer a partial index when you can describe the subset you actually query with a filter.
It is more precise than sparse (sparse only knows "field present or not"), it keeps the index smaller, and a partial index can also be unique over just the matching subset.
Reach for sparse only when the rule you want is simply "skip documents without this field."

Note that the planner can only use a partial index for a query whose filter is implied by the partial filter expression, so the indexed subset has to cover the queries you intend to serve.

## TTL index for expiring documents

A TTL index deletes documents some number of seconds after a date value in the indexed field.
Set `expireAfterSeconds` through the options.

```go
_, err := c.Indexes().CreateOne(ctx, doc.IndexModel{
    Keys:    doc.D{{Key: "createdAt", Value: 1}},
    Options: options.Index().SetExpireAfterSeconds(3600),
})
```

Here a document is eligible for removal one hour after its `createdAt` time.
Expiration is not exact to the second: a background sweeper runs periodically and removes documents whose deadline has passed, so there is a small lag between the deadline and the actual delete.
The indexed field has to hold a date value for the sweeper to act on it.

## Listing and dropping indexes

`ListSpecifications` returns the indexes as a slice you can iterate directly.
Each `*IndexSpecification` carries fields like `Name`, `Unique`, and `ExpireAfterSeconds`.

```go
specs, err := c.Indexes().ListSpecifications(ctx)
if err != nil {
    return err
}
for _, s := range specs {
    fmt.Printf("%s unique=%v ttl=%v\n", s.Name, s.Unique, s.ExpireAfterSeconds)
}
```

There is also `c.Indexes().List(ctx)`, which returns a cursor in the driver style if you prefer to iterate that way.

Drop a single index by name, or drop every index on the collection at once.
`DropAll` does not remove the `_id` index, which always stays.

```go
err := c.Indexes().DropOne(ctx, "email_1")
// or
err := c.Indexes().DropAll(ctx)
```

## Reading a plan with Explain

To see which plan the planner picked for a query, use Explain.
The library exposes an Explain path on the query, and the shell has an `.explain` command.

The shell form is:

```
.explain <coll> [filter] [verbosity]
```

Verbosity is either `queryPlanner` (the default) or `executionStats`.

`queryPlanner` shows the plan the planner chose and the candidate plans it considered, without running the query.
Use it to confirm an index is being used and to see whether the chosen plan is a collection scan, an index scan, or a covered scan.

```
.explain users {"email": "a@b.com"}
```

`executionStats` runs the query and adds the real numbers: how many documents were examined, how many index entries were scanned, and how many were returned.
Use it to confirm the plan is efficient in practice, for example that the number of documents examined is close to the number returned rather than the whole collection.

```
.explain users {"status": "active"} executionStats
```

If `executionStats` shows a collection scan examining far more documents than it returns, that is the signal to add an index that matches the filter.

## Covered queries

A covered query is one the planner answers entirely from an index, without fetching the underlying documents.
This happens when the index contains every field the query needs, both for the filter and for the projection.

For example, with a compound index on `{status, createdAt}`, a query that filters on `status` and returns only `status` and `createdAt` can be served as a covered scan.
Covered scans are the cheapest plan because they skip the document fetch step entirely.
In `.explain` output a covered query shows up as a covered scan rather than an index scan followed by a fetch.

If you have a hot query that only needs a couple of fields, building a compound index that includes those fields can turn it into a covered query.

## Next

For the columnar projection store (PRAGMA-controlled) that speeds up analytical scans over many documents and few fields, and for other throughput settings, see [/guides/performance-tuning/](/guides/performance-tuning/).
For backups, compaction, and running the database day to day, see [/guides/operations/](/guides/operations/).
