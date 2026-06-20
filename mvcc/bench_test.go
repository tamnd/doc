package mvcc

import (
	"testing"

	"github.com/tamnd/doc/storage"
)

// seedB inserts one record for a benchmark and returns its RID.
func seedB(b *testing.B, s *Store, n int32) storage.RID {
	b.Helper()
	var rid storage.RID
	if err := s.WithTransaction(func(tx *Txn) error {
		r, err := s.Insert(tx, doc(n))
		rid = r
		return err
	}); err != nil {
		b.Fatalf("seed: %v", err)
	}
	return rid
}

// BenchmarkInsert measures a full insert transaction: begin, buffer one version,
// conflict-check, publish, commit.
func BenchmarkInsert(b *testing.B) {
	s := NewStore()
	d := doc(1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = s.WithTransaction(func(tx *Txn) error {
			_, err := s.Insert(tx, d)
			return err
		})
	}
}

// BenchmarkSnapshotRead measures a read-only transaction reading one record:
// begin, visibility walk, release. This is the hot path the watermark oracle
// stays off of.
func BenchmarkSnapshotRead(b *testing.B) {
	s := NewStore()
	rid := seedB(b, s, 7)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rt := s.Begin(false)
		if _, err := s.Get(rt, rid); err != nil {
			b.Fatal(err)
		}
		_ = rt.Commit()
	}
}

// BenchmarkUpdateNoContention measures back-to-back updates of one record with no
// concurrent writer, the conflict-free commit path.
func BenchmarkUpdateNoContention(b *testing.B) {
	s := NewStore()
	rid := seedB(b, s, 0)
	d := doc(1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = s.WithTransaction(func(tx *Txn) error {
			return s.Update(tx, rid, d)
		})
	}
}

// BenchmarkVisibilityWalkDeepChain measures the worst-case visibility walk: an
// old snapshot reading a record that has since been updated 256 times, so the
// predicate must skip every newer invisible version down to the one it can see.
// It bounds the cost a long-lived reader pays before GC can collapse the chain.
// A fresh snapshot reads the head in O(1); this is the other end of that range.
func BenchmarkVisibilityWalkDeepChain(b *testing.B) {
	s := NewStore()
	rid := seedB(b, s, 0)
	old := s.Begin(false) // snapshot at the bottom of the chain it will read
	for i := int32(1); i <= 256; i++ {
		_ = s.WithTransaction(func(tx *Txn) error {
			return s.Update(tx, rid, doc(i))
		})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Get(old, rid); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	_ = old.Commit()
}
