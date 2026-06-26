---
title: "Configuration"
description: "The Go Open options, the full PRAGMA catalogue, and the on-disk file conventions."
weight: 20
---

doc is configured three ways: open options passed to `doc.Open` in Go, PRAGMAs read and written at runtime, and a handful of create-time choices baked into the file.

## Open options

`doc.Open(path string, opts ...doc.Option)` takes functional options.

| Option | Effect |
| ------ | ------ |
| `WithPageSize(n int)` | Page size at creation: 4096, 8192 (default), or 16384. Ignored on an existing file. |
| `WithCacheSize(n int64)` | Buffer pool size in bytes (default 64 MiB). |
| `WithSyncLevel(l SyncLevel)` | Durability: `SyncOff`, `SyncNormal` (default), `SyncFull`, `SyncExtra`. |
| `WithCodec(codec Codec)` | A custom marshalling codec for documents. |
| `WithEncryptionKey(key []byte)` | Open or create an encrypted file with a raw 32-byte key. |
| `WithPassphrase(p string)` | Open or create an encrypted file with a passphrase. |
| `WithBusyTimeout(d time.Duration)` | How long a blocked open waits for the file lock. |
| `WithReadOnly(b bool)` | Open the file read-only. |
| `WithTTLInterval(d time.Duration)` | How often the TTL sweeper runs. |
| `WithSlowOpThreshold(d time.Duration)` | Operations slower than this are recorded by the profiler. |
| `WithProfileLevel(level int)` | Profiler level: 0 off, 1, or 2. |

`OpenContext(ctx, path, opts...)` is the same with a context for the open itself.

```go
db, err := doc.Open("app.doc",
	doc.WithCacheSize(256<<20),
	doc.WithSyncLevel(doc.SyncFull),
)
```

## PRAGMA catalogue

Read a PRAGMA with `db.Pragma(name, "")`, write one with `db.Pragma(name, value)`.
From the shell, `.pragma name`, `.pragma name=value`, or `.pragma` with no argument to list them all.
A create-time or open-time PRAGMA is read-only at runtime and returns an error if you try to write it, rather than silently doing nothing.

| PRAGMA | Scope | Values | Meaning |
| ------ | ----- | ------ | ------- |
| `synchronous` | runtime | off, normal, full, extra | Durability level (fsync policy). |
| `default_isolation` | runtime | snapshot, serializable | Default transaction isolation. |
| `profile` | runtime | 0, 1, 2 | Slow-operation profiler level. |
| `wal_checkpoint` | runtime | (write triggers) | Force a checkpoint; reads the WAL size in pages. |
| `wal_autocheckpoint` | runtime | frame count | Auto-checkpoint threshold. |
| `auto_vacuum` | runtime | none, incremental, full | Free-page reclamation policy. |
| `incremental_vacuum` | runtime | page count | Reclaim up to N trailing free pages now. |
| `page_size` | create-time | 4096, 8192, 16384 | Page size; fixed at creation. |
| `journal_mode` | create-time | wal | Always WAL. |
| `cache_size` | open-time | bytes | Buffer pool size for this handle. |
| `busy_timeout_ms` | open-time | milliseconds | Lock-wait timeout for this handle. |
| `read_only` | open-time | true, false | Whether this handle is read-only. |
| `max_doc_size` | fixed | 16777216 | Largest document doc accepts (16 MiB, MongoDB parity). |

A name that is not in this catalogue is rejected, so a knob that does not exist reads as an error instead of appearing to take effect.

## On-disk conventions

A database is a single file, conventionally ending in `.doc`.
The first 16 bytes are a magic signature that doubles as a corruption and text-mangling check.
The header records the page size, the format version, and the roots of the free list, the catalog, and the optional columnar store.

The current format is major version 1, minor version 0.
A file written by an older v1 build opens in a newer v1 build, and a file whose major version is greater than the build understands is rejected with a clear error rather than misread.
See the [stability](/reference/stability/) page for the format guarantee.

Alongside the main file, doc keeps a write-ahead log while the database is open.
A checkpoint folds the log back into the main file; a clean close leaves nothing to replay.
Encryption, when enabled, applies at the page level, so the on-disk pages are ciphertext while the in-memory pages are plaintext.
