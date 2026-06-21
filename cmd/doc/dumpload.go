package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/extjson"
	"github.com/tamnd/doc/options"
)

// collMetadata is the per-collection sidecar a dump writes next to the BSON stream. It
// holds enough to recreate the collection and its secondary indexes on load (spec 2061
// doc 15 §8.2). It is plain JSON so a person can read it.
type collMetadata struct {
	Name    string          `json:"name"`
	Capped  bool            `json:"capped,omitempty"`
	MaxDocs int64           `json:"maxDocuments,omitempty"`
	Indexes []indexMetadata `json:"indexes"`
}

// indexMetadata describes one index in a dump sidecar.
type indexMetadata struct {
	Name               string          `json:"name"`
	Key                json.RawMessage `json:"key"`
	Unique             bool            `json:"unique,omitempty"`
	Sparse             bool            `json:"sparse,omitempty"`
	ExpireAfterSeconds *int32          `json:"expireAfterSeconds,omitempty"`
}

// dotDump writes a logical dump of the active database to a directory: one BSON stream
// and one metadata sidecar per collection (spec 2061 doc 15 §8.2). A target of "-"
// streams every collection as JSONL to stdout instead.
func (a *app) dotDump(args []string) error {
	fs := parseFlags(args)
	dbName := a.dbName
	if v := fs.values["db"]; v != "" {
		dbName = v
	}
	db := a.db.Database(dbName)

	colls, err := a.dumpCollections(db, fs.values["collection"])
	if err != nil {
		return classify(err)
	}

	if len(fs.positional) > 0 && fs.positional[0] == "-" {
		return a.dumpStdout(db, colls)
	}
	dir := "dump"
	if len(fs.positional) > 0 {
		dir = fs.positional[0]
	}
	outDir := filepath.Join(dir, dbName)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return cliError{code: exitIOError, msg: err.Error()}
	}
	for _, cn := range colls {
		if err := a.dumpCollection(db, cn, outDir, !fs.bools["skip-indexes"]); err != nil {
			return classify(err)
		}
	}
	return a.rend.writeText(fmt.Sprintf("dumped %d collections from %s to %s", len(colls), dbName, outDir))
}

// dumpCollections resolves the collection list for a dump: a single named one, or all
// of them sorted.
func (a *app) dumpCollections(db *doc.Database, only string) ([]string, error) {
	if only != "" {
		return []string{only}, nil
	}
	names, err := db.ListCollectionNames(a.ctx(), bson.NewBuilder().Build())
	if err != nil {
		return nil, err
	}
	return names, nil
}

// dumpStdout streams every named collection to stdout as JSONL, one document per line.
func (a *app) dumpStdout(db *doc.Database, colls []string) error {
	for _, cn := range colls {
		cur, err := db.Collection(cn).Find(a.ctx(), bson.NewBuilder().Build())
		if err != nil {
			return classify(err)
		}
		_, err = writeExport(os.Stdout, cur, a.ctx(), "jsonl", nil, false, false)
		_ = cur.Close(a.ctx())
		if err != nil {
			return classify(err)
		}
	}
	return nil
}

// dumpCollection writes one collection's BSON stream and, optionally, its index
// metadata sidecar.
func (a *app) dumpCollection(db *doc.Database, name, outDir string, withIndexes bool) error {
	bsonPath := filepath.Join(outDir, name+".bson")
	f, err := os.Create(bsonPath)
	if err != nil {
		return cliError{code: exitIOError, msg: err.Error()}
	}
	cur, err := db.Collection(name).Find(a.ctx(), bson.NewBuilder().Build())
	if err != nil {
		_ = f.Close()
		return err
	}
	_, werr := writeExport(f, cur, a.ctx(), "bson", nil, false, false)
	_ = cur.Close(a.ctx())
	if cerr := f.Close(); cerr != nil && werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}

	meta := collMetadata{Name: name}
	if st, err := db.Collection(name).Stats(a.ctx()); err == nil {
		meta.Capped = st.Capped
		meta.MaxDocs = st.MaxDocuments
	}
	if withIndexes {
		specs, err := db.Collection(name).Indexes().ListSpecifications(a.ctx())
		if err != nil {
			return err
		}
		for _, s := range specs {
			if s.Name == "_id_" {
				continue
			}
			keyJSON, err := extjson.MarshalRelaxed(s.KeysDocument)
			if err != nil {
				return err
			}
			meta.Indexes = append(meta.Indexes, indexMetadata{
				Name:               s.Name,
				Key:                keyJSON,
				Unique:             s.Unique,
				Sparse:             s.Sparse,
				ExpireAfterSeconds: s.ExpireAfterSeconds,
			})
		}
	}
	return writeMetadata(filepath.Join(outDir, name+".metadata.json"), meta)
}

// writeMetadata serializes a collection sidecar as indented JSON.
func writeMetadata(path string, meta collMetadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return cliError{code: exitIOError, msg: err.Error()}
	}
	return nil
}

// dotLoad reads a logical dump directory back into the database (spec 2061 doc 15 §8.3).
// Each database subdirectory holds one BSON stream per collection; the matching sidecar
// recreates the indexes unless --no-indexes is given.
func (a *app) dotLoad(args []string) error {
	fs := parseFlags(args)
	if len(fs.positional) < 1 {
		return usageErr(".load <dir> [--db <name>] [--drop] [--no-indexes]")
	}
	root := fs.positional[0]
	dbDirs, err := dumpDatabaseDirs(root)
	if err != nil {
		return cliError{code: exitIOError, msg: err.Error()}
	}
	loaded := 0
	for _, dd := range dbDirs {
		targetDB := dd.name
		if v := fs.values["db"]; v != "" {
			targetDB = v
		}
		n, err := a.loadDatabaseDir(dd.path, targetDB, fs)
		if err != nil {
			return classify(err)
		}
		loaded += n
	}
	return a.rend.writeText(fmt.Sprintf("loaded %d collections", loaded))
}

type dumpDir struct {
	name string
	path string
}

// dumpDatabaseDirs lists the per-database subdirectories of a dump root. A root that
// directly holds .bson files (a single-database dump pointed at without its parent) is
// treated as one unnamed database.
func dumpDatabaseDirs(root string) ([]dumpDir, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []dumpDir
	hasBSON := false
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, dumpDir{name: e.Name(), path: filepath.Join(root, e.Name())})
		} else if filepath.Ext(e.Name()) == ".bson" {
			hasBSON = true
		}
	}
	if hasBSON {
		dirs = append(dirs, dumpDir{name: filepath.Base(root), path: root})
	}
	return dirs, nil
}

// loadDatabaseDir loads every <coll>.bson file in dir into targetDB and returns the
// number of collections loaded.
func (a *app) loadDatabaseDir(dir, targetDB string, fs flagSet) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	db := a.db.Database(targetDB)
	count := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".bson" {
			continue
		}
		coll := e.Name()[:len(e.Name())-len(".bson")]
		if err := a.loadOneCollection(db, dir, coll, fs); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// loadOneCollection loads a single BSON file and recreates its indexes from the sidecar.
func (a *app) loadOneCollection(db *doc.Database, dir, coll string, fs flagSet) error {
	c := db.Collection(coll)
	if fs.bools["drop"] {
		if err := c.Drop(a.ctx()); err != nil {
			return err
		}
	}
	f, err := os.Open(filepath.Join(dir, coll+".bson"))
	if err != nil {
		return cliError{code: exitIOError, msg: err.Error()}
	}
	defer func() { _ = f.Close() }()
	if _, err := c.LoadBSON(a.ctx(), f); err != nil {
		return err
	}
	if fs.bools["no-indexes"] {
		return nil
	}
	return a.loadIndexes(c, dir, coll)
}

// loadIndexes recreates a collection's secondary indexes from its metadata sidecar, if
// the sidecar exists. A missing sidecar is not an error: a documents-only dump loads
// the data and skips index recreation.
func (a *app) loadIndexes(c *doc.Collection, dir, coll string) error {
	data, err := os.ReadFile(filepath.Join(dir, coll+".metadata.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return cliError{code: exitIOError, msg: err.Error()}
	}
	var meta collMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return queryError("bad metadata for " + coll + ": " + err.Error())
	}
	for _, im := range meta.Indexes {
		keys, err := extjson.Parse(im.Key)
		if err != nil {
			return queryError("bad index key for " + im.Name + ": " + err.Error())
		}
		io := options.Index().SetName(im.Name)
		if im.Unique {
			io.SetUnique(true)
		}
		if im.Sparse {
			io.SetSparse(true)
		}
		if im.ExpireAfterSeconds != nil {
			io.SetExpireAfterSeconds(*im.ExpireAfterSeconds)
		}
		if _, err := c.Indexes().CreateOne(a.ctx(), doc.IndexModel{Keys: keys, Options: io}); err != nil {
			return err
		}
	}
	return nil
}
