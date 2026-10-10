package livetree

import (
	"encoding/json"
	"strconv"
	"strings"
)

// frame is statics around dynamics: len(s) == len(d)+1. A dynamic is a
// string (markup), a *frame, a *block or an inst.
type frame struct {
	fp string // the statics, joined: what their id stands for
	s  []string
	d  []any
}

// block is a loop's items, each with its key.
type block struct {
	items []*frame
	keys  []string
}

// inst is a reference to a stateful component instance, by id.
type inst string

// builder collects a frame's statics and dynamics in order.
type builder struct {
	s   []string
	d   []any
	cur strings.Builder
}

func (b *builder) static(s string) { b.cur.WriteString(s) }

func (b *builder) dyn(v any) {
	b.s = append(b.s, b.cur.String())
	b.cur.Reset()
	b.d = append(b.d, v)
}

func (b *builder) frame() *frame {
	s := append(b.s, b.cur.String())
	return &frame{fp: strings.Join(s, "\x00"), s: s, d: b.d}
}

// split builds the trees of the instances in nodes, the markup of instance
// id: id's frame, then the instances it renders, in render order (preorder).
func split(id string, nodes []*node) (order []string, trees map[string]*frame) {
	trees = map[string]*frame{}
	var build func(id string, nodes []*node, root bool)
	build = func(id string, nodes []*node, root bool) {
		order = append(order, id)
		b := &builder{}
		var children []func()
		// top: ns are the instance's root (its element is the instance's
		// own, not a reference), or an item's (its key makes no block).
		const (
			inner = iota
			instRoot
			itemRoot
		)
		var walk func(ns []*node, top int)
		item := func(n *node) *frame {
			ib := &builder{}
			saved := b
			b = ib
			walk([]*node{n}, itemRoot)
			b = saved
			return ib.frame()
		}
		walk = func(ns []*node, top int) {
			for i := 0; i < len(ns); i++ {
				n := ns[i]
				if n.tag == "" {
					if n.isText {
						b.dyn(n.text)
					} else {
						b.static(n.text)
					}
					continue
				}
				if _, ok := n.attr("data-vk-key"); ok && top == inner {
					bl := &block{}
					for ; i < len(ns) && ns[i].tag != ""; i++ {
						k, ok := ns[i].attr("data-vk-key")
						if !ok {
							break
						}
						bl.items, bl.keys = append(bl.items, item(ns[i])), append(bl.keys, k)
					}
					i--
					b.dyn(bl)
					continue
				}
				if cid, ok := n.attr("data-vk-id"); ok && top != instRoot {
					b.dyn(inst(cid))
					children = append(children, func() { build(cid, []*node{n}, true) })
					continue
				}
				b.static("<" + n.tag)
				for _, a := range n.attrs {
					b.dyn(a.html())
				}
				if n.selfEnd {
					b.static("/>")
				} else {
					b.static(">")
				}
				walk(n.kids, inner)
				if n.hasClose {
					b.static("</" + n.tag + ">")
				}
			}
		}
		if root {
			walk(nodes, instRoot)
		} else {
			walk(nodes, inner)
		}
		trees[id] = b.frame()
		for _, c := range children {
			c()
		}
	}
	build(id, nodes, id != "c0")
	return order, trees
}

// Encoder is a connection's server side of protocol v2: the trees the
// client holds and the statics and strings it has been sent.
type Encoder struct {
	ids  map[string]int // statics fp → id
	strs map[string]int // long markup → id
	prev map[string]*frame
	html map[string]bool // instances the client holds as HTML

	// What the change under way gave ids, undone when it isn't sent.
	newIDs, newStrs []string
}

func (e *Encoder) undo() {
	for _, k := range e.newIDs {
		delete(e.ids, k)
	}
	for _, k := range e.newStrs {
		delete(e.strs, k)
	}
}

// Entry is one instance's item of a message's trees.
type Entry struct {
	ID   string  `json:"id"`
	Full bool    `json:"full,omitempty"`
	Tree any     `json:"tree,omitempty"`
	HTML *string `json:"html,omitempty"`
}

const (
	refMin   = 64   // markup kept by the connection from this length
	patchMin = 1024 // markup patched by tokens from this length
)

// Render is the entries of a render: every instance of the page, whose HTML
// is html, in full. It starts the connection's tables afresh.
func (e *Encoder) Render(html string) []Entry {
	e.ids, e.strs, e.prev, e.html = map[string]int{}, map[string]int{}, map[string]*frame{}, map[string]bool{}
	return e.Patch([]Update{{ID: "c0", HTML: html}})
}

// Update is an instance's new HTML, as a v1 session reports it.
type Update struct {
	ID   string
	HTML string
}

// Patch is the entries for instances whose HTML changed: each instance in
// them whose tree changed, parent first.
func (e *Encoder) Patch(ups []Update) []Entry {
	var out []Entry
	for _, u := range ups {
		order, trees := split(u.ID, parse(u.HTML))
		for _, id := range order {
			f := trees[id]
			old := e.prev[id]
			e.prev[id] = f
			var ch map[string]any
			e.newIDs, e.newStrs = e.newIDs[:0], e.newStrs[:0]
			if old != nil && old.fp == f.fp {
				if ch = e.frameChange(old, f); ch == nil {
					continue
				}
			}
			if ch == nil || e.html[id] {
				e.undo()
				delete(e.html, id)
				out = append(out, Entry{ID: id, Full: true, Tree: e.frame(f)})
				continue
			}
			if !hasInst(f) {
				if h := frameHTML(f); len(h)+16 < jsonLen(ch) {
					e.undo()
					e.html[id] = true // until a full tree
					out = append(out, Entry{ID: id, HTML: &h})
					continue
				}
			}
			out = append(out, Entry{ID: id, Tree: ch})
		}
	}
	return out
}

func jsonLen(v any) int {
	b, _ := json.Marshal(v)
	return len(b)
}

func hasInst(f *frame) bool {
	for _, d := range f.d {
		switch d := d.(type) {
		case inst:
			return true
		case *frame:
			if hasInst(d) {
				return true
			}
		case *block:
			for _, it := range d.items {
				if hasInst(it) {
					return true
				}
			}
		}
	}
	return false
}

func (e *Encoder) frame(f *frame) map[string]any {
	out := map[string]any{}
	id, ok := e.ids[f.fp]
	if !ok {
		id = len(e.ids)
		e.ids[f.fp] = id
		e.newIDs = append(e.newIDs, f.fp)
		out["s"] = f.s
	}
	out["t"] = id
	d := make([]any, len(f.d))
	for i, v := range f.d {
		d[i] = e.dyn(v)
	}
	out["d"] = d
	return out
}

func (e *Encoder) dyn(v any) any {
	switch v := v.(type) {
	case string:
		return e.str(v)
	case *frame:
		return e.frame(v)
	case *block:
		items := make([]any, len(v.items))
		for i, it := range v.items {
			items[i] = e.frame(it)
		}
		return map[string]any{"c": items}
	case inst:
		return map[string]any{"id": string(v)}
	}
	panic("livetree: a dynamic of no kind")
}

func (e *Encoder) str(s string) any {
	if len(s) < refMin {
		return s
	}
	if id, ok := e.strs[s]; ok {
		return map[string]any{"r": id}
	}
	id := len(e.strs)
	e.strs[s] = id
	e.newStrs = append(e.newStrs, s)
	return map[string]any{"r": id, "v": s}
}

// frameChange is the change from a to b, frames with the same statics: nil
// when nothing changed.
func (e *Encoder) frameChange(a, b *frame) map[string]any {
	u := map[string]any{}
	for i := range b.d {
		if c, ok := e.dynChange(a.d[i], b.d[i]); ok {
			u[strconv.Itoa(i)] = c
		}
	}
	if len(u) == 0 {
		return nil
	}
	return map[string]any{"u": u}
}

func (e *Encoder) dynChange(a, b any) (any, bool) {
	switch b := b.(type) {
	case string:
		old, ok := a.(string)
		if ok && old == b {
			return nil, false
		}
		if _, kept := e.strs[b]; ok && !kept && len(old) >= patchMin && len(b) >= patchMin {
			if p := tokenPatch(old, b); p != nil {
				return map[string]any{"p": p}, true
			}
		}
		return e.str(b), true
	case *frame:
		old, ok := a.(*frame)
		if !ok || old.fp != b.fp {
			return e.frame(b), true
		}
		u := e.frameChange(old, b)
		return u, u != nil
	case *block:
		old, ok := a.(*block)
		if !ok {
			return e.dyn(b), true
		}
		return e.blockChange(old, b)
	case inst:
		if old, ok := a.(inst); ok && old == b {
			return nil, false
		}
		return e.dyn(b), true
	}
	panic("livetree: a dynamic of no kind")
}

// blockChange builds the new items from the old: the longest common
// subsequence of keys stays in place (counts, changes), other old keys move
// ({"m":i}), new keys are frames.
func (e *Encoder) blockChange(a, b *block) (any, bool) {
	oldAt := map[string]int{}
	for i, k := range a.keys {
		if _, dup := oldAt[k]; dup {
			return e.dyn(b), true
		}
		oldAt[k] = i
	}
	stay := lcs(a.keys, b.keys) // old index → stays
	var steps []any
	add := func(n int) {
		if k := len(steps); k > 0 {
			if c, ok := steps[k-1].(int); ok && (c > 0) == (n > 0) {
				steps[k-1] = c + n
				return
			}
		}
		steps = append(steps, n)
	}
	cur, changed := 0, len(a.keys) != len(b.keys)
	for j, k := range b.keys {
		i, ok := oldAt[k]
		switch {
		case !ok:
			changed = true
			steps = append(steps, e.frame(b.items[j]))
		case stay[i]:
			if i > cur {
				add(-(i - cur))
				changed = true
			}
			cur = i + 1
			if old := a.items[i]; old.fp != b.items[j].fp {
				changed = true
				add(-1)
				steps = append(steps, e.frame(b.items[j]))
			} else if u := e.frameChange(old, b.items[j]); u != nil {
				changed = true
				steps = append(steps, u)
			} else {
				add(1)
			}
		default:
			changed = true
			m := map[string]any{"m": i}
			if old := a.items[i]; old.fp != b.items[j].fp {
				steps = append(steps, e.frame(b.items[j]))
				continue
			} else if u := e.frameChange(old, b.items[j]); u != nil {
				m["u"] = u["u"]
			}
			steps = append(steps, m)
		}
	}
	if !changed {
		return nil, false
	}
	return map[string]any{"k": steps}, true
}

// lcs marks the old indexes of a longest common subsequence of a and b.
func lcs(a, b []string) map[int]bool {
	n, m := len(a), len(b)
	t := make([][]int, n+1)
	for i := range t {
		t[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	out := map[int]bool{}
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case a[i] == b[j]:
			out[i] = true
			i, j = i+1, j+1
		case t[i+1][j] >= t[i][j+1]:
			i++
		default:
			j++
		}
	}
	return out
}

// tokens cuts markup as the token patch does: before every "<", after every
// ">", `"` and "&#34;".
func tokens(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			if i > start {
				out = append(out, s[start:i])
				start = i
			}
		case '>', '"':
			out = append(out, s[start:i+1])
			start = i + 1
		case ';':
			if i >= 4 && s[i-4:i+1] == "&#34;" {
				out = append(out, s[start:i+1])
				start = i + 1
			}
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// tokenPatch is a token patch from old to new keeping their common prefix
// and suffix, and one [p, n] copy of the old middle's longest run that the
// new middle starts with; nil when it isn't clearly smaller than new.
func tokenPatch(old, new string) []any {
	a, b := tokens(old), tokens(new)
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	var steps []any
	if p > 0 {
		steps = append(steps, p)
	}
	if mid := len(a) - p - s; mid > 0 {
		steps = append(steps, -mid)
	}
	ins := b[p : len(b)-s]
	if len(ins) > 0 && p > 0 && ins[0] == a[0] {
		// a copy from the start of the old string: exercises [p, n].
		n := 0
		for n < len(ins) && n < len(a) && ins[n] == a[n] {
			n++
		}
		steps = append(steps, []int{0, n})
		ins = ins[n:]
	}
	if len(ins) > 0 {
		steps = append(steps, strings.Join(ins, ""))
	}
	if s > 0 {
		steps = append(steps, s)
	}
	if jsonLen(steps)*2 > len(new) {
		return nil
	}
	return steps
}

// frameHTML is the markup of a frame whose dynamics hold no instance.
func frameHTML(f *frame) string {
	var b strings.Builder
	for i, s := range f.s {
		b.WriteString(s)
		if i < len(f.d) {
			switch d := f.d[i].(type) {
			case string:
				b.WriteString(d)
			case *frame:
				b.WriteString(frameHTML(d))
			case *block:
				for _, it := range d.items {
					b.WriteString(frameHTML(it))
				}
			}
		}
	}
	return b.String()
}
