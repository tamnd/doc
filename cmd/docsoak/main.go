// Command docsoak runs the endurance test from spec 2061 doc 19 §19.4: a fleet of
// reader and writer goroutines hammering a growing database for a long time, with
// periodic leak and latency-degradation checks. It is meant to run nightly on the main
// branch and before every release; the full run is 8 hours, but the duration is a flag
// so CI can run a shorter smoke and a dedicated host can run the full window.
//
// Usage:
//
//	docsoak -duration 8h
//	docsoak -duration 2m -readers 8 -writers 2   # CI smoke
//
// It exits non-zero the first time an interval check fails (a goroutine leak, an RSS
// blowup, or a p99 read latency that has degraded more than the allowed fraction).
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/doc"
)

func main() {
	var (
		duration  = flag.Duration("duration", 8*time.Hour, "total soak duration")
		interval  = flag.Duration("interval", 1*time.Hour, "how often to run the leak and latency checks")
		readers   = flag.Int("readers", 64, "concurrent reader goroutines")
		writers   = flag.Int("writers", 16, "concurrent writer goroutines")
		seed      = flag.Int("seed", 1000, "documents to preload before the readers start")
		latGrowth = flag.Float64("max-latency-growth", 0.50, "max allowed p99 read latency growth vs the first interval")
		rssBudget = flag.Int64("rss-budget-mb", 4096, "fail if heap in-use grows past this many MB")
	)
	flag.Parse()

	// Clamp the interval so a short smoke still runs at least a couple of checks.
	if *interval > *duration {
		*interval = *duration / 2
		if *interval <= 0 {
			*interval = *duration
		}
	}

	if err := soak(*duration, *interval, *readers, *writers, *seed, *latGrowth, *rssBudget); err != nil {
		fmt.Fprintln(os.Stderr, "docsoak: FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("docsoak: passed")
}

func soak(duration, interval time.Duration, readers, writers, seed int, latGrowth float64, rssBudgetMB int64) error {
	dir, err := os.MkdirTemp("", "docsoak")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	db, err := doc.Open(filepath.Join(dir, "soak.doc"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	c := db.Database("soak").Collection("docs")
	var nextID int64
	for i := 0; i < seed; i++ {
		if _, err := c.InsertOne(ctx, doc.M{"_id": int(atomic.AddInt64(&nextID, 1)), "v": i, "s": "soak payload"}); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}

	baseGoroutines := runtime.NumGoroutine()

	// readLatencies collects read durations between checks; the monitor drains it.
	var (
		latMu  sync.Mutex
		latBuf []int64
	)
	recordLat := func(d time.Duration) {
		latMu.Lock()
		latBuf = append(latBuf, int64(d))
		latMu.Unlock()
	}
	drainLat := func() []int64 {
		latMu.Lock()
		out := latBuf
		latBuf = nil
		latMu.Unlock()
		return out
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: a YCSB-A-ish insert and update mix that grows the dataset.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			prng := rand.New(rand.NewSource(int64(7919 * (w + 1))))
			for {
				select {
				case <-stop:
					return
				default:
				}
				id := int(atomic.AddInt64(&nextID, 1))
				_, _ = c.InsertOne(ctx, doc.M{"_id": id, "v": prng.Intn(1 << 20), "s": "soak payload value"})
				upd := prng.Intn(id) + 1
				_, _ = c.UpdateOne(ctx, doc.M{"_id": upd}, doc.M{"$set": doc.M{"v": prng.Intn(1 << 20)}})
			}
		}(w)
	}

	// Readers: point lookups by _id over the current key range (YCSB-B/C).
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			prng := rand.New(rand.NewSource(int64(104729 * (r + 1))))
			for {
				select {
				case <-stop:
					return
				default:
				}
				hi := atomic.LoadInt64(&nextID)
				if hi <= 0 {
					continue
				}
				id := prng.Int63n(hi) + 1
				start := time.Now()
				_ = c.FindOne(ctx, doc.M{"_id": int(id)}).Err()
				recordLat(time.Since(start))
			}
		}(r)
	}

	err = monitor(ctx, interval, duration, baseGoroutines, latGrowth, rssBudgetMB, drainLat)

	close(stop)
	wg.Wait()
	return err
}

// monitor runs the interval checks until the soak duration elapses. It returns an error
// the first time a check fails.
func monitor(ctx context.Context, interval, duration time.Duration, baseGoroutines int, latGrowth float64, rssBudgetMB int64, drainLat func() []int64) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.Now().Add(duration)

	var firstP99 int64
	checks := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		checks++

		// Goroutine leak: the worker fleet is fixed, so the count must not climb past
		// the baseline plus the fleet and a margin.
		if g := runtime.NumGoroutine(); g > baseGoroutines+512 {
			return fmt.Errorf("goroutine leak: %d goroutines, baseline was %d", g, baseGoroutines)
		}

		// Memory: heap in-use must stay within the budget.
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if inUseMB := int64(ms.HeapInuse) / (1 << 20); inUseMB > rssBudgetMB {
			return fmt.Errorf("heap in-use %d MB exceeds budget %d MB", inUseMB, rssBudgetMB)
		}

		// Latency degradation: p99 read latency must not grow past the first interval's
		// by more than the allowed fraction.
		lat := drainLat()
		if len(lat) > 0 {
			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			p99 := lat[int(0.99*float64(len(lat)-1))]
			if firstP99 == 0 {
				firstP99 = p99
			} else if p99 > int64(float64(firstP99)*(1+latGrowth)) {
				return fmt.Errorf("p99 read latency %s degraded past %.0f%% of the first interval's %s",
					time.Duration(p99), latGrowth*100, time.Duration(firstP99))
			}
			fmt.Printf("check %d: goroutines=%d heapInUse=%dMB reads=%d p99=%s\n",
				checks, runtime.NumGoroutine(), int64(ms.HeapInuse)/(1<<20), len(lat), time.Duration(p99))
		}

		if time.Now().After(deadline) {
			return nil
		}
	}
}
