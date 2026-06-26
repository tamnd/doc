package colstore

import (
	"encoding/binary"
	"errors"
)

// SegmentSize is the default number of documents per segment (spec 2061 doc 04
// §10.2). The in-progress buffer flushes to a new segment when it reaches this many
// entries or on commit for a partial segment.
const SegmentSize = 1024

// ErrBadSegment is returned when a serialized segment cannot be parsed.
var ErrBadSegment = errors.New("colstore: bad segment")

// Segment is one immutable run of a field's values over a contiguous block of RIDs
// in insertion order (spec 2061 doc 04 §10.2). The encoded value bytes and the RID
// and write-version arrays are fixed once the segment is built; only the delete
// stamps mutate, because a tombstone marks an entry deleted without rewriting the
// compressed column (§10.4, §10.6). The zone map (min and max over the non-null
// values) lets a range scan skip the whole segment without decoding it (§10.3).
type Segment struct {
	Field    string
	FirstRID uint64
	n        int      // entry count
	enc      []byte   // compressed column (immutable)
	rids     []uint64 // RID per entry (immutable)
	write    []uint64 // commit version each entry became visible (immutable)
	del      []uint64 // commit version each entry was deleted; 0 means live (mutable)

	hasZone bool
	zoneMin Value
	zoneMax Value
}

// buildSegment compresses a block of values into a segment. vals, rids, write, and
// del are parallel and equal length; the segment takes ownership of the slices. del
// carries any tombstones a row picked up while still in the in-progress buffer.
func buildSegment(field string, vals []Value, rids, write, del []uint64) *Segment {
	s := &Segment{
		Field: field,
		n:     len(vals),
		enc:   EncodeColumn(vals),
		rids:  rids,
		write: write,
		del:   del,
	}
	if len(rids) > 0 {
		s.FirstRID = rids[0]
	}
	s.computeZone(vals)
	return s
}

// computeZone records the min and max of the non-null values when every non-null
// value is zone-comparable (numeric or string). A segment with a mixed or opaque
// value carries no zone map and is never skipped by a range predicate.
func (s *Segment) computeZone(vals []Value) {
	first := true
	for _, v := range vals {
		if v.Kind == KindNull {
			continue
		}
		if !v.comparableForZone() {
			s.hasZone = false
			return
		}
		if first {
			s.zoneMin, s.zoneMax, s.hasZone, first = v, v, true, false
			continue
		}
		if c, ok := compareNumericOrString(v, s.zoneMin); ok && c < 0 {
			s.zoneMin = v
		}
		if c, ok := compareNumericOrString(v, s.zoneMax); ok && c > 0 {
			s.zoneMax = v
		}
	}
}

// Values decodes the segment's column. The result is positional: entry i lines up
// with rids[i], write[i], and del[i].
func (s *Segment) Values() ([]Value, error) {
	return DecodeColumn(s.enc)
}

// visible reports whether entry i is visible to a reader at snapshot snap: it must
// have committed at or before snap and not been deleted by a version at or before
// snap (spec 2061 doc 04 §10.6).
func (s *Segment) visible(i int, snap uint64) bool {
	if s.write[i] > snap {
		return false
	}
	return s.del[i] == 0 || s.del[i] > snap
}

// tombstone marks the entry for rid deleted at version ver, if the segment holds a
// live entry for it. It returns whether it found one.
func (s *Segment) tombstone(rid, ver uint64) bool {
	for i, r := range s.rids {
		if r == rid && s.del[i] == 0 {
			s.del[i] = ver
			return true
		}
	}
	return false
}

// skipForRange reports whether a range predicate of the given operator and bound can
// skip this whole segment using the zone map, without decoding it. ops are the four
// MQL range operators; a segment with no zone map is never skipped.
func (s *Segment) skipForRange(op string, bound Value) bool {
	if !s.hasZone {
		return false
	}
	// gt/gte: skip when every value is at or below the bound (zoneMax <= bound, or
	// < bound for gte's strict complement). lt/lte: skip when every value is at or
	// above the bound.
	switch op {
	case "$gt":
		c, ok := compareNumericOrString(s.zoneMax, bound)
		return ok && c <= 0
	case "$gte":
		c, ok := compareNumericOrString(s.zoneMax, bound)
		return ok && c < 0
	case "$lt":
		c, ok := compareNumericOrString(s.zoneMin, bound)
		return ok && c >= 0
	case "$lte":
		c, ok := compareNumericOrString(s.zoneMin, bound)
		return ok && c > 0
	default:
		return false
	}
}

// liveCount returns the number of entries visible at snapshot snap.
func (s *Segment) liveCount(snap uint64) int {
	c := 0
	for i := 0; i < s.n; i++ {
		if s.visible(i, snap) {
			c++
		}
	}
	return c
}

// EncodedSize reports the compressed value bytes, the figure the planner's cost
// model charges per segment read (spec 2061 doc 04 §10.5).
func (s *Segment) EncodedSize() int { return len(s.enc) }

// MarshalBinary serializes the segment to its on-disk form: a header, the RID and
// version arrays (RIDs delta-encoded against FirstRID, versions as uvarints), the
// zone map, and the compressed column. It is the inverse of UnmarshalBinary.
func (s *Segment) MarshalBinary() []byte {
	out := make([]byte, 0, len(s.enc)+s.n*4+len(s.Field)+32)
	out = binary.AppendUvarint(out, uint64(len(s.Field)))
	out = append(out, s.Field...)
	out = binary.AppendUvarint(out, s.FirstRID)
	out = binary.AppendUvarint(out, uint64(s.n))
	prev := s.FirstRID
	for _, r := range s.rids {
		out = binary.AppendUvarint(out, r-prev) // monotone non-decreasing within a segment
		prev = r
	}
	for _, w := range s.write {
		out = binary.AppendUvarint(out, w)
	}
	for _, d := range s.del {
		out = binary.AppendUvarint(out, d)
	}
	if s.hasZone {
		out = append(out, 1)
		out = putValue(out, s.zoneMin)
		out = putValue(out, s.zoneMax)
	} else {
		out = append(out, 0)
	}
	out = binary.AppendUvarint(out, uint64(len(s.enc)))
	out = append(out, s.enc...)
	return out
}

// UnmarshalSegment parses a segment serialized by MarshalBinary.
func UnmarshalSegment(b []byte) (*Segment, error) {
	rd := &reader{b: b}
	flen, ok := rd.uvarint()
	if !ok || rd.remaining() < int(flen) {
		return nil, ErrBadSegment
	}
	s := &Segment{Field: string(rd.take(int(flen)))}
	first, ok1 := rd.uvarint()
	n64, ok2 := rd.uvarint()
	if !ok1 || !ok2 {
		return nil, ErrBadSegment
	}
	s.FirstRID = first
	s.n = int(n64)
	s.rids = make([]uint64, s.n)
	prev := s.FirstRID
	for i := range s.rids {
		d, ok := rd.uvarint()
		if !ok {
			return nil, ErrBadSegment
		}
		prev += d
		s.rids[i] = prev
	}
	s.write = make([]uint64, s.n)
	for i := range s.write {
		w, ok := rd.uvarint()
		if !ok {
			return nil, ErrBadSegment
		}
		s.write[i] = w
	}
	s.del = make([]uint64, s.n)
	for i := range s.del {
		d, ok := rd.uvarint()
		if !ok {
			return nil, ErrBadSegment
		}
		s.del[i] = d
	}
	zflag, ok := rd.byteAt()
	if !ok {
		return nil, ErrBadSegment
	}
	if zflag == 1 {
		mn, n1, ok1 := getValue(rd.rest())
		if !ok1 {
			return nil, ErrBadSegment
		}
		rd.advance(n1)
		mx, n2, ok2 := getValue(rd.rest())
		if !ok2 {
			return nil, ErrBadSegment
		}
		rd.advance(n2)
		s.zoneMin, s.zoneMax, s.hasZone = mn, mx, true
	}
	elen, ok := rd.uvarint()
	if !ok || rd.remaining() < int(elen) {
		return nil, ErrBadSegment
	}
	s.enc = append([]byte(nil), rd.take(int(elen))...)
	return s, nil
}

// reader is a small cursor over a byte buffer used by UnmarshalSegment.
type reader struct {
	b   []byte
	off int
}

func (r *reader) remaining() int { return len(r.b) - r.off }
func (r *reader) rest() []byte   { return r.b[r.off:] }
func (r *reader) advance(n int)  { r.off += n }

func (r *reader) uvarint() (uint64, bool) {
	v, n := binary.Uvarint(r.b[r.off:])
	if n <= 0 {
		return 0, false
	}
	r.off += n
	return v, true
}

func (r *reader) take(n int) []byte {
	out := r.b[r.off : r.off+n]
	r.off += n
	return out
}

func (r *reader) byteAt() (byte, bool) {
	if r.remaining() < 1 {
		return 0, false
	}
	c := r.b[r.off]
	r.off++
	return c, true
}
