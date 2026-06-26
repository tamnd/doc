package main

import "strings"

// ANSI color codes for the shell's pretty output (spec 2061 doc 15 §14.5): keys bold,
// strings green, numbers cyan, bool and null yellow, errors red.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiGreen  = "\x1b[32m"
	ansiCyan   = "\x1b[36m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
)

// colorizeJSON scans valid JSON text and wraps each token in its color. It is a
// structural pass rather than a parse: it recognizes strings (and whether a string is
// a key by the colon that follows it), numbers, and the bare words true/false/null.
// Because the input is always the encoder's own output, the scan never has to recover
// from malformed JSON.
func colorizeJSON(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"':
			end := scanString(s, i)
			tok := s[i:end]
			if isKey(s, end) {
				b.WriteString(ansiBold)
				b.WriteString(tok)
				b.WriteString(ansiReset)
			} else {
				b.WriteString(ansiGreen)
				b.WriteString(tok)
				b.WriteString(ansiReset)
			}
			i = end
		case c == '-' || (c >= '0' && c <= '9'):
			end := scanNumber(s, i)
			b.WriteString(ansiCyan)
			b.WriteString(s[i:end])
			b.WriteString(ansiReset)
			i = end
		case strings.HasPrefix(s[i:], "true"):
			b.WriteString(ansiYellow + "true" + ansiReset)
			i += 4
		case strings.HasPrefix(s[i:], "false"):
			b.WriteString(ansiYellow + "false" + ansiReset)
			i += 5
		case strings.HasPrefix(s[i:], "null"):
			b.WriteString(ansiYellow + "null" + ansiReset)
			i += 4
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// scanString returns the index just past the closing quote of the JSON string that
// starts at s[start] (an opening quote), respecting backslash escapes.
func scanString(s string, start int) int {
	i := start + 1
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return i + 1
		}
		i++
	}
	return len(s)
}

// isKey reports whether the next non-space character at or after end is a colon, which
// is what distinguishes an object key from a string value.
func isKey(s string, end int) bool {
	for end < len(s) {
		switch s[end] {
		case ' ', '\t', '\n', '\r':
			end++
		case ':':
			return true
		default:
			return false
		}
	}
	return false
}

// scanNumber returns the index just past the JSON number beginning at s[start].
func scanNumber(s string, start int) int {
	i := start
	for i < len(s) {
		c := s[i]
		if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
			i++
			continue
		}
		break
	}
	return i
}

// colorError wraps a message in red when color is enabled.
func colorError(msg string, color bool) string {
	if !color {
		return msg
	}
	return ansiRed + msg + ansiReset
}
