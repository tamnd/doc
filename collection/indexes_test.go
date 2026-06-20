package collection

import (
	"errors"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/vfs"
)

// entryCount returns the number of live entries in a secondary index, for asserting
// index/heap consistency and multikey fan-out.
func (c *Collection) entryCount(name string) uint64 {
	bt := c.sidx[name]
	if bt == nil {
		return 0
	}
	return bt.Stats().Entries
}

func TestCreateIndexBuildsFromExisting(t *testing.T) {
	c := newTestColl(t)
	for i := int32(1); i <= 5; i++ {
		if _, err := c.InsertOne(docInt(i, "age", i*10)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	name, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "age"}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if name != "age_1" {
		t.Fatalf("default name = %q, want age_1", name)
	}
	if got := c.entryCount(name); got != 5 {
		t.Fatalf("entry count = %d, want 5", got)
	}
}

func TestCreateIndexMaintainsOnWrite(t *testing.T) {
	c := newTestColl(t)
	name, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "age"}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	for i := int32(1); i <= 4; i++ {
		if _, err := c.InsertOne(docInt(i, "age", 30)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if got := c.entryCount(name); got != 4 {
		t.Fatalf("after inserts entry count = %d, want 4", got)
	}
	if _, err := c.DeleteOne(filterID(2)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := c.entryCount(name); got != 3 {
		t.Fatalf("after delete entry count = %d, want 3", got)
	}
}

func TestCreateIndexDuplicateName(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "age"}}}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	_, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "age"}}})
	if !errors.Is(err, catalog.ErrIndexExists) {
		t.Fatalf("err = %v, want ErrIndexExists", err)
	}
}

func TestUniqueSecondaryIndexRejectsDuplicate(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "email"}}, Unique: true}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if _, err := c.InsertOne(docStr(1, "email", "a@x.com")); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	_, err := c.InsertOne(docStr(2, "email", "a@x.com"))
	if !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("dup insert err = %v, want ErrDuplicateKey", err)
	}
	// A different value is fine.
	if _, err := c.InsertOne(docStr(3, "email", "b@x.com")); err != nil {
		t.Fatalf("insert 3: %v", err)
	}
}

func TestUniqueSecondaryBuildRejectsExistingDuplicate(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.InsertOne(docStr(1, "email", "dup@x.com")); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if _, err := c.InsertOne(docStr(2, "email", "dup@x.com")); err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	_, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "email"}}, Unique: true})
	if !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("build err = %v, want ErrDuplicateKey", err)
	}
}

func TestMultikeyExpansion(t *testing.T) {
	c := newTestColl(t)
	// Two docs, one with a 3-element array and one with a 2-element array: 5 entries.
	if _, err := c.InsertOne(docArr(1, "tags", 3)); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if _, err := c.InsertOne(docArr(2, "tags", 2)); err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	name, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "tags"}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if got := c.entryCount(name); got != 5 {
		t.Fatalf("multikey entry count = %d, want 5", got)
	}
	if sp := c.cat.Find(name); sp == nil || !sp.Multikey {
		t.Fatalf("multikey flag not set on spec")
	}
}

func TestSparseIndexOmitsMissing(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.InsertOne(docInt(1, "age", 10)); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if _, err := c.InsertOne(docID(2)); err != nil { // no age field
		t.Fatalf("insert 2: %v", err)
	}
	name, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "age"}}, Sparse: true})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if got := c.entryCount(name); got != 1 {
		t.Fatalf("sparse entry count = %d, want 1", got)
	}
}

func TestIndexPersistsAcrossReopen(t *testing.T) {
	fs := vfs.NewMemFS()
	c, err := Open(fs, "p.doc", Options{IDGen: &sys.FixedIDGenerator{Timestamp: 1}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := int32(1); i <= 3; i++ {
		if _, err := c.InsertOne(docInt(i, "age", i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if _, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "age"}}, Unique: true}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c2, err := Open(fs, "p.doc", Options{IDGen: &sys.FixedIDGenerator{Timestamp: 1}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = c2.Close() }()
	infos := c2.ListIndexes()
	if len(infos) != 2 {
		t.Fatalf("ListIndexes after reopen = %d entries, want 2", len(infos))
	}
	if infos[1].Name != "age_1" || !infos[1].Unique {
		t.Fatalf("recovered spec = %+v, want age_1 unique", infos[1])
	}
	if got := c2.entryCount("age_1"); got != 3 {
		t.Fatalf("recovered entry count = %d, want 3", got)
	}
	// The recovered unique index still enforces uniqueness.
	if _, err := c2.InsertOne(docInt(99, "age", 1)); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("post-reopen dup err = %v, want ErrDuplicateKey", err)
	}
}

func TestDropIndex(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "age"}}}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err := c.DropIndex("age_1"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	if len(c.ListIndexes()) != 1 {
		t.Fatalf("after drop want only _id index")
	}
	if err := c.DropIndex(catalog.IDIndexName); !errors.Is(err, catalog.ErrCannotDropID) {
		t.Fatalf("drop _id err = %v, want ErrCannotDropID", err)
	}
}

// docStr builds {_id: id, field: s}.
func docStr(id int32, field, s string) bson.Raw {
	b := bson.NewBuilder()
	b.AppendInt32("_id", id)
	b.AppendString(field, s)
	return b.Build()
}

// docArr builds {_id: id, field: [0, 1, ... n-1]} as an int32 array.
func docArr(id int32, field string, n int) bson.Raw {
	var elems []bson.RawValue
	for i := range n {
		eb := bson.NewBuilder()
		eb.AppendInt32("v", int32(i))
		v, _ := eb.Build().Lookup("v")
		elems = append(elems, v)
	}
	b := bson.NewBuilder()
	b.AppendInt32("_id", id)
	b.AppendArray(field, bson.BuildArray(elems...))
	return b.Build()
}
