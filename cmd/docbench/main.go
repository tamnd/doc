// Command docbench runs the macro benchmark workloads from spec 2061 doc 19 §14 and
// compares the result against a stored baseline, exiting non-zero when a metric
// regresses past its threshold. It is the regression-gate driver: throughput must stay
// within 10% of baseline and p99 latency within 20% (§14.3).
//
// Usage:
//
//	docbench -out result.json                       run and print, no gate
//	docbench -baseline bench/baselines/ref.json     run and gate against a baseline
//	docbench -update bench/baselines/ref.json        run and overwrite the baseline
//
// Absolute numbers are hardware-specific, so the gate is meant for a reference machine
// (or a self-comparison on the same host), not for cross-runner PR checks. The PR CI
// runs the cheaper bench smoke instead.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/tamnd/doc"
)

// goVersion is the toolchain version stamped into a report, so a baseline records the
// compiler it was measured with.
func goVersion() string { return runtime.Version() }

func main() {
	var (
		outPath  = flag.String("out", "", "write the result JSON to this path")
		baseline = flag.String("baseline", "", "compare against this baseline JSON and gate")
		update   = flag.String("update", "", "run and overwrite this baseline JSON, no gate")
		warmup   = flag.Duration("warmup", 1*time.Second, "warmup duration per workload")
		measure  = flag.Duration("measure", 3*time.Second, "measurement duration per workload")
		dataset  = flag.Int("dataset", 50000, "documents to preload for read workloads")
	)
	flag.Parse()

	rep, err := run(*warmup, *measure, *dataset)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docbench:", err)
		os.Exit(1)
	}
	printReport(rep)

	if *outPath != "" {
		if err := writeJSON(*outPath, rep); err != nil {
			fmt.Fprintln(os.Stderr, "docbench:", err)
			os.Exit(1)
		}
	}
	if *update != "" {
		if err := writeJSON(*update, rep); err != nil {
			fmt.Fprintln(os.Stderr, "docbench:", err)
			os.Exit(1)
		}
		fmt.Println("baseline updated:", *update)
		return
	}
	if *baseline != "" {
		base, err := readReport(*baseline)
		if err != nil {
			fmt.Fprintln(os.Stderr, "docbench: read baseline:", err)
			os.Exit(1)
		}
		if regressions := gate(base, rep); len(regressions) > 0 {
			fmt.Fprintln(os.Stderr, "\ndocbench: regression gate FAILED")
			for _, r := range regressions {
				fmt.Fprintln(os.Stderr, "  "+r)
			}
			os.Exit(1)
		}
		fmt.Println("\ndocbench: regression gate passed")
	}
}

// Result is one workload's measured numbers.
type Result struct {
	Workload   string  `json:"workload"`
	Ops        int64   `json:"ops"`
	Seconds    float64 `json:"seconds"`
	Throughput float64 `json:"throughputOpsPerSec"`
	P50Ns      int64   `json:"p50Ns"`
	P99Ns      int64   `json:"p99Ns"`
	P999Ns     int64   `json:"p999Ns"`
	MaxNs      int64   `json:"maxNs"`
}

// Report is the full benchmark run.
type Report struct {
	GoVersion string   `json:"goVersion"`
	Results   []Result `json:"results"`
}

func run(warmup, measure time.Duration, dataset int) (*Report, error) {
	dir, err := os.MkdirTemp("", "docbench")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	rep := &Report{GoVersion: goVersion()}

	insert, err := benchInsert(filepath.Join(dir, "insert.doc"), warmup, measure)
	if err != nil {
		return nil, err
	}
	rep.Results = append(rep.Results, insert)

	read, err := benchFindByID(filepath.Join(dir, "read.doc"), warmup, measure, dataset)
	if err != nil {
		return nil, err
	}
	rep.Results = append(rep.Results, read...)

	return rep, nil
}

// benchInsert measures InsertOne throughput and latency under FULL sync, the durable
// write path (spec 2061 doc 19 §14.3 lists BenchmarkInsertOne FULL).
func benchInsert(path string, warmup, measure time.Duration) (Result, error) {
	db, err := doc.Open(path, doc.WithSyncLevel(doc.SyncFull))
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	c := db.Database("bench").Collection("ins")

	n := 0
	op := func() time.Duration {
		start := time.Now()
		_, _ = c.InsertOne(ctx, doc.M{"n": n, "s": "benchmark payload value", "ts": n})
		n++
		return time.Since(start)
	}
	return measureWorkload("insertOne_full", op, warmup, measure), nil
}

// benchFindByID preloads a dataset, then measures point lookups by _id, the warm-read
// hot path (spec 2061 doc 19 §14.3 lists BenchmarkFindOneByID). It reports both the
// in-cache and the random-key read as one workload here.
func benchFindByID(path string, warmup, measure time.Duration, dataset int) ([]Result, error) {
	db, err := doc.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	c := db.Database("bench").Collection("read")

	for i := 0; i < dataset; i++ {
		if _, err := c.InsertOne(ctx, doc.M{"_id": i, "n": i, "s": "row payload"}); err != nil {
			return nil, err
		}
	}

	prng := rand.New(rand.NewSource(42))
	op := func() time.Duration {
		id := prng.Intn(dataset)
		start := time.Now()
		_ = c.FindOne(ctx, doc.M{"_id": id}).Err()
		return time.Since(start)
	}
	return []Result{measureWorkload("findOneByID", op, warmup, measure)}, nil
}

// measureWorkload runs op in a tight loop for the warmup, discards those timings, then
// runs it for the measurement window recording every latency, and computes the result.
func measureWorkload(name string, op func() time.Duration, warmup, measure time.Duration) Result {
	deadline := time.Now().Add(warmup)
	for time.Now().Before(deadline) {
		op()
	}

	lat := make([]int64, 0, 1<<16)
	start := time.Now()
	deadline = start.Add(measure)
	var ops int64
	for time.Now().Before(deadline) {
		d := op()
		lat = append(lat, int64(d))
		ops++
	}
	elapsed := time.Since(start).Seconds()

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return Result{
		Workload:   name,
		Ops:        ops,
		Seconds:    elapsed,
		Throughput: float64(ops) / elapsed,
		P50Ns:      percentile(lat, 0.50),
		P99Ns:      percentile(lat, 0.99),
		P999Ns:     percentile(lat, 0.999),
		MaxNs:      maxOf(lat),
	}
}

// percentile reads a percentile out of an already-sorted slice of latencies.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

func maxOf(sorted []int64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[len(sorted)-1]
}

// gate compares a fresh report against a baseline and returns the regressions: a
// throughput at least 10% below baseline, or a p99 at least 20% above (spec §14.3).
func gate(base, got *Report) []string {
	byName := map[string]Result{}
	for _, r := range base.Results {
		byName[r.Workload] = r
	}
	var fails []string
	for _, r := range got.Results {
		b, ok := byName[r.Workload]
		if !ok {
			continue
		}
		if r.Throughput < b.Throughput*0.90 {
			fails = append(fails, fmt.Sprintf("%s throughput %.0f op/s is below 90%% of baseline %.0f op/s",
				r.Workload, r.Throughput, b.Throughput))
		}
		if b.P99Ns > 0 && r.P99Ns > int64(float64(b.P99Ns)*1.20) {
			fails = append(fails, fmt.Sprintf("%s p99 %s exceeds 120%% of baseline %s",
				r.Workload, time.Duration(r.P99Ns), time.Duration(b.P99Ns)))
		}
	}
	return fails
}

func printReport(rep *Report) {
	fmt.Printf("%-18s %12s %12s %12s %12s\n", "workload", "ops/s", "p50", "p99", "p999")
	for _, r := range rep.Results {
		fmt.Printf("%-18s %12.0f %12s %12s %12s\n",
			r.Workload, r.Throughput,
			time.Duration(r.P50Ns), time.Duration(r.P99Ns), time.Duration(r.P999Ns))
	}
}

func writeJSON(path string, rep *Report) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func readReport(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rep Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}
