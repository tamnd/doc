package engine

import "github.com/tamnd/doc/collection"

// CollectionCheck is one collection's entry in an engine check: its namespace and
// the collection-level report (heap, indexes, and heap-to-index consistency).
type CollectionCheck struct {
	Namespace string
	Report    collection.CheckReport
}

// CheckReport is the whole-file integrity verdict (spec 2061 doc 18 §7.3, doc 19
// §17). FileProblems are page-level violations the pager found (freelist integrity
// and, in full mode, page checksums); Collections carries one report per
// collection. Valid is true when nothing failed anywhere.
type CheckReport struct {
	FileProblems []string
	Collections  []CollectionCheck
	Valid        bool
}

// Check verifies every collection in the file and the file-level page invariants.
// When full is true it also checkpoints the WAL and verifies every page checksum,
// the slower pass of spec 2061 doc 18 §7.3. It mutates no document state.
func (e *Engine) Check(full bool) CheckReport {
	e.mu.Lock()
	type namedColl struct {
		ns string
		c  *collection.Collection
	}
	var colls []namedColl
	for _, rec := range e.mcat.ListCollections("") {
		if c := e.colls[nsKey(rec.DBName, rec.Name)]; c != nil {
			colls = append(colls, namedColl{ns: rec.DBName + "." + rec.Name, c: c})
		}
	}
	e.mu.Unlock()

	rep := CheckReport{Valid: true}
	if full {
		// A full checksum sweep reads the authoritative page images; fold the WAL
		// into the main file first so resident pages carry a written-back checksum.
		_ = e.pgr.Checkpoint()
	}
	rep.FileProblems = e.pgr.CheckPages(full)
	if len(rep.FileProblems) > 0 {
		rep.Valid = false
	}
	for _, nc := range colls {
		cr := nc.c.Check()
		if !cr.Valid {
			rep.Valid = false
		}
		rep.Collections = append(rep.Collections, CollectionCheck{Namespace: nc.ns, Report: cr})
	}
	return rep
}
