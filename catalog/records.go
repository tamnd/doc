package catalog

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/format"
)

// This file holds the database- and collection-level catalog records that the
// master catalog persists (spec 2061 doc 09 §3, §4). The index catalog record
// (§7) is represented per collection by the existing IndexSpec list in Store, so
// the master catalog tracks only databases and collections; each collection then
// owns its own secondary-index registry keyed by a distinct heap collection id.
// That split keeps a collection's index metadata next to the collection rather
// than in one global tree, which is an internal representation choice with the
// same observable catalog: the set of databases, collections, kinds, options, and
// indexes, recovered intact on reopen.

// CollectionKind enumerates the storage shapes a collection can take (spec 2061
// doc 09 §4.3). M6 ships Regular, Capped, and TTL; Clustered, View, and
// TimeSeries are reserved values that a later milestone fills in, and a record
// carrying one of them round-trips through the codec untouched.
type CollectionKind uint8

const (
	KindRegular    CollectionKind = 0
	KindCapped     CollectionKind = 1
	KindClustered  CollectionKind = 2
	KindTTL        CollectionKind = 3
	KindView       CollectionKind = 4
	KindTimeSeries CollectionKind = 5
)

// ValidationLevel controls which documents a validator is applied to (spec 2061
// doc 09 §10): Off skips validation, Moderate validates inserts and updates to
// already-valid documents, Strict validates every insert and update.
type ValidationLevel uint8

const (
	ValidationOff      ValidationLevel = 0
	ValidationModerate ValidationLevel = 1
	ValidationStrict   ValidationLevel = 2
)

// ValidationAction controls what a failed validation does (spec 2061 doc 09 §10):
// Error rejects the write, Warn admits it and records a warning.
type ValidationAction uint8

const (
	ValidationError ValidationAction = 0
	ValidationWarn  ValidationAction = 1
)

// CollectionOptions holds the kind-specific knobs persisted with a collection
// (spec 2061 doc 09 §6). Only the fields M6 reads are wired through the engine;
// the rest are carried so a record written by a later build that sets them
// survives a round trip through this one.
type CollectionOptions struct {
	// Capped (KindCapped).
	SizeBytes int64
	MaxDocs   int64
	HeadPage  uint32
	TailPage  uint32

	// View (KindView).
	ViewSource   string
	ViewPipeline bson.Raw

	// Storage tuning, all kinds.
	PageFill uint8
}

// CollectionRecord is the catalog entry for one collection (spec 2061 doc 09
// §4.1). It carries everything the engine needs to reopen the collection: its
// namespace and identity, its kind and options, its validator, and the physical
// anchors (the heap collection id that owns its document pages and the persisted
// root of its _id index).
type CollectionRecord struct {
	DBName     string
	Name       string
	UUID       [16]byte
	Kind       CollectionKind
	CreatedAt  int64
	ModifiedAt int64

	// CollID is the heap collection id that tags this collection's document
	// pages; its _id index shares the tag and the secondary-index catalog uses
	// CollID+1 (spec 2061 doc 09 §5.1, mapped onto self-identifying heap pages).
	CollID uint32
	// IDIndexRoot is the persisted B-tree root of the _id index, NullPage until
	// the first document is inserted.
	IDIndexRoot uint32

	Options          CollectionOptions
	Validator        bson.Raw // nil = no validator
	ValidationLevel  ValidationLevel
	ValidationAction ValidationAction

	FormatMinor uint8
}

// SecondaryCollID is the heap collection id a collection's secondary-index
// catalog occupies, derived from the document heap id so the two never collide.
func (r *CollectionRecord) SecondaryCollID() uint32 { return r.CollID + 1 }

// DatabaseRecord is the catalog entry for one database (spec 2061 doc 09 §3.2).
type DatabaseRecord struct {
	Name        string
	UUID        [16]byte
	CreatedAt   int64
	FormatMinor uint8
}

// ---- BSON codec ----------------------------------------------------------

func encodeUUID(b *bson.Builder, key string, u [16]byte) {
	b.AppendBinary(key, 0, u[:])
}

func decodeUUID(d bson.Raw, key string) [16]byte {
	var u [16]byte
	if v, ok := d.Lookup(key); ok && v.Type == bson.TypeBinary {
		if _, data, ok := v.Binary(); ok {
			copy(u[:], data)
		}
	}
	return u
}

// encodeDatabase serializes a database record to a BSON document.
func encodeDatabase(r *DatabaseRecord) bson.Raw {
	b := bson.NewBuilder()
	b.AppendString("name", r.Name)
	encodeUUID(b, "uuid", r.UUID)
	b.AppendInt64("createdAt", r.CreatedAt)
	b.AppendInt32("formatMinor", int32(r.FormatMinor))
	return b.Build()
}

func decodeDatabase(d bson.Raw) *DatabaseRecord {
	r := &DatabaseRecord{}
	if v, ok := d.Lookup("name"); ok {
		r.Name = v.StringValue()
	}
	r.UUID = decodeUUID(d, "uuid")
	if v, ok := d.Lookup("createdAt"); ok {
		r.CreatedAt = v.Int64()
	}
	if v, ok := d.Lookup("formatMinor"); ok {
		r.FormatMinor = uint8(v.Int32())
	}
	return r
}

// encodeCollection serializes a collection record to a BSON document.
func encodeCollection(r *CollectionRecord) bson.Raw {
	b := bson.NewBuilder()
	b.AppendString("db", r.DBName)
	b.AppendString("name", r.Name)
	encodeUUID(b, "uuid", r.UUID)
	b.AppendInt32("kind", int32(r.Kind))
	b.AppendInt64("createdAt", r.CreatedAt)
	b.AppendInt64("modifiedAt", r.ModifiedAt)
	b.AppendInt64("collID", int64(r.CollID))
	b.AppendInt64("idIndexRoot", int64(r.IDIndexRoot))
	b.AppendInt32("validationLevel", int32(r.ValidationLevel))
	b.AppendInt32("validationAction", int32(r.ValidationAction))
	if r.Validator != nil {
		b.AppendDocument("validator", r.Validator)
	}
	b.AppendInt32("formatMinor", int32(r.FormatMinor))

	ob := bson.NewBuilder()
	ob.AppendInt64("sizeBytes", r.Options.SizeBytes)
	ob.AppendInt64("maxDocs", r.Options.MaxDocs)
	ob.AppendInt64("headPage", int64(r.Options.HeadPage))
	ob.AppendInt64("tailPage", int64(r.Options.TailPage))
	if r.Options.ViewSource != "" {
		ob.AppendString("viewSource", r.Options.ViewSource)
	}
	if r.Options.ViewPipeline != nil {
		ob.AppendArray("viewPipeline", r.Options.ViewPipeline)
	}
	ob.AppendInt32("pageFill", int32(r.Options.PageFill))
	b.AppendDocument("options", ob.Build())

	return b.Build()
}

func decodeCollection(d bson.Raw) *CollectionRecord {
	r := &CollectionRecord{IDIndexRoot: format.NullPage}
	if v, ok := d.Lookup("db"); ok {
		r.DBName = v.StringValue()
	}
	if v, ok := d.Lookup("name"); ok {
		r.Name = v.StringValue()
	}
	r.UUID = decodeUUID(d, "uuid")
	if v, ok := d.Lookup("kind"); ok {
		r.Kind = CollectionKind(v.Int32())
	}
	if v, ok := d.Lookup("createdAt"); ok {
		r.CreatedAt = v.Int64()
	}
	if v, ok := d.Lookup("modifiedAt"); ok {
		r.ModifiedAt = v.Int64()
	}
	if v, ok := d.Lookup("collID"); ok {
		r.CollID = uint32(v.Int64())
	}
	if v, ok := d.Lookup("idIndexRoot"); ok {
		r.IDIndexRoot = uint32(v.Int64())
	}
	if v, ok := d.Lookup("validationLevel"); ok {
		r.ValidationLevel = ValidationLevel(v.Int32())
	}
	if v, ok := d.Lookup("validationAction"); ok {
		r.ValidationAction = ValidationAction(v.Int32())
	}
	if v, ok := d.Lookup("validator"); ok && v.Type == bson.TypeDocument {
		r.Validator = v.Document().Clone()
	}
	if v, ok := d.Lookup("formatMinor"); ok {
		r.FormatMinor = uint8(v.Int32())
	}
	if v, ok := d.Lookup("options"); ok && v.Type == bson.TypeDocument {
		od := v.Document()
		if ov, ok := od.Lookup("sizeBytes"); ok {
			r.Options.SizeBytes = ov.Int64()
		}
		if ov, ok := od.Lookup("maxDocs"); ok {
			r.Options.MaxDocs = ov.Int64()
		}
		if ov, ok := od.Lookup("headPage"); ok {
			r.Options.HeadPage = uint32(ov.Int64())
		}
		if ov, ok := od.Lookup("tailPage"); ok {
			r.Options.TailPage = uint32(ov.Int64())
		}
		if ov, ok := od.Lookup("viewSource"); ok {
			r.Options.ViewSource = ov.StringValue()
		}
		if ov, ok := od.Lookup("viewPipeline"); ok && ov.Type == bson.TypeArray {
			r.Options.ViewPipeline = ov.Document().Clone()
		}
		if ov, ok := od.Lookup("pageFill"); ok {
			r.Options.PageFill = uint8(ov.Int32())
		}
	}
	return r
}
