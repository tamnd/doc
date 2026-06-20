package plan

import (
	"errors"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/index"
	"github.com/tamnd/doc/query"
	"github.com/tamnd/doc/storage"
)

// errCoveredMismatch reports an index key whose decoded length does not match the
// key's bytes, so the covered fast path cannot trust the reconstruction (for
// example a decimal128 value sharing the numeric tag). It is an internal guard,
// never surfaced when the planner only chooses covered plans for decodable types.
var errCoveredMismatch = errors.New("plan: covered key did not decode cleanly")

// ---- CollectionScanStage -------------------------------------------------

// CollectionScanStage streams every document the source reports in natural order.
// It is the always-available access path, used when no index helps or when the
// read cannot safely use one (spec 2061 doc 11 §2.1).
type CollectionScanStage struct {
	rows  []Row
	pos   int
	stats StageStats
}

func newCollectionScan(rows []Row) *CollectionScanStage {
	return &CollectionScanStage{rows: rows, stats: StageStats{Stage: "COLLSCAN"}}
}

func (s *CollectionScanStage) GetNext() (*WorkingSetMember, error) {
	s.stats.Works++
	if s.pos >= len(s.rows) {
		return nil, EOF
	}
	r := s.rows[s.pos]
	s.pos++
	s.stats.DocsExamined++
	s.stats.Advanced++
	return &WorkingSetMember{RID: r.RID, Doc: r.Doc}, nil
}

func (s *CollectionScanStage) Stats() *StageStats { return &s.stats }
func (s *CollectionScanStage) Child() PlanStage   { return nil }
func (s *CollectionScanStage) explain(b *bson.Builder, _ bool) {
	b.AppendString("direction", "forward")
}

// ---- IndexScanStage ------------------------------------------------------

// IndexScanStage streams (RID, key) pairs from a secondary index over the planned
// byte range. It emits members without documents; a FetchStage or a covered
// projection turns the key or RID into a document (spec 2061 doc 11 §2.2).
type IndexScanStage struct {
	cur    storage.IndexCursor
	stats  StageStats
	desc   IndexDesc
	bounds scanBounds
}

func newIndexScan(cur storage.IndexCursor, desc IndexDesc, bounds scanBounds) *IndexScanStage {
	return &IndexScanStage{cur: cur, desc: desc, bounds: bounds, stats: StageStats{Stage: "IXSCAN"}}
}

func (s *IndexScanStage) GetNext() (*WorkingSetMember, error) {
	s.stats.Works++
	if !s.cur.Next() {
		if err := s.cur.Err(); err != nil {
			return nil, err
		}
		return nil, EOF
	}
	s.stats.KeysExamined++
	s.stats.Advanced++
	key := append(storage.IndexKey(nil), s.cur.Key()...)
	return &WorkingSetMember{RID: s.cur.RID(), Key: key}, nil
}

func (s *IndexScanStage) Stats() *StageStats { return &s.stats }
func (s *IndexScanStage) Child() PlanStage   { return nil }
func (s *IndexScanStage) explain(b *bson.Builder, _ bool) {
	kp := bson.NewBuilder()
	for _, part := range s.desc.Key {
		dir := int32(1)
		if part.Desc {
			dir = -1
		}
		kp.AppendInt32(part.Field, dir)
	}
	b.AppendDocument("keyPattern", kp.Build())
	b.AppendString("indexName", s.desc.Name)
	b.AppendBoolean("isMultiKey", s.desc.Multikey)
	b.AppendBoolean("isUnique", s.desc.Unique)
	b.AppendBoolean("isSparse", s.desc.Sparse)
	b.AppendBoolean("isPartial", s.desc.Partial)
	b.AppendString("direction", "forward")

	bounds := bson.NewBuilder()
	for i, part := range s.desc.Key {
		var ivs string
		if i < len(s.bounds.Intervals) {
			ivs = intervalString(s.bounds.Intervals[i])
		} else {
			ivs = "[MinKey, MaxKey]"
		}
		val, _ := bson.NewBuilder().AppendString("0", ivs).Build().Lookup("0")
		bounds.AppendArray(part.Field, bson.BuildArray(val))
	}
	b.AppendDocument("indexBounds", bounds.Build())
}

// ---- FetchStage ----------------------------------------------------------

// FetchStage resolves each child member's RID to the document visible at the
// read's snapshot. It deduplicates RIDs so a multikey index that emits the same
// document under several array entries yields it once, and it drops RIDs with no
// visible version (spec 2061 doc 11 §2.3, §5.4).
type FetchStage struct {
	child PlanStage
	src   Source
	seen  map[storage.RID]struct{}
	stats StageStats
}

func newFetch(child PlanStage, src Source) *FetchStage {
	return &FetchStage{child: child, src: src, seen: make(map[storage.RID]struct{}), stats: StageStats{Stage: "FETCH"}}
}

func (s *FetchStage) GetNext() (*WorkingSetMember, error) {
	for {
		s.stats.Works++
		m, err := s.child.GetNext()
		if err != nil {
			return nil, err
		}
		if _, dup := s.seen[m.RID]; dup {
			continue
		}
		s.seen[m.RID] = struct{}{}
		doc, ok := s.src.LookupRID(m.RID)
		if !ok {
			continue
		}
		s.stats.DocsExamined++
		s.stats.Advanced++
		m.Doc = doc
		return m, nil
	}
}

func (s *FetchStage) Stats() *StageStats          { return &s.stats }
func (s *FetchStage) Child() PlanStage            { return s.child }
func (s *FetchStage) explain(*bson.Builder, bool) {}

// ---- coveredFetchStage ---------------------------------------------------

// coveredFetchStage reconstructs each child member's document straight from its
// index key, with no heap fetch, for a covered plan. It is transparent in explain
// (it renders as its child) so the plan shows a projection directly over the index
// scan with no FETCH stage, matching MongoDB's covered shape (spec 2061 doc 11
// §5.6). The numeric type collapse in the key encoding means a numeric field is
// reconstructed as a double; the planner only chooses a covered plan when that is
// acceptable.
type coveredFetchStage struct {
	child PlanStage
	parts []catalog.KeyPart
	stats StageStats
}

func newCoveredFetch(child PlanStage, parts []catalog.KeyPart) *coveredFetchStage {
	return &coveredFetchStage{child: child, parts: parts, stats: StageStats{Stage: "COVERED"}}
}

func (s *coveredFetchStage) GetNext() (*WorkingSetMember, error) {
	s.stats.Works++
	m, err := s.child.GetNext()
	if err != nil {
		return nil, err
	}
	doc, derr := decodeKey(m.Key, s.parts)
	if derr != nil {
		return nil, derr
	}
	s.stats.Advanced++
	m.Doc = doc
	return m, nil
}

func (s *coveredFetchStage) Stats() *StageStats { return &s.stats }
func (s *coveredFetchStage) Child() PlanStage   { return s.child }

// explain is never reached: planNode renders a covered fetch transparently as its
// child so the plan shows a projection directly over the index scan with no FETCH.
func (s *coveredFetchStage) explain(*bson.Builder, bool) {}

// decodeKey reconstructs the indexed fields from a field key into a document. It
// fails if the key has trailing bytes after every field decodes, which guards
// against an ambiguous encoding (a decimal128 under the numeric tag).
func decodeKey(key storage.IndexKey, parts []catalog.KeyPart) (bson.Raw, error) {
	b := bson.NewBuilder()
	rest := []byte(key)
	for _, kp := range parts {
		n, err := index.AppendDecodedField(b, kp.Field, rest, kp.Desc)
		if err != nil {
			return nil, err
		}
		rest = rest[n:]
	}
	if len(rest) != 0 {
		return nil, errCoveredMismatch
	}
	return b.Build(), nil
}

// ---- FilterStage ---------------------------------------------------------

// FilterStage passes through only the documents matching the residual filter, the
// full compiled matcher applied to every candidate so the index bounds need only
// be a superset (spec 2061 doc 11 §2.4).
type FilterStage struct {
	child   PlanStage
	matcher *query.Matcher
	raw     bson.Raw
	stats   StageStats
}

func newFilter(child PlanStage, m *query.Matcher, raw bson.Raw) *FilterStage {
	return &FilterStage{child: child, matcher: m, raw: raw, stats: StageStats{Stage: "FILTER"}}
}

func (s *FilterStage) GetNext() (*WorkingSetMember, error) {
	for {
		s.stats.Works++
		m, err := s.child.GetNext()
		if err != nil {
			return nil, err
		}
		if s.matcher.Match(m.Doc) {
			s.stats.Advanced++
			return m, nil
		}
	}
}

func (s *FilterStage) Stats() *StageStats { return &s.stats }
func (s *FilterStage) Child() PlanStage   { return s.child }
func (s *FilterStage) explain(b *bson.Builder, _ bool) {
	if len(s.raw) != 0 {
		b.AppendDocument("filter", s.raw)
	}
}

// ---- SortStage -----------------------------------------------------------

// SortStage is a blocking stage: it drains its child, sorts the buffered
// documents by the compiled sort, then streams them in order (spec 2061 doc 11
// §2.6). A plan whose index already yields the requested order omits this stage.
type SortStage struct {
	child  PlanStage
	sort   *query.Sort
	raw    bson.Raw
	buf    []bson.Raw
	pos    int
	loaded bool
	stats  StageStats
}

func newSort(child PlanStage, s *query.Sort, raw bson.Raw) *SortStage {
	return &SortStage{child: child, sort: s, raw: raw, stats: StageStats{Stage: "SORT"}}
}

func (s *SortStage) load() error {
	for {
		m, err := s.child.GetNext()
		if errors.Is(err, EOF) {
			break
		}
		if err != nil {
			return err
		}
		s.buf = append(s.buf, m.Doc)
	}
	s.sort.Apply(s.buf)
	s.loaded = true
	return nil
}

func (s *SortStage) GetNext() (*WorkingSetMember, error) {
	if !s.loaded {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	s.stats.Works++
	if s.pos >= len(s.buf) {
		return nil, EOF
	}
	doc := s.buf[s.pos]
	s.pos++
	s.stats.Advanced++
	return &WorkingSetMember{Doc: doc}, nil
}

func (s *SortStage) Stats() *StageStats { return &s.stats }
func (s *SortStage) Child() PlanStage   { return s.child }
func (s *SortStage) explain(b *bson.Builder, _ bool) {
	if len(s.raw) != 0 {
		b.AppendDocument("sortPattern", s.raw)
	}
}

// ---- SkipStage -----------------------------------------------------------

// SkipStage discards the first n members it pulls, then streams the rest.
type SkipStage struct {
	child     PlanStage
	amount    int64
	remaining int64
	stats     StageStats
}

func newSkip(child PlanStage, n int64) *SkipStage {
	return &SkipStage{child: child, amount: n, remaining: n, stats: StageStats{Stage: "SKIP"}}
}

func (s *SkipStage) GetNext() (*WorkingSetMember, error) {
	for s.remaining > 0 {
		s.stats.Works++
		if _, err := s.child.GetNext(); err != nil {
			return nil, err
		}
		s.remaining--
	}
	s.stats.Works++
	m, err := s.child.GetNext()
	if err != nil {
		return nil, err
	}
	s.stats.Advanced++
	return m, nil
}

func (s *SkipStage) Stats() *StageStats { return &s.stats }
func (s *SkipStage) Child() PlanStage   { return s.child }
func (s *SkipStage) explain(b *bson.Builder, _ bool) {
	b.AppendInt64("skipAmount", s.amount)
}

// ---- LimitStage ----------------------------------------------------------

// LimitStage streams at most n members, then reports EOF without pulling more.
type LimitStage struct {
	child     PlanStage
	amount    int64
	remaining int64
	stats     StageStats
}

func newLimit(child PlanStage, n int64) *LimitStage {
	return &LimitStage{child: child, amount: n, remaining: n, stats: StageStats{Stage: "LIMIT"}}
}

func (s *LimitStage) GetNext() (*WorkingSetMember, error) {
	s.stats.Works++
	if s.remaining <= 0 {
		return nil, EOF
	}
	m, err := s.child.GetNext()
	if err != nil {
		return nil, err
	}
	s.remaining--
	s.stats.Advanced++
	return m, nil
}

func (s *LimitStage) Stats() *StageStats { return &s.stats }
func (s *LimitStage) Child() PlanStage   { return s.child }
func (s *LimitStage) explain(b *bson.Builder, _ bool) {
	b.AppendInt64("limitAmount", s.amount)
}

// ---- ProjectionStage -----------------------------------------------------

// ProjectionStage reshapes each document by the compiled projection. When covered
// is set the projection draws every field from the index key (no document fetch
// occurred), and the stage reports the PROJECTION_COVERED name (spec 2061 doc 11
// §5.6).
type ProjectionStage struct {
	child   PlanStage
	proj    *query.Projection
	covered bool
	raw     bson.Raw
	stats   StageStats
}

func newProjection(child PlanStage, p *query.Projection, covered bool, raw bson.Raw) *ProjectionStage {
	name := "PROJECTION_DEFAULT"
	if covered {
		name = "PROJECTION_COVERED"
	}
	return &ProjectionStage{child: child, proj: p, covered: covered, raw: raw, stats: StageStats{Stage: name}}
}

func (s *ProjectionStage) GetNext() (*WorkingSetMember, error) {
	s.stats.Works++
	m, err := s.child.GetNext()
	if err != nil {
		return nil, err
	}
	out, perr := s.proj.Apply(m.Doc)
	if perr != nil {
		return nil, perr
	}
	s.stats.Advanced++
	m.Doc = out
	return m, nil
}

func (s *ProjectionStage) Stats() *StageStats { return &s.stats }
func (s *ProjectionStage) Child() PlanStage   { return s.child }
func (s *ProjectionStage) explain(b *bson.Builder, _ bool) {
	if len(s.raw) != 0 {
		b.AppendDocument("transformBy", s.raw)
	}
}
