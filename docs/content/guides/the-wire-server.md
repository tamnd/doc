---
title: "The wire-protocol server"
description: "Serve a .doc file over the MongoDB wire protocol so mongosh and any MongoDB driver connect to it unchanged, with optional authentication and TLS."
weight: 40
---

doc is an embedded database first.
You open a `.doc` file from Go with `doc.Open` and call the API directly, in-process, with no network involved.
The wire server is the other way in.
It takes that same `.doc` file and serves it over the MongoDB wire protocol, so an existing MongoDB driver or `mongosh` can talk to it without any change to your code.

## When to use the server

Reach for the server when you already have something that speaks MongoDB and you do not want to rewrite it against the Go API.

That usually means one of:

- You have an application built on an official MongoDB driver and you want to point it at a `.doc` file instead of a MongoDB deployment.
- You want to poke at a `.doc` file with `mongosh` or a GUI that speaks the wire protocol.
- You want a small shared instance that a few clients can connect to over a socket, rather than each embedding the library.

If you are writing new Go code, the embedded path is simpler and faster.
The server exists so the rest of the MongoDB ecosystem can reach a `.doc` file unchanged.

## Starting a server

The server is a subcommand of the file:

```
doc app.doc serve
```

That opens `app.doc` and listens on the loopback address, port 27017, the default MongoDB port.

The common flags:

- `--bind <addr>` sets the listen address. The default is loopback, which only accepts local connections. Use `0.0.0.0` to listen on all interfaces.
- `--port <n>` sets the port. The default is 27017.
- `--readonly` serves the file without allowing writes.
- `--auth` requires clients to authenticate.
- `--tls --tls-cert <file> --tls-key <file>` enables TLS, with `--tls-ca <file>` to verify client certificates.
- `--max-conns` caps the number of concurrent connections.
- `--max-conn-idle` closes connections that sit idle too long.
- `--http` adds an HTTP surface for health and metrics.

A typical exposed instance:

```
doc app.doc serve --bind 0.0.0.0 --port 27017
```

Under the hood the server speaks the MongoDB wire protocol: the OP_MSG framing and the `hello`/`isMaster` handshake, the CRUD and query commands (`find`, `insert`, `update`, `delete`, `getMore`, `killCursors`, `aggregate`, `count`, `distinct`, `findAndModify`), wire compression (snappy, zlib, zstd), authentication with SCRAM-SHA-256 and role-based access control, TLS including x509, and wire-level sessions with multi-document transactions.
It has been checked against the official MongoDB drivers (Go, Node, Python) and `mongosh` for compatibility.

## Connecting

You connect with a standard MongoDB connection string.
Nothing about the client is doc-specific.

`mongosh`:

```
mongosh "mongodb://localhost:27017"
```

The Go driver, pointed at the same address:

```go
package main

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		panic(err)
	}
	defer client.Disconnect(ctx)

	coll := client.Database("app").Collection("users")
	if _, err := coll.InsertOne(ctx, map[string]any{"name": "ada"}); err != nil {
		panic(err)
	}
}
```

This is the unmodified `go.mongodb.org/mongo-driver`.
That is the whole point: the connection string is the only thing you change to move an app onto a `.doc` file.

## Authentication

Pass `--auth` to require clients to authenticate:

```
doc app.doc serve --auth
```

The server uses SCRAM-SHA-256 over the wire and applies role-based access control on top.
You create users and grant them roles, and each connection is checked against those roles before a command runs.
A read-only role can run queries but not writes; an admin role can manage users.
This is the same model MongoDB drivers expect, so the credentials flow through the connection string the usual way.

With auth on, point the client at the auth source:

```
mongodb://user:pass@localhost:27017/?authSource=admin
```

```
mongosh "mongodb://user:pass@localhost:27017/?authSource=admin"
```

The Go driver takes the same URI in `ApplyURI`.

## TLS

Enable TLS with the cert and key flags:

```
doc app.doc serve --tls --tls-cert server.pem --tls-key server-key.pem
```

Clients then connect with `mongodb://localhost:27017/?tls=true` (drivers and `mongosh` handle the rest).

To verify client certificates, add the CA:

```
doc app.doc serve --tls --tls-cert server.pem --tls-key server-key.pem --tls-ca ca.pem
```

With a CA in place the server can do x509 client authentication, where the client's certificate identity stands in for a username and password.
TLS on the wire is independent of encryption at rest for the file itself.
The file can be encrypted through its own open options regardless of whether the connection is using TLS.

## Read-only serving

Pass `--readonly` to serve a file that no client can write to:

```
doc app.doc serve --readonly
```

Reads work normally and any write command is rejected.
This is useful for exposing a snapshot or a reporting copy without risk of a client mutating it.

## Connection limits

Two flags bound how many connections the server carries:

- `--max-conns` caps concurrent connections. New connections past the cap are refused.
- `--max-conn-idle` closes a connection that has been idle longer than the limit.

```
doc app.doc serve --max-conns 100 --max-conn-idle 5m
```

Together they keep a small shared instance from being swamped by idle or runaway clients.

## Health and metrics

Add `--http` to expose an HTTP surface alongside the wire listener:

```
doc app.doc serve --http
```

That gives you a health check and metrics you can scrape, which is handy when the server runs under a process manager or behind a load balancer that wants a liveness signal.

## One writer at a time

The embedded library (`doc.Open` in Go) and the wire server both read and write the same kind of `.doc` file, and you can use either.
But a `.doc` file is single-writer.
Run one writer at a time against a given file.
That means do not run a writing server and a writing embedded process against the same file at once, and do not run two writing servers on the same file.
A read-only server (`--readonly`) alongside a single writer is fine.

## Next

For running the server in production, see [Operations](/guides/operations/).
For the full flag list, see the [CLI reference](/reference/cli/).
