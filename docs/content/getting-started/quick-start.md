---
title: "Quick start"
description: "Create a .doc file, insert documents, and run a query and an aggregation, first from the command line and then from Go."
weight: 30
---

This page goes from nothing to a working query twice: once with the `doc` command line and once with the Go library.
Both end with real documents coming back.

## From the command line

Insert a few documents.
The shell speaks a `mongosh`-style surface, so the calls look like the ones you would type in a Mongo shell:

```sh
doc app.doc --eval 'db.users.insertMany([
  {name: "ada", age: 36, city: "london"},
  {name: "grace", age: 45, city: "new york"},
  {name: "linus", age: 21, city: "helsinki"}
])'
```

`app.doc` did not exist, so doc created it.
Now query it:

```sh
doc app.doc --eval 'db.users.find({age: {$gte: 30}})'
```

That prints the two matching documents.
Run an aggregation to count users by city:

```sh
doc app.doc --eval 'db.users.aggregate([
  {$group: {_id: "$city", n: {$sum: 1}}},
  {$sort: {n: -1}}
])'
```

Open the interactive shell to poke around without `--eval` each time:

```sh
doc app.doc
```

Inside, `.help` lists the dot-commands, `.collections` lists collections, and `.pragma` shows the engine settings.
Type `.quit` to leave.

## From Go

The same database, opened as a library, is a short program:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tamnd/doc"
)

func main() {
	ctx := context.Background()

	db, err := doc.Open("app.doc")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	users := db.Database("default").Collection("users")

	// Insert.
	if _, err := users.InsertMany(ctx, []any{
		doc.M{"name": "ada", "age": 36, "city": "london"},
		doc.M{"name": "grace", "age": 45, "city": "new york"},
		doc.M{"name": "linus", "age": 21, "city": "helsinki"},
	}); err != nil {
		log.Fatal(err)
	}

	// Find one and decode it into a struct.
	var ada struct {
		Name string `bson:"name"`
		Age  int    `bson:"age"`
		City string `bson:"city"`
	}
	if err := users.FindOne(ctx, doc.M{"name": "ada"}).Decode(&ada); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s is %d, in %s\n", ada.Name, ada.Age, ada.City)

	// Find many.
	cur, err := users.Find(ctx, doc.M{"age": doc.M{"$gte": 30}})
	if err != nil {
		log.Fatal(err)
	}
	var adults []doc.M
	if err := cur.All(ctx, &adults); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d adults\n", len(adults))

	// Aggregate: count by city.
	pipeline := doc.A{
		doc.M{"$group": doc.M{"_id": "$city", "n": doc.M{"$sum": 1}}},
		doc.M{"$sort": doc.M{"n": -1}},
	}
	agg, err := users.Aggregate(ctx, pipeline)
	if err != nil {
		log.Fatal(err)
	}
	var byCity []doc.M
	if err := agg.All(ctx, &byCity); err != nil {
		log.Fatal(err)
	}
	fmt.Println(byCity)
}
```

Run it:

```sh
go run .
```

`doc.M` is a `map[string]any`, the same role `bson.M` plays in the MongoDB driver.
`doc.A` is the ordered slice you build a pipeline from.
A `FindOne` that matches nothing returns `doc.ErrNoDocuments`, which you check with `errors.Is`.

## Use an in-memory database

Pass `:memory:` as the path to get a database that never touches disk, which is handy for tests:

```go
db, err := doc.Open(":memory:")
```

## What is next

- Work through the [CRUD and queries](/guides/crud-and-queries/) guide for the full read and write surface.
- Add [indexes](/guides/indexes-and-planning/) and read a query plan.
- Coming from MongoDB? See the [migration guide](/reference/migration-from-the-mongodb-go-driver/).
