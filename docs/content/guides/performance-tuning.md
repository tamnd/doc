---
title: "Performance tuning"
description: "The knobs that matter: durability levels, the page cache, the columnar projection store for analytics, and the benchmark gate that guards against regressions."
weight: 60
---

## Measure first

Before you turn any knob, find out where the time actually goes.

`doc` ships a slow-op profiler and a benchmark tool, and both beat guessing.
Turn on the profiler to see which operations are slow in your real workload, and run the bench tool when you want repeatable numbers under controlled load.
Most "the database is slow" reports turn out to be one unindexed query or one collection scan, and the profiler points straight at it.

The rest of this page covers the knobs in rough order of how often they matter: durability, page cache, page size, WAL checkpointing, the columnar store, the profiler, and the benchmark gate.

## Durability and `PRAGMA synchronous`

Durability is the biggest dial, and it trades crash safety for write throughput.
You set it with `PRAGMA synchronous`, which takes four values: `off`, `normal` (the default), `full`, and `extra`.

`off` is the fastest and the least safe.
It does no fsync, so a crash can lose recently committed data.
Use it for throwaway data, bulk imports you can replay, or test fixtures where the file is disposable.

`normal` is the default and the right choice for almost everything.
It fsyncs the WAL at the points that matter, so a committed transaction survives both a process crash and an OS or power crash.
You get durability without paying for a flush on every single step.

`full` and `extra` add more flushing on top of `normal`.
They cost more write latency for a small additional safety margin, and you only want them when you have a specific reason (for example a regulatory requirement or a known-flaky storage stack).

You can set the level several ways:

```go
db, err := doc.Open("app.doc", doc.WithSyncLevel(doc.SyncNormal))
```

Or at runtime through a pragma:

```go
db.Pragma("synchronous", "normal")
```

From the CLI use `--sync` on the command line, or `.pragma synchronous=normal` in the shell.

Concrete guidance: leave it at `normal` in production.
Drop to `off` only for ephemeral or rebuildable data, and reach for `full`/`extra` only when you can name the threat you are defending against.

## Sizing the page cache

The page cache (a 2Q buffer pool) keeps hot pages in memory so reads do not hit the file.
A bigger cache holds more of your working set, which means fewer page faults and faster reads, at the cost of more RAM.

The default is 64M.
Set it at open time:

```go
db, err := doc.Open("app.doc", doc.WithCacheSize(256<<20)) // 256 MiB
```

Or on the CLI with `--cache`, which accepts K/M/G suffixes:

```
doc --cache 256M app.doc
```

You can read the current value through `PRAGMA cache_size`, but it is read-only at runtime, so size it when you open the database.

How to size it: aim to fit your hot working set, not the whole database.
If your reads touch a few hundred megabytes of pages over and over, a cache that covers that range eliminates most file reads.
Watch the profiler and your process RSS, bump the cache, and stop when the read latencies flatten out.

## Page size (create time only)

Page size is fixed when the file is created and cannot change afterward.
Allowed sizes are 4096, 8192 (the default), and 16384 bytes.

```go
db, err := doc.Open("app.doc", doc.WithPageSize(16384))
```

After creation, `PRAGMA page_size` is read-only and just reports what the file uses.

The default of 8192 is a good middle ground.
Larger pages (16384) can help workloads with big documents or large sequential scans because each page read covers more data.
Smaller pages (4096) reduce write amplification for workloads dominated by tiny random updates.
If you are not sure, leave it at 8192 and spend your tuning budget elsewhere.

## WAL checkpoint tuning

In WAL mode, writes go to the WAL first and later fold back into the main file during a checkpoint.

`PRAGMA wal_autocheckpoint` sets the frame threshold that triggers an automatic checkpoint.
A larger threshold batches more work per checkpoint (good for write throughput) but lets the WAL grow larger on disk and can make a single checkpoint pause longer.
A smaller threshold keeps the WAL small and checkpoints cheap, at the cost of checkpointing more often.

```go
db.Pragma("wal_autocheckpoint", "10000") // checkpoint after ~10k frames
```

You can also force a checkpoint at a quiet moment with `PRAGMA wal_checkpoint`, for example right after a bulk load or before a backup:

```go
db.Pragma("wal_checkpoint", "")
```

Practical rule: raise `wal_autocheckpoint` if you are write-heavy and can spare the disk for a bigger WAL, and force a checkpoint manually around batch jobs so the automatic ones do not fire in the middle of latency-sensitive traffic.

## The columnar projection store

The columnar projection store is an optional, derived, auxiliary structure for analytical queries.
The heap is always the source of truth.
The column store shreds chosen fields out of the heap into compressed, encoded segments, using plain, dictionary, RLE, bit-packing, frame-of-reference, and delta+bit-packing encodings, alongside zone maps and null bitmaps.

The payoff is bytes read.
An analytical query that touches many documents but only a few fields can read a small fraction of what a full heap scan would, because it only pulls the columns it needs and skips segments that the zone maps rule out.
It accelerates `$group`, `$sum`, and `$avg`, and it speeds up range scans.

### When it helps

It helps when you read a lot of rows but few fields: dashboards, aggregations, range filters over a column, time-series rollups.
It does not help point lookups (a `FindOne` by `_id` already resolves through the `_id` index), and it adds write and storage cost, so it is not free.
Turn it on for collections you aggregate over, not for collections you mostly read one document at a time.

### Three modes

The column store has three modes, set per collection:

- `off`: no column store. The default. Queries use the heap.
- `transactional`: the column store is updated synchronously inside the writing transaction. Reads always see fresh columnar data, at the cost of more work on every write.
- `lazy`: the column store is refreshed in the background. Writes stay cheap, and reads merge the latest heap state with the columnar segments so results are still correct while the background refresh catches up.

Pick `transactional` when you need every write reflected in column-store reads immediately and your write rate can absorb the cost.
Pick `lazy` when write throughput matters more and a short refresh lag on the analytical path is acceptable.

### Enabling it per collection

Control it through `PRAGMA columnar_store` on the collection:

```go
db.Pragma("columnar_store", "lazy") // for the target collection
```

Set it to `transactional`, `lazy`, or `off` as needed.

### The covering requirement

The planner uses a cost model to decide between the heap path and the column path.
It only takes the column path when that path covers all the fields the query needs.
If a query references a field that is not in the column store, the planner falls back to the heap, because reading the missing field from the column store is not possible.
So when you choose which fields to shred, include every field the analytical query reads (filters, group keys, and aggregated fields), or the query will not get the columnar speedup.

## The slow-op profiler

The profiler records operations that run slower than a threshold, so you can find the expensive ones instead of guessing.

```go
db, err := doc.Open("app.doc",
    doc.WithSlowOpThreshold(5*time.Millisecond),
    doc.WithProfileLevel(2),
)
```

You can also drive it with `PRAGMA profile`, which takes a level of 0, 1, or 2.
Level 0 is off, and higher levels capture more detail.
Set a threshold that matches your latency budget, run real traffic, and read off which operations crossed it.

## The benchmark gate

The repo ships a `bench` package and a `docbench` tool, plus a nightly soak run.
CI runs the benchmarks as a smoke check on every change, and the nightly job runs a regression gate with allocation ceilings and latency percentiles, so a change that adds allocations or blows a percentile fails before it lands.

Several hot paths are genuinely zero-allocation: predicate evaluation, BSON lookup, and read-only snapshot begins allocate nothing per call.
A `FindOne` by `_id` resolves straight through the `_id` index.

Run the benchmarks locally before you push a performance-sensitive change:

```
make bench
```

For finer control, run the `docbench` tool directly to target specific scenarios and compare runs.
If you are changing anything on the hot path, run it before and after and diff the allocations and percentiles, because the gate will check the same thing in CI.

## Honest latency note

Calibrate your expectations against your own hardware, not the headline targets.

On a developer laptop (Apple M-series), a `FindOne` by `_id` over a 10k-document collection measures roughly p50 ~300ns and p99 ~1us, well inside the latency budget.
A columnar `$group` over 50k documents measures p50 around 9ms single-threaded.

The spec's headline targets (for example a `$group` over a million documents) assume the documented reference machine and the deferred intra-query parallelism.
Single-threaded on a laptop, a million-document group is larger than those targets, so do not over-extrapolate from the spec.
Measure on the hardware you will actually run on.

## Next

For the full list of options, pragmas, and defaults, see [/reference/configuration/](/reference/configuration/).
For the benchmark methodology and the numbers the gate enforces, see [/reference/benchmarks/](/reference/benchmarks/).
