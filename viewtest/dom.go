package viewtest

import (
	"html"
	"strings"
)

// node is an element or a text of a page as Open holds it: enough of a
// DOM to find elements by CSS selector, read their text and values, and
// walk up to the element answering an event.
type node struct {
	tag    string // lower case; "" for a text
	attrs  [][2]string
	kids   []*node
	parent *node
	text   string // a text's, unescaped
}

func (n *node) attr(name string) (string, bool) {
	for _, a := range n.attrs {
		if a[0] == name {
			return a[1], true
		}
	}
	return "", false
}

func (n *node) get(name string) string { v, _ := n.attr(name); return v }

func (n *node) has(name string) bool { _, ok := n.attr(name); return ok }

// walk calls f on n and every element below it, in document order, until
// f returns false.
func (n *node) walk(f func(*node) bool) bool {
	if n.tag != "" && !f(n) {
		return false
	}
	for _, k := range n.kids {
		if k.tag != "" && !k.walk(f) {
			return false
		}
	}
	return true
}

// closest is n or its nearest ancestor f holds for; nil when none.
func (n *node) closest(f func(*node) bool) *node {
	for e := n; e != nil; e = e.parent {
		if e.tag != "" && e.tag != "#root" && f(e) {
			return e
		}
	}
	return nil
}

// textContent is the text below n, its runs of white space one space and
// trimmed: what Expect compares.
func (n *node) textContent() string {
	var b strings.Builder
	var rec func(*node)
	rec = func(n *node) {
		if n.tag == "" {
			b.WriteString(n.text)
		}
		for _, k := range n.kids {
			rec(k)
		}
	}
	rec(n)
	return squash(b.String())
}

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

var voids = map[string]bool{"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
	"input": true, "link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true}

// raw are the elements whose content is text up to their end tag.
var raw = map[string]bool{"script": true, "style": true, "textarea": true, "title": true}

// parse parses markup as the server renders it: well formed, every
// element but the void ones closed.
func parse(src string) *node {
	root := &node{tag: "#root"}
	cur := root
	text := func(s string, escaped bool) {
		if s == "" {
			return
		}
		if escaped {
			s = html.UnescapeString(s)
		}
		cur.kids = append(cur.kids, &node{text: s, parent: cur})
	}
	for i := 0; i < len(src); {
		lt := strings.IndexByte(src[i:], '<')
		if lt < 0 {
			text(src[i:], true)
			break
		}
		text(src[i:i+lt], true)
		i += lt
		switch {
		case strings.HasPrefix(src[i:], "<!--"):
			end := strings.Index(src[i+4:], "-->")
			if end < 0 {
				return root
			}
			i += 4 + end + 3
		case strings.HasPrefix(src[i:], "<!") || strings.HasPrefix(src[i:], "<?"):
			end := strings.IndexByte(src[i:], '>')
			if end < 0 {
				return root
			}
			i += end + 1
		case strings.HasPrefix(src[i:], "</"):
			end := strings.IndexByte(src[i:], '>')
			if end < 0 {
				return root
			}
			name := strings.ToLower(strings.TrimSpace(src[i+2 : i+end]))
			for e := cur; e != root; e = e.parent {
				if e.tag == name {
					cur = e.parent
					break
				}
			}
			i += end + 1
		default:
			n, next := openTag(src, i)
			if n == nil {
				text("<", false)
				i++
				continue
			}
			n.parent = cur
			cur.kids = append(cur.kids, n)
			i = next
			if selfClosed(src, next) || voids[n.tag] {
				continue
			}
			if raw[n.tag] {
				end := strings.Index(strings.ToLower(src[i:]), "</"+n.tag)
				if end < 0 {
					end = len(src) - i
				}
				body := src[i : i+end]
				cur = n
				text(body, n.tag == "textarea" || n.tag == "title")
				cur = n.parent
				i += end
				if gt := strings.IndexByte(src[i:], '>'); gt >= 0 {
					i += gt + 1
				}
				continue
			}
			cur = n
		}
	}
	return root
}

func selfClosed(src string, next int) bool { return next >= 2 && src[next-2] == '/' }

// openTag parses the start tag at src[i]: the element and where it ends.
func openTag(src string, i int) (*node, int) {
	j := i + 1
	for j < len(src) && isNameByte(src[j]) {
		j++
	}
	if j == i+1 {
		return nil, i
	}
	n := &node{tag: strings.ToLower(src[i+1 : j])}
	for j < len(src) {
		for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r' || src[j] == '/') {
			j++
		}
		if j >= len(src) {
			break
		}
		if src[j] == '>' {
			return n, j + 1
		}
		k := j
		for k < len(src) && src[k] != '=' && src[k] != '>' && src[k] != ' ' && src[k] != '\t' && src[k] != '\n' && src[k] != '/' {
			k++
		}
		name := strings.ToLower(src[j:k])
		val := ""
		if k < len(src) && src[k] == '=' {
			k++
			switch {
			case k < len(src) && (src[k] == '"' || src[k] == '\''):
				q := src[k]
				end := strings.IndexByte(src[k+1:], q)
				if end < 0 {
					end = len(src) - k - 1
				}
				val = html.UnescapeString(src[k+1 : k+1+end])
				k += end + 2
			default:
				e := k
				for e < len(src) && src[e] != ' ' && src[e] != '>' && src[e] != '\t' && src[e] != '\n' {
					e++
				}
				val = html.UnescapeString(src[k:e])
				k = e
			}
		}
		if name != "" {
			n.attrs = append(n.attrs, [2]string{name, val})
		}
		if k == j {
			k++
		}
		j = k
	}
	return n, len(src)
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == ':' || c == '.'
}

// A selector is a group of complex selectors, matched as querySelector
// does: the tag, #id, .class and [attr] (=, ~=, ^=, $=, *=) of compounds
// joined by descendant and child combinators. What it doesn't know (a
// pseudo-class) makes it match nothing.
type selector [][]step

type step struct {
	child bool // joined to the step before it by >
	tag   string
	conds []cond
}

type cond struct{ attr, op, val string }

func parseSelector(s string) (selector, bool) {
	var sel selector
	for _, part := range splitTop(s, ',') {
		var steps []step
		child := false
		for _, tok := range tokens(strings.TrimSpace(part)) {
			if tok == ">" {
				child = true
				continue
			}
			st, ok := compound(tok)
			if !ok {
				return nil, false
			}
			st.child = child && len(steps) > 0
			child = false
			steps = append(steps, st)
		}
		if len(steps) == 0 {
			return nil, false
		}
		sel = append(sel, steps)
	}
	return sel, len(sel) > 0
}

// splitTop splits s at sep outside brackets and quotes.
func splitTop(s string, sep byte) []string {
	var out []string
	depth, q, start := 0, byte(0), 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '[':
			depth++
		case c == ']':
			depth--
		case c == sep && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// tokens are a complex selector's compounds and > combinators.
func tokens(s string) []string {
	var out []string
	for _, f := range splitTop(strings.ReplaceAll(s, ">", " > "), ' ') {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func compound(s string) (step, bool) {
	var st step
	i := 0
	ident := func() string {
		j := i
		for i < len(s) && (isNameByte(s[i]) && s[i] != '.' && s[i] != ':' || s[i] >= 0x80) {
			i++
		}
		return s[j:i]
	}
	if i < len(s) && s[i] == '*' {
		i++
	} else if t := ident(); t != "" {
		st.tag = strings.ToLower(t)
	}
	for i < len(s) {
		switch s[i] {
		case '#':
			i++
			id := ident()
			if id == "" {
				return st, false
			}
			st.conds = append(st.conds, cond{"id", "=", id})
		case '.':
			i++
			c := ident()
			if c == "" {
				return st, false
			}
			st.conds = append(st.conds, cond{"class", "~=", c})
		case '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return st, false
			}
			body := s[i+1 : i+end]
			i += end + 1
			c := cond{}
			if at := strings.IndexByte(body, '='); at >= 0 {
				c.attr, c.op = body[:at], "="
				if at > 0 && strings.ContainsRune("~^$*|", rune(body[at-1])) {
					c.attr, c.op = body[:at-1], body[at-1:at+1]
				}
				c.val = strings.Trim(strings.TrimSpace(body[at+1:]), `"'`)
			} else {
				c.attr, c.op = body, ""
			}
			c.attr = strings.ToLower(strings.TrimSpace(c.attr))
			if c.attr == "" {
				return st, false
			}
			st.conds = append(st.conds, c)
		default:
			return st, false
		}
	}
	return st, true
}

func (sel selector) matches(e *node) bool {
	for _, steps := range sel {
		if matchSteps(steps, e) {
			return true
		}
	}
	return false
}

func matchSteps(steps []step, e *node) bool {
	last := steps[len(steps)-1]
	if !last.match(e) {
		return false
	}
	if len(steps) == 1 {
		return true
	}
	for p := e.parent; p != nil && p.tag != "#root"; p = p.parent {
		if matchSteps(steps[:len(steps)-1], p) {
			return true
		}
		if last.child {
			return false
		}
	}
	return false
}

func (st step) match(e *node) bool {
	if e.tag == "" || e.tag == "#root" || st.tag != "" && st.tag != e.tag {
		return false
	}
	for _, c := range st.conds {
		v, ok := e.attr(c.attr)
		if !ok {
			return false
		}
		switch c.op {
		case "=":
			ok = v == c.val
		case "~=":
			ok = false
			for _, f := range strings.Fields(v) {
				ok = ok || f == c.val
			}
		case "^=":
			ok = strings.HasPrefix(v, c.val)
		case "$=":
			ok = strings.HasSuffix(v, c.val)
		case "*=":
			ok = strings.Contains(v, c.val)
		case "|=":
			ok = v == c.val || strings.HasPrefix(v, c.val+"-")
		}
		if !ok {
			return false
		}
	}
	return true
}

// query is the first element below root sel matches; nil when none, or
// when sel isn't a selector the model knows.
func query(root *node, sel string) *node {
	s, ok := parseSelector(sel)
	if !ok {
		return nil
	}
	var found *node
	root.walk(func(e *node) bool {
		if s.matches(e) {
			found = e
		}
		return found == nil
	})
	return found
}

func isField(e *node) bool { return e.tag == "input" || e.tag == "textarea" || e.tag == "select" }

func checkable(e *node) bool {
	t := strings.ToLower(e.get("type"))
	return e.tag == "input" && (t == "checkbox" || t == "radio")
}

// clickable is what a locator naming text looks among: what a person
// clicks by its label.
func clickable(e *node) bool {
	switch e.tag {
	case "button", "a", "summary", "label":
		return true
	case "input":
		t := strings.ToLower(e.get("type"))
		return t == "submit" || t == "button" || t == "reset"
	}
	return e.get("role") == "button" || e.get("role") == "link" || e.get("role") == "tab" || e.get("role") == "menuitem"
}

// The kinds of element a locator names, deciding the order it is read in.
const (
	anyKind   = iota // a selector, a field's name, a clickable's text
	fieldKind        // a field's name, a selector
	clickKind        // a selector, a clickable's text
)

// find is the element loc names. A locator is read in an order fixed by
// what the element is for: a field's name first for a field (so "title"
// is the form's title field, not the document's <title>), a CSS selector
// first otherwise, then the text of a button, link or label.
func find(root *node, loc string, kind int) *node {
	byName := func() *node {
		var found *node
		root.walk(func(e *node) bool {
			if isField(e) && e.get("name") == loc {
				found = e
			}
			return found == nil
		})
		return found
	}
	byText := func() *node {
		want := squash(loc)
		var found *node
		root.walk(func(e *node) bool {
			if clickable(e) && (e.textContent() == want || e.tag == "input" && e.get("value") == want || e.get("aria-label") == want) {
				found = e
			}
			return found == nil
		})
		return found
	}
	order := map[int][]func() *node{
		anyKind:   {func() *node { return query(root, loc) }, byName, byText},
		fieldKind: {byName, func() *node { return query(root, loc) }},
		clickKind: {func() *node { return query(root, loc) }, byText},
	}[kind]
	for _, f := range order {
		if e := f(); e != nil {
			return e
		}
	}
	return nil
}

// shown reports whether e would show: neither it nor an ancestor hidden,
// display:none, a closed dialog, a template or in the head; not a hidden
// input.
func shown(e *node) bool {
	if e.tag == "input" && strings.EqualFold(e.get("type"), "hidden") {
		return false
	}
	for p := e; p != nil && p.tag != "#root"; p = p.parent {
		style := strings.ReplaceAll(strings.ToLower(p.get("style")), " ", "")
		if p.has("hidden") || strings.Contains(style, "display:none") || p.tag == "template" || p.tag == "head" ||
			p.tag == "dialog" && !p.has("open") || p.tag == "script" || p.tag == "style" {
			return false
		}
	}
	return true
}
