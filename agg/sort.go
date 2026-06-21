package agg

import (
	"container/heap"
	"io"
	"sort"

	"github.com/tamnd/doc/bson"
)

// sortKey is one component of a sort specification: a dotted field path and a
// direction (+1 ascending, -1 descending).
type sortKey struct {
	path []string
	dir  int
}

// sortSpec is a compiled sort specification (spec 2061 doc 12 §8.1).
type sortSpec struct {
	keys []sortKey
}

// compileSortSpec parses a {field: 1, field2: -1} document, preserving key order.
func compileSortSpec(d bson.Raw) (*sortSpec, error) {
	elems, err := d.Elements()
	if err != nil {
		return nil, err
	}
	if len(elems) == 0 {
		return nil, ErrBadStage
	}
	spec := &sortSpec{}
	for _, e := range elems {
		dir, ok := sortDir(e.Value)
		if !ok {
			return nil, ErrBadStage
		}
		spec.keys = append(spec.keys, sortKey{path: splitPath(e.Key), dir: dir})
	}
	return spec, nil
}

// sortDir reads a sort direction: a positive number ascends, a negative number
// descends; zero and non-numbers are invalid.
func sortDir(v bson.RawValue) (int, bool) {
	i, f, k := numOf(v)
	if k == kindNotNum {
		return 0, false
	}
	switch {
	case i > 0 || f > 0:
		return 1, true
	case i < 0 || f < 0:
		return -1, true
	default:
		return 0, false
	}
}

// compare ranks two documents by the sort spec: negative when a sorts before b,
// positive when after, zero when equal on every key. A missing field sorts as
// BSON null (spec 2061 doc 12 §8.1).
func (s *sortSpec) compare(a, b bson.Raw) int {
	ad, bd := mkDoc(a), mkDoc(b)
	for _, k := range s.keys {
		av := cmpVal(resolvePath(ad, k.path))
		bv := cmpVal(resolvePath(bd, k.path))
		c := bson.Compare(av, bv)
		if c != 0 {
			return c * k.dir
		}
	}
	return 0
}

// ---- $sort ---------------------------------------------------------------

func compileSort(arg bson.RawValue) (stageSpec, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	spec, err := compileSortSpec(arg.Document())
	if err != nil {
		return nil, err
	}
	return &sortStage{spec: spec}, nil
}

type sortStage struct {
	spec *sortSpec
	// limit, when positive, bounds output to the top-K documents; the optimizer
	// sets it when a $limit immediately follows the $sort (spec 2061 doc 12 §8.4).
	limit int
}

func (s *sortStage) open(in src, _ *execCtx) src {
	return &sortSrc{in: in, stage: s}
}

type sortSrc struct {
	in     src
	stage  *sortStage
	out    []bson.Raw
	i      int
	loaded bool
}

func (s *sortSrc) next() (bson.Raw, error) {
	if !s.loaded {
		if err := s.load(); err != nil {
			return nil, err
		}
		s.loaded = true
	}
	if s.i >= len(s.out) {
		return nil, io.EOF
	}
	d := s.out[s.i]
	s.i++
	return d, nil
}

// load drains the upstream, sorting fully or maintaining a bounded top-K heap.
func (s *sortSrc) load() error {
	if s.stage.limit > 0 {
		return s.loadTopK()
	}
	docs, err := drain(s.in)
	if err != nil {
		return err
	}
	sort.SliceStable(docs, func(i, j int) bool {
		return s.stage.spec.compare(docs[i], docs[j]) < 0
	})
	s.out = docs
	return nil
}

// loadTopK keeps only the K smallest documents by the sort order, using a bounded
// max-heap so memory stays O(K) rather than O(N) (spec 2061 doc 12 §8.4).
func (s *sortSrc) loadTopK() error {
	h := &topKHeap{spec: s.stage.spec}
	for {
		doc, err := s.in.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Len() < s.stage.limit {
			heap.Push(h, doc)
			continue
		}
		// Replace the current worst when this document sorts before it.
		if s.stage.spec.compare(doc, h.docs[0]) < 0 {
			h.docs[0] = doc
			heap.Fix(h, 0)
		}
	}
	out := make([]bson.Raw, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(h).(bson.Raw)
	}
	s.out = out
	return nil
}

// topKHeap is a max-heap by the sort order: its root is the document that sorts
// last, the first to evict when a better candidate arrives.
type topKHeap struct {
	spec *sortSpec
	docs []bson.Raw
}

func (h *topKHeap) Len() int { return len(h.docs) }

func (h *topKHeap) Less(i, j int) bool {
	// Invert the order so the largest (worst) document is at the root.
	return h.spec.compare(h.docs[i], h.docs[j]) > 0
}

func (h *topKHeap) Swap(i, j int) { h.docs[i], h.docs[j] = h.docs[j], h.docs[i] }

func (h *topKHeap) Push(x any) { h.docs = append(h.docs, x.(bson.Raw)) }

func (h *topKHeap) Pop() any {
	n := len(h.docs)
	d := h.docs[n-1]
	h.docs = h.docs[:n-1]
	return d
}

// drain reads every remaining document from a source into a slice.
func drain(in src) ([]bson.Raw, error) {
	var docs []bson.Raw
	for {
		doc, err := in.next()
		if err == io.EOF {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
}
