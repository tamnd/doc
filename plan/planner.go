package plan

import (
	"math"
	"strings"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/query"
	"github.com/tamnd/doc/storage"
)

// Request is one find to plan: the filter and the optional projection, sort, and
// skip/limit bounds, plus the indexes the planner may consider and whether an
// index access path is safe to use for this read. AllowIndex is false when the
// caller's snapshot might not match the indexes' committed state (a stale snapshot
// or a transaction with its own buffered writes), in which case the planner emits
// a collection scan that reads through the snapshot-correct overlay.
type Request struct {
	Filter     bson.Raw
	Projection bson.Raw
	Sort       bson.Raw
	Skip       int64
	Limit      int64
	Indexes    []IndexDesc
	AllowIndex bool
}

// Plan is a chosen access path ready to execute or explain. It holds the compiled
// query shapes and the winning candidate, and rebuilds a fresh stage tree on each
// run because stages and index cursors are single-use.
type Plan struct {
	req     Request
	src     Source
	rows    []Row
	matcher *query.Matcher
	proj    *query.Projection
	sortc   *query.Sort
	cand    *candidate // nil => collection scan
}

// candidate is one index access path under consideration: the index, its encoded
// byte range, the cost estimate, and whether the index supplies the requested sort
// order or lets the projection be covered.
type candidate struct {
	desc         IndexDesc
	sb           scanBounds
	cost         float64
	providesSort bool
	covered      bool
}

// New analyzes a request and chooses an access path. It compiles the filter,
// projection, and sort, reads the collection's documents once (the overlay is in
// memory, and the count drives the cost estimate), enumerates the candidate
// indexes, and keeps the cheapest one that beats a collection scan.
func New(req Request, src Source) (*Plan, error) {
	m, err := query.Compile(req.Filter)
	if err != nil {
		return nil, err
	}
	proj, err := query.CompileProjection(req.Projection)
	if err != nil {
		return nil, err
	}
	srt, err := query.CompileSort(req.Sort)
	if err != nil {
		return nil, err
	}
	rows, err := src.ScanDocuments()
	if err != nil {
		return nil, err
	}
	p := &Plan{req: req, src: src, rows: rows, matcher: m, proj: proj, sortc: srt}
	if req.AllowIndex {
		p.cand = chooseIndex(req, rows)
	}
	return p, nil
}

// chooseIndex enumerates the index candidates and returns the cheapest whose cost
// is strictly below a collection scan, or nil to keep the collection scan. The
// strict comparison breaks ties toward the collection scan, avoiding index
// overhead when an index would not actually narrow the read.
func chooseIndex(req Request, rows []Row) *candidate {
	fb := extractBounds(req.Filter)
	n := len(rows)
	sortNeeded := !sortEmpty(req.Sort)
	base := collscanCost(n, sortNeeded)

	var best *candidate
	for _, idx := range req.Indexes {
		if idx.Partial {
			continue // a partial index needs the filter to imply its predicate; deferred
		}
		sb, err := buildScanBounds(idx.Key, fb)
		if err != nil {
			continue
		}
		provides := providesSort(idx.Key, req.Sort)
		if !sb.Usable && !sb.Empty && !provides {
			continue // the index neither narrows the scan nor supplies the sort
		}
		covered := canCover(req, idx)
		cost := indexCost(sb, n, sortNeeded, provides, covered)
		if cost >= base {
			continue
		}
		if best == nil || cost < best.cost {
			c := candidate{desc: idx, sb: sb, cost: cost, providesSort: provides, covered: covered}
			best = &c
		}
	}
	return best
}

// buildTree constructs a fresh execution tree for the chosen access path. The
// stages are stacked bottom-up in MongoDB's order: scan, fetch, residual filter,
// sort, skip, limit, projection (spec 2061 doc 11 §3).
func (p *Plan) buildTree() (PlanStage, error) {
	var root PlanStage
	covered := false
	if p.cand != nil {
		opts := storage.ScanOpts{IncludeLo: true, IncludeHi: true}
		cur, err := p.src.OpenIndexCursor(p.cand.desc.Name, p.cand.sb.Lo, p.cand.sb.Hi, opts)
		if err != nil {
			return nil, err
		}
		root = newIndexScan(cur, p.cand.desc, p.cand.sb)
		if p.cand.covered {
			root = newCoveredFetch(root, p.cand.desc.Key)
			covered = true
		} else {
			root = newFetch(root, p.src)
		}
	} else {
		root = newCollectionScan(p.rows)
	}

	if !filterEmpty(p.req.Filter) {
		root = newFilter(root, p.matcher, p.req.Filter)
	}
	if !sortEmpty(p.req.Sort) && !(p.cand != nil && p.cand.providesSort) {
		root = newSort(root, p.sortc, p.req.Sort)
	}
	if p.req.Skip > 0 {
		root = newSkip(root, p.req.Skip)
	}
	if lim := normalizeLimit(p.req.Limit); lim > 0 {
		root = newLimit(root, lim)
	}
	if !projectionEmpty(p.req.Projection) {
		root = newProjection(root, p.proj, covered, p.req.Projection)
	}
	return root, nil
}

// Execute runs the plan and returns the matching documents as independent clones.
func (p *Plan) Execute() ([]bson.Raw, error) {
	root, err := p.buildTree()
	if err != nil {
		return nil, err
	}
	var out []bson.Raw
	for {
		m, err := root.GetNext()
		if err == EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, m.Doc.Clone())
	}
	return out, nil
}

// ---- cost model ----------------------------------------------------------

// indexCost estimates the work of an index access path: a selectivity guess from
// the bound shape (each equality field divides the row count by ten, a range by
// three), plus a fetch surcharge unless the plan is covered, plus a sort surcharge
// unless the index already supplies the order.
func indexCost(sb scanBounds, n int, sortNeeded, providesSort, covered bool) float64 {
	eq := 0
	hasRange := false
	for _, iv := range sb.Intervals {
		if iv.Point {
			eq++
			continue
		}
		if !iv.AlwaysTrue {
			hasRange = true
		}
		break
	}
	scan := float64(n)
	switch {
	case eq > 0:
		scan = float64(n) / math.Pow(10, float64(eq))
	case hasRange:
		scan = float64(n) / 3
	}
	if scan < 1 {
		scan = 1
	}
	cost := scan
	if !covered {
		cost += scan * 0.5
	}
	if sortNeeded && !providesSort {
		cost += scan * math.Log2(scan+2)
	}
	return cost
}

// collscanCost estimates a collection scan: every document, plus a blocking-sort
// surcharge when the query sorts.
func collscanCost(n int, sortNeeded bool) float64 {
	c := float64(n)
	if sortNeeded {
		c += float64(n) * math.Log2(float64(n)+2)
	}
	return c
}

// ---- coverage and sort analysis ------------------------------------------

// providesSort reports whether an index's leading key parts match the sort exactly
// in field order and direction, so the index scan yields sorted output and the
// blocking sort can be dropped. Only a forward match qualifies, since the M1 index
// cursor scans forward only.
func providesSort(key []catalog.KeyPart, sort bson.Raw) bool {
	if sortEmpty(sort) {
		return false
	}
	elems, err := sort.Elements()
	if err != nil || len(elems) > len(key) {
		return false
	}
	for i, e := range elems {
		dir, ok := e.Value.AsFloat64()
		if !ok {
			return false
		}
		if key[i].Field != e.Key || key[i].Desc != (dir < 0) {
			return false
		}
	}
	return true
}

// canCover reports whether the index can answer the query without fetching
// documents: the index must be non-multikey with simple (non-dotted) key fields,
// and the filter, sort, and projection may reference only those key fields, with
// _id excluded unless _id is itself a key field (spec 2061 doc 11 §5.6). A covered
// plan reconstructs values from the key, so a numeric field comes back as a double;
// that is acceptable for the projection but the planner still requires the shape to
// qualify.
func canCover(req IndexDescRequest, idx IndexDesc) bool {
	if idx.Multikey {
		return false
	}
	keyset := make(map[string]struct{}, len(idx.Key))
	for _, kp := range idx.Key {
		if strings.Contains(kp.Field, ".") {
			return false
		}
		keyset[kp.Field] = struct{}{}
	}
	if projectionEmpty(req.Projection) {
		return false // a covered plan exists to satisfy a projection
	}
	if !projectionCoverable(req.Projection, keyset) {
		return false
	}
	if fields, ok := filterFields(req.Filter); !ok || !subset(fields, keyset) {
		return false
	}
	if sf, ok := sortFields(req.Sort); !ok || !subset(sf, keyset) {
		return false
	}
	return true
}

// IndexDescRequest is the subset of Request canCover needs. It lets canCover take
// the request shape without depending on the index list or execution flags.
type IndexDescRequest = Request

// projectionCoverable reports whether a projection is a pure inclusion of fields
// all present in keyset, with _id excluded unless _id is a key field. An exclusion
// projection on any other field disqualifies coverage, since it would need the full
// document.
func projectionCoverable(proj bson.Raw, keyset map[string]struct{}) bool {
	elems, err := proj.Elements()
	if err != nil || len(elems) == 0 {
		return false
	}
	idExcluded := false
	if _, ok := keyset["_id"]; ok {
		idExcluded = true // _id is in the index, so it is covered either way
	}
	for _, e := range elems {
		inc := projTruthy(e.Value)
		if e.Key == "_id" {
			if !inc {
				idExcluded = true
			}
			continue
		}
		if !inc {
			return false // an exclusion projection needs the document
		}
		if _, ok := keyset[e.Key]; !ok {
			return false
		}
	}
	return idExcluded
}

// projTruthy reports whether a projection value selects inclusion (a non-zero
// number or true).
func projTruthy(v bson.RawValue) bool {
	if f, ok := v.AsFloat64(); ok {
		return f != 0
	}
	return v.Type == bson.TypeBoolean && v.Boolean()
}

// filterFields returns every field name the filter references and whether the
// filter is simple enough to reason about for coverage. A filter with constructs
// that introduce sub-paths ($elemMatch) is reported as not coverable.
func filterFields(filter bson.Raw) ([]string, bool) {
	var fields []string
	ok := collectFilterFields(filter, &fields)
	return fields, ok
}

func collectFilterFields(d bson.Raw, fields *[]string) bool {
	elems, err := d.Elements()
	if err != nil {
		return false
	}
	for _, e := range elems {
		switch {
		case e.Key == "$and" || e.Key == "$or" || e.Key == "$nor":
			for _, sub := range arrayDocs(e.Value) {
				if !collectFilterFields(sub, fields) {
					return false
				}
			}
		case strings.HasPrefix(e.Key, "$"):
			return false // an unrecognized top-level operator: do not risk coverage
		default:
			if hasElemMatch(e.Value) {
				return false
			}
			*fields = append(*fields, e.Key)
		}
	}
	return true
}

func hasElemMatch(v bson.RawValue) bool {
	if v.Type != bson.TypeDocument {
		return false
	}
	elems, err := v.Document().Elements()
	if err != nil {
		return false
	}
	for _, e := range elems {
		if e.Key == "$elemMatch" {
			return true
		}
	}
	return false
}

// sortFields returns the sort's field names and whether they are simple enough for
// coverage (no dotted paths, which a covered key cannot reconstruct nested).
func sortFields(sort bson.Raw) ([]string, bool) {
	if sortEmpty(sort) {
		return nil, true
	}
	elems, err := sort.Elements()
	if err != nil {
		return nil, false
	}
	out := make([]string, 0, len(elems))
	for _, e := range elems {
		if strings.Contains(e.Key, ".") {
			return nil, false
		}
		out = append(out, e.Key)
	}
	return out, true
}

func subset(fields []string, set map[string]struct{}) bool {
	for _, f := range fields {
		if _, ok := set[f]; !ok {
			return false
		}
	}
	return true
}

// ---- small predicates ----------------------------------------------------

func filterEmpty(filter bson.Raw) bool   { return docEmpty(filter) }
func projectionEmpty(proj bson.Raw) bool { return docEmpty(proj) }
func sortEmpty(sort bson.Raw) bool       { return docEmpty(sort) }

func docEmpty(d bson.Raw) bool {
	if len(d) == 0 {
		return true
	}
	elems, err := d.Elements()
	return err == nil && len(elems) == 0
}

// normalizeLimit folds MongoDB's negative single-batch limit to its magnitude.
func normalizeLimit(limit int64) int64 {
	if limit < 0 {
		return -limit
	}
	return limit
}
