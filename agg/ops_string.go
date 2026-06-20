package agg

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/tamnd/doc/bson"
)

// strOf returns the string view of a value when it is a BSON string.
func strOf(v bson.RawValue) (string, bool) {
	if v.Type == bson.TypeString {
		return v.StringValue(), true
	}
	return "", false
}

// opConcat joins string operands; any nullish operand yields null.
func opConcat(vals []bson.RawValue) bson.RawValue {
	var b strings.Builder
	for _, v := range vals {
		if isNullish(v) {
			return mkNull()
		}
		s, ok := strOf(v)
		if !ok {
			return mkNull()
		}
		b.WriteString(s)
	}
	return mkString(b.String())
}

// caseFn builds $toLower/$toUpper, which map a nullish input to the empty string.
func caseFn(upper bool) opCompiler {
	return eager(1, 1, func(vals []bson.RawValue) bson.RawValue {
		v := vals[0]
		if isNullish(v) {
			return mkString("")
		}
		s, ok := strOf(v)
		if !ok {
			return mkString("")
		}
		if upper {
			return mkString(strings.ToUpper(s))
		}
		return mkString(strings.ToLower(s))
	})
}

// opSubstrBytes returns a byte-range substring; $substr is its alias.
func opSubstrBytes(vals []bson.RawValue) bson.RawValue {
	s, ok := strOf(vals[0])
	if !ok {
		if isNullish(vals[0]) {
			return mkString("")
		}
		return mkString("")
	}
	start, sok := intArg(vals[1])
	length, lok := intArg(vals[2])
	if !sok || !lok {
		return mkString("")
	}
	if start < 0 || start >= len(s) {
		return mkString("")
	}
	end := len(s)
	if length >= 0 && start+length < end {
		end = start + length
	}
	return mkString(s[start:end])
}

// opSubstrCP returns a code-point-range substring.
func opSubstrCP(vals []bson.RawValue) bson.RawValue {
	s, ok := strOf(vals[0])
	if !ok {
		return mkString("")
	}
	start, sok := intArg(vals[1])
	length, lok := intArg(vals[2])
	if !sok || !lok || start < 0 {
		return mkString("")
	}
	runes := []rune(s)
	if start >= len(runes) {
		return mkString("")
	}
	end := len(runes)
	if length >= 0 && start+length < end {
		end = start + length
	}
	return mkString(string(runes[start:end]))
}

// strLen builds $strLenBytes (byte count) or $strLenCP (code-point count).
func strLen(cp bool) opCompiler {
	return eager(1, 1, func(vals []bson.RawValue) bson.RawValue {
		s, ok := strOf(vals[0])
		if !ok {
			return mkNull()
		}
		if cp {
			return mkInt32(int32(utf8.RuneCountInString(s)))
		}
		return mkInt32(int32(len(s)))
	})
}

// opSplit divides a string by a delimiter into an array of substrings.
func opSplit(vals []bson.RawValue) bson.RawValue {
	if isNullish(vals[0]) || isNullish(vals[1]) {
		return mkNull()
	}
	s, sok := strOf(vals[0])
	sep, dok := strOf(vals[1])
	if !sok || !dok {
		return mkNull()
	}
	parts := strings.Split(s, sep)
	out := make([]bson.RawValue, len(parts))
	for i, p := range parts {
		out[i] = mkString(p)
	}
	return mkArray(out)
}

// indexOf builds $indexOfBytes/$indexOfCP, returning the position of a substring
// within an optional [start, end) window, or -1.
func indexOf(cp bool) opCompiler {
	return eager(2, 4, func(vals []bson.RawValue) bson.RawValue {
		if isNullish(vals[0]) {
			return mkNull()
		}
		s, sok := strOf(vals[0])
		sub, subok := strOf(vals[1])
		if !sok || !subok {
			return mkNull()
		}
		start := 0
		if len(vals) >= 3 {
			if v, ok := intArg(vals[2]); ok {
				start = v
			}
		}
		if cp {
			return indexOfCP(s, sub, start, vals)
		}
		return indexOfBytes(s, sub, start, vals)
	})
}

func indexOfBytes(s, sub string, start int, vals []bson.RawValue) bson.RawValue {
	end := len(s)
	if len(vals) == 4 {
		if v, ok := intArg(vals[3]); ok && v < end {
			end = v
		}
	}
	if start < 0 || start > len(s) || start > end {
		return mkInt32(-1)
	}
	idx := strings.Index(s[start:end], sub)
	if idx < 0 {
		return mkInt32(-1)
	}
	return mkInt32(int32(start + idx))
}

func indexOfCP(s, sub string, start int, vals []bson.RawValue) bson.RawValue {
	runes := []rune(s)
	end := len(runes)
	if len(vals) == 4 {
		if v, ok := intArg(vals[3]); ok && v < end {
			end = v
		}
	}
	if start < 0 || start > len(runes) || start > end {
		return mkInt32(-1)
	}
	window := string(runes[start:end])
	idx := strings.Index(window, sub)
	if idx < 0 {
		return mkInt32(-1)
	}
	return mkInt32(int32(start + utf8.RuneCountInString(window[:idx])))
}

// intArg coerces a numeric operand to an int for index/length parameters.
func intArg(v bson.RawValue) (int, bool) {
	i, f, k := numOf(v)
	switch k {
	case kindInt32, kindInt64:
		return int(i), true
	case kindDouble:
		return int(f), true
	default:
		return 0, false
	}
}

// compileTrim builds $trim/$ltrim/$rtrim from a {input, chars} document.
func compileTrim(side int) opCompiler {
	return func(arg bson.RawValue) (Expr, error) {
		if arg.Type != bson.TypeDocument {
			return nil, ErrBadExpr
		}
		d := arg.Document()
		inv, ok := d.Lookup("input")
		if !ok {
			return nil, ErrBadExpr
		}
		ine, err := compileExpr(inv)
		if err != nil {
			return nil, err
		}
		var charsE Expr
		if cv, ok := d.Lookup("chars"); ok {
			charsE, err = compileExpr(cv)
			if err != nil {
				return nil, err
			}
		}
		return trimExpr{input: ine, chars: charsE, side: side}, nil
	}
}

// trim sides.
const (
	trimBoth = iota
	trimLeft
	trimRight
)

type trimExpr struct {
	input Expr
	chars Expr // nil means whitespace
	side  int
}

func (e trimExpr) eval(c *evalCtx) bson.RawValue {
	iv := e.input.eval(c)
	if isNullish(iv) {
		return mkNull()
	}
	s, ok := strOf(iv)
	if !ok {
		return mkNull()
	}
	cutset := " \t\n\r\v\f"
	if e.chars != nil {
		cv := e.chars.eval(c)
		if isNullish(cv) {
			return mkNull()
		}
		cs, cok := strOf(cv)
		if !cok {
			return mkNull()
		}
		cutset = cs
	}
	switch e.side {
	case trimLeft:
		return mkString(strings.TrimLeft(s, cutset))
	case trimRight:
		return mkString(strings.TrimRight(s, cutset))
	default:
		return mkString(strings.Trim(s, cutset))
	}
}

// compileReplace builds $replaceOne/$replaceAll from {input, find, replacement}.
func compileReplace(all bool) opCompiler {
	return func(arg bson.RawValue) (Expr, error) {
		if arg.Type != bson.TypeDocument {
			return nil, ErrBadExpr
		}
		d := arg.Document()
		inv, ok1 := d.Lookup("input")
		fv, ok2 := d.Lookup("find")
		rv, ok3 := d.Lookup("replacement")
		if !ok1 || !ok2 || !ok3 {
			return nil, ErrBadExpr
		}
		ine, err := compileExpr(inv)
		if err != nil {
			return nil, err
		}
		fe, err := compileExpr(fv)
		if err != nil {
			return nil, err
		}
		re, err := compileExpr(rv)
		if err != nil {
			return nil, err
		}
		return replaceExpr{input: ine, find: fe, repl: re, all: all}, nil
	}
}

type replaceExpr struct {
	input, find, repl Expr
	all               bool
}

func (e replaceExpr) eval(c *evalCtx) bson.RawValue {
	iv := e.input.eval(c)
	fv := e.find.eval(c)
	rv := e.repl.eval(c)
	if isNullish(iv) || isNullish(fv) || isNullish(rv) {
		return mkNull()
	}
	s, sok := strOf(iv)
	find, fok := strOf(fv)
	repl, rok := strOf(rv)
	if !sok || !fok || !rok {
		return mkNull()
	}
	if e.all {
		return mkString(strings.ReplaceAll(s, find, repl))
	}
	return mkString(strings.Replace(s, find, repl, 1))
}

// compileRegex builds $regexMatch/$regexFind/$regexFindAll from a
// {input, regex, options} document.
func compileRegex(mode int) opCompiler {
	return func(arg bson.RawValue) (Expr, error) {
		if arg.Type != bson.TypeDocument {
			return nil, ErrBadExpr
		}
		d := arg.Document()
		inv, ok1 := d.Lookup("input")
		rv, ok2 := d.Lookup("regex")
		if !ok1 || !ok2 {
			return nil, ErrBadExpr
		}
		ine, err := compileExpr(inv)
		if err != nil {
			return nil, err
		}
		re, err := compileExpr(rv)
		if err != nil {
			return nil, err
		}
		var optE Expr
		if ov, ok := d.Lookup("options"); ok {
			optE, err = compileExpr(ov)
			if err != nil {
				return nil, err
			}
		}
		return regexExpr{input: ine, regex: re, opts: optE, mode: mode}, nil
	}
}

// regex modes.
const (
	regexMatch = iota
	regexFindOne
	regexFindAll
)

type regexExpr struct {
	input, regex, opts Expr
	mode               int
}

func (e regexExpr) eval(c *evalCtx) bson.RawValue {
	iv := e.input.eval(c)
	if isNullish(iv) {
		if e.mode == regexMatch {
			return mkBool(false)
		}
		if e.mode == regexFindAll {
			return mkArray(nil)
		}
		return mkNull()
	}
	s, sok := strOf(iv)
	if !sok {
		return mkNull()
	}
	re, ok := e.compile(c)
	if !ok {
		return mkNull()
	}
	switch e.mode {
	case regexMatch:
		return mkBool(re.MatchString(s))
	case regexFindAll:
		return regexFindAllResult(re, s)
	default:
		return regexFindResult(re, s)
	}
}

// compile builds the Go regexp from the regex and options operands.
func (e regexExpr) compile(c *evalCtx) (*regexp.Regexp, bool) {
	rv := e.regex.eval(c)
	pattern, pok := strOf(rv)
	if !pok {
		return nil, false
	}
	var flags string
	if e.opts != nil {
		ov := e.opts.eval(c)
		if s, ok := strOf(ov); ok {
			for _, r := range s {
				switch r {
				case 'i', 'm', 's':
					flags += string(r)
				}
			}
		}
	}
	if flags != "" {
		pattern = "(?" + flags + ")" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false
	}
	return re, true
}

// regexResult builds the {match, idx, captures} document for one match.
func regexResult(s string, loc []int, re *regexp.Regexp) bson.RawValue {
	match := s[loc[0]:loc[1]]
	idx := utf8.RuneCountInString(s[:loc[0]])
	caps := make([]bson.RawValue, 0)
	for g := 1; g*2 < len(loc); g++ {
		if loc[g*2] < 0 {
			caps = append(caps, mkNull())
			continue
		}
		caps = append(caps, mkString(s[loc[g*2]:loc[g*2+1]]))
	}
	return mkDoc(bson.NewBuilder().
		AppendString("match", match).
		AppendInt32("idx", int32(idx)).
		AppendArray("captures", bson.BuildArray(caps...)).
		Build())
}

func regexFindResult(re *regexp.Regexp, s string) bson.RawValue {
	loc := re.FindStringSubmatchIndex(s)
	if loc == nil {
		return mkDoc(bson.NewBuilder().
			AppendNull("match").
			AppendInt32("idx", -1).
			AppendArray("captures", bson.BuildArray()).
			Build())
	}
	return regexResult(s, loc, re)
}

func regexFindAllResult(re *regexp.Regexp, s string) bson.RawValue {
	all := re.FindAllStringSubmatchIndex(s, -1)
	out := make([]bson.RawValue, 0, len(all))
	for _, loc := range all {
		out = append(out, regexResult(s, loc, re))
	}
	return mkArray(out)
}
