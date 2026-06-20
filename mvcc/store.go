package mvcc

import (
	"errors"
	"sync"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/storage"
)

// MaxRetries bounds the automatic retries WithTransaction makes on a write-write
// conflict before giving up (spec 2061 doc 06 §8.4).
const MaxRetries = 3

// ErrMaxRetries reports that WithTransaction exhausted its conflict retries.
var ErrMaxRetries = errors.New("mvcc: transaction retries exhausted")

// Store is an in-memory MVCC record store: a map of record key to version chain
// behind the oracle. It is the reference engine the property tests exercise and
// the seam the durable collection layer plugs into, since its begin/read/write/
// commit flow is exactly the one the heap-backed engine will follow. Keys are
// storage.RID.Encode() values so the oracle's conflict index and a future durable
// engine agree on identity.
type Store struct {
	o *Oracle

	mu      sync.RWMutex
	chains  map[uint64]*VersionChain
	nextRID uint64
}

// NewStore returns an empty store with a fresh oracle.
func NewStore() *Store {
	return &Store{
		o:      NewOracle(0),
		chains: make(map[uint64]*VersionChain),
	}
}

// Oracle exposes the store's oracle for watermark and version inspection.
func (s *Store) Oracle() *Oracle { return s.o }

// Begin starts a transaction bound to this store as its publishing engine.
func (s *Store) Begin(writable bool) *Txn {
	t := s.o.Begin(writable)
	t.eng = s
	return t
}

// allocRID hands out a fresh, unique, valid-looking record identifier. The in-
// memory store does not place records on pages, so it synthesizes RIDs whose
// Encode() values are distinct and monotonic.
func (s *Store) allocRID() storage.RID {
	s.mu.Lock()
	s.nextRID++
	n := s.nextRID
	s.mu.Unlock()
	return storage.RID{PageNo: uint32(n>>16) + 1, Slot: uint16(n)}
}

// Insert buffers a new record in the transaction and returns its RID. The
// document is cloned so the caller's buffer can be reused.
func (s *Store) Insert(t *Txn, doc bson.Raw) (storage.RID, error) {
	if !t.writable {
		return storage.NullRID, storage.ErrReadOnly
	}
	rid := s.allocRID()
	t.note(rid.Encode(), &version{txnID: t.txnID, kind: KindInsert, data: doc.Clone()})
	return rid, nil
}

// Update buffers a new full-document version of an existing record. It is
// snapshot-checked: updating a record not visible at the transaction's snapshot
// (and not its own pending insert) returns storage.ErrNotFound.
func (s *Store) Update(t *Txn, rid storage.RID, newDoc bson.Raw) error {
	if !t.writable {
		return storage.ErrReadOnly
	}
	if _, ok := s.read(t, rid); !ok {
		return storage.ErrNotFound
	}
	t.note(rid.Encode(), &version{txnID: t.txnID, kind: KindUpdate, data: newDoc.Clone()})
	return nil
}

// Delete buffers a tombstone for an existing record.
func (s *Store) Delete(t *Txn, rid storage.RID) error {
	if !t.writable {
		return storage.ErrReadOnly
	}
	if _, ok := s.read(t, rid); !ok {
		return storage.ErrNotFound
	}
	t.note(rid.Encode(), &version{txnID: t.txnID, kind: KindDelete})
	return nil
}

// Get returns the document the transaction sees for rid, or storage.ErrNotFound.
func (s *Store) Get(t *Txn, rid storage.RID) (bson.Raw, error) {
	if doc, ok := s.read(t, rid); ok {
		return doc, nil
	}
	return nil, storage.ErrNotFound
}

// read resolves a record for the transaction: its own pending write first
// (read-your-writes), then the committed chain visible at its snapshot (spec 2061
// doc 06 §5.5).
func (s *Store) read(t *Txn, rid storage.RID) (bson.Raw, bool) {
	key := rid.Encode()
	if t.pending != nil {
		if v, ok := t.pending[key]; ok {
			if v.kind == KindDelete {
				return nil, false
			}
			return v.data, true
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ch := s.chains[key]
	if ch == nil {
		return nil, false
	}
	return ch.visibleAt(t.startVer)
}

// publish installs the transaction's pending versions onto their chains at the
// assigned commit version. It runs after the oracle has ordered the commit, so
// the new versions become visible atomically to every later snapshot (spec 2061
// doc 06 §7.5 step 5).
func (s *Store) publish(t *Txn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, v := range t.pending {
		v.commitVer = t.commitVer
		v.txnID = 0
		ch := s.chains[key]
		if ch == nil {
			ch = &VersionChain{}
			s.chains[key] = ch
		}
		v.prev = ch.head
		ch.head = v
	}
}

// discard drops a transaction's buffered writes. Nothing durable was written, so
// an abort is just forgetting the pending map (spec 2061 doc 06 §7.6).
func (s *Store) discard(t *Txn) { t.pending = nil }

// GC reclaims version-chain tails no live snapshot can reach and returns the
// number of versions dropped (spec 2061 doc 06 §14). It reads the watermark and
// truncates each chain; a chain whose only reachable state is a sub-watermark
// tombstone is removed entirely, reclaiming the record.
func (s *Store) GC() int {
	wm := s.o.Watermark()
	s.mu.Lock()
	defer s.mu.Unlock()
	reclaimed := 0
	for key, ch := range s.chains {
		before := ch.Len()
		dead := ch.gc(wm)
		reclaimed += before - ch.Len()
		if dead {
			reclaimed += ch.Len()
			delete(s.chains, key)
		}
	}
	return reclaimed
}

// Versions returns the number of live version-chain entries across the store, a
// test and accounting hook for GC.
func (s *Store) Versions() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, ch := range s.chains {
		n += ch.Len()
	}
	return n
}

// WithTransaction runs fn inside a write transaction, committing on success and
// retrying on a write-write conflict up to MaxRetries times (spec 2061 doc 06
// §7.2, §8.4). fn must use the provided transaction for all reads and writes; a
// non-conflict error from fn aborts without retry.
func (s *Store) WithTransaction(fn func(t *Txn) error) error {
	for range MaxRetries {
		t := s.Begin(true)
		if err := fn(t); err != nil {
			_ = t.Rollback()
			if errors.Is(err, storage.ErrConflict) {
				continue
			}
			return err
		}
		err := t.Commit()
		if err == nil {
			return nil
		}
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return err
	}
	return ErrMaxRetries
}
