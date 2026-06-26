# doc

An embedded, single-file, MongoDB-compatible document database for Go.
It is to MongoDB what SQLite is to relational data: a library you link into your process, one ordinary file on disk, no server, no daemon.

The whole database is a single self-describing `.doc` file, durability comes from a write-ahead log, and you open it with a path and a line of code.
doc speaks the MongoDB document model (BSON documents in collections, ObjectId `_id`s) and the MongoDB Query Language, its Go API is shaped like the official MongoDB Go driver, and a server mode answers the MongoDB wire protocol so existing drivers connect unchanged.

It is written in pure Go with `CGO_ENABLED=0`, with no third-party dependencies in the core, so it cross-compiles to a static binary on every platform Go targets.

Documentation: https://doc.tamnd.com

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

	users := db.Database("shop").Collection("users")
	if _, err := users.InsertOne(ctx, doc.M{"name": "ada", "age": 36}); err != nil {
		log.Fatal(err)
	}

	var u doc.M
	if err := users.FindOne(ctx, doc.M{"name": "ada"}).Decode(&u); err != nil {
		log.Fatal(err)
	}
	fmt.Println(u)
}
```

The same engine is a command-line tool:

```sh
doc app.doc --eval 'db.users.find({age: {$gte: 18}})'
```

And it serves the MongoDB wire protocol when you want an existing driver or `mongosh` to connect:

```sh
doc app.doc serve --port 27017
mongosh "mongodb://localhost:27017"
```

## Install

```sh
go install github.com/tamnd/doc/cmd/doc@latest   # the CLI
go get github.com/tamnd/doc@latest               # the library
```

Or with a package manager:

```sh
brew install tamnd/tap/doc                        # macOS, Linux
scoop install doc                                 # Windows (after adding the bucket)
docker run --rm -v "$PWD:/data" ghcr.io/tamnd/doc app.doc --eval 'db.users.count()'
```

See the [installation guide](https://doc.tamnd.com/getting-started/installation/) for the Linux apt and dnf repositories and the release archives.

## What you get

- The MongoDB document model and query language, with dotted paths and array fan-out.
- The aggregation pipeline, including `$lookup` with the MongoDB 5.0 pipeline form.
- A Go API shaped like the official MongoDB driver, so existing code moves over with little change.
- Multi-document transactions under snapshot or serializable isolation, on an MVCC core where readers never block writers.
- Single-field, compound, multikey, unique, sparse, partial, and TTL indexes, with a cost-based planner and `Explain`.
- A write-ahead log, group commit, crash recovery, online backup, WAL archiving, and point-in-time restore.
- A MongoDB wire server with SCRAM authentication, RBAC, and TLS, checked against the official Go, Node, and Python drivers and `mongosh`.
- At-rest page-level encryption, and an interactive shell and operational tooling.

doc is at v1: the library API, the PRAGMA catalogue, and the file format are stable.
See [stability](https://doc.tamnd.com/reference/stability/).

## Documentation

- [Introduction](https://doc.tamnd.com/getting-started/introduction/) and [quick start](https://doc.tamnd.com/getting-started/quick-start/).
- [Guides](https://doc.tamnd.com/guides/): CRUD and queries, indexes, transactions, the wire server, operations, and tuning.
- [Migration from the MongoDB Go driver](https://doc.tamnd.com/reference/migration-from-the-mongodb-go-driver/).
- [CLI](https://doc.tamnd.com/reference/cli/) and [configuration](https://doc.tamnd.com/reference/configuration/) reference.

## Build and test

doc has no third-party dependencies.

```sh
make build   # go build ./...
make test    # go test -race ./...
make lint    # gofmt check + go vet
make bench   # run every benchmark once as a smoke check
```

All Go commands run with `CGO_ENABLED=0`.

## License

Apache-2.0. See [LICENSE](LICENSE).
