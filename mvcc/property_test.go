package mvcc

import (
	"errors"
	"sync"
	"testing"

	"github.com/tamnd/doc/storage"
)

// xorshift is a tiny deterministic PRNG so the property tests are reproducible
// without depending on the disallowed math/rand global seed timing.
type xorshift uint64

func (x *xorshift) next() uint64 {
	v := uint64(*x)
	v ^= v << 13
	v ^= v >> 7
	v ^= v << 17
	*x = xorshift(v)
	return v
}

func (x *xorshift) intn(n int) int { return int(x.next() % uint64(n)) }

// TestSnapshotIsolationManyOps drives a long single-threaded history of inserts,
// updates, deletes and snapshot reads against a model map, asserting every read
// matches the value the model held at the reading transaction's snapshot. This is
// the SI invariant: a read sees exactly the committed state as of its begin.
func TestSnapshotIsolationManyOps(t *testing.T) {
	const ops = 200_000
	s := NewStore()
	rng := xorshift(0x9e3779b97f4a7c15)

	// model: rid -> latest committed value, -1 means deleted/absent.
	model := map[uint64]int32{}
	var rids []storage.RID

	for i := range ops {
		switch rng.intn(4) {
		case 0: // insert
			val := int32(rng.intn(1 << 20))
			var rid storage.RID
			if err := s.WithTransaction(func(tx *Txn) error {
				r, err := s.Insert(tx, doc(val))
				rid = r
				return err
			}); err != nil {
				t.Fatalf("op %d insert: %v", i, err)
			}
			model[rid.Encode()] = val
			rids = append(rids, rid)
		case 1: // update
			if len(rids) == 0 {
				continue
			}
			rid := rids[rng.intn(len(rids))]
			val := int32(rng.intn(1 << 20))
			err := s.WithTransaction(func(tx *Txn) error {
				return s.Update(tx, rid, doc(val))
			})
			if errors.Is(err, storage.ErrNotFound) {
				if model[rid.Encode()] >= 0 {
					t.Fatalf("op %d update got NotFound but model has %d", i, model[rid.Encode()])
				}
				continue
			}
			if err != nil {
				t.Fatalf("op %d update: %v", i, err)
			}
			model[rid.Encode()] = val
		case 2: // delete
			if len(rids) == 0 {
				continue
			}
			rid := rids[rng.intn(len(rids))]
			err := s.WithTransaction(func(tx *Txn) error {
				return s.Delete(tx, rid)
			})
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				t.Fatalf("op %d delete: %v", i, err)
			}
			model[rid.Encode()] = -1
		case 3: // snapshot read of a known rid, must match the model now
			if len(rids) == 0 {
				continue
			}
			rid := rids[rng.intn(len(rids))]
			rt := s.Begin(false)
			got, err := s.Get(rt, rid)
			_ = rt.Commit()
			want := model[rid.Encode()]
			if errors.Is(err, storage.ErrNotFound) {
				if want >= 0 {
					t.Fatalf("op %d read NotFound, model has %d", i, want)
				}
				continue
			}
			if err != nil {
				t.Fatalf("op %d read: %v", i, err)
			}
			if v := valOf(t, got); v != want {
				t.Fatalf("op %d read %d, model %d", i, v, want)
			}
		}
		// Occasionally GC; it must never change visible state.
		if i%5000 == 0 {
			s.GC()
		}
	}
}

// TestStableSnapshotAcrossManyCommits opens a long-lived reader, then commits a
// stream of updates to one record, and asserts the reader keeps seeing its
// original value the whole time. The watermark cannot advance past the reader, so
// GC cannot reclaim the version it reads.
func TestStableSnapshotAcrossManyCommits(t *testing.T) {
	s := NewStore()
	rid := seed(t, s, 0)

	reader := s.Begin(false)
	for i := int32(1); i <= 1000; i++ {
		if err := s.WithTransaction(func(tx *Txn) error {
			return s.Update(tx, rid, doc(i))
		}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
		s.GC() // GC under the live reader must not drop its version
		got, err := s.Get(reader, rid)
		if err != nil {
			t.Fatalf("reader get at %d: %v", i, err)
		}
		if v := valOf(t, got); v != 0 {
			t.Fatalf("long reader saw %d at step %d, want 0", v, i)
		}
	}
	_ = reader.Commit()
}

// TestGCReclaimsSupersededVersions checks that once no snapshot can reach old
// versions, GC truncates them: a record updated N times with no live old reader
// collapses to a single live version.
func TestGCReclaimsSupersededVersions(t *testing.T) {
	s := NewStore()
	rid := seed(t, s, 0)
	for i := int32(1); i <= 50; i++ {
		if err := s.WithTransaction(func(tx *Txn) error {
			return s.Update(tx, rid, doc(i))
		}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	if got := s.Versions(); got != 51 {
		t.Fatalf("before GC %d versions, want 51", got)
	}
	// No live snapshot below the latest commit: GC must collapse to one version.
	s.GC()
	if got := s.Versions(); got != 1 {
		t.Fatalf("after GC %d versions, want 1", got)
	}
	// The surviving version is the newest, still readable.
	rt := s.Begin(false)
	got, _ := s.Get(rt, rid)
	if v := valOf(t, got); v != 50 {
		t.Fatalf("after GC read %d, want 50", v)
	}
	_ = rt.Commit()
}

// TestGCReclaimsDeletedRecord checks a tombstoned record whose tombstone is below
// the watermark is removed entirely by GC.
func TestGCReclaimsDeletedRecord(t *testing.T) {
	s := NewStore()
	rid := seed(t, s, 1)
	if err := s.WithTransaction(func(tx *Txn) error {
		return s.Delete(tx, rid)
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Advance the watermark strictly past the tombstone with an unrelated commit.
	_ = seed(t, s, 2)
	s.GC()
	if got := s.Versions(); got != 1 {
		t.Fatalf("after GC %d versions, want 1 (only the unrelated record)", got)
	}
	rt := s.Begin(false)
	if _, err := s.Get(rt, rid); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("reclaimed record: %v, want ErrNotFound", err)
	}
	_ = rt.Commit()
}

// TestConcurrentWritersLinearizable runs many goroutines each doing a
// read-modify-write increment of a shared counter through WithTransaction. The
// final value must equal the number of successful increments, with no lost
// updates: every committed RMW is serialized by the conflict detector.
func TestConcurrentWritersLinearizable(t *testing.T) {
	s := NewStore()
	rid := seed(t, s, 0)

	const goroutines = 8
	const perG = 250
	var wg sync.WaitGroup
	var mu sync.Mutex
	committed := 0

	for range goroutines {
		wg.Go(func() {
			for range perG {
				// Retry until this increment lands; each success is one commit.
				for {
					tx := s.Begin(true)
					cur, err := s.Get(tx, rid)
					if err != nil {
						_ = tx.Rollback()
						t.Errorf("get: %v", err)
						return
					}
					if err := s.Update(tx, rid, doc(valOf(t, cur)+1)); err != nil {
						_ = tx.Rollback()
						t.Errorf("update: %v", err)
						return
					}
					err = tx.Commit()
					if err == nil {
						mu.Lock()
						committed++
						mu.Unlock()
						break
					}
					if errors.Is(err, storage.ErrConflict) {
						continue
					}
					t.Errorf("commit: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()

	want := goroutines * perG
	if committed != want {
		t.Fatalf("committed %d increments, want %d", committed, want)
	}
	rt := s.Begin(false)
	got, _ := s.Get(rt, rid)
	if v := valOf(t, got); int(v) != want {
		t.Fatalf("counter is %d, want %d (lost update)", v, want)
	}
	_ = rt.Commit()
}

// TestLinearizableHistories runs many independent small concurrent histories and
// checks each one's counter is exactly its successful-commit count. This is the
// 10k-history linearizability sweep in miniature: every history is a fresh store,
// and a lost update or phantom commit in any one fails the run.
func TestLinearizableHistories(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 10k-history sweep in short mode")
	}
	const histories = 10_000
	for h := range histories {
		s := NewStore()
		rid := seed(t, s, 0)
		const writers = 4
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok := 0
		for range writers {
			wg.Go(func() {
				for {
					tx := s.Begin(true)
					cur, err := s.Get(tx, rid)
					if err != nil {
						_ = tx.Rollback()
						return
					}
					if err := s.Update(tx, rid, doc(valOf(t, cur)+1)); err != nil {
						_ = tx.Rollback()
						return
					}
					if err := tx.Commit(); err == nil {
						mu.Lock()
						ok++
						mu.Unlock()
						return
					} else if !errors.Is(err, storage.ErrConflict) {
						return
					}
				}
			})
		}
		wg.Wait()
		rt := s.Begin(false)
		got, _ := s.Get(rt, rid)
		_ = rt.Commit()
		if int(valOf(t, got)) != ok {
			t.Fatalf("history %d: counter %d, commits %d", h, valOf(t, got), ok)
		}
	}
}
