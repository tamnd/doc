package doc

import "context"

// IndexCheck is one index's slice of a check: its name, the number of live entries
// it holds, whether it is consistent with the heap, and any violations found.
type IndexCheck struct {
	Name     string
	Entries  int64
	Valid    bool
	Problems []string
}

// CollectionCheck is one collection's check: its namespace, its live document
// count, the per-index results, record-store problems, heap-to-index consistency
// problems, and the combined verdict.
type CollectionCheck struct {
	Namespace    string
	Documents    int64
	Indexes      []IndexCheck
	HeapProblems []string
	Problems     []string
	Valid        bool
}

// CheckReport is the result of DB.Check: the file-level problems, one entry per
// collection, and the overall verdict. Valid is true only when no problem was
// found at any level (spec 2061 doc 18 §7.3, doc 19 §17).
type CheckReport struct {
	FileProblems []string
	Collections  []CollectionCheck
	Valid        bool
}

// Check runs a full structural and consistency verification of the database and
// returns a report (spec 2061 doc 18 §7.3, doc 19 §17). It verifies the file's
// freelist integrity, every collection's record-store invariants, every B-tree's
// structure, and the two-way agreement between each index and the heap. With
// full, it also checkpoints the WAL and verifies every page checksum, which reads
// the whole file and is correspondingly slower. Check holds the read lock and
// mutates no document state, so it is safe to run against a live database.
func (db *DB) Check(ctx context.Context, full bool) (*CheckReport, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	er := db.eng.Check(full)
	rep := &CheckReport{
		FileProblems: er.FileProblems,
		Valid:        er.Valid,
	}
	for _, cc := range er.Collections {
		out := CollectionCheck{
			Namespace:    cc.Namespace,
			Documents:    cc.Report.Documents,
			HeapProblems: cc.Report.HeapProblems,
			Problems:     cc.Report.Problems,
			Valid:        cc.Report.Valid,
		}
		for _, ix := range cc.Report.Indexes {
			out.Indexes = append(out.Indexes, IndexCheck{
				Name:     ix.Name,
				Entries:  ix.Entries,
				Valid:    ix.Valid,
				Problems: ix.Problems,
			})
		}
		rep.Collections = append(rep.Collections, out)
	}
	return rep, nil
}
