// Package livetree is a stand-in for the Vuka live session's protocol v2
// until the session speaks it: it runs a v1 session and turns the HTML of
// each changed instance into render trees and their changes, in the format of
// Vuka's "Protocol v2: render trees" (docs/features/components.md). The
// trees come from the markup, not from the compiler: every text node and
// attribute is a dynamic, every run of keyed sibling elements a block. Tests
// install it (Install) to drive the browser runtime's v2 path.
package livetree

import (
	"strings"
)

// node is an element or a piece of markup of the HTML a v1 session renders.
type node struct {
	tag      string // "" for markup
	text     string // markup: text, a comment, a doctype, raw script/style content
	isText   bool   // markup that is a text node (a dynamic)
	attrs    []attr
	kids     []*node
	void     bool
	selfEnd  bool // written <x/>
	hasClose bool
}

type attr struct {
	name, value string
	bare        bool
}

func (n *node) attr(name string) (string, bool) {
	for _, a := range n.attrs {
		if a.name == name {
			return a.value, true
		}
	}
	return "", false
}

var voids = map[string]bool{"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
	"input": true, "link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true}

// parse reads the well-formed HTML a Vuka render writes: quoted attribute
// values, escaped text, void elements unclosed.
func parse(html string) []*node {
	root := &node{tag: "#root"}
	stack := []*node{root}
	top := func() *node { return stack[len(stack)-1] }
	i := 0
	for i < len(html) {
		if html[i] != '<' {
			j := strings.IndexByte(html[i:], '<')
			if j < 0 {
				j = len(html) - i
			}
			top().kids = append(top().kids, &node{text: html[i : i+j], isText: true})
			i += j
			continue
		}
		switch {
		case strings.HasPrefix(html[i:], "<!--"):
			j := strings.Index(html[i:], "-->")
			if j < 0 {
				j = len(html) - i - 3
			}
			top().kids = append(top().kids, &node{text: html[i : i+j+3]})
			i += j + 3
		case strings.HasPrefix(html[i:], "<!"), strings.HasPrefix(html[i:], "<?"):
			j := strings.IndexByte(html[i:], '>')
			top().kids = append(top().kids, &node{text: html[i : i+j+1]})
			i += j + 1
		case strings.HasPrefix(html[i:], "</"):
			j := strings.IndexByte(html[i:], '>')
			name := strings.TrimSpace(html[i+2 : i+j])
			for k := len(stack) - 1; k > 0; k-- {
				if strings.EqualFold(stack[k].tag, name) {
					stack[k].hasClose = true
					stack = stack[:k]
					break
				}
			}
			i += j + 1
		default:
			n, next := startTag(html, i)
			i = next
			top().kids = append(top().kids, n)
			if n.void || n.selfEnd {
				continue
			}
			if t := strings.ToLower(n.tag); t == "script" || t == "style" {
				end := strings.Index(strings.ToLower(html[i:]), "</"+t)
				if end < 0 {
					end = len(html) - i
				}
				if end > 0 {
					n.kids = []*node{{text: html[i : i+end]}}
				}
				i += end
				if j := strings.IndexByte(html[i:], '>'); j >= 0 {
					i += j + 1
					n.hasClose = true
				}
				continue
			}
			stack = append(stack, n)
		}
	}
	return root.kids
}

func startTag(html string, i int) (*node, int) {
	i++
	j := i
	for j < len(html) && !strings.ContainsRune(" \t\n\r/>", rune(html[j])) {
		j++
	}
	n := &node{tag: html[i:j]}
	n.void = voids[strings.ToLower(n.tag)]
	i = j
	for i < len(html) {
		for i < len(html) && strings.ContainsRune(" \t\n\r", rune(html[i])) {
			i++
		}
		if i >= len(html) {
			break
		}
		if html[i] == '>' {
			return n, i + 1
		}
		if strings.HasPrefix(html[i:], "/>") {
			n.selfEnd = true
			return n, i + 2
		}
		j := i
		for j < len(html) && !strings.ContainsRune(" \t\n\r=/>", rune(html[j])) {
			j++
		}
		if j == i {
			i++
			continue
		}
		a := attr{name: html[i:j]}
		i = j
		if i < len(html) && html[i] == '=' {
			i++
			if i < len(html) && (html[i] == '"' || html[i] == '\'') {
				q := html[i]
				k := strings.IndexByte(html[i+1:], q)
				a.value = html[i+1 : i+1+k]
				i += k + 2
			} else {
				k := i
				for k < len(html) && !strings.ContainsRune(" \t\n\r>", rune(html[k])) {
					k++
				}
				a.value, i = html[i:k], k
			}
		} else {
			a.bare = true
		}
		n.attrs = append(n.attrs, a)
	}
	return n, i
}

func (a attr) html() string {
	if a.bare {
		return " " + a.name
	}
	return " " + a.name + `="` + a.value + `"`
}
