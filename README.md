# doc

A modern, high-performance, low-latency embedded document database for Go that looks and feels like SQLite.
The whole database is a single self-describing `.doc` file, durability comes from a write-ahead log, and you open it with a path and a line of code.

doc speaks the MongoDB document model (BSON documents in collections, ObjectId `_id`s) and a subset of the MongoDB Query Language.
A future server mode answers the MongoDB wire protocol so existing drivers connect unchanged.

It is written in pure Go with `CGO_ENABLED=0`, so it cross-compiles to a static binary on every platform Go targets.

## Status

Early. The storage engine is being built milestone by milestone (spec 2061 doc 19).
What works today:

- **M0** file format, the storage SPI seam, the WAL substrate, and the WAL-mode pager with a 2Q buffer pool.
- **M1** the slotted-page record store with durable inserts and the `_id` B-tree over the storage seam.
- **M2** the full BSON value codec and the snapshot-isolation MVCC core (version chains, the watermark oracle, first-committer-wins conflict detection, and version GC).

The embedded `Open`/`DB`/`Collection` API and the `doc` binary land as later milestones fill in the layers above this foundation.

## Layout

| Package    | Role                                                                 |
| ---------- | -------------------------------------------------------------------- |
| `format`   | On-disk file format: magic, header, page types.                      |
| `vfs`      | Virtual file system abstraction (OS files and an in-memory FS).      |
| `wal`      | Write-ahead log: frames, group commit, recovery.                     |
| `pager`    | WAL-mode pager and 2Q buffer pool over the VFS.                      |
| `storage`  | The storage SPI: the seam every layer above builds against.          |
| `heap`     | Slotted-page document record store.                                  |
| `index`    | B-tree indexes, starting with the `_id` index.                       |
| `bson`     | BSON document codec and order-preserving key encoding.               |
| `mvcc`     | Snapshot isolation: version chains, the oracle, conflict detection.  |
| `oracle`   | Behavior-comparison test harness (reference vs subject).             |
| `sys`      | Clock and id generation.                                             |

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
