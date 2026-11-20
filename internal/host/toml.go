package host

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// config.toml is edited in place rather than decoded and re-encoded, so an
// operator's comments, ordering and spacing survive every change. That needs
// to know where each statement starts and ends, which TOML decoders do not
// report; this small lexer does, and knows just enough TOML for it: table
// headers, comments, basic and literal strings (multi-line too) and arrays or
// inline tables spanning lines.

// tomlStmt is one statement of a TOML document: a table header or a key/value
// pair, with its byte offsets in the document.
type tomlStmt struct {
	// table is the header of the table the statement is in, spaces removed:
	// "[[runners]]", "[runners.cache]"; "" is the root table. For a header,
	// the header itself.
	table string
	// tableN numbers the headers from 1 (0 is the root table), so the first
	// and the second [[runners]] entry are told apart.
	tableN int
	header bool
	key    string // as written, spaces around the dots removed
	// value is the value as written, comments inside a multi-line array removed.
	value string
	// start is where the key (or the header's "[") begins, valStart where the
	// value begins, end where the value (or header) ends: before the trailing
	// spaces and comment.
	start, valStart, end int
}

// tomlStatements lexes doc into statements. It does not validate: a broken
// line is skipped to its end, never mistaken for a header.
func tomlStatements(doc string) []tomlStmt {
	var out []tomlStmt
	table, tableN := "", 0
	for i := 0; i < len(doc); {
		switch c := doc[i]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '#':
			i = lineEnd(doc, i)
		case c == '[':
			end, name := scanHeader(doc, i)
			if end < 0 {
				i = lineEnd(doc, i)
				continue
			}
			table, tableN = name, tableN+1
			out = append(out, tomlStmt{table: table, tableN: tableN, header: true, start: i, valStart: end, end: end})
			i = afterComment(doc, end)
		default:
			eq := scanKey(doc, i)
			if eq < 0 {
				i = lineEnd(doc, i)
				continue
			}
			v := eq + 1
			for v < len(doc) && (doc[v] == ' ' || doc[v] == '\t') {
				v++
			}
			end, value := scanValue(doc, v)
			out = append(out, tomlStmt{table: table, tableN: tableN, key: stripBlanks(doc[i:eq]), value: value,
				start: i, valStart: v, end: end})
			i = afterComment(doc, end)
		}
	}
	return out
}

// lineEnd is the offset of the newline ending the line at i, or len(doc).
func lineEnd(doc string, i int) int {
	if n := strings.IndexByte(doc[i:], '\n'); n >= 0 {
		return i + n
	}
	return len(doc)
}

// afterComment skips the spaces and the comment after a statement ending at i.
func afterComment(doc string, i int) int {
	for i < len(doc) && (doc[i] == ' ' || doc[i] == '\t') {
		i++
	}
	if i < len(doc) && doc[i] == '#' {
		return lineEnd(doc, i)
	}
	return i
}

// skipQuoted returns the offset after the single-line string starting at i
// (a basic "..." or literal '...' one); at a line end it stops there.
func skipQuoted(doc string, i int) int {
	q := doc[i]
	for j := i + 1; j < len(doc); j++ {
		switch {
		case doc[j] == '\n':
			return j
		case doc[j] == '\\' && q == '"':
			j++
		case doc[j] == q:
			return j + 1
		}
	}
	return len(doc)
}

// skipMultiline returns the offset after the multi-line string (three double
// or three single quotes) starting at i.
func skipMultiline(doc string, i int) int {
	q := doc[i : i+3]
	for j := i + 3; j < len(doc); j++ {
		if doc[j] == '\\' && q[0] == '"' {
			j++
			continue
		}
		if strings.HasPrefix(doc[j:], q) {
			j += 3
			// Up to two quotes may sit right before the closing three.
			for n := 0; n < 2 && j < len(doc) && doc[j] == q[0]; n++ {
				j++
			}
			return j
		}
	}
	return len(doc)
}

// scanHeader reads the [table] or [[array]] header at i. It returns the
// offset after it and its name with the spaces removed, or -1 when the line
// is not a header.
func scanHeader(doc string, i int) (int, string) {
	open := "["
	if strings.HasPrefix(doc[i:], "[[") {
		open = "[["
	}
	j := i + len(open)
	for j < len(doc) && doc[j] != '\n' {
		switch doc[j] {
		case '"', '\'':
			j = skipQuoted(doc, j)
		case ']':
			closer := strings.Repeat("]", len(open))
			if !strings.HasPrefix(doc[j:], closer) {
				return -1, ""
			}
			return j + len(closer), open + stripBlanks(doc[i+len(open):j]) + closer
		default:
			j++
		}
	}
	return -1, ""
}

// scanKey returns the offset of the "=" after the key starting at i, or -1
// when the line has none.
func scanKey(doc string, i int) int {
	for j := i; j < len(doc) && doc[j] != '\n'; {
		switch doc[j] {
		case '"', '\'':
			j = skipQuoted(doc, j)
		case '=':
			return j
		case '#':
			return -1
		default:
			j++
		}
	}
	return -1
}

// scanValue reads the value starting at i: up to the end of its line, or for
// an array or inline table up to its closing bracket, however many lines that
// takes. It returns the offset after the value's last character and the value
// without the comments inside it.
func scanValue(doc string, i int) (int, string) {
	var b strings.Builder
	depth, end := 0, i
	for i < len(doc) {
		c := doc[i]
		next := i + 1
		switch {
		case strings.HasPrefix(doc[i:], `"""`) || strings.HasPrefix(doc[i:], "'''"):
			next = skipMultiline(doc, i)
		case c == '"' || c == '\'':
			next = skipQuoted(doc, i)
		case c == '#':
			if depth == 0 {
				return end, strings.TrimRight(b.String(), " \t\r")
			}
			i = lineEnd(doc, i)
			continue
		case c == '\n':
			if depth == 0 {
				return end, strings.TrimRight(b.String(), " \t\r")
			}
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		}
		b.WriteString(doc[i:next])
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			end = next
		}
		i = next
	}
	return end, strings.TrimRight(b.String(), " \t\r")
}

// stripBlanks removes the spaces and tabs outside quotes: "a . b" -> "a.b".
func stripBlanks(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch s[i] {
		case ' ', '\t':
			i++
		case '"', '\'':
			j := skipQuoted(s, i)
			b.WriteString(s[i:j])
			i = j
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// tomlQuote writes s as a TOML basic string with the escapes TOML 1.0
// defines only: strconv.Quote also writes \a, \v and \xNN, which TOML 1.0
// readers refuse. Other control characters become \uXXXX.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlUnquote returns the content of a single-line basic or literal string;
// ok is false for any other value. Basic strings take TOML's escapes, with
// TOML 1.1's \e and \xHH, and no others: Go's strconv.Unquote refuses \e and
// accepts \a or \v, which TOML does not.
func tomlUnquote(v string) (string, bool) {
	switch {
	case len(v) < 2 || strings.HasPrefix(v, `"""`) || strings.HasPrefix(v, "'''"):
		return "", false
	case v[0] == '\'' && v[len(v)-1] == '\'':
		body := v[1 : len(v)-1]
		return body, !strings.ContainsAny(body, "'\n")
	case v[0] != '"' || v[len(v)-1] != '"':
		return "", false
	}
	body := v[1 : len(v)-1]
	var b strings.Builder
	for i := 0; i < len(body); {
		c := body[i]
		if c == '"' || c == '\n' {
			return "", false
		}
		if c != '\\' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(body) {
			return "", false
		}
		e := body[i+1]
		i += 2
		if simple, ok := map[byte]byte{'b': '\b', 't': '\t', 'n': '\n', 'f': '\f', 'r': '\r', '"': '"', '\\': '\\', 'e': 0x1b}[e]; ok {
			b.WriteByte(simple)
			continue
		}
		digits := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
		if digits == 0 || i+digits > len(body) {
			return "", false
		}
		n, err := strconv.ParseUint(body[i:i+digits], 16, 32)
		if err != nil || !utf8.ValidRune(rune(n)) {
			return "", false
		}
		b.WriteRune(rune(n))
		i += digits
	}
	return b.String(), true
}
