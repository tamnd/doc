package agg

import (
	"strings"

	"github.com/tamnd/doc/bson"
)

// projection leaf kinds.
const (
	projNone = iota
	projInclude
	projExclude
	projCompute
)

// projNode is a node in the projection tree: an interior node has children keyed
// by field name, a leaf carries an inclusion flag or a computed expression.
type projNode struct {
	order []string
	kids  map[string]*projNode
	kind  int
	expr  Expr
}

func newProjNode() *projNode {
	return &projNode{kids: map[string]*projNode{}}
}

// child returns the named child, creating it on first use.
func (n *projNode) child(name string) *projNode {
	c, ok := n.kids[name]
	if !ok {
		c = newProjNode()
		n.order = append(n.order, name)
		n.kids[name] = c
	}
	return c
}

// compileProject compiles a $project stage into a projection tree, then a stage.
func compileProject(arg bson.RawValue) (stageSpec, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	root := newProjNode()
	if err := buildProj(root, arg.Document()); err != nil {
		return nil, err
	}
	inclusion, err := projMode(root)
	if err != nil {
		return nil, err
	}
	return &projectStage{root: root, inclusion: inclusion}, nil
}

// buildProj fills the projection tree from a projection document, descending into
// dotted paths and nested sub-documents.
func buildProj(node *projNode, d bson.Raw) error {
	elems, err := d.Elements()
	if err != nil {
		return err
	}
	for _, e := range elems {
		target := node
		parts := strings.Split(e.Key, ".")
		for _, p := range parts[:len(parts)-1] {
			target = target.child(p)
		}
		leaf := target.child(parts[len(parts)-1])
		if err := setProjLeaf(leaf, e.Value); err != nil {
			return err
		}
	}
	return nil
}

// setProjLeaf classifies one projection value: a number or bool is include or
// exclude; a sub-document with a $-key (or any non-document) is a computed
// expression; a plain sub-document is a nested projection.
func setProjLeaf(leaf *projNode, v bson.RawValue) error {
	switch v.Type {
	case bson.TypeBoolean:
		if v.Boolean() {
			leaf.kind = projInclude
		} else {
			leaf.kind = projExclude
		}
		return nil
	case bson.TypeInt32, bson.TypeInt64, bson.TypeDouble:
		// In a projection 0 means exclude and any nonzero number means include,
		// which is the opposite of expression truthiness where 0 is truthy.
		if numIsZero(v) {
			leaf.kind = projExclude
		} else {
			leaf.kind = projInclude
		}
		return nil
	case bson.TypeDocument:
		sub := v.Document()
		subElems, err := sub.Elements()
		if err != nil {
			return err
		}
		if len(subElems) > 0 && strings.HasPrefix(subElems[0].Key, "$") {
			ex, cerr := compileExpr(v)
			if cerr != nil {
				return cerr
			}
			leaf.kind = projCompute
			leaf.expr = ex
			return nil
		}
		return buildProj(leaf, sub)
	default:
		ex, cerr := compileExpr(v)
		if cerr != nil {
			return cerr
		}
		leaf.kind = projCompute
		leaf.expr = ex
		return nil
	}
}

// numIsZero reports a numeric value equal to zero.
func numIsZero(v bson.RawValue) bool {
	_, f, k := numOf(v)
	return k != kindNotNum && f == 0
}

// projMode determines inclusion versus exclusion, ignoring an _id directive, and
// rejects a projection that mixes the two.
func projMode(root *projNode) (bool, error) {
	sawInclude, sawExclude := false, false
	for _, k := range root.order {
		if k == "_id" {
			continue
		}
		switch leafKind(root.kids[k]) {
		case projExclude:
			sawExclude = true
		default:
			sawInclude = true
		}
	}
	if sawInclude && sawExclude {
		return false, ErrBadStage
	}
	return sawInclude || !sawExclude, nil
}

// leafKind reports a node's effective kind: a computed leaf, an explicit
// include/exclude, or include when the node has children (a nested inclusion).
func leafKind(n *projNode) int {
	if n.kind != projNone {
		return n.kind
	}
	if len(n.kids) > 0 {
		return projInclude
	}
	return projNone
}

type projectStage struct {
	root      *projNode
	inclusion bool
}

func (s *projectStage) open(in src, ec *execCtx) src {
	return &projectSrc{in: in, ec: ec, stage: s}
}

type projectSrc struct {
	in    src
	ec    *execCtx
	stage *projectStage
}

func (s *projectSrc) next() (bson.Raw, error) {
	doc, err := s.in.next()
	if err != nil {
		return nil, err
	}
	ctx := docCtx(doc, s.ec)
	if s.stage.inclusion {
		return projectInclude(s.stage.root, doc, ctx, true), nil
	}
	return projectExclude(s.stage.root, doc), nil
}

// projectInclude builds the output for an inclusion projection. atRoot governs the
// _id default: _id is kept unless explicitly excluded.
func projectInclude(node *projNode, doc bson.Raw, ctx *evalCtx, atRoot bool) bson.Raw {
	elems, err := doc.Elements()
	if err != nil {
		elems = nil
	}
	b := bson.NewBuilder()
	emitted := map[string]bool{}
	// _id comes first when present and not excluded.
	if atRoot {
		emitID(b, node, doc, ctx, emitted)
	}
	// Existing fields in input order that the projection keeps.
	for _, e := range elems {
		if atRoot && e.Key == "_id" {
			continue
		}
		kid, ok := node.kids[e.Key]
		if !ok {
			continue
		}
		emitInclude(b, e.Key, kid, e.Value, ctx)
		emitted[e.Key] = true
	}
	// Computed or added fields in projection order.
	for _, k := range node.order {
		if emitted[k] || (atRoot && k == "_id") {
			continue
		}
		kid := node.kids[k]
		if kid.kind == projCompute {
			v := kid.expr.eval(ctx)
			if !isMissing(v) {
				b.AppendValue(k, v)
			}
		} else if len(kid.kids) > 0 {
			// A nested subtree over an absent field still yields its computed leaves.
			sub := projectInclude(kid, bson.NewBuilder().Build(), ctx, false)
			if hasFields(sub) {
				b.AppendDocument(k, sub)
			}
		}
	}
	return b.Build()
}

// emitID applies the _id directive for an inclusion projection.
func emitID(b *bson.Builder, node *projNode, doc bson.Raw, ctx *evalCtx, emitted map[string]bool) {
	idNode, has := node.kids["_id"]
	if has && idNode.kind == projExclude {
		emitted["_id"] = true
		return
	}
	if has && idNode.kind == projCompute {
		v := idNode.expr.eval(ctx)
		if !isMissing(v) {
			b.AppendValue("_id", v)
		}
		emitted["_id"] = true
		return
	}
	if idv, ok := doc.Lookup("_id"); ok {
		if has && len(idNode.kids) > 0 {
			emitInclude(b, "_id", idNode, idv, ctx)
		} else {
			b.AppendValue("_id", idv)
		}
	}
	emitted["_id"] = true
}

// emitInclude writes one field for an inclusion projection, descending subtrees.
func emitInclude(b *bson.Builder, key string, kid *projNode, val bson.RawValue, ctx *evalCtx) {
	switch {
	case kid.kind == projCompute:
		v := kid.expr.eval(ctx)
		if !isMissing(v) {
			b.AppendValue(key, v)
		}
	case kid.kind == projInclude || len(kid.kids) == 0:
		b.AppendValue(key, val)
	case val.Type == bson.TypeDocument:
		b.AppendDocument(key, projectInclude(kid, val.Document(), ctx, false))
	case val.Type == bson.TypeArray:
		b.AppendArray(key, projectArray(kid, val, ctx))
	default:
		// Subtree over a scalar: drop unless the subtree adds computed leaves.
		sub := projectInclude(kid, bson.NewBuilder().Build(), ctx, false)
		if hasFields(sub) {
			b.AppendDocument(key, sub)
		}
	}
}

// projectArray applies a subtree projection to each document element of an array.
func projectArray(kid *projNode, arr bson.RawValue, ctx *evalCtx) bson.Raw {
	elems, err := arrayElements(arr)
	if err != nil {
		return bson.BuildArray()
	}
	out := make([]bson.RawValue, 0, len(elems))
	for _, el := range elems {
		if el.Type == bson.TypeDocument {
			out = append(out, mkDoc(projectInclude(kid, el.Document(), ctx, false)))
		}
	}
	return bson.BuildArray(out...)
}

// projectExclude builds the output for an exclusion projection: every field is
// kept except those the tree marks for exclusion, descending into subtrees.
func projectExclude(node *projNode, doc bson.Raw) bson.Raw {
	elems, err := doc.Elements()
	if err != nil {
		elems = nil
	}
	b := bson.NewBuilder()
	for _, e := range elems {
		kid, ok := node.kids[e.Key]
		if !ok {
			b.AppendValue(e.Key, e.Value)
			continue
		}
		if kid.kind == projExclude {
			continue
		}
		if len(kid.kids) > 0 && e.Value.Type == bson.TypeDocument {
			b.AppendDocument(e.Key, projectExclude(kid, e.Value.Document()))
			continue
		}
		b.AppendValue(e.Key, e.Value)
	}
	return b.Build()
}

// hasFields reports whether a document has at least one element.
func hasFields(d bson.Raw) bool {
	elems, err := d.Elements()
	return err == nil && len(elems) > 0
}
