package colstore

import (
	"sort"
	"sync"

	"github.com/tamnd/doc/bson"
)

// Mode is how a collection's column store is maintained (spec 2061 doc 04 §10.4,
// PRAGMA columnar_store in doc 09).
type Mode uint8

const (
	// ModeOff: no column store; the planner only has the heap path.
	ModeOff Mode = iota
	// ModeTransactional: the store is updated synchronously inside the writing
	// transaction, so reads at the writer's snapshot are consistent.
	ModeTransactional
	// ModeLazy: the store is refreshed in the background and may lag the heap; the
	// engine routes stale reads through a merge with the heap (§10.6).
	ModeLazy
)

// Store is a collection's columnar projection store: for each projected field, a
// sequence of immutable segments plus an in-progress buffer, all sharing one RID
// timeline so the fields stay row-aligned (spec 2061 doc 04 §10.2). Every row is
// one RID carrying one value per projected field; an update tombstones the old RID
// across all fields and appends a fresh row, so segments never overwrite (§10.4).
//
// The store is safe for concurrent maintenance and scans under its own mutex.
type Store struct {
	mu     sync.Mutex
	mode   Mode
	fields []string

	segs   map[string][]*Segment // segs[f][k] aligns with bounds[k]
	bounds []segBound            // flushed segment boundaries on the shared RID line

	bufVals  map[string][]Value // per field, parallel to bufRIDs
	bufRIDs  []uint64
	bufWrite []uint64
	bufDel   []uint64

	live    map[string]uint64 // document id key -> live RID
	nextRID uint64
}

// segBound records one flushed segment's RID range, shared across all fields.
type segBound struct {
	first uint64
	n     int
}

// New creates an empty store for the given projected fields and maintenance mode.
// An empty field list means "project every field", which the engine resolves to the
// concrete field set; the store itself works on whatever fields it is given.
func New(mode Mode, fields []string) *Store {
	s := &Store{
		mode:    mode,
		fields:  append([]string(nil), fields...),
		segs:    make(map[string][]*Segment, len(fields)),
		bufVals: make(map[string][]Value, len(fields)),
		live:    make(map[string]uint64),
	}
	for _, f := range s.fields {
		s.segs[f] = nil
		s.bufVals[f] = nil
	}
	return s
}

// Mode returns the maintenance mode.
func (s *Store) Mode() Mode { return s.mode }

// Fields returns the projected field paths.
func (s *Store) Fields() []string { return s.fields }

// idKey turns a document's _id into a stable map key for the live index.
func idKey(doc bson.Raw) string {
	id, ok := bson.IDOf(doc)
	if !ok {
		return ""
	}
	return FromRawValue(id).strictKey()
}

// Insert appends one document as a new row visible from version ver. It projects
// each field, assigns the next RID, and flushes the buffer when it fills.
func (s *Store) Insert(doc bson.Raw, ver uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertLocked(idKey(doc), doc, ver)
}

func (s *Store) insertLocked(key string, doc bson.Raw, ver uint64) {
	rid := s.nextRID
	s.nextRID++
	if len(s.bufRIDs) == 0 {
		// new buffer starts at this RID
	}
	for _, f := range s.fields {
		s.bufVals[f] = append(s.bufVals[f], FromField(doc, f))
	}
	s.bufRIDs = append(s.bufRIDs, rid)
	s.bufWrite = append(s.bufWrite, ver)
	s.bufDel = append(s.bufDel, 0)
	if key != "" {
		s.live[key] = rid
	}
	if len(s.bufRIDs) >= SegmentSize {
		s.flushLocked()
	}
}

// Update tombstones the row's old RID at version ver and appends the new document
// as a fresh row, matching the append-only update contract (spec 2061 doc 04 §10.4).
func (s *Store) Update(oldID bson.Raw, newDoc bson.Raw, ver uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rid, ok := s.live[idKey(oldID)]; ok {
		s.tombstoneLocked(rid, ver)
	}
	s.insertLocked(idKey(newDoc), newDoc, ver)
}

// Delete tombstones the row for doc's _id at version ver.
func (s *Store) Delete(doc bson.Raw, ver uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := idKey(doc)
	if rid, ok := s.live[key]; ok {
		s.tombstoneLocked(rid, ver)
		delete(s.live, key)
	}
}

// tombstoneLocked sets the delete version for rid in the buffer or in the flushed
// segment that holds it, across every field.
func (s *Store) tombstoneLocked(rid, ver uint64) {
	if len(s.bufRIDs) > 0 && rid >= s.bufRIDs[0] {
		off := int(rid - s.bufRIDs[0])
		if off < len(s.bufDel) && s.bufDel[off] == 0 {
			s.bufDel[off] = ver
		}
		return
	}
	k := s.locateBound(rid)
	if k < 0 {
		return
	}
	off := int(rid - s.bounds[k].first)
	for _, f := range s.fields {
		seg := s.segs[f][k]
		if off < len(seg.del) && seg.del[off] == 0 {
			seg.del[off] = ver
		}
	}
}

// locateBound binary-searches the flushed boundaries for the one holding rid, or -1.
func (s *Store) locateBound(rid uint64) int {
	k := sort.Search(len(s.bounds), func(i int) bool {
		return s.bounds[i].first+uint64(s.bounds[i].n) > rid
	})
	if k < len(s.bounds) && rid >= s.bounds[k].first {
		return k
	}
	return -1
}

// Flush seals the in-progress buffer into a segment, the partial-segment flush the
// spec performs on transaction commit (spec 2061 doc 04 §10.4).
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}

func (s *Store) flushLocked() {
	if len(s.bufRIDs) == 0 {
		return
	}
	for _, f := range s.fields {
		vals := s.bufVals[f]
		rids := append([]uint64(nil), s.bufRIDs...)
		write := append([]uint64(nil), s.bufWrite...)
		del := append([]uint64(nil), s.bufDel...)
		s.segs[f] = append(s.segs[f], buildSegment(f, vals, rids, write, del))
		s.bufVals[f] = nil
	}
	s.bounds = append(s.bounds, segBound{first: s.bufRIDs[0], n: len(s.bufRIDs)})
	s.bufRIDs = nil
	s.bufWrite = nil
	s.bufDel = nil
}

// Rebuild discards all state and rebuilds the store from a committed snapshot of the
// collection, the path lazy mode uses for a background refresh (spec 2061 doc 04
// §10.4). Every document becomes visible at ver.
func (s *Store) Rebuild(docs []bson.Raw, ver uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.segs = make(map[string][]*Segment, len(s.fields))
	s.bounds = nil
	s.bufVals = make(map[string][]Value, len(s.fields))
	s.bufRIDs, s.bufWrite, s.bufDel = nil, nil, nil
	s.live = make(map[string]uint64, len(docs))
	s.nextRID = 0
	for _, f := range s.fields {
		s.segs[f] = nil
		s.bufVals[f] = nil
	}
	for _, d := range docs {
		s.insertLocked(idKey(d), d, ver)
	}
	s.flushLocked()
}

// SegmentCount returns the number of flushed segments per field, the figure the
// planner's cost model multiplies by the per-segment read cost.
func (s *Store) SegmentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bounds)
}

// RowCount returns the number of rows visible at snapshot snap.
func (s *Store) RowCount(snap uint64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	if len(s.fields) > 0 {
		f := s.fields[0]
		for _, seg := range s.segs[f] {
			total += seg.liveCount(snap)
		}
	}
	for i := range s.bufRIDs {
		if s.bufWrite[i] <= snap && (s.bufDel[i] == 0 || s.bufDel[i] > snap) {
			total++
		}
	}
	return total
}

// Covers reports whether every field in need is projected into this store, the
// precondition for the planner to use the column path without a heap fetch.
func (s *Store) Covers(need []string) bool {
	set := make(map[string]bool, len(s.fields))
	for _, f := range s.fields {
		set[f] = true
	}
	for _, f := range need {
		if !set[f] {
			return false
		}
	}
	return true
}
