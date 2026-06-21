package main

import (
	"fmt"
	"strings"
)

// helperCall is a parsed mongosh-style expression: db.<coll>.<method>(args) with an
// optional chain of cursor modifiers such as .sort().skip().limit() (spec 2061 doc 15
// §4.1, §5). The args and each chain argument are kept as their raw JSON source so the
// executor can parse them in the shape each method expects (a document, an array, a
// bare value).
type helperCall struct {
	coll   string
	method string
	args   []string
	chain  []chainCall
}

type chainCall struct {
	name string
	arg  string
}

// parseHelper parses a db.<coll>.<method>(...) expression. It returns ok=false when the
// line is not a db. helper at all (so the caller can try the other input modes), and an
// error when it starts as a helper but is malformed.
func parseHelper(line string) (helperCall, bool, error) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "db.") {
		return helperCall{}, false, nil
	}
	rest := s[len("db."):]

	// Collection name runs up to the dot before the method.
	dot := strings.IndexByte(rest, '.')
	if dot < 0 {
		return helperCall{}, true, fmt.Errorf("expected db.<collection>.<method>(...)")
	}
	hc := helperCall{coll: rest[:dot]}
	rest = rest[dot+1:]

	method, rest, err := readCall(rest)
	if err != nil {
		return helperCall{}, true, err
	}
	hc.method = method.name
	hc.args = method.args

	// Any remaining .name(args) segments are cursor-chain modifiers.
	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			break
		}
		if rest[0] != '.' {
			return helperCall{}, true, fmt.Errorf("unexpected trailing input: %q", rest)
		}
		seg, more, err := readCall(rest[1:])
		if err != nil {
			return helperCall{}, true, err
		}
		arg := ""
		if len(seg.args) == 1 {
			arg = seg.args[0]
		} else if len(seg.args) > 1 {
			return helperCall{}, true, fmt.Errorf(".%s takes one argument", seg.name)
		}
		hc.chain = append(hc.chain, chainCall{name: seg.name, arg: arg})
		rest = more
	}
	return hc, true, nil
}

type callSeg struct {
	name string
	args []string
}

// readCall reads "name(args)" from the front of s and returns the segment plus the
// unconsumed tail. The collection method or chain method name runs up to '(', and the
// argument list is split on top-level commas inside the balanced parentheses.
func readCall(s string) (callSeg, string, error) {
	paren := strings.IndexByte(s, '(')
	if paren < 0 {
		return callSeg{}, "", fmt.Errorf("expected '(' after method name")
	}
	name := strings.TrimSpace(s[:paren])
	if name == "" {
		return callSeg{}, "", fmt.Errorf("missing method name")
	}
	body, end, err := matchParen(s, paren)
	if err != nil {
		return callSeg{}, "", err
	}
	args, err := splitArgs(body)
	if err != nil {
		return callSeg{}, "", err
	}
	return callSeg{name: name, args: args}, s[end+1:], nil
}

// matchParen returns the text between the '(' at index open and its matching ')', plus
// the index of that ')'. It tracks nesting and skips over string literals so a paren
// inside a JSON string does not throw off the count.
func matchParen(s string, open int) (string, int, error) {
	depth := 0
	inStr := false
	for i := open; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], i, nil
			}
		}
	}
	return "", 0, fmt.Errorf("unbalanced parentheses")
}

// splitArgs splits a call's argument text on the commas that sit at the top level,
// outside any string, object, or array. An empty argument list yields no arguments.
func splitArgs(body string) ([]string, error) {
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}
	var args []string
	depth := 0
	inStr := false
	start := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inStr {
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced brackets in arguments")
	}
	args = append(args, strings.TrimSpace(body[start:]))
	return args, nil
}
