package index

import (
	"bytes"
	"fmt"

	"github.com/tamnd/doc/format"
)

// CheckResult reports a B-tree integrity walk: the number of node pages visited,
// the number of live leaf entries seen, and every structural violation found. A
// nil Problems slice means the tree's node types, page checksums, key ordering,
// and leaf-chain coverage all held.
type CheckResult struct {
	NodePages int
	LeafKeys  uint64
	Problems  []string
}

// Check verifies the B-tree's structure from the root down (spec 2061 doc 19 §17,
// the "Index correctness" row of §13): every reachable node page carries a valid
// checksum and the expected interior or leaf type, keys are strictly increasing
// within every node, and the same key total is reached two independent ways, by
// descending the tree and by walking the leaf right-sibling chain. A mismatch
// between the two means a leaf is unreachable from the root or double-linked. It
// reads under t.mu and mutates nothing.
func (t *BTree) Check() CheckResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	var res CheckResult
	if t.root == format.NullPage {
		return res
	}
	treeKeys := t.checkNode(t.root, &res)
	chainKeys := t.checkLeafChain(&res)
	if treeKeys != chainKeys {
		res.Problems = append(res.Problems, fmt.Sprintf(
			"key total mismatch: tree walk %d, leaf chain %d", treeKeys, chainKeys))
	}
	res.LeafKeys = chainKeys
	return res
}

// checkNode recursively verifies the subtree rooted at pno and returns the number
// of leaf keys beneath it. The caller holds t.mu.
func (t *BTree) checkNode(pno uint32, res *CheckResult) uint64 {
	res.NodePages++
	f, err := t.pgr.Fetch(uint64(pno), false)
	if err != nil {
		res.Problems = append(res.Problems, fmt.Sprintf("node %d: fetch failed: %v", pno, err))
		return 0
	}
	ty := format.DecodePageHeader(f.Buf).Type
	if vErr := format.VerifyPageChecksum(f.Buf, t.pgr.Checksum()); vErr != nil {
		res.Problems = append(res.Problems, fmt.Sprintf("node %d: %v", pno, vErr))
	}
	t.pgr.Unpin(f)

	switch ty {
	case format.PageBTreeLeaf:
		keys, _, lerr := t.loadLeaf(pno)
		if lerr != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("leaf %d: load failed: %v", pno, lerr))
			return 0
		}
		t.checkSorted(pno, keys, res)
		return uint64(len(keys))
	case format.PageBTreeInterior:
		ents, ierr := t.loadInterior(pno)
		if ierr != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("interior %d: load failed: %v", pno, ierr))
			return 0
		}
		var total uint64
		for i, e := range ents {
			if i > 0 && bytes.Compare(ents[i-1].sep, e.sep) >= 0 {
				res.Problems = append(res.Problems, fmt.Sprintf(
					"interior %d: separators out of order at entry %d", pno, i))
			}
			total += t.checkNode(e.child, res)
		}
		return total
	default:
		res.Problems = append(res.Problems, fmt.Sprintf(
			"node %d: reachable from root but its type is %v, not a B-tree node", pno, ty))
		return 0
	}
}

// checkSorted reports any pair of adjacent leaf keys that are not strictly
// increasing. The caller holds t.mu.
func (t *BTree) checkSorted(pno uint32, keys [][]byte, res *CheckResult) {
	for i := 1; i < len(keys); i++ {
		if bytes.Compare(keys[i-1], keys[i]) >= 0 {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"leaf %d: keys out of order at entry %d", pno, i))
		}
	}
}

// checkLeafChain walks the leaf right-sibling chain from the leftmost leaf and
// returns the total live key count, verifying that keys stay ordered across leaf
// boundaries and that the chain terminates rather than cycling. The caller holds
// t.mu.
func (t *BTree) checkLeafChain(res *CheckResult) uint64 {
	leaf, _, err := t.descend([]byte{})
	if err != nil {
		res.Problems = append(res.Problems, fmt.Sprintf("leaf chain: descent to leftmost leaf failed: %v", err))
		return 0
	}
	var total uint64
	var prev []byte
	seen := make(map[uint32]struct{})
	for leaf != format.NullPage {
		if _, dup := seen[leaf]; dup {
			res.Problems = append(res.Problems, fmt.Sprintf("leaf chain: cycle detected at leaf %d", leaf))
			break
		}
		seen[leaf] = struct{}{}
		keys, rightSib, lerr := t.loadLeaf(leaf)
		if lerr != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("leaf chain: leaf %d load failed: %v", leaf, lerr))
			break
		}
		for _, k := range keys {
			if prev != nil && bytes.Compare(prev, k) >= 0 {
				res.Problems = append(res.Problems, fmt.Sprintf(
					"leaf chain: keys out of order crossing into leaf %d", leaf))
			}
			prev = k
			total++
		}
		leaf = rightSib
	}
	return total
}
