package language

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SyntaxError is a document that doesn't parse: what is wrong, and where.
type SyntaxError struct {
	Message      string
	Line, Column int
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("Syntax Error: %s (%d:%d)", e.Message, e.Line, e.Column)
}

type tokKind int

const (
	tokEOF tokKind = iota
	tokBang
	tokDollar
	tokAmp
	tokParenL
	tokParenR
	tokSpread
	tokColon
	tokEquals
	tokAt
	tokBracketL
	tokBracketR
	tokBraceL
	tokPipe
	tokBraceR
	tokName
	tokInt
	tokFloat
	tokString
	tokBlockString
)

var tokNames = [...]string{
	tokEOF: "<EOF>", tokBang: "\"!\"", tokDollar: "\"$\"", tokAmp: "\"&\"", tokParenL: "\"(\"", tokParenR: "\")\"",
	tokSpread: "\"...\"", tokColon: "\":\"", tokEquals: "\"=\"", tokAt: "\"@\"", tokBracketL: "\"[\"", tokBracketR: "\"]\"",
	tokBraceL: "\"{\"", tokPipe: "\"|\"", tokBraceR: "\"}\"", tokName: "Name", tokInt: "Int", tokFloat: "Float",
	tokString: "String", tokBlockString: "BlockString",
}

func (k tokKind) String() string { return tokNames[k] }

type token struct {
	kind  tokKind
	value string // a name, a number as written, a string's value
	loc   Loc
}

// describe is the token as a message names it: "Name \"foo\"", "\"{\"".
func (t token) describe() string {
	switch t.kind {
	case tokName, tokInt, tokFloat:
		return t.kind.String() + " " + strconv.Quote(t.value)
	case tokString, tokBlockString:
		return t.kind.String() + " " + strconv.Quote(t.value)
	}
	return t.kind.String()
}

type lexer struct {
	src       string
	pos       int // byte offset
	line      int
	lineStart int // byte offset of the line's start
}

func newLexer(src string) *lexer {
	l := &lexer{src: src, line: 1}
	if strings.HasPrefix(src, "\xEF\xBB\xBF") {
		l.pos = 3
		l.lineStart = 3
	}
	return l
}

func (l *lexer) locAt(pos int) Loc {
	return Loc{Line: l.line, Column: utf8.RuneCountInString(l.src[l.lineStart:pos]) + 1}
}

func (l *lexer) errAt(pos int, format string, args ...any) *SyntaxError {
	loc := l.locAt(pos)
	return &SyntaxError{Message: fmt.Sprintf(format, args...), Line: loc.Line, Column: loc.Column}
}

func (l *lexer) newline(pos int) {
	l.line++
	l.lineStart = pos
}

// printChar is a character as a message names it.
func printChar(r rune) string {
	if r == utf8.RuneError {
		return "U+FFFD"
	}
	if r < 0x20 || r == 0x7f || r > 0x7e {
		return fmt.Sprintf("U+%04X", r)
	}
	return strconv.Quote(string(r))
}

// next reads the next token, skipping ignored tokens: whitespace, line
// terminators, commas, comments and a leading byte order mark.
func (l *lexer) next() (token, error) {
	src := l.src
	for l.pos < len(src) {
		c := src[l.pos]
		switch c {
		case ' ', '\t', ',':
			l.pos++
			continue
		case '\n':
			l.pos++
			l.newline(l.pos)
			continue
		case '\r':
			l.pos++
			if l.pos < len(src) && src[l.pos] == '\n' {
				l.pos++
			}
			l.newline(l.pos)
			continue
		case '#':
			for l.pos < len(src) && src[l.pos] != '\n' && src[l.pos] != '\r' {
				l.pos++
			}
			continue
		}
		if c == 0xEF && strings.HasPrefix(src[l.pos:], "\xEF\xBB\xBF") {
			l.pos += 3
			continue
		}
		break
	}
	start := l.pos
	loc := l.locAt(start)
	if start >= len(src) {
		return token{kind: tokEOF, loc: loc}, nil
	}
	punct := func(k tokKind, n int) (token, error) {
		l.pos += n
		return token{kind: k, loc: loc}, nil
	}
	switch c := src[start]; c {
	case '!':
		return punct(tokBang, 1)
	case '$':
		return punct(tokDollar, 1)
	case '&':
		return punct(tokAmp, 1)
	case '(':
		return punct(tokParenL, 1)
	case ')':
		return punct(tokParenR, 1)
	case '.':
		if strings.HasPrefix(src[start:], "...") {
			return punct(tokSpread, 3)
		}
		return token{}, l.errAt(start, "Unexpected character: \".\"; a spread is \"...\".")
	case ':':
		return punct(tokColon, 1)
	case '=':
		return punct(tokEquals, 1)
	case '@':
		return punct(tokAt, 1)
	case '[':
		return punct(tokBracketL, 1)
	case ']':
		return punct(tokBracketR, 1)
	case '{':
		return punct(tokBraceL, 1)
	case '|':
		return punct(tokPipe, 1)
	case '}':
		return punct(tokBraceR, 1)
	case '"':
		if strings.HasPrefix(src[start:], `"""`) {
			return l.blockString(loc)
		}
		return l.string(loc)
	}
	c := src[start]
	if isNameStart(c) {
		end := start + 1
		for end < len(src) && isNameContinue(src[end]) {
			end++
		}
		l.pos = end
		return token{kind: tokName, value: src[start:end], loc: loc}, nil
	}
	if c == '-' || isDigit(c) {
		return l.number(loc)
	}
	r, _ := utf8.DecodeRuneInString(src[start:])
	if c == '\'' {
		return token{}, l.errAt(start, "Unexpected single quote character ('); did you mean to use a double quote (\")?")
	}
	return token{}, l.errAt(start, "Unexpected character: %s.", printChar(r))
}

func isNameStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isNameContinue(c byte) bool { return isNameStart(c) || isDigit(c) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func (l *lexer) number(loc Loc) (token, error) {
	src := l.src
	start := l.pos
	p := start
	float := false
	if src[p] == '-' {
		p++
	}
	if p < len(src) && src[p] == '0' {
		p++
		if p < len(src) && isDigit(src[p]) {
			return token{}, l.errAt(p, "Invalid number, unexpected digit after 0: %s.", printChar(rune(src[p])))
		}
	} else {
		var err error
		if p, err = l.digits(p); err != nil {
			return token{}, err
		}
	}
	if p < len(src) && src[p] == '.' {
		float = true
		var err error
		if p, err = l.digits(p + 1); err != nil {
			return token{}, err
		}
	}
	if p < len(src) && (src[p] == 'e' || src[p] == 'E') {
		float = true
		p++
		if p < len(src) && (src[p] == '+' || src[p] == '-') {
			p++
		}
		var err error
		if p, err = l.digits(p); err != nil {
			return token{}, err
		}
	}
	if p < len(src) && (src[p] == '.' || isNameStart(src[p])) {
		return token{}, l.errAt(p, "Invalid number, expected digit but got: %s.", printChar(rune(src[p])))
	}
	l.pos = p
	k := tokInt
	if float {
		k = tokFloat
	}
	return token{kind: k, value: src[start:p], loc: loc}, nil
}

// digits reads one or more digits at p.
func (l *lexer) digits(p int) (int, error) {
	src := l.src
	if p >= len(src) || !isDigit(src[p]) {
		if p >= len(src) {
			return 0, l.errAt(p, "Invalid number, expected digit but got: <EOF>.")
		}
		r, _ := utf8.DecodeRuneInString(src[p:])
		return 0, l.errAt(p, "Invalid number, expected digit but got: %s.", printChar(r))
	}
	for p < len(src) && isDigit(src[p]) {
		p++
	}
	return p, nil
}

func (l *lexer) string(loc Loc) (token, error) {
	src := l.src
	p := l.pos + 1
	var b strings.Builder
	chunk := p
	for p < len(src) {
		c := src[p]
		switch {
		case c == '"':
			b.WriteString(src[chunk:p])
			l.pos = p + 1
			return token{kind: tokString, value: b.String(), loc: loc}, nil
		case c == '\n' || c == '\r':
			return token{}, l.errAt(p, "Unterminated string.")
		case c == '\\':
			b.WriteString(src[chunk:p])
			if p+1 >= len(src) {
				return token{}, l.errAt(p, "Unterminated string.")
			}
			switch e := src[p+1]; e {
			case '"', '\\', '/':
				b.WriteByte(e)
				p += 2
			case 'b':
				b.WriteByte('\b')
				p += 2
			case 'f':
				b.WriteByte('\f')
				p += 2
			case 'n':
				b.WriteByte('\n')
				p += 2
			case 'r':
				b.WriteByte('\r')
				p += 2
			case 't':
				b.WriteByte('\t')
				p += 2
			case 'u':
				r, n, err := l.unicodeEscape(p)
				if err != nil {
					return token{}, err
				}
				b.WriteRune(r)
				p += n
			default:
				r, _ := utf8.DecodeRuneInString(src[p+1:])
				return token{}, l.errAt(p, "Invalid character escape sequence: %s.", strconv.Quote("\\"+string(r)))
			}
			chunk = p
		default:
			r, size := utf8.DecodeRuneInString(src[p:])
			if r == utf8.RuneError && size == 1 || r < 0x20 && r != '\t' {
				return token{}, l.errAt(p, "Invalid character within String: %s.", printChar(r))
			}
			p += size
		}
	}
	return token{}, l.errAt(p, "Unterminated string.")
}

// unicodeEscape reads \uXXXX (a surrogate pair's two escapes together) or
// \u{X…} at p: the character, and how many bytes the escape takes.
func (l *lexer) unicodeEscape(p int) (rune, int, error) {
	src := l.src
	bad := func(n int) (rune, int, error) {
		end := min(p+n, len(src))
		return 0, 0, l.errAt(p, "Invalid Unicode escape sequence: %s.", strconv.Quote(src[p:end]))
	}
	if p+2 < len(src) && src[p+2] == '{' {
		end := strings.IndexByte(src[p+3:], '}')
		if end < 0 {
			return bad(3)
		}
		hex := src[p+3 : p+3+end]
		v, err := strconv.ParseUint(hex, 16, 32)
		if hex == "" || err != nil || v > 0x10FFFF || v >= 0xD800 && v <= 0xDFFF {
			return bad(4 + end)
		}
		return rune(v), 4 + end, nil
	}
	read4 := func(at int) (rune, bool) {
		if at+6 > len(src) || src[at] != '\\' || src[at+1] != 'u' {
			return 0, false
		}
		v, err := strconv.ParseUint(src[at+2:at+6], 16, 32)
		if err != nil {
			return 0, false
		}
		return rune(v), true
	}
	r, ok := read4(p)
	if !ok {
		return bad(6)
	}
	switch {
	case r >= 0xD800 && r <= 0xDBFF:
		lo, ok := read4(p + 6)
		if ok && lo >= 0xDC00 && lo <= 0xDFFF {
			return (r-0xD800)<<10 + (lo - 0xDC00) + 0x10000, 12, nil
		}
		return bad(6)
	case r >= 0xDC00 && r <= 0xDFFF:
		return bad(6)
	}
	return r, 6, nil
}

func (l *lexer) blockString(loc Loc) (token, error) {
	src := l.src
	p := l.pos + 3
	var raw strings.Builder
	chunk := p
	for p < len(src) {
		switch {
		case strings.HasPrefix(src[p:], `"""`):
			raw.WriteString(src[chunk:p])
			l.pos = p + 3
			return token{kind: tokBlockString, value: BlockStringValue(raw.String()), loc: loc}, nil
		case strings.HasPrefix(src[p:], `\"""`):
			raw.WriteString(src[chunk:p])
			raw.WriteString(`"""`)
			p += 4
			chunk = p
		case src[p] == '\n':
			p++
			l.newline(p)
		case src[p] == '\r':
			p++
			if p < len(src) && src[p] == '\n' {
				p++
			}
			l.newline(p)
		default:
			r, size := utf8.DecodeRuneInString(src[p:])
			if r == utf8.RuneError && size == 1 || r < 0x20 && r != '\t' {
				return token{}, l.errAt(p, "Invalid character within String: %s.", printChar(r))
			}
			p += size
		}
	}
	return token{}, l.errAt(p, "Unterminated string.")
}

// BlockStringValue is a block string's value from its raw text (between
// the quotes, \""" already unescaped): its common indentation and its
// leading and trailing blank lines removed, lines joined with \n.
func BlockStringValue(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")
	common := -1
	for i, line := range lines {
		if i == 0 {
			continue
		}
		indent := leadingWhitespace(line)
		if indent == len(line) {
			continue
		}
		if common < 0 || indent < common {
			common = indent
		}
	}
	if common > 0 {
		for i := 1; i < len(lines); i++ {
			if len(lines[i]) >= common {
				lines[i] = lines[i][common:]
			} else {
				lines[i] = ""
			}
		}
	}
	for len(lines) > 0 && leadingWhitespace(lines[0]) == len(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && leadingWhitespace(lines[len(lines)-1]) == len(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func leadingWhitespace(s string) int {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}
