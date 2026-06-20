package index

import (
	"bytes"
	"errors"

	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/storage"
)

// ErrReverseUnsupported reports a reverse index scan, which M1 does not implement.
// The leaf chain is singly linked (right-sibling only), so high→low iteration
// needs either a backward link or a stack of ancestors; both arrive with the
// find path in M3 (spec 2061 doc 07 §5, roadmap doc 19 §22).
var ErrReverseUnsupported = errors.New("index: reverse scan is not supported in M1")

// pos is a resolved position in the leaf chain: a live entry keys[idx] on leaf,
// with rightSib cached so iteration can cross to the next leaf without a descent.
type pos struct {
	leaf     uint32
	keys     [][]byte
	idx      int
	rightSib uint32
	done     bool
}

// seek resolves the first tree key >= search, crossing right-siblings when the
// descent lands on a leaf whose keys are all smaller than search.
func (t *BTree) seek(search []byte) (*pos, error) {
	leaf, _, err := t.descend(search)
	if err != nil {
		return nil, err
	}
	for {
		keys, rightSib, lerr := t.loadLeaf(leaf)
		if lerr != nil {
			return nil, lerr
		}
		idx := 0
		for idx < len(keys) && bytes.Compare(keys[idx], search) < 0 {
			idx++
		}
		if idx < len(keys) {
			return &pos{leaf: leaf, keys: keys, idx: idx, rightSib: rightSib}, nil
		}
		if rightSib == format.NullPage {
			return &pos{done: true}, nil
		}
		leaf = rightSib
	}
}

// cursor is a forward IndexCursor over a [lo, hi) field range.
type cursor struct {
	t         *BTree
	hi        []byte // field upper bound, nil = unbounded
	includeHi bool

	leaf     uint32
	keys     [][]byte
	idx      int
	rightSib uint32

	primed bool // the current idx is a candidate not yet returned by Next
	done   bool
	err    error
	curKey []byte
}

// Scan returns a forward cursor over entries whose field key lies in [lo, hi),
// honoring ScanOpts. lo or hi may be nil for an open bound. Reverse scans are
// rejected in M1.
func (t *BTree) Scan(txn storage.Txn, lo, hi storage.IndexKey, opts storage.ScanOpts) (storage.IndexCursor, error) {
	if opts.Reverse {
		return nil, ErrReverseUnsupported
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	c := &cursor{t: t, hi: hi, includeHi: opts.IncludeHi}
	if t.root == format.NullPage {
		c.done = true
		return c, nil
	}

	var search []byte
	if lo == nil {
		search = []byte{} // leftmost leaf
	} else {
		search = treeKey(lo, storage.RID{PageNo: 0, Slot: 0})
	}
	p, err := t.seek(search)
	if err != nil {
		return nil, err
	}
	if p.done {
		c.done = true
		return c, nil
	}
	c.leaf, c.keys, c.idx, c.rightSib = p.leaf, p.keys, p.idx, p.rightSib

	// Exclusive lower bound: skip the run of entries whose field equals lo.
	if lo != nil && !opts.IncludeLo {
		for !c.done && bytes.Equal(fieldOf(c.keys[c.idx]), lo) {
			if !c.step() {
				c.done = true
			}
		}
	}
	c.primed = !c.done
	return c, nil
}

// step advances to the next live entry, crossing leaves via the right-sibling
// chain and skipping empty leaves. It returns false when the chain is exhausted.
func (c *cursor) step() bool {
	c.idx++
	for c.idx >= len(c.keys) {
		if c.rightSib == format.NullPage {
			return false
		}
		keys, rs, err := c.t.loadLeaf(c.rightSib)
		if err != nil {
			c.err = err
			return false
		}
		c.leaf = c.rightSib
		c.keys = keys
		c.rightSib = rs
		c.idx = 0
	}
	return true
}

func (c *cursor) Next() bool {
	if c.err != nil || c.done {
		return false
	}
	if c.primed {
		c.primed = false
	} else if !c.step() {
		c.done = true
		return false
	}
	if c.idx >= len(c.keys) {
		c.done = true
		return false
	}
	k := c.keys[c.idx]
	if c.hi != nil {
		cmp := bytes.Compare(fieldOf(k), c.hi)
		if (c.includeHi && cmp > 0) || (!c.includeHi && cmp >= 0) {
			c.done = true
			return false
		}
	}
	c.curKey = k
	return true
}

func (c *cursor) Key() storage.IndexKey { return storage.IndexKey(fieldOf(c.curKey)) }
func (c *cursor) RID() storage.RID      { return ridFromSuffix(c.curKey) }
func (c *cursor) Err() error            { return c.err }
func (c *cursor) Close() error          { c.done = true; return nil }
