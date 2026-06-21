package agg

import (
	"io"
	"math/rand"

	"github.com/tamnd/doc/bson"
)

// ---- $sample -------------------------------------------------------------

// compileSample compiles {$sample: {size: N}}. The engine always uses the
// scan-and-reservoir strategy (Algorithm R), which guarantees a uniform sample;
// the random-cursor fast path is a storage-layer optimization that does not apply
// to an in-memory pipeline source (spec 2061 doc 12 §9.4).
func compileSample(arg bson.RawValue) (stageSpec, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	sv, ok := arg.Document().Lookup("size")
	if !ok {
		return nil, ErrBadStage
	}
	n, ok := intArg(sv)
	if !ok || n < 0 {
		return nil, ErrBadStage
	}
	return &sampleStage{size: n}, nil
}

type sampleStage struct{ size int }

func (s *sampleStage) open(in src, _ *execCtx) src {
	return &sampleSrc{in: in, size: s.size}
}

type sampleSrc struct {
	in     src
	size   int
	out    []bson.Raw
	i      int
	loaded bool
}

func (s *sampleSrc) next() (bson.Raw, error) {
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

// load draws a uniform sample with reservoir sampling: keep the first size
// documents, then replace a random slot with decreasing probability.
func (s *sampleSrc) load() error {
	if s.size == 0 {
		return nil
	}
	res := make([]bson.Raw, 0, s.size)
	seen := 0
	for {
		doc, err := s.in.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if len(res) < s.size {
			res = append(res, doc)
		} else {
			j := rand.Intn(seen + 1)
			if j < s.size {
				res[j] = doc
			}
		}
		seen++
	}
	s.out = res
	return nil
}
