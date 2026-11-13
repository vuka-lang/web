package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// The frame-scoped morph (live.js, loop): a patch to a loop that is all of
// its container's content morphs only the changed rows. These pages render
// with templates (ui.HTML), whose loops travel as blocks of item frames.
// The browser tests run live.js's verify mode, which morphs the whole
// instance again after each row-by-row morph and fails the test when that
// changes anything.

type scopedRow struct {
	ID, N int
}

type scopedGroup struct {
	Name  string
	Items []int
}

type scopedLog struct {
	mu   sync.Mutex
	hits []string
}

func (l *scopedLog) add(s string) { l.mu.Lock(); l.hits = append(l.hits, s); l.mu.Unlock() }
func (l *scopedLog) got() string  { l.mu.Lock(); defer l.mu.Unlock(); return strings.Join(l.hits, ",") }

type scopedPage struct {
	ui.Live
	Log    *scopedLog
	rows   []scopedRow
	groups []scopedGroup
	comps  []int
	plain  []int
	framed []int
	items  ui.Stream[scopedRow]
	next   int
	n      int
}

func (p *scopedPage) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }
func (p *scopedPage) mount() error {
	p.items.Reset(scopedRow{ID: 1}, scopedRow{ID: 2}, scopedRow{ID: 3})
	p.next = 3
	return nil
}
func (p *scopedPage) StreamTop() {
	p.next++
	p.items.Prepend(scopedRow{ID: p.next})
}
func (p *scopedPage) StreamEdit() { p.items.Insert(scopedRow{ID: 2, N: 7}) }
func (p *scopedPage) PlainBump()  { p.plain[2]++ }
func (p *scopedPage) FramedBump() { p.framed[2]++ }

func newScopedPage(log *scopedLog) *scopedPage {
	p := &scopedPage{Log: log}
	for i := range 9 {
		p.rows = append(p.rows, scopedRow{ID: i + 1, N: 0})
	}
	for g := range 3 {
		p.groups = append(p.groups, scopedGroup{Name: "g" + strconv.Itoa(g), Items: []int{0, 0, 0, 0}})
	}
	p.comps = []int{1, 2, 3, 4}
	p.plain = []int{0, 0, 0, 0, 0}
	p.framed = []int{0, 0, 0, 0, 0}
	return p
}

func (p *scopedPage) Inc()  { p.n++ }
func (p *scopedPage) Bump() { p.rows[len(p.rows)/2].N++ }
func (p *scopedPage) Deep() { p.groups[1].Items[2]++ }
func (p *scopedPage) Insert() {
	p.rows = append([]scopedRow{{ID: 100 + len(p.rows)}}, p.rows...)
}
func (p *scopedPage) Rekey() { p.rows[len(p.rows)/2].ID += 1000 }

// hit logs the row's id and bumps the middle row: the row clicked is
// another than the one changed.
func (p *scopedPage) hit(id int) {
	p.Log.add(strconv.Itoa(id))
	p.Bump()
}

func (p *scopedPage) Render() ui.Node {
	var rows, groups, comps []ui.Node
	for _, r := range p.rows {
		id := r.ID
		rows = append(rows, ui.HTML(`<li key={k} data-id={id}><span class="n">{n}</span><input class="f" name={name}/><input type="checkbox" class="c"/><button class="hit" onClick={hit}>hit</button><b class="x" id={ign} data-vk-ignore>ign</b></li>`,
			id, id, r.N, "f"+strconv.Itoa(id), func() { p.hit(id) }, "ign"+strconv.Itoa(id)))
	}
	for gi, g := range p.groups {
		var inner []ui.Node
		for k, v := range g.Items {
			inner = append(inner, ui.HTML(`<li key={ik}>{iv}</li>`, k, v))
		}
		groups = append(groups, ui.HTML(`<section key={gk} class="g"><h2>{gname}</h2><ul class="inner">{inner}</ul></section>`, gi, g.Name, inner))
	}
	var plain, items, framed, bound []ui.Node
	for k, v := range p.framed {
		framed = append(framed, ui.HTML(`<li key={fk}>{fv}</li>`, k, v))
		bound = append(bound, ui.HTML(`<li key={bk} title="hi" data-vk-b-title='"hi"'><i>{bv}</i></li>`, k, v))
	}
	for _, v := range p.plain {
		plain = append(plain, ui.HTML(`<li><i>{pv}</i></li>`, v))
	}
	for _, it := range p.items.Rows() {
		items = append(items, ui.HTML(`<li key={sid}><span>{sn}</span></li>`, it.ID, it.N))
	}
	block := p.items.Block(items)
	for _, c := range p.comps {
		comps = append(comps, ui.HTML(`<li key={ck}>{box}</li>`, c, ui.Component("comp"+strconv.Itoa(c), nil, &scopedBox{})))
	}
	return ui.HTML(`<main><h1 id="count">{n}</h1>
<button id="inc" onClick={inc}>inc</button>
<button id="bump" onClick={bump}>bump</button>
<button id="deep" onClick={deep}>deep</button>
<button id="insert" onClick={insert}>insert</button>
<button id="rekey" onClick={rekey}>rekey</button>
<button id="plainbump" onClick={plainbump}>plain</button>
<button id="fbump" onClick={fbump}>framed</button>
<button id="stop" onClick={stop}>stream top</button>
<button id="sedit" onClick={sedit}>stream edit</button>
<input id="outside" name="outside"/>
<ul id="rows">{rows}</ul>
<div id="groups">{groups}</div>
<ol id="comps">{comps}</ol>
<ol id="plain">{plain}</ol>
<ul id="framed">
  <li class="first">first</li>
  {framed}
  <li class="last">last</li>
</ul>
<ol id="bound">{bound}</ol>
<ul id="items" {attrs...}>{block}</ul>
</main>`, p.n, p.Inc, p.Bump, p.Deep, p.Insert, p.Rekey, p.PlainBump, p.FramedBump, p.StreamTop, p.StreamEdit, rows, groups, comps, plain, framed, bound, block.Attrs(), block)
}

// scopedBox is a stateful component in each row of a loop.
type scopedBox struct {
	ui.Live
	n int
}

func (b *scopedBox) Inc() { b.n++ }
func (b *scopedBox) Render() ui.Node {
	return ui.HTML(`<span class="box"><i class="bn">{n}</i><button class="binc" onClick={inc}>+</button></span>`, b.n, b.Inc)
}

func scopedServer(t *testing.T, opts ...Option) (*chrome.Page, *scopedLog) {
	t.Helper()
	log := &scopedLog{}
	a := New(append([]Option{Only("none")}, opts...)...)
	a.Live("/scoped", func() *scopedPage { return newScopedPage(log) })
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Open(srv.URL + "/scoped")
	// the first patch maps the loops' rows to their items
	clickJS(p, "#inc")
	p.WaitText("#count", "1")
	return p, log
}

// scopedRuns is how many times live.js has morphed the container of id
// row by row.
func scopedRuns(p *chrome.Page, id string) int {
	var n int
	p.Eval(fmt.Sprintf(`(window.__vkScoped || {})[%q] || 0`, id), &n)
	return n
}

// tagRows marks every element of sel with its text, as a property: a node
// a morph replaced has none.
func tagRows(p *chrome.Page, sel string) {
	p.Eval(fmt.Sprintf(`document.querySelectorAll(%q).forEach((e, i) => { e.__tag = "t" + i; }); 0`, sel), nil)
}

func untagged(t *testing.T, p *chrome.Page, sel string) {
	t.Helper()
	var bad []string
	p.Eval(fmt.Sprintf(`Array.from(document.querySelectorAll(%q)).map((e, i) => e.__tag === "t" + i ? "" : "row " + i).filter(Boolean)`, sel), &bad)
	if len(bad) > 0 {
		t.Fatalf("%s: %v aren't the elements they were", sel, bad)
	}
}

func clickWait(t *testing.T, p *chrome.Page, sel, waitJS string) {
	t.Helper()
	clickJS(p, sel)
	p.Wait(waitJS)
}

// TestScopedRowsKeepIdentity: a patch changing one row, and one changing
// only a counter beside the loop, morph row by row; every row stays the
// element it was, the changed one showing its new value.
func TestScopedRowsKeepIdentity(t *testing.T) {
	p, _ := scopedServer(t)
	tagRows(p, "#rows > li")
	before := scopedRuns(p, "rows")
	clickWait(t, p, "#bump", `document.querySelectorAll("#rows .n")[4].textContent === "1"`)
	clickWait(t, p, "#inc", `document.querySelector("#count").textContent === "2"`)
	if got := scopedRuns(p, "rows") - before; got < 2 {
		t.Fatalf("%d row-by-row morphs, want each patch's", got)
	}
	untagged(t, p, "#rows > li")
	if got := p.Text("#rows"); !strings.Contains(got, "1") {
		t.Fatalf("rows %q", got)
	}
}

// TestScopedFocusTyping: what a visitor types in a field, and its
// selection, stay through a row-by-row morph: in a row left alone, in the
// changed row (a cell beside it changing), and outside the loop.
func TestScopedFocusTyping(t *testing.T) {
	p, _ := scopedServer(t)
	for _, c := range []struct {
		field, name string
	}{{`#rows input[name="f1"]`, "clean row"}, {`#rows input[name="f5"]`, "changed row"}, {`#outside`, "outside"}} {
		t.Run(c.name, func(t *testing.T) {
			p.Click(c.field)
			p.Keys("abcd")
			p.Eval(fmt.Sprintf(`document.querySelector(%q).setSelectionRange(1, 3); 0`, c.field), nil)
			before := scopedRuns(p, "rows")
			var was string
			p.Eval(`document.querySelectorAll("#rows .n")[4].textContent`, &was)
			n, _ := strconv.Atoi(was)
			clickWait(t, p, "#bump", fmt.Sprintf(`document.querySelectorAll("#rows .n")[4].textContent === "%d"`, n+1))
			if scopedRuns(p, "rows") == before {
				t.Fatal("the patch didn't morph row by row")
			}
			var got string
			p.Eval(fmt.Sprintf(`(() => { const f = document.querySelector(%q); return (document.activeElement === f) + ":" + f.value + ":" + f.selectionStart + "-" + f.selectionEnd; })()`, c.field), &got)
			if got != "true:abcd:1-3" {
				t.Fatalf("the field after the patch: %s, want focused, abcd, 1-3", got)
			}
		})
	}
}

// TestScopedBusy: a button waiting for its event's reply is busy, and the
// reply — a patch to another row, morphed row by row — ends that.
func TestScopedBusy(t *testing.T) {
	var delay atomic.Int64
	p, log := scopedServer(t, GuardLiveEvents(func(context.Context, LiveHandler) error {
		time.Sleep(time.Duration(delay.Load()))
		return nil
	}))
	delay.Store(int64(400 * time.Millisecond))
	before := scopedRuns(p, "rows")
	clickJS(p, `#rows li[data-id="1"] .hit`)
	p.Wait(`document.querySelector('#rows li[data-id="1"] .hit').getAttribute("aria-busy") === "true"`)
	p.Wait(`document.querySelectorAll("#rows .n")[4].textContent === "1"`)
	p.Wait(`!document.querySelector('#rows li[data-id="1"] .hit').hasAttribute("aria-busy")`)
	if scopedRuns(p, "rows") == before {
		t.Fatal("the reply didn't morph row by row")
	}
	if log.got() != "1" {
		t.Fatalf("hits %q", log.got())
	}
}

// TestScopedForeignAndIgnored: what scripts did inside rows — an element
// put in one, a drawing in an ignored element, an attribute of their own
// and one named as the runtime's — comes out of a row-by-row morph as out
// of a whole one (the rows they touched morphed again: verify mode checks
// it); an element a script added to the container itself, which a morph
// keeps, has the patches morph the container whole.
func TestScopedForeignAndIgnored(t *testing.T) {
	p, _ := scopedServer(t)
	p.Eval(`document.querySelector('#rows li[data-id="2"]').appendChild(document.createElement("i")).className = "foreign";
		document.querySelector("#ign2").textContent = "drawn";
		document.querySelector('#rows li[data-id="3"]').setAttribute("data-open", "yes");
		document.querySelector('#rows li[data-id="7"] .hit').setAttribute("data-vk-stray", ""); 0`, nil)
	before := scopedRuns(p, "rows")
	clickWait(t, p, "#bump", `document.querySelectorAll("#rows .n")[4].textContent === "1"`)
	if scopedRuns(p, "rows") == before {
		t.Fatal("the patch didn't morph row by row")
	}
	var got string
	p.Eval(`[!!document.querySelector('#rows li[data-id="2"] i.foreign'), document.querySelector("#ign2").textContent,
		document.querySelector('#rows li[data-id="3"]').getAttribute("data-open"),
		document.querySelector('#rows li[data-id="7"] .hit').hasAttribute("data-vk-stray")].join(":")`, &got)
	if got != "true:drawn:yes:false" {
		t.Fatalf("foreign element, ignored element, script's attribute, runtime-named attribute: %s", got)
	}
	p.Eval(`document.querySelector("#rows").appendChild(document.createElement("li")).className = "added"; 0`, nil)
	before = scopedRuns(p, "rows")
	clickWait(t, p, "#bump", `document.querySelectorAll("#rows .n")[4].textContent === "2"`)
	if scopedRuns(p, "rows") != before {
		t.Fatal("a container a script added to was morphed row by row")
	}
	clickWait(t, p, "#bump", `document.querySelectorAll("#rows .n")[4].textContent === "3"`)
	if scopedRuns(p, "rows") != before {
		t.Fatal("a container holding an element a script added was morphed row by row")
	}
	var kept bool
	if p.Eval(`document.querySelector("#rows > li.added") !== null`, &kept); !kept {
		t.Fatal("the morph took out the element a script added")
	}
}

// TestScopedNestedLoops: a change to an item of a loop inside a loop's
// row morphs that row whole and leaves its siblings.
func TestScopedNestedLoops(t *testing.T) {
	p, _ := scopedServer(t)
	tagRows(p, "#groups > section")
	tagRows(p, "#groups li")
	before := scopedRuns(p, "groups")
	clickWait(t, p, "#deep", `document.querySelectorAll("#groups section")[1].querySelectorAll("li")[2].textContent === "1"`)
	if scopedRuns(p, "groups") == before {
		t.Fatal("the patch didn't morph row by row")
	}
	untagged(t, p, "#groups > section")
	untagged(t, p, "#groups li")
}

// TestScopedComponents: a loop whose rows hold stateful components keeps
// each component's state, by its place, through patches to other loops
// (morphed row by row) and its own.
func TestScopedComponents(t *testing.T) {
	p, _ := scopedServer(t)
	p.Click(`#comps li:nth-child(2) .binc`)
	p.Wait(`document.querySelectorAll("#comps .bn")[1].textContent === "1"`)
	tagRows(p, "#comps > li")
	before := scopedRuns(p, "rows")
	clickWait(t, p, "#bump", `document.querySelectorAll("#rows .n")[4].textContent === "1"`)
	if scopedRuns(p, "rows") == before {
		t.Fatal("the patch didn't morph row by row")
	}
	p.Click(`#comps li:nth-child(2) .binc`)
	p.Wait(`document.querySelectorAll("#comps .bn")[1].textContent === "2"`)
	untagged(t, p, "#comps > li")
	if got := p.Text("#comps"); strings.Count(got, "0") != 3 {
		t.Fatalf("components %q", got)
	}
}

// TestScopedHandlers: handlers in rows left alone and in the changed row
// still fire after row-by-row morphs.
func TestScopedHandlers(t *testing.T) {
	p, log := scopedServer(t)
	before := scopedRuns(p, "rows")
	for i, id := range []string{"2", "5", "9", "5"} {
		clickWait(t, p, `#rows li[data-id="`+id+`"] .hit`, fmt.Sprintf(`document.querySelectorAll("#rows .n")[4].textContent === "%d"`, i+1))
	}
	if got := log.got(); got != "2,5,9,5" {
		t.Fatalf("hits %q", got)
	}
	if scopedRuns(p, "rows")-before < 4 {
		t.Fatal("the patches didn't morph row by row")
	}
}

// TestScopedStructural: rows coming, a key changing, and rows a script
// reordered morph the container whole — the server's order back — and the
// patches after row by row again.
func TestScopedStructural(t *testing.T) {
	p, _ := scopedServer(t)
	order := func() string {
		var s string
		p.Eval(`Array.from(document.querySelectorAll("#rows > li"), (l) => l.dataset.id).join(",")`, &s)
		return s
	}
	before := scopedRuns(p, "rows")
	clickWait(t, p, "#insert", `document.querySelectorAll("#rows > li").length === 10`)
	clickWait(t, p, "#rekey", `document.querySelectorAll("#rows > li")[5].dataset.id === "1005"`)
	if scopedRuns(p, "rows") != before {
		t.Fatal("an insert or a key change was morphed row by row")
	}
	want := order()
	p.Eval(`const u = document.querySelector("#rows"); u.insertBefore(u.lastElementChild, u.firstElementChild); 0`, nil)
	clickWait(t, p, "#bump", `document.querySelectorAll("#rows .n")[5].textContent === "1"`)
	if got := order(); got != want {
		t.Fatalf("rows %s, want the server's %s", got, want)
	}
	clickWait(t, p, "#bump", `document.querySelectorAll("#rows .n")[5].textContent === "2"`)
	if scopedRuns(p, "rows") == before {
		t.Fatal("the patches after didn't morph row by row")
	}
}

// TestScopedUnkeyed: a loop without keys morphs row by row as well, its
// rows matched by place.
func TestScopedUnkeyed(t *testing.T) {
	p, _ := scopedServer(t)
	tagRows(p, "#plain > li")
	before := scopedRuns(p, "plain")
	clickWait(t, p, "#plainbump", `document.querySelectorAll("#plain i")[2].textContent === "1"`)
	clickWait(t, p, "#inc", `document.querySelector("#count").textContent === "2"`)
	if scopedRuns(p, "plain")-before < 2 {
		t.Fatal("the patches didn't morph row by row")
	}
	untagged(t, p, "#plain > li")
}

// TestScopedStream: a stream beside a loop keeps applying each batch once,
// in order, while the loop's patches morph row by row.
func TestScopedStream(t *testing.T) {
	p, _ := scopedServer(t)
	rows := func() string {
		var s string
		p.Eval(`Array.from(document.querySelectorAll("#items > li"), (l) => l.textContent).join(",") + "|" + document.querySelector("#items").getAttribute("data-vk-rows")`, &s)
		return s
	}
	before := scopedRuns(p, "rows")
	clickWait(t, p, "#stop", `document.querySelectorAll("#items > li").length === 4`)
	clickWait(t, p, "#bump", `document.querySelectorAll("#rows .n")[4].textContent === "1"`)
	clickWait(t, p, "#sedit", `document.querySelectorAll("#items > li")[2].textContent === "7"`)
	clickWait(t, p, "#bump", `document.querySelectorAll("#rows .n")[4].textContent === "2"`)
	if got := rows(); got != "0,0,7,0|4" {
		t.Fatalf("stream rows %s", got)
	}
	if scopedRuns(p, "rows")-before < 2 {
		t.Fatal("the loop's patches didn't morph row by row")
	}
}

// TestScopedIndeterminate: a checkbox a script made indeterminate, in a row
// a patch leaves alone, is reset from the server's markup as a whole morph
// resets it.
func TestScopedIndeterminate(t *testing.T) {
	p, _ := scopedServer(t)
	p.Eval(`document.querySelector('#rows li[data-id="1"] .c').indeterminate = true; 0`, nil)
	before := scopedRuns(p, "rows")
	clickWait(t, p, "#bump", `document.querySelectorAll("#rows .n")[4].textContent === "1"`)
	if scopedRuns(p, "rows") == before {
		t.Fatal("the patch didn't morph row by row")
	}
	var ind bool
	p.Eval(`document.querySelector('#rows li[data-id="1"] .c').indeterminate`, &ind)
	if ind {
		t.Fatal("the checkbox stayed indeterminate")
	}
}

// TestScopedFramed: a loop between fixed siblings (and the whitespace
// around them) morphs row by row, the siblings and rows staying the
// elements they were; a script's change to a fixed sibling has the next
// patch morph the container whole, which keeps it.
func TestScopedFramed(t *testing.T) {
	p, _ := scopedServer(t)
	tagRows(p, "#framed > li")
	before := scopedRuns(p, "framed")
	clickWait(t, p, "#fbump", `document.querySelectorAll("#framed > li")[3].textContent === "1"`)
	clickWait(t, p, "#inc", `document.querySelector("#count").textContent === "2"`)
	if scopedRuns(p, "framed")-before < 2 {
		t.Fatal("the patches didn't morph row by row")
	}
	untagged(t, p, "#framed > li")
	p.Eval(`document.querySelector("#framed .first").textContent = "changed"; 0`, nil)
	before = scopedRuns(p, "framed")
	clickWait(t, p, "#fbump", `document.querySelectorAll("#framed > li")[3].textContent === "2"`)
	if scopedRuns(p, "framed") != before {
		t.Fatal("a container whose fixed sibling a script changed was morphed row by row")
	}
	if got := p.Text("#framed .first"); got != "changed" {
		t.Fatalf("the fixed sibling shows %q, want the script's (a morph keeps it while the server renders the same)", got)
	}
}

// TestScopedBindings: rows with bindings, left alone by a row-by-row
// morph, aren't bound again, and show what a whole morph and bind leave
// (verify mode checks both); a row whose bound attribute a script changed
// is morphed and bound again.
func TestScopedBindings(t *testing.T) {
	p, _ := scopedServer(t)
	p.Eval(`document.querySelectorAll("#bound > li")[1].setAttribute("title", "script"); 0`, nil)
	before := scopedRuns(p, "bound")
	clickWait(t, p, "#fbump", `document.querySelectorAll("#bound i")[2].textContent === "1"`)
	if scopedRuns(p, "bound") == before {
		t.Fatal("the patch didn't morph row by row")
	}
	var titles string
	p.Eval(`Array.from(document.querySelectorAll("#bound > li"), (l) => l.title).join(",")`, &titles)
	if titles != "hi,hi,hi,hi,hi" {
		t.Fatalf("titles %s", titles)
	}
}
