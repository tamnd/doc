package collection

import (
	"fmt"
	"sort"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/storage"
)

// IndexCheck is the per-index slice of a collection check: the index name, the
// number of live entries it holds, and any consistency violations found between
// the index and the heap. Valid is true when Problems is empty.
type IndexCheck struct {
	Name     string
	Entries  int64
	Valid    bool
	Problems []string
}

// CheckReport is the structural and consistency verdict for one collection (spec
// 2061 doc 18 §7.3, doc 19 §17). Documents is the live document count the heap
// reports; HeapProblems are record-store invariant violations; Problems are
// heap-to-index consistency violations; Indexes carries one entry per index,
// including the implicit _id index. Valid is true when no problem was found
// anywhere.
type CheckReport struct {
	Documents    int64
	Indexes      []IndexCheck
	HeapProblems []string
	Problems     []string
	Valid        bool
}

// Check runs a full structural and consistency verification of the collection:
// the heap's record-store invariants, each B-tree's structure, and the two-way
// agreement between the heap and every index (every index entry resolves to a
// live document that produces its key, and every live document contributes the
// keys it should). It takes no write locks and mutates nothing, so it can run on
// an open collection (spec 2061 doc 19 §17, the index-correctness row of §13).
func (c *Collection) Check() CheckReport {
	var rep CheckReport
	rep.HeapProblems = c.hp.Check().Problems

	live, liveProblems := c.liveByID()
	rep.Documents = int64(len(live))
	rep.Problems = append(rep.Problems, liveProblems...)

	rep.Indexes = append(rep.Indexes, c.checkIDIndex(live))
	for _, sp := range c.cat.Specs() {
		rep.Indexes = append(rep.Indexes, c.checkSecondary(sp, live))
	}

	rep.Valid = len(rep.HeapProblems) == 0 && len(rep.Problems) == 0
	for _, ix := range rep.Indexes {
		if !ix.Valid {
			rep.Valid = false
		}
	}
	return rep
}

// liveDoc is the heap-side truth for one live document: its _id (as an index key)
// and the document bytes. The checker keys live documents by their _id key, which
// is unique across a consistent collection.
type liveDoc struct {
	idKey string
	doc   bson.Raw
}

// liveByID reads every live document from the durable heap and keys it by its _id
// index encoding, the identity the checker matches index entries against. It also
// verifies each document is well-formed BSON and carries an _id, and that no two
// live documents share an _id. The caller does not hold c.mu.
func (c *Collection) liveByID() (map[string]liveDoc, []string) {
	recs, err := c.hp.Records()
	if err != nil {
		return nil, []string{fmt.Sprintf("heap: reading live records failed: %v", err)}
	}
	var problems []string
	live := make(map[string]liveDoc, len(recs))
	for _, r := range recs {
		if werr := r.Doc.WellFormed(); werr != nil {
			problems = append(problems, fmt.Sprintf("document at %d:%d is not well-formed: %v", r.RID.PageNo, r.RID.Slot, werr))
			continue
		}
		idv, ok := bson.IDOf(r.Doc)
		if !ok {
			problems = append(problems, fmt.Sprintf("document at %d:%d has no _id", r.RID.PageNo, r.RID.Slot))
			continue
		}
		key, kerr := overlayKey(idv)
		if kerr != nil {
			problems = append(problems, fmt.Sprintf("document at %d:%d has an unencodable _id: %v", r.RID.PageNo, r.RID.Slot, kerr))
			continue
		}
		if _, dup := live[key]; dup {
			problems = append(problems, fmt.Sprintf("duplicate live _id at %d:%d", r.RID.PageNo, r.RID.Slot))
			continue
		}
		live[key] = liveDoc{idKey: key, doc: r.Doc}
	}
	return live, problems
}

// resolveDoc follows an index entry's RID through the heap, returning the live
// document it addresses or an error when the RID resolves to nothing live. It is
// the bridge that turns "index points at a RID" into "index points at a document".
func (c *Collection) resolveDoc(rid storage.RID) (bson.Raw, error) {
	return c.hp.Lookup(writeTxn{}, rid)
}

// checkIDIndex verifies the _id index against the live document set both ways:
// every index entry resolves to a live document whose _id matches the entry's
// key, and every live document's _id appears exactly once in the index.
func (c *Collection) checkIDIndex(live map[string]liveDoc) IndexCheck {
	res := IndexCheck{Name: catalog.IDIndexName}
	seen := make(map[string]struct{}, len(live))

	cur, err := c.idx.Scan(writeTxn{}, nil, nil, storage.ScanOpts{})
	if err != nil {
		res.Problems = append(res.Problems, fmt.Sprintf("scan failed: %v", err))
		return res
	}
	for cur.Next() {
		res.Entries++
		keyStr := string(cur.Key())
		rid := cur.RID()
		doc, derr := c.resolveDoc(rid)
		if derr != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("entry for _id key resolves to no live document at %d:%d: %v", rid.PageNo, rid.Slot, derr))
			continue
		}
		idv, ok := bson.IDOf(doc)
		if !ok {
			res.Problems = append(res.Problems, fmt.Sprintf("entry at %d:%d points to a document with no _id", rid.PageNo, rid.Slot))
			continue
		}
		got, kerr := overlayKey(idv)
		if kerr != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("entry at %d:%d points to a document with an unencodable _id: %v", rid.PageNo, rid.Slot, kerr))
			continue
		}
		if got != keyStr {
			res.Problems = append(res.Problems, fmt.Sprintf("entry at %d:%d points to a document whose _id does not match the index key", rid.PageNo, rid.Slot))
			continue
		}
		if _, dup := seen[keyStr]; dup {
			res.Problems = append(res.Problems, "duplicate _id entry in the unique _id index")
			continue
		}
		seen[keyStr] = struct{}{}
		if _, ok := live[keyStr]; !ok {
			res.Problems = append(res.Problems, "_id index entry has no matching live document")
		}
	}
	if cerr := cur.Err(); cerr != nil {
		res.Problems = append(res.Problems, fmt.Sprintf("scan error: %v", cerr))
	}
	_ = cur.Close()

	for key := range live {
		if _, ok := seen[key]; !ok {
			res.Problems = append(res.Problems, "live document is missing from the _id index")
		}
	}
	res.Valid = len(res.Problems) == 0
	return res
}

// indexEntryKey identifies one expected or actual secondary-index entry: the
// field key the index orders by, paired with the _id of the document it points
// at. Comparing the multiset of these from the heap against the multiset from the
// index catches a missing entry, a stray entry, and an entry pointing at the
// wrong document, all at once.
type indexEntryKey struct {
	field string
	id    string
}

// checkSecondary verifies one secondary index against the live document set. The
// B-tree's own structure is checked first, then the entry-by-entry agreement: the
// keys the live documents should contribute (honoring the partial filter, sparse
// rule, and multikey expansion) must equal the keys the index actually holds.
func (c *Collection) checkSecondary(sp *catalog.IndexSpec, live map[string]liveDoc) IndexCheck {
	res := IndexCheck{Name: sp.Name}
	bt := c.sidx[sp.Name]
	if bt == nil {
		res.Problems = append(res.Problems, "index has no open B-tree")
		return res
	}
	res.Problems = append(res.Problems, bt.Check().Problems...)

	expected := make(map[indexEntryKey]int)
	for _, ld := range live {
		keys, _, err := c.indexableKeys(sp, ld.doc)
		if err != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("computing keys for a document failed: %v", err))
			continue
		}
		for _, k := range keys {
			expected[indexEntryKey{field: string(k), id: ld.idKey}]++
		}
	}

	actual := make(map[indexEntryKey]int)
	cur, err := bt.Scan(writeTxn{}, nil, nil, storage.ScanOpts{})
	if err != nil {
		res.Problems = append(res.Problems, fmt.Sprintf("scan failed: %v", err))
		return res
	}
	for cur.Next() {
		res.Entries++
		field := string(cur.Key())
		rid := cur.RID()
		doc, derr := c.resolveDoc(rid)
		if derr != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("entry resolves to no live document at %d:%d: %v", rid.PageNo, rid.Slot, derr))
			continue
		}
		idv, ok := bson.IDOf(doc)
		if !ok {
			res.Problems = append(res.Problems, fmt.Sprintf("entry at %d:%d points to a document with no _id", rid.PageNo, rid.Slot))
			continue
		}
		idKey, kerr := overlayKey(idv)
		if kerr != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("entry at %d:%d points to a document with an unencodable _id: %v", rid.PageNo, rid.Slot, kerr))
			continue
		}
		actual[indexEntryKey{field: field, id: idKey}]++
	}
	if cerr := cur.Err(); cerr != nil {
		res.Problems = append(res.Problems, fmt.Sprintf("scan error: %v", cerr))
	}
	_ = cur.Close()

	res.Problems = append(res.Problems, diffEntries(expected, actual)...)
	res.Valid = len(res.Problems) == 0
	return res
}

// diffEntries reports the symmetric difference between the keys the documents
// should produce and the keys the index holds. Missing means a live document's
// key is absent from the index; stray means the index holds a key no live
// document produces. The output is sorted so a report is deterministic.
func diffEntries(expected, actual map[indexEntryKey]int) []string {
	var problems []string
	for k, n := range expected {
		if actual[k] < n {
			problems = append(problems, fmt.Sprintf("index is missing %d entr(y/ies) a live document should contribute", n-actual[k]))
		}
	}
	for k, n := range actual {
		if expected[k] < n {
			problems = append(problems, fmt.Sprintf("index holds %d stray entr(y/ies) no live document produces", n-expected[k]))
		}
	}
	sort.Strings(problems)
	return problems
}
