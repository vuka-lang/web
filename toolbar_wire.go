package web

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// What a live page's socket messages carried, for the toolbar's Live panel:
// the client's frame (sensitive form values masked) and each server frame
// the message produced, decoded against a copy of the trees the browser
// holds — so each changed dynamic shows with its path and its old value,
// and a reply's bytes split into statics, dynamics, JS and framing.
// Development only: a connection keeps this copy while the toolbar is on.

const (
	wireFrameMax = 64 << 10 // bytes of a frame kept
	wireRowsMax  = 300      // diff rows kept per frame
	wireValueMax = 400      // characters of a value shown in a diff row
	wirePageMax  = 1 << 20  // bytes of frames a page's record keeps: the oldest messages' go first
)

// weight is about what a frame keeps in memory.
func (f tbFrame) weight() int {
	n := len(f.Body)
	if f.Report != nil {
		for _, r := range f.Report.Rows {
			n += len(r.Path) + len(r.Old) + len(r.New) + len(r.At) + 64
		}
	}
	return n
}

// trimFrames drops the frames of the page's oldest messages while they
// weigh more than wirePageMax.
func (p *tbEntry) trimFrames() {
	p.mu.Lock()
	live := slices.Clone(p.Live)
	p.mu.Unlock()
	total, weights := 0, make([]int, len(live))
	for i, m := range live {
		m.mu.Lock()
		for _, f := range m.Frames {
			weights[i] += f.weight()
		}
		m.mu.Unlock()
		total += weights[i]
	}
	for i := 0; total > wirePageMax && i < len(live)-1; i++ {
		if weights[i] > 0 {
			m := live[i]
			m.mu.Lock()
			m.Frames, m.FramesDropped = nil, true
			m.mu.Unlock()
			total -= weights[i]
		}
	}
}

// tbFrame is one frame of a socket message.
type tbFrame struct {
	Dir       string // "client" or "server"
	Body      string // the frame, at most wireFrameMax bytes
	Size      int    // its whole size
	Truncated bool
	Report    *wireReport // a server frame's decoding
}

func newFrame(dir string, b []byte) tbFrame {
	f := tbFrame{Dir: dir, Size: len(b)}
	if len(b) > wireFrameMax {
		b, f.Truncated = b[:wireFrameMax], true
	}
	f.Body = string(b)
	return f
}

// wireReport is a server frame decoded.
type wireReport struct {
	Type, Error, URL string
	V                int
	Size             int
	Split            []share
	Insts            []instReport
	JS               []string // "c3: 2 commands"
	Rows             []diffRow
	MoreRows         int
}

type share struct {
	Name  string
	Bytes int
	Note  string
}

// instReport is what a frame sent of one instance.
type instReport struct {
	ID, Type string
	Kind     string // "full", "diff", "html"
	Bytes    int    // its entry's size
	Dyn      int    // dynamics changed
	Steps    int    // list steps
	Statics  int    // statics newly sent
	SBytes   int    // their bytes
	Patches  int    // token patches
	Refs     int    // strings sent by reference
	NewRefs  int    // strings the connection keeps from now on
}

// diffRow is a changed dynamic: its path, the value the browser held and
// the new one.
type diffRow struct {
	Path, Old, New, Note string
	At                   string // the markup before the dynamic: where it is
	HasOld               bool
}

// wireMirror is a connection's copy of what the browser holds: v2's statics,
// kept strings and instance trees, or v1's instance HTML.
type wireMirror struct {
	statics map[int][]string
	strs    map[int]string
	inst    map[string]any // *mFrame, or an instance's HTML
	typeOf  func(id string) string
}

type mFrame struct {
	t int
	d []any // string, *mFrame, *mList, mInst
}

type mList struct{ items []*mFrame }

type mInst string

func newWireMirror(typeOf func(string) string) *wireMirror {
	m := &wireMirror{typeOf: typeOf}
	m.reset()
	return m
}

func (m *wireMirror) reset() {
	m.statics, m.strs, m.inst = map[int][]string{}, map[int]string{}, map[string]any{}
}

// decoding is the state of one frame's decoding.
type decoding struct {
	m       *wireMirror
	r       *wireReport
	ir      *instReport
	statics int // bytes of statics
	dyn     int // bytes of dynamics
	js      int
	fresh   int // statics ids first sent
}

func (d *decoding) row(path string, old any, hasOld bool, nw any, note string) {
	if len(d.r.Rows) >= wireRowsMax {
		d.r.MoreRows++
		return
	}
	r := diffRow{Path: path, Note: note, HasOld: hasOld}
	if hasOld {
		r.Old, r.New = shorten(d.m.html(old, 0), d.m.html(nw, 0))
	} else {
		r.New = clip(d.m.html(nw, 0))
	}
	d.r.Rows = append(d.r.Rows, r)
}

// apply decodes a server frame, bringing the mirror up to date.
func (m *wireMirror) apply(b []byte) *wireReport {
	r := &wireReport{Size: len(b)}
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		r.Type = "?"
		return r
	}
	str := func(k string) string { var s string; json.Unmarshal(top[k], &s); return s }
	r.Type, r.Error, r.URL = str("type"), str("error"), str("url")
	json.Unmarshal(top["v"], &r.V)
	d := &decoding{m: m, r: r}
	if r.Type == "render" {
		m.reset()
	}
	if raw, ok := top["html"]; ok { // v1 render
		var html string
		json.Unmarshal(raw, &html)
		d.dyn += len(raw)
		r.Insts = append(r.Insts, instReport{ID: "c0", Type: m.typeOf("c0"), Kind: "html", Bytes: len(raw)})
		old, had := m.inst["c0"]
		m.inst = map[string]any{"c0": html}
		m.v1Instances(html)
		d.row("c0", old, had, html, "the page")
	}
	if raw, ok := top["updates"]; ok { // v1 patch
		var ups []struct {
			ID   string          `json:"id"`
			HTML json.RawMessage `json:"html"`
		}
		json.Unmarshal(raw, &ups)
		for _, u := range ups {
			var html string
			json.Unmarshal(u.HTML, &html)
			d.dyn += len(u.HTML)
			r.Insts = append(r.Insts, instReport{ID: u.ID, Type: m.typeOf(u.ID), Kind: "html", Bytes: len(u.HTML)})
			old, had := m.inst[u.ID]
			m.inst[u.ID] = html
			m.v1Instances(html)
			d.row(u.ID, old, had, html, "")
		}
	}
	if raw, ok := top["trees"]; ok {
		var entries []json.RawMessage
		json.Unmarshal(raw, &entries)
		for _, e := range entries {
			d.entry(e)
		}
	}
	if raw, ok := top["js"]; ok {
		d.js = len(raw) + len(`,"js":`)
		var pushes []struct {
			ID  string            `json:"id"`
			Ops []json.RawMessage `json:"ops"`
		}
		json.Unmarshal(raw, &pushes)
		for _, p := range pushes {
			r.JS = append(r.JS, fmt.Sprintf("%s: %d command%s", p.ID, len(p.Ops), plural(len(p.Ops))))
		}
	}
	note := ""
	if d.fresh > 0 {
		note = fmt.Sprintf("new fingerprints %d", d.fresh)
	}
	r.Split = []share{{"statics", d.statics, note}, {"dynamics", d.dyn, ""}}
	if d.js > 0 {
		r.Split = append(r.Split, share{"js", d.js, ""})
	}
	r.Split = append(r.Split, share{"framing", len(b) - d.statics - d.dyn - d.js, ""})
	return r
}

// entry decodes a v2 trees entry.
func (d *decoding) entry(raw json.RawMessage) {
	var e struct {
		ID   string          `json:"id"`
		Full bool            `json:"full"`
		Tree json.RawMessage `json:"tree"`
		HTML json.RawMessage `json:"html"`
	}
	json.Unmarshal(raw, &e)
	d.r.Insts = append(d.r.Insts, instReport{ID: e.ID, Type: d.m.typeOf(e.ID), Bytes: len(raw)})
	d.ir = &d.r.Insts[len(d.r.Insts)-1]
	old, had := d.m.inst[e.ID]
	switch {
	case e.HTML != nil:
		var html string
		json.Unmarshal(e.HTML, &html)
		d.dyn += len(e.HTML)
		d.ir.Kind = "html"
		d.m.inst[e.ID] = html
		d.row(e.ID, old, had, html, "whole HTML: smaller than the change")
	case e.Full:
		d.ir.Kind = "full"
		f := d.frame(e.Tree)
		d.m.inst[e.ID] = f
		d.row(e.ID, old, had, f, "full tree")
	default:
		d.ir.Kind = "diff"
		var ch struct {
			U json.RawMessage `json:"u"`
		}
		json.Unmarshal(e.Tree, &ch)
		f, ok := old.(*mFrame)
		if !ok {
			f = &mFrame{t: -1}
		}
		d.frameChange(f, ch.U, e.ID)
		d.m.inst[e.ID] = f
	}
}

func obj(raw json.RawMessage) map[string]json.RawMessage {
	var o map[string]json.RawMessage
	if len(raw) > 0 && raw[0] == '{' {
		json.Unmarshal(raw, &o)
	}
	return o
}

func num(raw json.RawMessage) int {
	n, _ := strconv.Atoi(string(raw))
	return n
}

// frame decodes a whole frame {"t","s"?,"d"}.
func (d *decoding) frame(raw json.RawMessage) *mFrame {
	o := obj(raw)
	f := &mFrame{t: num(o["t"])}
	if s, ok := o["s"]; ok {
		var statics []string
		json.Unmarshal(s, &statics)
		if _, known := d.m.statics[f.t]; !known {
			d.fresh++
		}
		d.m.statics[f.t] = statics
		d.statics += len(s) + len(`,"s":`)
		if d.ir != nil {
			d.ir.Statics++
			d.ir.SBytes += len(s) + len(`,"s":`)
		}
	}
	var ds []json.RawMessage
	json.Unmarshal(o["d"], &ds)
	for _, x := range ds {
		f.d = append(f.d, d.value(x))
	}
	return f
}

// value decodes a whole dynamic.
func (d *decoding) value(raw json.RawMessage) any {
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		json.Unmarshal(raw, &s)
		d.dyn += len(raw)
		return s
	}
	o := obj(raw)
	switch {
	case o["t"] != nil:
		return d.frame(raw)
	case o["c"] != nil:
		var items []json.RawMessage
		json.Unmarshal(o["c"], &items)
		l := &mList{}
		for _, it := range items {
			l.items = append(l.items, d.frame(it))
		}
		return l
	case o["r"] != nil:
		return d.ref(o)
	case o["id"] != nil:
		var id string
		json.Unmarshal(o["id"], &id)
		return mInst(id)
	}
	return ""
}

func (d *decoding) ref(o map[string]json.RawMessage) string {
	id := num(o["r"])
	if v, ok := o["v"]; ok {
		var s string
		json.Unmarshal(v, &s)
		d.m.strs[id] = s
		d.dyn += len(v)
		if d.ir != nil {
			d.ir.NewRefs++
		}
		return s
	}
	if d.ir != nil {
		d.ir.Refs++
	}
	if s, ok := d.m.strs[id]; ok {
		return s
	}
	return fmt.Sprintf("‹string %d›", id)
}

// frameChange applies {"i":X,…} to f.
func (d *decoding) frameChange(f *mFrame, raw json.RawMessage, path string) {
	o := obj(raw)
	idx := make([]int, 0, len(o))
	for k := range o {
		if i, err := strconv.Atoi(k); err == nil {
			idx = append(idx, i)
		}
	}
	slices.Sort(idx)
	for _, i := range idx {
		for len(f.d) <= i {
			f.d = append(f.d, nil)
		}
		n := len(d.r.Rows)
		f.d[i] = d.change(f.d[i], o[strconv.Itoa(i)], path+" › u."+strconv.Itoa(i))
		if at := d.m.place(f, i); at != "" {
			for j := n; j < len(d.r.Rows); j++ {
				if d.r.Rows[j].At == "" {
					d.r.Rows[j].At = at
				}
			}
		}
	}
}

// change applies a dynamic's change to old, the new value.
func (d *decoding) change(old any, raw json.RawMessage, path string) any {
	hasOld := old != nil
	if len(raw) > 0 && raw[0] == '"' {
		nw := d.value(raw)
		d.ir.Dyn++
		d.row(path, old, hasOld, nw, "")
		return nw
	}
	o := obj(raw)
	switch {
	case o["u"] != nil:
		f, ok := old.(*mFrame)
		if !ok {
			f = &mFrame{t: -1}
		}
		d.frameChange(f, o["u"], path)
		return f
	case o["k"] != nil:
		l, _ := old.(*mList)
		if l == nil {
			l = &mList{}
		}
		return d.listChange(l, o["k"], path)
	case o["p"] != nil:
		s, ok := old.(string)
		d.dyn += len(o["p"])
		d.ir.Patches++
		d.ir.Dyn++
		var steps []json.RawMessage
		json.Unmarshal(o["p"], &steps)
		nw := patchTokens(s, steps)
		if !ok {
			nw = "‹token patch of a value the toolbar doesn't hold›"
		}
		d.row(path, old, ok, nw, "token patch")
		return nw
	case o["r"] != nil:
		nw := d.ref(o)
		d.ir.Dyn++
		note := "kept string " + string(o["r"])
		if o["v"] != nil {
			note = "string " + string(o["r"]) + ", kept from now on"
		}
		d.row(path, old, hasOld, nw, note)
		return nw
	}
	nw := d.value(raw)
	d.ir.Dyn++
	note := "replaced"
	switch nw.(type) {
	case *mFrame:
		note = "another frame"
	case *mList:
		note = "the whole list"
	case mInst:
		note = "an instance"
	}
	d.row(path, old, hasOld, nw, note)
	return nw
}

// listChange applies loop steps to l.
func (d *decoding) listChange(l *mList, raw json.RawMessage, path string) *mList {
	var steps []json.RawMessage
	json.Unmarshal(raw, &steps)
	d.ir.Steps += len(steps)
	out := &mList{}
	cur := 0
	at := func() string { return path + " › k." + strconv.Itoa(len(out.items)) }
	old := func(i int) *mFrame {
		if i >= 0 && i < len(l.items) {
			return l.items[i]
		}
		return &mFrame{t: -1}
	}
	for _, s := range steps {
		if len(s) > 0 && s[0] != '{' {
			n := num(s)
			if n > 0 {
				for range n {
					out.items = append(out.items, old(cur))
					cur++
				}
				continue
			}
			var gone []string
			for range -n {
				if cur < len(l.items) {
					gone = append(gone, d.m.html(l.items[cur], 0))
				}
				cur++
			}
			d.row(path+" › k."+strconv.Itoa(len(out.items)), strings.Join(gone, ""), true, "", fmt.Sprintf("%d item%s removed", -n, plural(-n)))
			continue
		}
		o := obj(s)
		switch {
		case o["m"] != nil:
			i := num(o["m"])
			it := old(i)
			p := at()
			d.row(p, nil, false, "", fmt.Sprintf("moved from k.%d", i))
			if o["u"] != nil {
				d.frameChange(it, o["u"], p)
			}
			out.items = append(out.items, it)
		case o["u"] != nil:
			it := old(cur)
			cur++
			d.frameChange(it, o["u"], at())
			out.items = append(out.items, it)
		default:
			p := at()
			f := d.frame(s)
			d.row(p, nil, false, f, "new item")
			out.items = append(out.items, f)
		}
	}
	return out
}

// patchTokens applies a token patch to old.
func patchTokens(old string, steps []json.RawMessage) string {
	a := tokenizeHTML(old)
	var b strings.Builder
	cur := 0
	for _, s := range steps {
		switch {
		case len(s) > 0 && s[0] == '"':
			var t string
			json.Unmarshal(s, &t)
			b.WriteString(t)
		case len(s) > 0 && s[0] == '[':
			var pn [2]int
			json.Unmarshal(s, &pn)
			for i := pn[0]; i < pn[0]+pn[1] && i < len(a); i++ {
				b.WriteString(a[i])
			}
		default:
			n := num(s)
			if n < 0 {
				cur -= n
				continue
			}
			for i := cur; i < cur+n && i < len(a); i++ {
				b.WriteString(a[i])
			}
			cur += n
		}
	}
	return b.String()
}

// tokenizeHTML cuts markup as token patches do: before every '<', after
// every '>', '"' and "&#34;".
func tokenizeHTML(html string) []string {
	var out []string
	start := 0
	for i := 0; i < len(html); i++ {
		switch html[i] {
		case '<':
			if i > start {
				out = append(out, html[start:i])
				start = i
			}
		case '>', '"':
			out = append(out, html[start:i+1])
			start = i + 1
		case ';':
			if i >= 4 && html[i-4:i+1] == "&#34;" {
				out = append(out, html[start:i+1])
				start = i + 1
			}
		}
	}
	if start < len(html) {
		out = append(out, html[start:])
	}
	return out
}

// v1Instances keeps the HTML of each instance in a v1 render's markup, so
// that a later update of one shows what it replaced.
func (m *wireMirror) v1Instances(html string) {
	for at := 0; ; {
		i := strings.Index(html[at:], `data-vk-id="`)
		if i < 0 {
			return
		}
		i += at
		at = i + 1
		start := strings.LastIndexByte(html[:i], '<')
		idEnd := strings.IndexByte(html[i+12:], '"')
		if start < 0 || idEnd < 0 {
			continue
		}
		id := html[i+12 : i+12+idEnd]
		name := html[start+1:]
		name = name[:strings.IndexFunc(name, func(r rune) bool { return r == ' ' || r == '>' || r == '/' || r == '\t' || r == '\n' })]
		if end := closeOf(html, start, name); end > start {
			m.inst[id] = html[start:end]
		}
	}
}

// closeOf is the end of the element named name starting at start, counting
// nested elements of the same name; -1 when it isn't closed.
func closeOf(html string, start int, name string) int {
	open, closing := "<"+name, "</"+name+">"
	depth := 0
	for i := start; i < len(html); {
		switch {
		case strings.HasPrefix(html[i:], closing):
			depth--
			i += len(closing)
			if depth == 0 {
				return i
			}
		case strings.HasPrefix(html[i:], open) && i+len(open) < len(html) && strings.IndexByte(" >/\t\n", html[i+len(open)]) >= 0:
			depth++
			i += len(open)
		default:
			i++
		}
	}
	return -1
}

// place is the markup before f's dynamic i, from its tag: where it renders.
func (m *wireMirror) place(f *mFrame, i int) string {
	s, ok := m.statics[f.t]
	if !ok || i >= len(s) {
		return ""
	}
	st := s[i]
	if j := strings.LastIndexByte(st, '<'); j >= 0 {
		st = st[j:]
	}
	st = strings.Join(strings.Fields(st), " ")
	if len(st) > 60 {
		st = st[:28] + "…" + st[len(st)-28:]
	}
	return st
}

// html is a value's markup; an instance in it is ‹c3›, or its own markup
// when depth > 0 (that many levels down).
func (m *wireMirror) html(v any, depth int) string {
	var b strings.Builder
	m.write(&b, v, depth)
	return b.String()
}

func (m *wireMirror) write(b *strings.Builder, v any, depth int) {
	switch v := v.(type) {
	case string:
		b.WriteString(v)
	case *mFrame:
		s, ok := m.statics[v.t]
		for i, x := range v.d {
			if ok && i < len(s) {
				b.WriteString(s[i])
			}
			m.write(b, x, depth)
		}
		if ok && len(s) > len(v.d) {
			b.WriteString(s[len(s)-1])
		}
	case *mList:
		for _, it := range v.items {
			m.write(b, it, depth)
		}
	case mInst:
		if in, ok := m.inst[string(v)]; ok && depth > 0 {
			m.write(b, in, depth-1)
		} else {
			b.WriteString("‹" + string(v) + "›")
		}
	}
}

func clip(s string) string {
	if len(s) > wireValueMax {
		return s[:wireValueMax] + "…"
	}
	return s
}

// shorten shows two long values around where they differ.
func shorten(a, b string) (string, string) {
	if len(a) <= wireValueMax && len(b) <= wireValueMax {
		return a, b
	}
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	cut := func(s string) string {
		from, to := max(0, pre-40), min(len(s), len(s)-suf+40)
		out := s[from:to]
		if from > 0 {
			out = "…" + out
		}
		if to < len(s) {
			out += "…"
		}
		return clip(out)
	}
	return cut(a), cut(b)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// The client frame, masked: a password field's values, and fields named like
// a password, token or secret.

var (
	inputTag     = regexp.MustCompile(`(?is)<input\b[^>]*>`)
	passwordType = regexp.MustCompile(`(?i)\btype\s*=\s*["']?password\b`)
	nameAttr     = regexp.MustCompile(`(?i)\bname\s*=\s*["']?([^"'\s>]+)`)
	secretName   = regexp.MustCompile(`(?i)pass|token|secret|pin\b|otp|cvc|cvv`)
)

const hidden = "(hidden)"

// maskClient is the client frame as the toolbar keeps it.
func (m *wireMirror) maskClient(data []byte) []byte {
	var msg map[string]json.RawMessage
	if json.Unmarshal(data, &msg) != nil || msg["form"] == nil && msg["value"] == nil {
		return data
	}
	var target string
	json.Unmarshal(msg["target"], &target)
	names, onPassword := m.passwordFields(target)
	changed := false
	if raw := msg["form"]; raw != nil {
		var form url.Values
		if json.Unmarshal(raw, &form) == nil {
			for k, vs := range form {
				if names[k] || secretName.MatchString(k) {
					for i := range vs {
						vs[i] = hidden
					}
					changed = true
				}
			}
			msg["form"], _ = json.Marshal(form)
		}
	}
	if msg["value"] != nil && onPassword {
		msg["value"], _ = json.Marshal(hidden)
		changed = true
	}
	if !changed {
		return data
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.Encode(msg)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// passwordFields reads the target's instance, as the browser holds it: the
// names of its password inputs, and whether the target is one.
func (m *wireMirror) passwordFields(target string) (map[string]bool, bool) {
	id, _, _ := strings.Cut(target, ":")
	in, ok := m.inst[id]
	if !ok {
		return nil, false
	}
	names, on := map[string]bool{}, false
	for _, tag := range inputTag.FindAllString(m.html(in, 8), -1) {
		if !passwordType.MatchString(tag) {
			continue
		}
		if n := nameAttr.FindStringSubmatch(tag); n != nil {
			names[n[1]] = true
		}
		if target != "" && strings.Contains(tag, `"`+target+`"`) {
			on = true
		}
	}
	return names, on
}

// The views of a message's frames: Summary, Diff and Raw.

type wireView struct {
	Seq     int
	Client  *frameView
	Server  []frameView
	Changes int // diff rows in all
}

type frameView struct {
	tbFrame
	Label, Size, Split, Detail string
	Bars                       []shareBar
	JSON                       template.HTML
}

type shareBar struct {
	Name, Text string
	Style      template.CSS
}

func renderWire(w io.Writer, m tbRecord) error {
	v := wireView{Seq: m.Seq}
	for _, f := range m.Frames {
		fv := frameView{tbFrame: f, Size: bytesText(f.Size), JSON: highlightJSON(f.Body, f.Truncated)}
		if f.Dir == "client" {
			fv.Label, fv.Detail = clientLabel(f.Body)
			v.Client = &fv
			continue
		}
		if r := f.Report; r != nil {
			fv.Label = r.Type
			if r.V == 2 || len(r.Insts) > 0 && r.Insts[0].Kind != "html" {
				fv.Label += " · v2"
			}
			fv.Split = splitText(r)
			for _, s := range r.Split {
				if s.Bytes > 0 && r.Size > 0 {
					fv.Bars = append(fv.Bars, shareBar{s.Name, fmt.Sprintf("%s %s", s.Name, bytesText(s.Bytes)),
						template.CSS(fmt.Sprintf("width:%.2f%%", 100*float64(s.Bytes)/float64(r.Size)))})
				}
			}
			v.Changes += len(r.Rows)
		}
		v.Server = append(v.Server, fv)
	}
	return panelTemplates.ExecuteTemplate(w, "wire", v)
}

// clientLabel is a client frame's type and what it says: "c1:1 click",
// "/pets?page=2 (patch)", "form name, password".
func clientLabel(body string) (string, string) {
	var m struct {
		Type, Target, Event, Key, URL string
		V                             int
		Patch                         bool
		Form                          map[string]json.RawMessage
	}
	json.Unmarshal([]byte(body), &m)
	var parts []string
	switch m.Type {
	case "join":
		if m.V > 0 {
			parts = append(parts, "protocol v"+strconv.Itoa(m.V))
		}
	case "navigate":
		parts = append(parts, m.URL)
		if m.Patch {
			parts = append(parts, "(patch)")
		}
	default:
		parts = append(parts, m.Target, m.Event)
		if m.Key != "" {
			parts = append(parts, "key "+m.Key)
		}
		if len(m.Form) > 0 {
			names := slices.Sorted(maps.Keys(m.Form))
			parts = append(parts, "form "+strings.Join(names, ", "))
		}
	}
	return cmp.Or(m.Type, "message"), strings.TrimSpace(strings.Join(parts, " "))
}

// splitText is a frame's bytes by what they carry: "974 B: statics 610 B
// (new fingerprints 2), dynamics 280 B, framing 84 B".
func splitText(r *wireReport) string {
	var parts []string
	for _, s := range r.Split {
		p := s.Name + " " + bytesText(s.Bytes)
		if s.Note != "" {
			p += " (" + s.Note + ")"
		}
		parts = append(parts, p)
	}
	return bytesText(r.Size) + ": " + strings.Join(parts, ", ")
}

// highlightJSON pretty-prints a frame and colours its keys, strings,
// numbers and literals; a truncated frame is shown as it is.
func highlightJSON(body string, truncated bool) template.HTML {
	src := body
	if !truncated {
		var b bytes.Buffer
		if json.Indent(&b, []byte(body), "", "  ") == nil {
			src = b.String()
		}
	}
	var out strings.Builder
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j+1, len(src))
			k := j
			for k < len(src) && (src[k] == ' ' || src[k] == '\t') {
				k++
			}
			class := "js"
			if k < len(src) && src[k] == ':' {
				class = "jk"
			}
			out.WriteString(`<span class="` + class + `">` + template.HTMLEscapeString(src[i:j]) + `</span>`)
			i = j
		case c == '-' || c >= '0' && c <= '9':
			j := i + 1
			for j < len(src) && strings.IndexByte("0123456789.eE+-", src[j]) >= 0 {
				j++
			}
			out.WriteString(`<span class="jn">` + src[i:j] + `</span>`)
			i = j
		case strings.HasPrefix(src[i:], "true") || strings.HasPrefix(src[i:], "false") || strings.HasPrefix(src[i:], "null"):
			n := 4
			if c == 'f' {
				n = 5
			}
			out.WriteString(`<span class="jb">` + src[i:i+n] + `</span>`)
			i += n
		default:
			j := i + 1
			for j < len(src) && strings.IndexByte(`"-0123456789tfn`, src[j]) < 0 {
				j++
			}
			out.WriteString(template.HTMLEscapeString(src[i:j]))
			i = j
		}
	}
	return template.HTML(out.String())
}

// marked is one side of a changed value, quoted, the part that differs from
// the other side marked.
func marked(a, b string, side int) template.HTML {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	for pre > 0 && !utf8.RuneStart(a[pre]) && pre < len(a) {
		pre--
	}
	for suf > 0 && len(a)-suf < len(a) && !utf8.RuneStart(a[len(a)-suf]) {
		suf--
	}
	s := a
	if side == 1 {
		s = b
	}
	esc := template.HTMLEscapeString
	mid := s[pre : len(s)-suf]
	out := `"` + esc(s[:pre])
	if mid != "" {
		out += "<mark>" + esc(mid) + "</mark>"
	}
	return template.HTML(out + esc(s[len(s)-suf:]) + `"`)
}
