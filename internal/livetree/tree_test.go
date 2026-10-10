package livetree

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The browser runtime's v2 code (live.js) runs in node over messages this
// encoder makes for random pages and random changes to them: after every
// message the page the client renders from its trees must be the page.

type gnode struct {
	kind  byte // t text, e element, l keyed list, c instance
	text  string
	tag   string
	attrs [][2]string
	kids  []*gnode
	keys  []int
	id    string
}

type gen struct {
	r    *rand.Rand
	next int // instance ids
	keys int
}

var words = []string{"a", "milk", "eggs", "x &amp; y", "&#34;q&#34;", "bread", "≈ü", "1", "42"}

func (g *gen) word() string {
	switch g.r.Intn(12) {
	case 0:
		return strings.Repeat("long text ", 7+g.r.Intn(3)) // kept by reference
	case 1:
		return strings.Repeat("item &#34;plain&#34; and more ", 40+g.r.Intn(3)) // token patched
	}
	return words[g.r.Intn(len(words))]
}

func (g *gen) attrs() [][2]string {
	var out [][2]string
	for i, n := 0, g.r.Intn(3); i < n; i++ {
		out = append(out, [2]string{[]string{"class", "title", "data-vk-on-click", "value"}[i], g.word()})
	}
	if g.r.Intn(5) == 0 {
		out = append(out, [2]string{"checked", ""})
	}
	return out
}

func (g *gen) node(depth int) *gnode {
	switch k := g.r.Intn(10); {
	case k < 3 || depth > 3:
		return &gnode{kind: 't', text: g.word()}
	case k < 6:
		return &gnode{kind: 'e', tag: []string{"p", "span", "ul", "em"}[g.r.Intn(4)], attrs: g.attrs(), kids: g.nodes(depth + 1)}
	case k < 8:
		l := &gnode{kind: 'l'}
		for i, n := 0, 1+g.r.Intn(4); i < n; i++ {
			g.addItem(l, i, depth)
		}
		return l
	default:
		return g.comp(depth)
	}
}

func (g *gen) comp(depth int) *gnode {
	g.next++
	return &gnode{kind: 'c', id: "c" + strconv.Itoa(g.next), tag: []string{"div", "section"}[g.r.Intn(2)], attrs: g.attrs(), kids: g.nodes(depth + 1)}
}

func (g *gen) addItem(l *gnode, at, depth int) {
	g.keys++
	var it *gnode
	if g.r.Intn(4) == 0 {
		it = g.comp(depth + 1)
	} else {
		it = &gnode{kind: 'e', tag: "li", attrs: g.attrs(), kids: g.nodes(depth + 1)}
	}
	l.kids = append(l.kids[:at], append([]*gnode{it}, l.kids[at:]...)...)
	l.keys = append(l.keys[:at], append([]int{g.keys}, l.keys[at:]...)...)
}

func (g *gen) nodes(depth int) []*gnode {
	var out []*gnode
	for i, n := 0, g.r.Intn(4); i < n; i++ {
		out = append(out, g.node(depth))
	}
	return out
}

func render(n *gnode, key string, b *strings.Builder) {
	switch n.kind {
	case 't':
		b.WriteString(n.text)
	case 'e', 'c':
		b.WriteString("<" + n.tag)
		for _, a := range n.attrs {
			if a[0] == "checked" {
				b.WriteString(" checked")
			} else {
				b.WriteString(" " + a[0] + `="` + a[1] + `"`)
			}
		}
		if n.kind == 'c' {
			b.WriteString(` data-vk-id="` + n.id + `"`)
		}
		if key != "" {
			b.WriteString(` data-vk-key="` + key + `"`)
		}
		b.WriteString(">")
		for _, k := range n.kids {
			render(k, "", b)
		}
		b.WriteString("</" + n.tag + ">")
	case 'l':
		for i, k := range n.kids {
			render(k, strconv.Itoa(n.keys[i]), b)
		}
	}
}

// mutate changes the tree in place, somewhere.
func (g *gen) mutate(root *gnode) {
	var all []*gnode
	var walk func(n *gnode)
	walk = func(n *gnode) {
		all = append(all, n)
		for _, k := range n.kids {
			walk(k)
		}
	}
	walk(root)
	n := all[g.r.Intn(len(all))]
	switch n.kind {
	case 't':
		if g.r.Intn(2) == 0 && len(n.text) > 100 {
			n.text = strings.Replace(n.text, "plain", "changed", 1+g.r.Intn(3))
		} else {
			n.text = g.word()
		}
	case 'e', 'c':
		switch g.r.Intn(5) {
		case 0:
			n.attrs = g.attrs()
		case 1:
			if n.kind == 'e' {
				n.tag = []string{"p", "span", "ul", "em"}[g.r.Intn(4)]
			}
		case 2:
			if len(n.kids) > 0 {
				i := g.r.Intn(len(n.kids))
				n.kids = append(n.kids[:i], n.kids[i+1:]...)
			}
		default:
			n.kids = append(n.kids, g.node(2))
		}
	case 'l':
		switch g.r.Intn(4) {
		case 0:
			g.addItem(n, g.r.Intn(len(n.kids)+1), 2)
		case 1:
			if len(n.kids) > 1 {
				i := g.r.Intn(len(n.kids))
				n.kids, n.keys = append(n.kids[:i], n.kids[i+1:]...), append(n.keys[:i], n.keys[i+1:]...)
			}
		default:
			g.r.Shuffle(len(n.kids), func(i, j int) {
				n.kids[i], n.kids[j] = n.kids[j], n.kids[i]
				n.keys[i], n.keys[j] = n.keys[j], n.keys[i]
			})
		}
	}
}

type step struct {
	Name string            `json:"name,omitempty"`
	Msg  any               `json:"msg"`
	Page string            `json:"page"`
	HTML map[string]string `json:"html,omitempty"`
}

type fixture struct {
	Name  string `json:"name"`
	Steps []step `json:"steps"`
}

func page(root *gnode) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>t</title></head><body><main>")
	for _, k := range root.kids {
		render(k, "", &b)
	}
	b.WriteString("</main></body></html>")
	return b.String()
}

// treeHTML is the HTML of instance id from the encoder's trees.
func (e *Encoder) treeHTML(id string) string {
	var fh func(f *frame) string
	fh = func(f *frame) string {
		var b strings.Builder
		for i, s := range f.s {
			b.WriteString(s)
			if i < len(f.d) {
				switch d := f.d[i].(type) {
				case string:
					b.WriteString(d)
				case *frame:
					b.WriteString(fh(d))
				case *block:
					for _, it := range d.items {
						b.WriteString(fh(it))
					}
				case inst:
					b.WriteString(e.treeHTML(string(d)))
				}
			}
		}
		return b.String()
	}
	return fh(e.prev[id])
}

func TestRandomTreesInNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	var fixtures []fixture
	counts := map[string]int{}
	seeds := 300
	if n, err := strconv.Atoi(os.Getenv("LIVETREE_SEEDS")); err == nil {
		seeds = n
	}
	for seed := 1; seed <= seeds; seed++ {
		g := &gen{r: rand.New(rand.NewSource(int64(seed)))}
		root := &gnode{kind: 'e', kids: g.nodes(0)}
		root.kids = append(root.kids, g.comp(1))
		var e Encoder
		html := page(root)
		fx := fixture{Name: fmt.Sprint("seed ", seed)}
		fx.Steps = append(fx.Steps, step{Msg: Message{Type: "render", V: 2, Trees: e.Render(html)}, Page: html})
		if got := e.treeHTML("c0"); got != html {
			t.Fatalf("seed %d: trees render\n%s\nnot\n%s", seed, got, html)
		}
		for i := 0; i < 12; i++ {
			for n := 1 + g.r.Intn(3); n > 0; n-- {
				g.mutate(root)
			}
			html := page(root)
			trees := e.Patch([]Update{{"c0", html}})
			b, _ := json.Marshal(trees)
			for _, k := range []string{`"m":`, `"k":`, `"p":`, `"r":`, `"html":`, `"full":true`, `"u":`} {
				counts[k] += strings.Count(string(b), k)
			}
			fx.Steps = append(fx.Steps, step{Msg: Message{Type: "patch", Ref: i + 1, Trees: trees}, Page: html})
		}
		fixtures = append(fixtures, fx)
	}
	t.Logf("random messages use: %v", counts)
	for _, k := range []string{`"m":`, `"k":`, `"p":`, `"r":`, `"html":`, `"u":`} {
		if counts[k] == 0 {
			t.Errorf("no message used %s", k)
		}
	}
	file := filepath.Join(t.TempDir(), "random.json")
	b, _ := json.Marshal(fixtures)
	os.WriteFile(file, b, 0o644)
	out, err := exec.Command(node, "../../testdata/livejs/run.js", "../../live.js", file, "../../testdata/livejs/spec.json").CombinedOutput()
	t.Logf("node: %s", lastLine(out))
	if err != nil {
		t.Fatalf("%s", out)
	}
}

func lastLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	return s[strings.LastIndexByte(s, '\n')+1:]
}

func TestBlockSteps(t *testing.T) {
	var e Encoder
	e.Render("")
	item := func(k, text string) *frame {
		return &frame{fp: "<tr\x00><td>\x00</td></tr>", s: []string{"<tr", "><td>", "</td></tr>"}, d: []any{` data-vk-key="` + k + `"`, text}}
	}
	bl := func(ks ...string) *block {
		b := &block{}
		for _, k := range ks {
			b.items, b.keys = append(b.items, item(k, "row "+k)), append(b.keys, k)
		}
		return b
	}
	// The spec's example: keys 1 2 3 4 become 4 1 5 3.
	e.frame(item("0", "")) // the item statics are known
	ch, ok := e.blockChange(bl("1", "2", "3", "4"), bl("4", "1", "5", "3"))
	got, _ := json.Marshal(ch)
	want := `{"k":[{"m":3},1,{"d":[" data-vk-key=\"5\"","row 5"],"t":1},-1,1]}`
	if !ok || string(got) != want {
		t.Errorf("move: %s, want %s", got, want)
	}
	if _, ok := e.blockChange(bl("1", "2"), bl("1", "2")); ok {
		t.Error("an unchanged block changed")
	}
	b2 := bl("1", "2")
	b2.items[1] = item("2", "new")
	ch, _ = e.blockChange(bl("1", "2"), b2)
	if got, _ := json.Marshal(ch); string(got) != `{"k":[1,{"u":{"1":"new"}}]}` {
		t.Errorf("change: %s", got)
	}
	ch, _ = e.blockChange(bl("1", "2", "3"), bl("1"))
	if got, _ := json.Marshal(ch); string(got) != `{"k":[1]}` {
		t.Errorf("truncate: %s", got)
	}
}

func TestEntries(t *testing.T) {
	var e Encoder
	leaf := func(v string) string {
		return `<div><p data-vk-id="c1">` + strings.Repeat(`<i class="`+v+`">`+v+`</i>`, 6) + `</p><b data-vk-id="c2">` + v + `</b></div>`
	}
	trees := e.Render(leaf("a"))
	if len(trees) != 3 || trees[0].ID != "c0" || trees[1].ID != "c1" || trees[2].ID != "c2" || !trees[1].Full {
		t.Fatalf("render: %+v", trees)
	}
	trees = e.Patch([]Update{{"c0", leaf("b")}})
	if len(trees) != 2 || trees[0].ID != "c1" || trees[0].HTML == nil || trees[1].ID != "c2" || trees[1].Tree == nil {
		t.Fatalf("patch: %s", must(json.Marshal(trees)))
	}
	trees = e.Patch([]Update{{"c1", `<p data-vk-id="c1">` + strings.Repeat(`<i class="b">b</i>`, 6) + `</p>`}})
	if len(trees) != 0 {
		t.Fatalf("no change: %s", must(json.Marshal(trees)))
	}
	trees = e.Patch([]Update{{"c1", `<p data-vk-id="c1">` + strings.Repeat(`<i class="b">c</i>`, 6) + `</p>`}})
	if len(trees) != 1 || !trees[0].Full {
		t.Fatalf("after html, a full tree: %s", must(json.Marshal(trees)))
	}
	if !reflect.DeepEqual(tokens(`<a title="&#34;x&#34;">y</a>`), []string{`<a title="`, `&#34;`, `x&#34;`, `"`, `>`, `y`, `</a>`}) {
		t.Errorf("tokens: %q", tokens(`<a title="&#34;x&#34;">y</a>`))
	}
}

func must(b []byte, _ error) string { return string(b) }
