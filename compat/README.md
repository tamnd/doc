# Driver compatibility suite

These suites prove that real MongoDB drivers talk to `doc serve` over the wire protocol. They
live outside the root Go module on purpose: the core `github.com/tamnd/doc` module has no
external dependencies, and the drivers pull in plenty. Keeping the suites in their own modules
(or scripts) means the core stays dependency-free while the compatibility check still runs the
genuine, unmodified drivers.

The arms follow the conformance matrix in spec 2061 doc 19 appendix G:

| Arm | Driver | Scope |
|-----|--------|-------|
| `go/` | `go.mongodb.org/mongo-driver/v2` | CRUD, aggregation, transactions, indexes |
| `python/` | `pymongo` | CRUD, aggregation, transactions, indexes |
| `node/` | `mongodb` | CRUD, aggregation, indexes |

The Go driver is the primary one (this is a Go project), so it gets the widest suite. Python
and Node are the most widely used drivers and the most likely to surface wire edge cases.

## How each arm works

Every arm does the same four things, which is the runner shape the spec describes:

1. Build the `doc` binary (or reuse one named by the `DOC_BIN` environment variable).
2. Start `doc serve` on an ephemeral loopback port over an in-memory database, and read the
   bound address from the line it prints (`doc serve listening on mongodb://HOST:PORT`).
3. Connect the driver under test and run the suite.
4. Signal the server to stop and wait for it to exit.

In-memory means a run leaves nothing behind, and the ephemeral port means arms can run in
parallel without colliding.

## Running everything

```
compat/run.sh
```

The script builds the binary once, exports `DOC_BIN`, and runs each arm whose toolchain is
installed. An arm with no toolchain is skipped with a notice rather than failing the run, so a
machine with only Go still checks the Go arm. The exit status is non-zero if any arm that ran
reported a failure.

## Running one arm

Go:

```
cd compat/go && go test ./...
```

Python (needs `pip install pymongo`):

```
python3 compat/python/run_compat.py
```

Node (needs `cd compat/node && npm install`):

```
node compat/node/run_compat.mjs
```

Set `DOC_BIN=/path/to/doc` to skip the per-arm build and point at a binary a release pipeline
already produced.

## What a compatibility gap looks like

The suite is here to catch real divergences from MongoDB, not to paper over them. The first
run surfaced one: the `$count` aggregation stage emitted a 64-bit integer where MongoDB emits a
32-bit integer for counts that fit. That was fixed in the engine rather than worked around in
the test, which is the intended response. When an arm fails, the fix belongs in `doc`, and the
suite stays strict.
