package web

import (
	"bytes"
	"strings"

	"github.com/vuka-lang/vuka/v2/diag"
)

// dotenv parses a .env file. Each value is expanded as it is read, from the
// process environment, then the keys the file defined before it, then vars
// (the files read before it). Errors name the file and line, never a value.
type dotenv struct {
	name   string
	src    string
	i      int
	line   int
	lookup func(string) (string, bool)
}

type envPair struct{ key, value string }

func parseDotEnv(name string, data []byte, env func(string) (string, bool), vars map[string]string) ([]envPair, error) {
	defined := map[string]string{}
	p := &dotenv{name: name, line: 1, lookup: func(k string) (string, bool) {
		if v, ok := env(k); ok {
			return v, true
		}
		if v, ok := defined[k]; ok {
			return v, true
		}
		v, ok := vars[k]
		return v, ok
	}}
	p.src = strings.ReplaceAll(string(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))), "\r\n", "\n")
	var out []envPair
	for p.i < len(p.src) {
		k, v, ok, err := p.entry()
		if err != nil {
			return nil, err
		}
		if ok {
			defined[k] = v
			out = append(out, envPair{k, v})
		}
	}
	return out, nil
}

func (p *dotenv) errorf(format string, args ...any) error {
	return diag.New("E0416", format, args...).AtLine(p.name, p.line)
}

func (p *dotenv) skipBlanks() {
	for p.i < len(p.src) && (p.src[p.i] == ' ' || p.src[p.i] == '\t') {
		p.i++
	}
}

// rest consumes the remainder of the line, which may hold only a comment.
func (p *dotenv) rest() error {
	p.skipBlanks()
	if p.i < len(p.src) && p.src[p.i] != '\n' && p.src[p.i] != '#' {
		return p.errorf("unexpected text after the closing quote")
	}
	p.nextLine()
	return nil
}

func (p *dotenv) nextLine() {
	if j := strings.IndexByte(p.src[p.i:], '\n'); j >= 0 {
		p.i += j + 1
		p.line++
	} else {
		p.i = len(p.src)
	}
}

func isKeyStart(c byte) bool { return c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }
func isKeyByte(c byte) bool  { return isKeyStart(c) || '0' <= c && c <= '9' }

func (p *dotenv) entry() (key, value string, ok bool, err error) {
	p.skipBlanks()
	if p.i == len(p.src) || p.src[p.i] == '\n' || p.src[p.i] == '#' {
		p.nextLine()
		return "", "", false, nil
	}
	if strings.HasPrefix(p.src[p.i:], "export") && p.i+6 < len(p.src) && (p.src[p.i+6] == ' ' || p.src[p.i+6] == '\t') {
		p.i += 6
		p.skipBlanks()
	}
	start := p.i
	if p.i < len(p.src) && isKeyStart(p.src[p.i]) {
		for p.i < len(p.src) && (isKeyByte(p.src[p.i]) || p.src[p.i] == '.') {
			p.i++
		}
	}
	key = p.src[start:p.i]
	p.skipBlanks()
	if key == "" || p.i == len(p.src) || p.src[p.i] != '=' {
		return "", "", false, p.errorf("expected KEY=value")
	}
	p.i++
	p.skipBlanks()
	if p.i < len(p.src) {
		switch q := p.src[p.i]; q {
		case '\'', '`':
			end := strings.IndexByte(p.src[p.i+1:], q)
			if end < 0 {
				return "", "", false, p.errorf("unterminated %c-quoted value of %s", q, key)
			}
			value = p.src[p.i+1 : p.i+1+end]
			p.line += strings.Count(value, "\n")
			p.i += end + 2
			return key, value, true, p.rest()
		case '"':
			startLine := p.line
			j := p.i + 1
			for ; j < len(p.src) && p.src[j] != '"'; j++ {
				if p.src[j] == '\\' {
					j++
				}
			}
			if j >= len(p.src) {
				return "", "", false, p.errorf("unterminated double-quoted value of %s", key)
			}
			raw := p.src[p.i+1 : j]
			p.line += strings.Count(raw, "\n")
			p.i = j + 1
			if value, err = p.expand(raw, true, startLine); err != nil {
				return "", "", false, err
			}
			return key, value, true, p.rest()
		}
	}
	end := strings.IndexByte(p.src[p.i:], '\n')
	if end < 0 {
		end = len(p.src) - p.i
	}
	raw := p.src[p.i : p.i+end]
	for j := 1; j < len(raw); j++ {
		if raw[j] == '#' && (raw[j-1] == ' ' || raw[j-1] == '\t') {
			raw = raw[:j]
			break
		}
	}
	if strings.HasPrefix(raw, "#") {
		raw = ""
	}
	value, err = p.expand(strings.TrimRight(raw, " \t"), false, p.line)
	p.nextLine()
	return key, value, err == nil, err
}

// expand replaces $VAR, ${VAR} and ${VAR:-default} in s; with escapes, it
// also reads \n \t \r \" \\ and \$ (a literal dollar sign).
func (p *dotenv) expand(s string, escapes bool, line int) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escapes && c == '\\' && i+1 < len(s):
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"', '\\', '$':
				b.WriteByte(s[i])
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
		case c == '$' && i+1 < len(s) && s[i+1] == '{':
			depth, j := 1, i+2
			for ; j < len(s) && depth > 0; j++ {
				if s[j] == '}' {
					depth--
				} else if s[j] == '{' && s[j-1] == '$' {
					depth++
				}
			}
			if depth > 0 {
				return "", diag.New("E0416", "unterminated ${").AtLine(p.name, line).WithHelp("close it: ${NAME}")
			}
			ref := s[i+2 : j-1]
			name, def, hasDef := strings.Cut(ref, ":-")
			if !validKey(name) {
				return "", diag.New("E0416", "bad variable reference ${%s}", name).AtLine(p.name, line).WithHelp("a reference is ${NAME} or ${NAME:-default}")
			}
			v, ok := p.lookup(name)
			if (!ok || v == "") && hasDef {
				var err error
				if v, err = p.expand(def, escapes, line); err != nil {
					return "", err
				}
			}
			b.WriteString(v)
			i = j - 1
		case c == '$' && i+1 < len(s) && isKeyStart(s[i+1]):
			j := i + 1
			for j < len(s) && isKeyByte(s[j]) {
				j++
			}
			v, _ := p.lookup(s[i+1 : j])
			b.WriteString(v)
			i = j - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

func validKey(k string) bool {
	if k == "" || !isKeyStart(k[0]) {
		return false
	}
	for i := 1; i < len(k); i++ {
		if !isKeyByte(k[i]) && k[i] != '.' {
			return false
		}
	}
	return true
}
