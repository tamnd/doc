# Benchmark baselines

`docbench` (under `cmd/docbench`) measures the macro workloads from spec 2061 doc 19
§14 and gates a run against a baseline in this directory:

- throughput fails if it drops 10% or more below baseline
- p99 latency fails if it rises 20% or more above baseline

`reference.json` is a sample baseline. The absolute numbers are hardware-specific, so
they are only meaningful on the machine they were captured on. The authoritative gate
runs on the reference machine described in doc 19 §12.2; on other hosts use docbench for
relative before/after comparison on the same box, not against this file.

Regenerate the baseline on a reference host after an intentional performance change,
with a commit message that explains the trade-off:

```
docbench -update bench/baselines/reference.json
```

Gate a fresh run against it:

```
docbench -baseline bench/baselines/reference.json
```
