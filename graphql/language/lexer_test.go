package language

import (
	"strings"
	"testing"
)

func lexOne(t *testing.T, src string) token {
	t.Helper()
	tok, err := newLexer(src).next()
	if err != nil {
		t.Fatalf("lex %q: %v", src, err)
	}
	return tok
}

func lexErr(src string) *SyntaxError {
	l := newLexer(src)
	for {
		tok, err := l.next()
		if err != nil {
			return err.(*SyntaxError)
		}
		if tok.kind == tokEOF {
			return nil
		}
	}
}

func TestLexIgnored(t *testing.T) {
	tok := lexOne(t, "\xEF\xBB\xBF  ,,\t\n\r\n# a comment\r  foo")
	if tok.kind != tokName || tok.value != "foo" || tok.loc != (Loc{4, 3}) {
		t.Fatalf("got %+v", tok)
	}
	if tok := lexOne(t, "\n\n    # only a comment\n"); tok.kind != tokEOF || tok.loc.Line != 4 {
		t.Fatalf("got %+v", tok)
	}
}

func TestLexPunctuators(t *testing.T) {
	l := newLexer("! $ & ( ) ... : = @ [ ] { | }")
	want := []tokKind{tokBang, tokDollar, tokAmp, tokParenL, tokParenR, tokSpread, tokColon, tokEquals, tokAt, tokBracketL, tokBracketR, tokBraceL, tokPipe, tokBraceR, tokEOF}
	for i, k := range want {
		tok, err := l.next()
		if err != nil || tok.kind != k {
			t.Fatalf("token %d: got %v %v, want %v", i, tok.kind, err, k)
		}
	}
}

func TestLexColumnsCountCharacters(t *testing.T) {
	l := newLexer(`"héllo" x`)
	l.next()
	tok, _ := l.next()
	if tok.loc != (Loc{1, 9}) {
		t.Fatalf("x at %+v, want 1:9", tok.loc)
	}
}

func TestLexNumbers(t *testing.T) {
	for _, c := range []struct {
		src  string
		kind tokKind
	}{
		{"0", tokInt}, {"-0", tokInt}, {"9", tokInt}, {"123", tokInt}, {"-123", tokInt},
		{"0.0", tokFloat}, {"-1.5", tokFloat}, {"1e10", tokFloat}, {"1E-3", tokFloat}, {"1.5e+2", tokFloat}, {"-0.5e1", tokFloat},
	} {
		tok := lexOne(t, c.src)
		if tok.kind != c.kind || tok.value != c.src {
			t.Errorf("%q: got %v %q", c.src, tok.kind, tok.value)
		}
	}
	for _, c := range []struct{ src, msg string }{
		{"00", `Invalid number, unexpected digit after 0: "0".`},
		{"01", `Invalid number, unexpected digit after 0: "1".`},
		{"1.", "Invalid number, expected digit but got: <EOF>."},
		{"1.a", `Invalid number, expected digit but got: "a".`},
		{".5", `Unexpected character: "."; a spread is "...".`},
		{"1e", "Invalid number, expected digit but got: <EOF>."},
		{"1e+", "Invalid number, expected digit but got: <EOF>."},
		{"1.0.1", `Invalid number, expected digit but got: ".".`},
		{"1a", `Invalid number, expected digit but got: "a".`},
		{"-", "Invalid number, expected digit but got: <EOF>."},
		{"-a", `Invalid number, expected digit but got: "a".`},
		{"1_000", `Invalid number, expected digit but got: "_".`},
	} {
		err := lexErr(c.src)
		if err == nil || err.Message != c.msg {
			t.Errorf("%q: got %v, want %q", c.src, err, c.msg)
		}
	}
}

func TestLexStrings(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{`""`, ""},
		{`"simple"`, "simple"},
		{`" white space "`, " white space "},
		{`"quote \""`, `quote "`},
		{`"escaped \n\r\b\t\f\/\\"`, "escaped \n\r\b\t\f/\\"},
		{`"unicode \u1234\u5678\u90AB\uCDEF"`, "unicode \u1234\u5678\u90AB\uCDEF"},
		{`"\u{1F600} and \u{0}"`, "\U0001F600 and \x00"},
		{`"\uD83D\uDE00"`, "\U0001F600"},
		{`"tab	inside"`, "tab\tinside"},
	} {
		tok := lexOne(t, c.src)
		if tok.kind != tokString || tok.value != c.want {
			t.Errorf("%s: got %v %q, want %q", c.src, tok.kind, tok.value, c.want)
		}
	}
	for _, c := range []struct {
		src, msg string
		col      int
	}{
		{`"`, "Unterminated string.", 2},
		{`"no end`, "Unterminated string.", 8},
		{"\"line\nbreak\"", "Unterminated string.", 6},
		{"\"bell \x07\"", "Invalid character within String: U+0007.", 7},
		{`"bad \x"`, `Invalid character escape sequence: "\\x".`, 6},
		{`"bad \u12"`, `Invalid Unicode escape sequence: "\\u12\"".`, 6},
		{`"bad \uXXXX"`, `Invalid Unicode escape sequence: "\\uXXXX".`, 6},
		{`"\uD83D"`, `Invalid Unicode escape sequence: "\\uD83D".`, 2},
		{`"\uDE00"`, `Invalid Unicode escape sequence: "\\uDE00".`, 2},
		{`"\u{110000}"`, `Invalid Unicode escape sequence: "\\u{110000}".`, 2},
		{`"\u{}"`, `Invalid Unicode escape sequence: "\\u{}".`, 2},
	} {
		err := lexErr(c.src)
		if err == nil || err.Message != c.msg || err.Column != c.col {
			t.Errorf("%q: got %+v, want %q at column %d", c.src, err, c.msg, c.col)
		}
	}
}

func TestLexBlockStrings(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{`""""""`, ""},
		{`"""simple"""`, "simple"},
		{`""" white space """`, " white space "},
		{`"""contains " quote"""`, `contains " quote`},
		{`"""contains \""" triple quote"""`, `contains """ triple quote`},
		{"\"\"\"multi\nline\"\"\"", "multi\nline"},
		{"\"\"\"multi\r\nline\rnormalized\"\"\"", "multi\nline\nnormalized"},
		{`"""unescaped \n\r\b\t\f\u1234"""`, `unescaped \n\r\b\t\f\u1234`},
		{"\"\"\"\n\n    spans\n      multiple\n    lines\n\n  \"\"\"", "spans\n  multiple\nlines"},
	} {
		tok := lexOne(t, c.src)
		if tok.kind != tokBlockString || tok.value != c.want {
			t.Errorf("%q: got %v %q, want %q", c.src, tok.kind, tok.value, c.want)
		}
	}
	if err := lexErr(`"""no end`); err == nil || err.Message != "Unterminated string." {
		t.Errorf("got %v", err)
	}
	if err := lexErr("\"\"\"bell \x07\"\"\""); err == nil || !strings.Contains(err.Message, "U+0007") {
		t.Errorf("got %v", err)
	}
	// Lines inside a block string count towards later tokens' lines.
	l := newLexer("\"\"\"a\nb\nc\"\"\" x")
	l.next()
	tok, _ := l.next()
	if tok.loc != (Loc{3, 6}) {
		t.Errorf("x at %+v, want 3:6", tok.loc)
	}
}

func TestBlockStringValue(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"", ""},
		{"  only first line kept indented", "  only first line kept indented"},
		{"\n    Hello,\n      World!\n\n    Yours,\n      GraphQL.\n  ", "Hello,\n  World!\n\nYours,\n  GraphQL."},
		{"\t\tfirst\n\t\tsecond", "\t\tfirst\nsecond"},
		{"   \n  \n", ""},
	} {
		if got := BlockStringValue(c.raw); got != c.want {
			t.Errorf("%q: got %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestLexUnexpected(t *testing.T) {
	for _, c := range []struct{ src, msg string }{
		{"?", `Unexpected character: "?".`},
		{"\u203B", "Unexpected character: U+203B."},
		{"..", `Unexpected character: "."; a spread is "...".`},
		{"'x'", `Unexpected single quote character ('); did you mean to use a double quote (")?`},
		{"\x07", "Unexpected character: U+0007."},
	} {
		err := lexErr(c.src)
		if err == nil || err.Message != c.msg || err.Line != 1 || err.Column != 1 {
			t.Errorf("%q: got %+v, want %q at 1:1", c.src, err, c.msg)
		}
	}
	err := lexErr("\n\n   ?")
	if err.Line != 3 || err.Column != 4 {
		t.Errorf("got %+v, want 3:4", err)
	}
	if got := err.Error(); got != `Syntax Error: Unexpected character: "?". (3:4)` {
		t.Errorf("Error() = %q", got)
	}
}
