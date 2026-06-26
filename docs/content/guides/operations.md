---
title: "Operations"
description: "Keep a .doc file healthy: integrity checks, compaction, checkpoints, vacuum, online backup, WAL archiving and point-in-time restore, and import and export."
weight: 50
---

A database is one `.doc` file plus a write-ahead log.
This guide covers the day-to-day jobs that keep that file healthy: checking it for corruption, reclaiming space, backing it up and restoring it, and moving data in and out.

Every operation here is available three ways: as a CLI subcommand (`doc <file> <command> ...`), as an interactive shell dot-command (the same name with a leading dot), and most of them from Go on a `*DB`.
Some operations run online (the database stays open and writable) and some run offline (nothing else touches the file while they run).
Each section below says which.

## Health checks

### Validate

`validate` walks the freelist, the heap, and every index, and checks that they agree with each other.
It is the first thing to run when you suspect a file is damaged.

```
doc data.doc validate
```

The shell equivalent is `.check`. The subcommand `check` is an alias for `validate`.

```
doc data.doc
> .check
```

By default the check verifies structure without re-reading every page.
Add `full` to re-read every page and verify its checksum, which catches silent bit-rot on disk at the cost of a full scan.

```
doc data.doc validate full
```

```
> .check full
```

When corruption is found, the command exits non-zero with exit code 4.
That makes it easy to wire into a cron job or a CI step that branches on the result.

```
doc data.doc validate || echo "validation failed with code $?"
```

From Go the same check is `db.Check`, which returns a `*CheckReport` you can inspect instead of parsing output.

```go
report, err := db.Check(ctx, true) // true = full page-checksum pass
if err != nil {
    log.Fatal(err)
}
if !report.OK {
    log.Fatalf("integrity problems: %v", report.Errors)
}
```

### Stats

`.stats` prints the same documents a driver would get from the `collStats` and `dbStats` commands.
With a collection name it prints `collStats` for that collection; with no argument it prints `dbStats` for the whole database.

```
> .stats users
> .stats
```

Use these to see document counts, storage sizes, and index sizes without writing a query.
Because they are the real `collStats`/`dbStats` responses, anything you learn here matches what your application sees through the driver.

## Reclaiming space

Deleting documents does not shrink the file on its own.
Three different operations reclaim space, and they are not interchangeable.

### Compact (offline)

`.compact` rebuilds the file into a fresh hole-free copy.
It reclaims space held by deleted documents, superseded versions, and forwarding tombstones, all the dead weight that accumulates as a database is written to over time.

```
> .compact
```

From Go this is `db.Compact`.

```go
if err := db.Compact(ctx); err != nil {
    log.Fatal(err)
}
```

Compaction is offline: nothing else runs against the database while it works, because it is rewriting the whole file.
Run it during a maintenance window, not under live traffic.
It gives back the most space of the three, since it physically repacks every live page.

### Checkpoint (online)

`.checkpoint` folds the write-ahead log into the main file and starts a fresh WAL.
It runs online, without closing the database.

```
> .checkpoint
```

From Go this is `db.Checkpoint`.

```go
if err := db.Checkpoint(ctx, "full"); err != nil {
    log.Fatal(err)
}
```

The mode argument (`passive`, `full`, `restart`, `truncate`) is accepted for SQLite compatibility.
doc runs the same full checkpoint for each of them, so the mode you pass does not change the behavior; it only lets existing SQLite tooling and habits work unchanged.

A checkpoint does not shrink the file.
It moves data from the WAL into the main file and bounds WAL growth.
Run it to keep the WAL from growing without limit and to make recent writes durable in the main file.

### Vacuum (online)

`.vacuum` reclaims trailing free pages back to the operating system, shrinking the file on disk.

```
> .vacuum
> .vacuum 100
```

The optional argument caps how many pages to release in one pass, which lets you spread the work out instead of releasing everything at once.
From Go this is `db.IncrementalVacuum`, where `n` is the page cap.

```go
if err := db.IncrementalVacuum(ctx, 100); err != nil {
    log.Fatal(err)
}
```

Vacuum requires `PRAGMA auto_vacuum` to be set to `incremental` or `full`.
Without that, there is no freelist for it to walk and the file will not shrink.
Vacuum is online.

### Which one to reach for

- WAL getting large, or you want recent writes folded into the main file: `.checkpoint` (online).
- File on disk larger than the live data, and auto_vacuum is on: `.vacuum` (online, gives back trailing space only).
- Lots of churn from deletes and updates, and you can take a maintenance window: `.compact` (offline, repacks everything and reclaims the most).

## Backup and restore

### Online backup

`.backup` streams a consistent physical image of the database while it stays open and writable.

```
> .backup --out backup.doc
```

The image is captured as of one version, so it opens cleanly with no WAL replay needed.
Because the copy runs against a single version, writes can continue during the backup without corrupting the image.

Add `--verify` to re-check every page checksum as the image is copied, which catches bad pages at backup time rather than at restore time.

```
> .backup --out backup.doc --verify
```

Use `-` as the output to write the raw image to stdout, which is handy for piping into compression or straight to remote storage.

```
doc data.doc backup --out - | zstd > backup.doc.zst
```

From Go this is `db.Backup`, which writes the image to any `io.Writer`.

```go
out, err := os.Create("backup.doc")
if err != nil {
    log.Fatal(err)
}
defer out.Close()

if err := db.Backup(ctx, out, doc.BackupOptions{Verify: true}); err != nil {
    log.Fatal(err)
}
```

Backup is online. The database does not close and does not stop taking writes.

For an incremental backup, pass `--since-version <n>` together with `--archive <dir>`.
That writes a delta of only the commits after version `n` instead of a full image, so you can take a full base image occasionally and small deltas often.

```
> .backup --since-version 4200 --archive /var/lib/doc/deltas
```

### Restore

`.restore` builds a fresh database from a base image and, optionally, replays WAL segments and deltas over it.

The minimum is `--base` and `--out`, which copies an image to a new file.

```
doc dummy.doc restore --base backup.doc --out restored.doc
```

The other flags layer replay on top of that base:

- `--wal-source <dir>` replays archived WAL segments over the base image.
- `--apply-delta <seg>` replays one incremental delta produced by an incremental backup.
- `--target-version <n>` stops the replay at version `n`.
- `--target-time <t>` stops the replay at a time, given as unix seconds or RFC3339.

Together these are the point-in-time recovery path: a base image gives you a starting point, the archived WAL carries you forward, and a target tells the replay where to stop.

### WAL archiving and point-in-time recovery

A single base backup gives you the state at one moment.
To recover to any moment between backups, archive WAL segments continuously and replay them on restore.

From Go, `db.ArchiveWAL` starts a background archiver and returns a `*WALArchiver`.

```go
archiver, err := db.ArchiveWAL(doc.ArchiveOptions{Dir: "/var/lib/doc/wal-archive"})
if err != nil {
    log.Fatal(err)
}
defer archiver.Stop()
```

The continuous-recovery setup has three parts working together:

1. A base image from `.backup` (or `db.Backup`), taken periodically.
2. Archived WAL segments from `db.ArchiveWAL`, written continuously into a directory.
3. A `.restore` that copies the base and replays the archived WAL up to a target.

To recover, point `--base` at the most recent image before the moment you want, point `--wal-source` at the WAL archive, and set a target.
This example restores everything up to 2026-06-20 14:30:00 UTC and stops there, leaving out anything that happened after:

```
doc dummy.doc restore \
  --base backup-2026-06-20.doc \
  --wal-source /var/lib/doc/wal-archive \
  --out recovered.doc \
  --target-time 2026-06-20T14:30:00Z
```

Without a target, the replay runs through the end of the archive and you get the latest recoverable state.
With `--target-version` instead of `--target-time`, the replay stops at a specific commit version, which is the right tool when you know exactly which write you want to land before or undo.

## Moving data

Two pairs of commands move data. `.import`/`.export` move a single collection. `.dump`/`.load` move whole databases.
In all of them, `-` means stdin or stdout.

### Import and export a collection

`.import` reads a file into one collection.

```
> .import users.jsonl --collection users --format jsonl
```

The `--format` flag accepts `json`, `jsonl`, `csv`, and `bson`.
Add `--drop` to empty the collection before loading, so the import replaces the contents instead of appending to them.

```
> .import users.json --collection users --format json --drop
```

`.export` writes one collection out, with optional filtering and projection.

```
> .export users.jsonl --collection users --format jsonl
```

`--filter` takes a JSON query document, `--fields` takes a comma-separated list of fields to keep, and `--format` matches the import formats.

```
> .export active.csv --collection users --filter '{"active":true}' --fields name,email --format csv
```

Use `-` to pipe. This exports straight to another tool:

```
doc data.doc export - --collection users --format jsonl | wc -l
```

### Dump and load a database

`.dump` writes whole databases, including index definitions in sidecar files, so a restore rebuilds the same indexes.

```
> .dump ./backup-dir
> .dump ./backup-dir --db analytics
> .dump ./backup-dir --db analytics --collection events
```

With no arguments it dumps to the default directory; `--db` and `--collection` narrow what is written.

`.load` reads a dump directory back in.

```
> .load ./backup-dir
> .load ./backup-dir --db analytics --drop
```

`--db` targets a database name and `--drop` empties collections before loading.
Because the dump carries index sidecars, the loaded database comes back with its indexes intact, which is the difference between this and a plain `.import` of each collection.

## Next

For tuning throughput, indexes, and cache behavior once the file is healthy, see [Performance tuning](/guides/performance-tuning/).
For the full list of subcommands, flags, and shell dot-commands, see the [CLI reference](/reference/cli/).
