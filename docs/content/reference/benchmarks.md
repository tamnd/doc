---
title: "Benchmarks"
description: "Measured numbers with the full setup disclosed, how to reproduce them, and an honest note on what to expect on your own hardware."
weight: 50
---

This page reports numbers we actually measured, with enough about the setup that you can judge them and reproduce them.
Treat them as a calibration point, not a promise.
The honest move is to run the benchmarks on the hardware you care about, which the last section explains.

## Setup

Unless noted, the numbers below come from the project's own benchmark and latency harnesses run on the development machine:

- Apple silicon laptop (arm64), macOS.
- Go 1.26, `CGO_ENABLED=0`.
- A single process, no other significant load.
- The database opened in-memory or on a local SSD as noted per measurement.

This is a developer laptop, not a tuned server.
It is deliberately a modest machine so the numbers are not flattering.

## Point reads

A `FindOne` by `_id` resolves through the `_id` index directly, with no residual matcher to compile.
Over a 10,000-document collection, sampled across 20,000 lookups:

| Percentile | Latency |
| ---------- | ------- |
| p50 | about 0.3 microseconds |
| p99 | about 1.0 microsecond |
| p99.9 | about 5.4 microseconds |

That is the round trip for a keyed lookup inside the process: no network, no serialization across a socket, a page-cache hit, and an index descent.

## Allocations on the hot path

Three things on the read path are genuinely zero-allocation, verified by allocation tests in CI:

- Predicate evaluation (matching a filter against a document) when the path resolves through plain documents.
- BSON field lookup.
- Beginning a read-only snapshot transaction.

A keyed `FindOne` carries a small bounded number of allocations (single digits) for the result document.
A durable `InsertOne` allocates more, because a full durable commit walks the WAL and the page path; the allocation test there is a gross-regression guard, not a low-allocation claim.
The buffer-pool and WAL-pool work that would drive the insert path down further is future work, and it is called out as such rather than hidden.

## Analytical aggregation

The columnar projection store accelerates `$group`, `$sum`, and `$avg` over many documents and few fields by reading compressed, encoded segments instead of scanning the heap.
A `$group` over a 50,000-document collection, single-threaded, measures a p50 of roughly 9 milliseconds, and the planner confirms it took the column path rather than the heap.

## What to expect on your hardware, honestly

The spec sets headline targets against a documented reference machine and assumes intra-query parallelism that is not in this release yet.
Two consequences worth stating plainly:

- Point reads and small queries clear the latency budget comfortably even on this laptop, by a wide margin.
- A large analytical aggregation, for example a `$group` over a million documents, is single-threaded today.
  Extrapolated from the 50,000-document figure, that is well over the sub-second headline target on a laptop.
  On the reference machine, and once intra-query parallelism lands, the gap closes; on your laptop, single-threaded, a million-row group is a larger job than the headline number suggests.

Do not over-extrapolate from a small sample.
Measure the workload you actually run.

## Reproducing these

The benchmark code lives in the `bench` package, with a `docbench` command and a nightly soak.

```sh
make bench          # run every benchmark once as a smoke check
go test -run x -bench . ./...   # the full Go benchmark suite
```

CI runs the benchmarks on every change as a smoke check, and a nightly job runs a regression gate with a percentage threshold and allocation ceilings, so a change that slows the hot path or starts allocating where it should not is caught.
The latency harness (the percentile tables above) lives alongside the benchmarks and is what produced the point-read numbers.
