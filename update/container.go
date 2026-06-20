package update

import "github.com/tamnd/doc/bson"

// container is a mutable, lazily-decoded BSON document or array used during an
// update. A document or array is decoded one level into parallel key/node slices;
// untouched values stay verbatim in their node's raw field, so re-encoding an
// unmodified subtree reproduces its original bytes (which is how Apply detects a
// no-op update by byte equality). array marks a container that re-encodes as a
// BSON array rather than a document.
type container struct {
	keys  []string
	nodes []node
	array bool
}

// node is one value in a container: either a verbatim leaf (raw, possibly a whole
// document/array kept undecoded) or a decoded sub-container (sub). Exactly one is
// set; descending into a leaf document/array converts it to a sub.
type node struct {
	raw bson.RawValue
	sub *container
}

// decodeDoc decodes one level of a document into a container, leaving every value
// as a verbatim leaf.
func decodeDoc(d bson.Raw) (*container, error) {
	elems, err := d.Elements()
	if err != nil {
		return nil, err
	}
	c := &container{
		keys:  make([]string, len(elems)),
		nodes: make([]node, len(elems)),
	}
	for i, e := range elems {
		c.keys[i] = e.Key
		c.nodes[i] = node{raw: e.Value}
	}
	return c, nil
}

// encode re-frames the container into a finished BSON document (or array body).
func (c *container) encode() bson.Raw {
	b := bson.NewBuilder()
	for i, k := range c.keys {
		n := c.nodes[i]
		if n.sub != nil {
			body := n.sub.encode()
			if n.sub.array {
				b.AppendArray(k, body)
			} else {
				b.AppendDocument(k, body)
			}
			continue
		}
		b.AppendValue(k, n.raw)
	}
	return b.Build()
}

// find returns the index of key, or -1.
func (c *container) find(key string) int {
	for i, k := range c.keys {
		if k == key {
			return i
		}
	}
	return -1
}

// child returns the sub-container at key, decoding a leaf document/array in place
// on first descent. With create, a missing key becomes a new empty document; an
// existing scalar leaf is a path conflict. ok is false only when create is false
// and the key is absent. An attempt to grow an array via a new path component is a
// path conflict (array growth through a path is deferred to M4).
func (c *container) child(key string, create bool) (sub *container, ok bool, err error) {
	i := c.find(key)
	if i < 0 {
		if !create {
			return nil, false, nil
		}
		if c.array {
			return nil, false, ErrPathConflict
		}
		nc := &container{}
		c.keys = append(c.keys, key)
		c.nodes = append(c.nodes, node{sub: nc})
		return nc, true, nil
	}
	n := &c.nodes[i]
	if n.sub != nil {
		return n.sub, true, nil
	}
	switch n.raw.Type {
	case bson.TypeDocument:
		dec, derr := decodeDoc(n.raw.Document())
		if derr != nil {
			return nil, false, derr
		}
		n.sub = dec
		n.raw = bson.RawValue{}
		return dec, true, nil
	case bson.TypeArray:
		dec, derr := decodeDoc(n.raw.Document())
		if derr != nil {
			return nil, false, derr
		}
		dec.array = true
		n.sub = dec
		n.raw = bson.RawValue{}
		return dec, true, nil
	default:
		return nil, false, ErrPathConflict
	}
}

// setLeaf sets key to a verbatim value, replacing any existing node or appending a
// new field at the end (preserving the order MongoDB keeps for new fields).
func (c *container) setLeaf(key string, v bson.RawValue) {
	if i := c.find(key); i >= 0 {
		c.nodes[i] = node{raw: v}
		return
	}
	c.keys = append(c.keys, key)
	c.nodes = append(c.nodes, node{raw: v})
}

// setNode sets key to a whole node (leaf or sub-container), used to move a value
// for $rename.
func (c *container) setNode(key string, n node) {
	if i := c.find(key); i >= 0 {
		c.nodes[i] = n
		return
	}
	c.keys = append(c.keys, key)
	c.nodes = append(c.nodes, n)
}

// remove deletes key, reporting whether it was present.
func (c *container) remove(key string) bool {
	i := c.find(key)
	if i < 0 {
		return false
	}
	c.keys = append(c.keys[:i], c.keys[i+1:]...)
	c.nodes = append(c.nodes[:i], c.nodes[i+1:]...)
	return true
}

// leafValue returns the value at key as a RawValue, encoding a sub-container back
// to bytes when needed (so comparison operators can compare against any value).
// present is false when the key is absent.
func (c *container) leafValue(key string) (v bson.RawValue, present bool) {
	i := c.find(key)
	if i < 0 {
		return bson.RawValue{}, false
	}
	n := c.nodes[i]
	if n.sub != nil {
		body := n.sub.encode()
		t := bson.TypeDocument
		if n.sub.array {
			t = bson.TypeArray
		}
		return bson.RawValue{Type: t, Data: body}, true
	}
	return n.raw, true
}

// takeNode removes key and returns its node, for $rename.
func (c *container) takeNode(key string) (node, bool) {
	i := c.find(key)
	if i < 0 {
		return node{}, false
	}
	n := c.nodes[i]
	c.keys = append(c.keys[:i], c.keys[i+1:]...)
	c.nodes = append(c.nodes[:i], c.nodes[i+1:]...)
	return n, true
}

// resolve walks path to the container holding its final component, returning that
// parent and the leaf key. With create, intermediate documents are created as
// needed; without create, a missing intermediate yields ok=false (the caller
// treats that as a no-op). When forbidArray is set, traversing an array is an
// error ($rename).
func resolve(root *container, path []string, create, forbidArray bool) (parent *container, leaf string, ok bool, err error) {
	cur := root
	for i := 0; i < len(path)-1; i++ {
		if forbidArray && cur.array {
			return nil, "", false, ErrRenameArray
		}
		next, found, cerr := cur.child(path[i], create)
		if cerr != nil {
			return nil, "", false, cerr
		}
		if !found {
			return nil, "", false, nil
		}
		cur = next
	}
	if forbidArray && cur.array {
		return nil, "", false, ErrRenameArray
	}
	return cur, path[len(path)-1], true, nil
}
