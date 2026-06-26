package doc

import (
	"context"
	"strconv"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/engine"
	"github.com/tamnd/doc/options"
)

// MongoDB error codes the command dispatcher reports (spec 2061 doc 14 §17);
// codeNamespaceNotFound (26) lives in write_errors.go.
const (
	codeFailedToParse   = 9
	codeCommandNotFound = 59
)

// RunCommand executes a raw command document against the database and returns its
// reply as a SingleResult (spec 2061 doc 14 §13.5). The command name is the first
// key; for collection commands its value is the target collection name. This is
// the same surface a driver reaches over the wire, served here in-process against
// the open engine. Unsupported commands return a CommandError with code
// CommandNotFound so a caller can branch on it.
func (d *Database) RunCommand(ctx context.Context, runCommand any, opts ...*options.RunCmdOptions) *SingleResult {
	if err := d.db.check(ctx); err != nil {
		return newSingleResult(nil, err)
	}
	raw, err := toDoc(runCommand)
	if err != nil {
		return newSingleResult(nil, err)
	}
	cmd := bson.Raw(raw)
	elems, err := cmd.Elements()
	if err != nil || len(elems) == 0 {
		return cmdErr(codeFailedToParse, "FailedToParse", "empty command document")
	}
	name := elems[0].Key
	target := ""
	if elems[0].Value.Type == bson.TypeString {
		target = elems[0].Value.StringValue()
	}

	switch name {
	case "ping", "hello", "isMaster", "ismaster":
		return cmdResult(okBuilder().Build())
	case "buildInfo", "buildinfo":
		return cmdResult(d.cmdBuildInfo())
	case "serverStatus":
		return cmdResult(d.cmdServerStatus())
	case "collStats":
		return d.cmdCollStats(target)
	case "dbStats":
		return d.cmdDBStats()
	case "listCollections":
		return d.cmdListCollections(ctx)
	case "listIndexes":
		return d.cmdListIndexes(ctx, target)
	case "create":
		return d.cmdCreate(ctx, target, cmd)
	case "drop":
		return d.cmdDrop(ctx, target)
	case "createIndexes":
		return d.cmdCreateIndexes(ctx, target, cmd)
	case "dropIndexes":
		return d.cmdDropIndexes(ctx, target, cmd)
	case "collMod":
		return d.cmdCollMod(target, cmd)
	case "count":
		return d.cmdCount(ctx, target, cmd)
	case "distinct":
		return d.cmdDistinct(ctx, target, cmd)
	case "getParameter":
		return d.cmdGetParameter(cmd)
	case "setParameter":
		return d.cmdSetParameter(cmd)
	case "profile", "setProfilingLevel":
		return d.cmdProfile(cmd)
	default:
		return cmdErr(codeCommandNotFound, "CommandNotFound", "no such command: "+name)
	}
}

// okBuilder returns a builder seeded with the standard {ok: 1.0} success field.
func okBuilder() *bson.Builder { return bson.NewBuilder().AppendDouble("ok", 1) }

func cmdResult(raw bson.Raw) *SingleResult { return newSingleResult(raw, nil) }

func cmdErr(code int32, name, msg string) *SingleResult {
	return newSingleResult(nil, CommandError{Code: code, Name: name, Message: msg})
}

// int64Field reads a numeric command field as an int64, accepting int32, int64,
// or double. The bool reports whether the field was present and numeric.
func int64Field(cmd bson.Raw, key string) (int64, bool) {
	v, ok := cmd.Lookup(key)
	if !ok {
		return 0, false
	}
	f, ok := v.AsFloat64()
	return int64(f), ok
}

// cmdProfile reads or sets the profiler level, the MongoDB profile and
// setProfilingLevel command (spec 2061 doc 18 §3.4). A level of -1 reads the
// current level without changing it; 0, 1, or 2 set it. The reply carries the
// previous level in the "was" field, matching MongoDB.
func (d *Database) cmdProfile(cmd bson.Raw) *SingleResult {
	prev := int32(d.db.prof.Level())
	level, ok := int64Field(cmd, "profile")
	if !ok {
		if level, ok = int64Field(cmd, "setProfilingLevel"); !ok {
			return cmdErr(codeFailedToParse, "FailedToParse", "profile wants a numeric level")
		}
	}
	if level != -1 {
		if level < 0 || level > 2 {
			return cmdErr(codeBadValue, "BadValue", "profiling level must be 0, 1, or 2")
		}
		d.db.prof.SetLevel(int(level))
	}
	b := okBuilder().AppendInt32("was", prev)
	if ms := d.db.prof.slowThresh.Milliseconds(); ms > 0 {
		b.AppendInt64("slowms", ms)
	}
	return cmdResult(b.Build())
}

func (d *Database) cmdBuildInfo() bson.Raw {
	return okBuilder().
		AppendString("version", Version).
		AppendString("gitVersion", "").
		AppendBoolean("debug", false).
		AppendInt32("maxBsonObjectSize", 16*1024*1024).
		Build()
}

func (d *Database) cmdServerStatus() bson.Raw {
	d.db.refreshMetrics()
	m, _ := d.db.Metrics(context.Background())
	if m == nil {
		m = &MetricsSnapshot{}
	}
	// The doc sub-document carries the doc_* metric catalogue in camelCase, so a
	// MongoDB monitoring tool scraping serverStatus sees the same numbers as the
	// Prometheus endpoint (spec 2061 doc 18 §2.4).
	docSub := bson.NewBuilder().
		AppendInt64("opsTotal", m.OpsTotal).
		AppendInt64("slowQueryTotal", m.SlowQueryTotal).
		AppendInt64("pageReads", m.PageReads).
		AppendInt64("pageWrites", m.PageWrites).
		AppendInt64("bytesRead", m.BytesRead).
		AppendInt64("bytesWritten", m.BytesWritten).
		AppendInt64("cacheHits", m.CacheHits).
		AppendInt64("cacheMisses", m.CacheMisses).
		AppendInt64("cacheEvictions", m.CacheEvictions).
		AppendInt64("walFramesTotal", m.WALFramesTotal).
		AppendInt64("walSizePages", m.WALSizePages).
		AppendInt64("checkpointTotal", m.Checkpoints).
		AppendInt64("fsyncTotal", m.Fsyncs).
		AppendInt64("fsyncErrorsTotal", m.FsyncErrors).
		AppendInt64("fileSizeBytes", m.FileSizeBytes).
		AppendInt64("freelistPages", m.FreelistPages).
		AppendInt64("collectionCount", m.Collections).
		AppendInt64("indexCount", m.Indexes).
		AppendInt64("documentCount", m.DocumentCount).
		Build()
	opcounters := bson.NewBuilder().
		AppendInt64("query", m.OpsTotal).
		Build()
	storage := bson.NewBuilder().
		AppendString("name", "doc").
		AppendBoolean("persistent", true).
		Build()
	return okBuilder().
		AppendString("host", "").
		AppendString("version", Version).
		AppendString("process", "doc").
		AppendDocument("storageEngine", storage).
		AppendDocument("opcounters", opcounters).
		AppendDocument("doc", docSub).
		Build()
}

func (d *Database) cmdCollStats(target string) *SingleResult {
	if target == "" {
		return cmdErr(codeFailedToParse, "FailedToParse", "collStats needs a collection name")
	}
	cs, err := d.db.eng.CollectionStats(d.name, target)
	if err != nil {
		return cmdErr(codeNamespaceNotFound, "NamespaceNotFound", d.name+"."+target+" not found")
	}
	b := okBuilder().
		AppendString("ns", cs.Namespace).
		AppendInt64("count", cs.DocumentCount).
		AppendInt64("size", cs.StorageSize).
		AppendInt64("storageSize", cs.StorageSize).
		AppendInt32("nindexes", int32(len(cs.IndexSizes))).
		AppendInt64("totalIndexSize", cs.IndexSize).
		AppendInt64("totalSize", cs.StorageSize+cs.IndexSize).
		AppendBoolean("capped", cs.Capped)
	if cs.MaxDocuments > 0 {
		b.AppendInt64("max", cs.MaxDocuments)
	}
	ib := bson.NewBuilder()
	for n, sz := range cs.IndexSizes {
		ib.AppendInt64(n, sz)
	}
	b.AppendDocument("indexSizes", ib.Build())
	return cmdResult(b.Build())
}

func (d *Database) cmdDBStats() *SingleResult {
	ds := d.db.eng.DatabaseStats(d.name)
	b := okBuilder().
		AppendString("db", ds.Database).
		AppendInt64("collections", ds.Collections).
		AppendInt64("indexes", ds.Indexes).
		AppendInt64("objects", ds.DocumentCount).
		AppendInt64("dataSize", ds.StorageSize).
		AppendInt64("storageSize", ds.StorageSize).
		AppendInt64("indexSize", ds.IndexSize).
		AppendInt64("totalSize", ds.TotalSize)
	return cmdResult(b.Build())
}

// cursorReply frames a first-batch list the way MongoDB's list commands do:
// {cursor: {id: 0, ns: ns, firstBatch: [...]}, ok: 1}. The cursor id is zero
// because the whole result is returned in one batch.
func cursorReply(ns string, docs []bson.Raw) bson.Raw {
	ab := bson.NewBuilder()
	for i, doc := range docs {
		ab.AppendDocument(strconv.Itoa(i), doc)
	}
	cur := bson.NewBuilder().
		AppendInt64("id", 0).
		AppendString("ns", ns).
		AppendArray("firstBatch", ab.Build()).
		Build()
	return okBuilder().AppendDocument("cursor", cur).Build()
}

func (d *Database) cmdListCollections(ctx context.Context) *SingleResult {
	names, err := d.ListCollectionNames(ctx, nil)
	if err != nil {
		return newSingleResult(nil, err)
	}
	docs := make([]bson.Raw, 0, len(names))
	for _, n := range names {
		docs = append(docs, bson.NewBuilder().
			AppendString("name", n).
			AppendString("type", "collection").
			Build())
	}
	return cmdResult(cursorReply(d.name+".$cmd.listCollections", docs))
}

func (d *Database) cmdListIndexes(ctx context.Context, target string) *SingleResult {
	if target == "" {
		return cmdErr(codeFailedToParse, "FailedToParse", "listIndexes needs a collection name")
	}
	specs, err := d.Collection(target).Indexes().ListSpecifications(ctx)
	if err != nil {
		return newSingleResult(nil, err)
	}
	if specs == nil {
		return cmdErr(codeNamespaceNotFound, "NamespaceNotFound", d.name+"."+target+" not found")
	}
	docs := make([]bson.Raw, 0, len(specs))
	for _, s := range specs {
		b := bson.NewBuilder().
			AppendInt32("v", 2).
			AppendDocument("key", bson.Raw(s.KeysDocument)).
			AppendString("name", s.Name)
		if s.Unique {
			b.AppendBoolean("unique", true)
		}
		if s.Sparse {
			b.AppendBoolean("sparse", true)
		}
		if s.ExpireAfterSeconds != nil {
			b.AppendInt32("expireAfterSeconds", *s.ExpireAfterSeconds)
		}
		docs = append(docs, b.Build())
	}
	return cmdResult(cursorReply(d.name+"."+target, docs))
}

func (d *Database) cmdCreate(ctx context.Context, target string, cmd bson.Raw) *SingleResult {
	if target == "" {
		return cmdErr(codeFailedToParse, "FailedToParse", "create needs a collection name")
	}
	o := options.CreateCollection()
	if v, ok := cmd.Lookup("capped"); ok && v.Type == bson.TypeBoolean {
		o.SetCapped(v.Boolean())
	}
	if n, ok := int64Field(cmd, "size"); ok {
		o.SetSizeInBytes(n)
	}
	if n, ok := int64Field(cmd, "max"); ok {
		o.SetMaxDocuments(n)
	}
	hasValidator := false
	hasLevel := false
	if v, ok := cmd.Lookup("validator"); ok && v.Type == bson.TypeDocument {
		o.SetValidator(v.Document())
		hasValidator = true
	}
	if v, ok := cmd.Lookup("validationLevel"); ok && v.Type == bson.TypeString {
		o.SetValidationLevel(v.StringValue())
		hasLevel = true
	}
	if v, ok := cmd.Lookup("validationAction"); ok && v.Type == bson.TypeString {
		o.SetValidationAction(v.StringValue())
	}
	if hasValidator && !hasLevel {
		o.SetValidationLevel("strict")
	}
	// storageEngine.columnarStore / columnarFields turn on the columnar projection
	// store at create time (spec 2061 doc 09 §6, doc 04 §10).
	if v, ok := cmd.Lookup("storageEngine"); ok && v.Type == bson.TypeDocument {
		se := v.Document()
		if sv, ok := se.Lookup("columnarStore"); ok && sv.Type == bson.TypeString {
			o.SetColumnarStore(sv.StringValue())
		}
		if fv, ok := se.Lookup("columnarFields"); ok && fv.Type == bson.TypeArray {
			els, _ := fv.Document().Elements()
			fields := make([]string, 0, len(els))
			for _, e := range els {
				if e.Value.Type == bson.TypeString {
					fields = append(fields, e.Value.StringValue())
				}
			}
			o.SetColumnarFields(fields)
		}
	}
	if err := d.CreateCollection(ctx, target, o); err != nil {
		return newSingleResult(nil, err)
	}
	return cmdResult(okBuilder().Build())
}

func (d *Database) cmdDrop(ctx context.Context, target string) *SingleResult {
	if target == "" {
		return cmdErr(codeFailedToParse, "FailedToParse", "drop needs a collection name")
	}
	if err := d.Collection(target).Drop(ctx); err != nil {
		return newSingleResult(nil, err)
	}
	return cmdResult(okBuilder().AppendString("ns", d.name+"."+target).Build())
}

func (d *Database) cmdCreateIndexes(ctx context.Context, target string, cmd bson.Raw) *SingleResult {
	if target == "" {
		return cmdErr(codeFailedToParse, "FailedToParse", "createIndexes needs a collection name")
	}
	v, ok := cmd.Lookup("indexes")
	if !ok || v.Type != bson.TypeArray {
		return cmdErr(codeFailedToParse, "FailedToParse", "createIndexes needs an indexes array")
	}
	elems, _ := v.Document().Elements()
	models := make([]IndexModel, 0, len(elems))
	for _, e := range elems {
		if e.Value.Type != bson.TypeDocument {
			continue
		}
		spec := e.Value.Document()
		key, ok := spec.Lookup("key")
		if !ok || key.Type != bson.TypeDocument {
			return cmdErr(codeFailedToParse, "FailedToParse", "index spec needs a key document")
		}
		io := options.Index()
		if nv, ok := spec.Lookup("name"); ok && nv.Type == bson.TypeString {
			io.SetName(nv.StringValue())
		}
		if uv, ok := spec.Lookup("unique"); ok && uv.Type == bson.TypeBoolean {
			io.SetUnique(uv.Boolean())
		}
		if sv, ok := spec.Lookup("sparse"); ok && sv.Type == bson.TypeBoolean {
			io.SetSparse(sv.Boolean())
		}
		if ev, ok := int64Field(spec, "expireAfterSeconds"); ok {
			io.SetExpireAfterSeconds(int32(ev))
		}
		if pf, ok := spec.Lookup("partialFilterExpression"); ok && pf.Type == bson.TypeDocument {
			io.SetPartialFilterExpression(pf.Document())
		}
		models = append(models, IndexModel{Keys: key.Document(), Options: io})
	}
	before := 0
	if specs, err := d.Collection(target).Indexes().ListSpecifications(ctx); err == nil {
		before = len(specs)
	}
	names, err := d.Collection(target).Indexes().CreateMany(ctx, models)
	if err != nil {
		return newSingleResult(nil, err)
	}
	return cmdResult(okBuilder().
		AppendInt32("numIndexesBefore", int32(before)).
		AppendInt32("numIndexesAfter", int32(before+len(names))).
		AppendBoolean("createdCollectionAutomatically", false).
		Build())
}

func (d *Database) cmdDropIndexes(ctx context.Context, target string, cmd bson.Raw) *SingleResult {
	if target == "" {
		return cmdErr(codeFailedToParse, "FailedToParse", "dropIndexes needs a collection name")
	}
	v, ok := cmd.Lookup("index")
	if !ok {
		return cmdErr(codeFailedToParse, "FailedToParse", "dropIndexes needs an index name")
	}
	iv := d.Collection(target).Indexes()
	if v.Type == bson.TypeString && v.StringValue() == "*" {
		if _, err := iv.DropAll(ctx); err != nil {
			return newSingleResult(nil, err)
		}
		return cmdResult(okBuilder().Build())
	}
	if v.Type != bson.TypeString {
		return cmdErr(codeFailedToParse, "FailedToParse", "dropIndexes index must be a name or \"*\"")
	}
	if _, err := iv.DropOne(ctx, v.StringValue()); err != nil {
		return newSingleResult(nil, err)
	}
	return cmdResult(okBuilder().Build())
}

func (d *Database) cmdCollMod(target string, cmd bson.Raw) *SingleResult {
	if target == "" {
		return cmdErr(codeFailedToParse, "FailedToParse", "collMod needs a collection name")
	}
	var spec engineCollMod
	if v, ok := cmd.Lookup("validator"); ok && v.Type == bson.TypeDocument {
		spec.setValidator = true
		spec.validator = v.Document()
	}
	if v, ok := cmd.Lookup("validationLevel"); ok && v.Type == bson.TypeString {
		lvl := validationLevel(v.StringValue())
		spec.level = &lvl
	}
	if v, ok := cmd.Lookup("validationAction"); ok && v.Type == bson.TypeString {
		act := validationAction(v.StringValue())
		spec.action = &act
	}
	if v, ok := cmd.Lookup("index"); ok && v.Type == bson.TypeDocument {
		idx := v.Document()
		if nv, ok := idx.Lookup("name"); ok && nv.Type == bson.TypeString {
			spec.indexName = nv.StringValue()
		}
		if ev, ok := int64Field(idx, "expireAfterSeconds"); ok {
			spec.expireAfterSeconds = &ev
		}
	}
	// MongoDB defaults validationLevel to strict when a validator is attached and
	// no level is given, so the new validator actually applies.
	if spec.setValidator && len(spec.validator) > 0 && spec.level == nil {
		lvl := catalog.ValidationStrict
		spec.level = &lvl
	}
	if err := d.collMod(target, spec); err != nil {
		return newSingleResult(nil, err)
	}
	return cmdResult(okBuilder().Build())
}

func (d *Database) cmdCount(ctx context.Context, target string, cmd bson.Raw) *SingleResult {
	if target == "" {
		return cmdErr(codeFailedToParse, "FailedToParse", "count needs a collection name")
	}
	var filter any
	if v, ok := cmd.Lookup("query"); ok && v.Type == bson.TypeDocument {
		filter = v.Document()
	}
	n, err := d.Collection(target).CountDocuments(ctx, filter)
	if err != nil {
		return newSingleResult(nil, err)
	}
	return cmdResult(okBuilder().AppendInt64("n", n).Build())
}

func (d *Database) cmdDistinct(ctx context.Context, target string, cmd bson.Raw) *SingleResult {
	if target == "" {
		return cmdErr(codeFailedToParse, "FailedToParse", "distinct needs a collection name")
	}
	kv, ok := cmd.Lookup("key")
	if !ok || kv.Type != bson.TypeString {
		return cmdErr(codeFailedToParse, "FailedToParse", "distinct needs a key string")
	}
	var filter any
	if v, ok := cmd.Lookup("query"); ok && v.Type == bson.TypeDocument {
		filter = v.Document()
	}
	vals, err := d.Collection(target).Distinct(ctx, kv.StringValue(), filter)
	if err != nil {
		return newSingleResult(nil, err)
	}
	rvs := make([]bson.RawValue, 0, len(vals))
	for _, val := range vals {
		ty, data, merr := MarshalValue(val)
		if merr != nil {
			return newSingleResult(nil, merr)
		}
		rvs = append(rvs, bson.RawValue{Type: ty, Data: data})
	}
	return cmdResult(okBuilder().AppendArray("values", bson.BuildArray(rvs...)).Build())
}

// cmdGetParameter reads engine PRAGMAs through the command surface (spec 2061 doc 19
// §20). It mirrors MongoDB's getParameter: {getParameter: 1, <name>: 1} returns the
// named parameters, while {getParameter: "*"} returns every catalogued one. Each
// requested name maps onto DB.Pragma; an unknown name is reported rather than
// silently dropped so a caller can tell a typo from a real value.
func (d *Database) cmdGetParameter(cmd bson.Raw) *SingleResult {
	elems, _ := cmd.Elements()
	all := len(elems) > 0 && elems[0].Value.Type == bson.TypeString && elems[0].Value.StringValue() == "*"
	var names []string
	if all {
		names = PragmaNames()
	} else {
		for _, e := range elems[1:] {
			if e.Key == "*" {
				names = PragmaNames()
				break
			}
			names = append(names, e.Key)
		}
	}
	if len(names) == 0 {
		return cmdErr(codeFailedToParse, "FailedToParse", "getParameter needs at least one parameter name")
	}
	b := okBuilder()
	for _, n := range names {
		val, err := d.db.Pragma(n, "")
		if err != nil {
			return cmdErr(codeFailedToParse, "FailedToParse", err.Error())
		}
		b.AppendString(n, val)
	}
	return cmdResult(b.Build())
}

// cmdSetParameter writes engine PRAGMAs through the command surface (spec 2061 doc 19
// §20). It mirrors MongoDB's setParameter: every field after the command name is a
// parameter to set, and the reply carries each prior value under its name. A field
// that names a read-only or unknown PRAGMA fails the whole command, so a partial
// apply never goes unreported.
func (d *Database) cmdSetParameter(cmd bson.Raw) *SingleResult {
	elems, _ := cmd.Elements()
	if len(elems) < 2 {
		return cmdErr(codeFailedToParse, "FailedToParse", "setParameter needs at least one parameter to set")
	}
	b := okBuilder()
	for _, e := range elems[1:] {
		prev, err := d.db.Pragma(e.Key, "")
		if err != nil {
			return cmdErr(codeFailedToParse, "FailedToParse", err.Error())
		}
		if _, err := d.db.Pragma(e.Key, rawValueToString(e.Value)); err != nil {
			return cmdErr(codeFailedToParse, "FailedToParse", err.Error())
		}
		b.AppendString(e.Key, prev)
	}
	return cmdResult(b.Build())
}

// rawValueToString renders a command argument as the string DB.Pragma consumes. The
// PRAGMA values are short scalars (an enum word, a number, a bool), so a string,
// number, or boolean argument all collapse to their textual form; anything else is
// passed through empty and rejected by the PRAGMA writer.
func rawValueToString(v bson.RawValue) string {
	switch v.Type {
	case bson.TypeString:
		return v.StringValue()
	case bson.TypeBoolean:
		return strconv.FormatBool(v.Boolean())
	case bson.TypeInt32, bson.TypeInt64, bson.TypeDouble:
		if f, ok := v.AsFloat64(); ok {
			return strconv.FormatInt(int64(f), 10)
		}
	}
	return ""
}

// engineCollMod is the public layer's gathered collMod request before it is folded
// into the engine's CollModSpec.
type engineCollMod struct {
	setValidator       bool
	validator          bson.Raw
	level              *catalog.ValidationLevel
	action             *catalog.ValidationAction
	indexName          string
	expireAfterSeconds *int64
}

// collMod drives a collection modification through the engine (spec 2061 doc 09
// §8.7). It exists on Database so both RunCommand and any future helper share one
// path.
func (d *Database) collMod(target string, spec engineCollMod) error {
	return mapEngineErr(d.db.eng.CollMod(d.name, target, engine.CollModSpec{
		SetValidator:       spec.setValidator,
		Validator:          spec.validator,
		ValidationLevel:    spec.level,
		ValidationAction:   spec.action,
		IndexName:          spec.indexName,
		ExpireAfterSeconds: spec.expireAfterSeconds,
	}))
}
