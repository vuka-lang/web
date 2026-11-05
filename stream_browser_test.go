package web

import (
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

type feedItem struct {
	ID   int
	Text string
}

// feedPage is a live page whose items are a stream, written as the
// compiler writes a .vuka.html file's: the rows a loop over Rows(), the
// container's attributes and rows from the stream's Block. Its buttons
// queue each kind of operation; each row pushes Edit and Remove with its
// key.
type feedPage struct {
	ui.Live
	Log   *keyedLog
	Start []feedItem
	Max   int
	title string
	next  int
	items ui.Stream[feedItem]
}

func (f *feedPage) LiveSchema(l *ui.LiveSchema) { l.Mount(f.mount) }

func (f *feedPage) mount() error {
	f.title, f.next = "Feed", 100
	f.items.Limit(f.Max)
	f.items.Reset(f.Start...)
	return nil
}

func (f *feedPage) item() feedItem {
	f.next++
	return feedItem{f.next, fmt.Sprint("n", f.next)}
}

func (f *feedPage) Add()    { f.items.Insert(f.item()) }
func (f *feedPage) Top()    { f.items.Prepend(f.item()) }
func (f *feedPage) Rename() { f.title += "!" }
func (f *feedPage) Clear()  { f.items.Reset() }
func (f *feedPage) Reload() { f.items.Reset(feedItem{1, "one"}, feedItem{2, "two"}) }

func (f *feedPage) At()    { f.items.InsertAt(1, f.item(), f.item()) }
func (f *feedPage) AtEnd() { f.items.InsertAt(99, f.item()) }

func (f *feedPage) Edit(id int) { f.items.Insert(feedItem{id, "edited"}) }

func (f *feedPage) Move(id int) {
	f.items.Delete(id)
	f.items.Insert(feedItem{id, "moved"})
}

func (f *feedPage) Remove(id int) {
	f.Log.add(fmt.Sprint("remove ", id))
	f.items.Delete(id)
}

func (f *feedPage) Render() ui.Node {
	var lis []ui.Node
	for _, it := range f.items.Rows() {
		lis = append(lis, ui.HTML(`<li key={id}><span>{text}</span><input class="f"/><button class="edit" onClick={edit}>edit</button><button class="move" onClick={move}>move</button><button class="del" onClick={del}>del</button></li>`,
			it.ID, it.Text, ui.JS.Push(f.Edit, it.ID), ui.JS.Push(f.Move, it.ID), ui.JS.Push(f.Remove, it.ID)))
	}
	rows := f.items.Block(lis)
	return ui.HTML(`<main>
<h1>{title}</h1>
<button id="add" onClick={add}>add</button>
<button id="top" onClick={top}>top</button>
<button id="rename" onClick={rename}>rename</button>
<button id="clear" onClick={clear}>clear</button>
<button id="reload" onClick={reload}>reload</button>
<button id="at" onClick={at}>at</button>
<button id="atend" onClick={atend}>at end</button>
<ul id="feed" {attrs...}>{rows}</ul>
</main>`, f.title, f.Add, f.Top, f.Rename, f.Clear, f.Reload, f.At, f.AtEnd, rows.Attrs(), rows)
}

// TestBrowserStream: live.js applies a stream's batches — insert, prepend,
// delete, reset, a limit trimming the far end — each once, by key; the
// page's HTTP render and its join send the rows once; a row's push carries
// its key; a reconnect starts a fresh series.
func TestBrowserStream(t *testing.T) {
	log := &keyedLog{}
	a := New(Only("none"))
	a.Live("/feed", func() *feedPage {
		return &feedPage{Log: log, Max: 5, Start: []feedItem{{1, "one"}, {2, "two"}, {3, "three"}}}
	})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Start(srv.URL+"/feed", `document.documentElement.dataset.vkState === "connected"`, recordWire)

	keys := func() string {
		var ks []string
		if err := p.Eval(`[...document.querySelectorAll("#feed > li")].map((l) => l.dataset.vkRow)`, &ks); err != nil {
			t.Fatal(err)
		}
		return strings.Join(ks, " ")
	}
	want := func(ks string) {
		t.Helper()
		p.Wait(`[...document.querySelectorAll("#feed > li")].map((l) => l.dataset.vkRow).join(" ") === ` + fmt.Sprintf("%q", ks) + ` && !document.querySelector("[aria-busy]")`)
		var n string
		p.Eval(`document.querySelector("#feed").getAttribute("data-vk-rows")`, &n)
		if fmt.Sprint(len(strings.Fields(ks))) != n {
			t.Fatalf("data-vk-rows is %s with rows %q", n, ks)
		}
	}
	click := func(sel string) {
		t.Helper()
		p.Click(sel)
	}

	want("1 2 3")
	var join string
	p.Eval(`window.__got[0]`, &join)
	if strings.Contains(join, "data-vk-row") {
		t.Fatalf("the join sends the rows the page's HTTP render had: %s", join)
	}

	click("#add")
	want("1 2 3 101")
	click("#top")
	want("102 1 2 3 101")
	click("#add") // past the limit of 5: the start trimmed
	want("1 2 3 101 103")
	click("#top") // the end trimmed
	want("104 1 2 3 101")

	// in place: the row stays, keeping the element and what was typed
	p.Eval(`document.querySelector('#feed > li[data-vk-row="2"]').__mark = 1`, nil)
	p.Type(`#feed > li[data-vk-row="2"] input`, "typed")
	click(`#feed > li[data-vk-row="2"] .edit`)
	p.WaitText(`#feed > li[data-vk-row="2"] span`, "edited")
	want("104 1 2 3 101")
	var kept struct {
		Mark  int
		Value string
	}
	p.Eval(`(() => { const l = document.querySelector('#feed > li[data-vk-row="2"]'); return {mark: l.__mark, value: l.querySelector("input").value}; })()`, &kept)
	if kept.Mark != 1 || kept.Value != "typed" {
		t.Fatalf("the edited row is a new element, or lost what was typed: %+v", kept)
	}

	// the instance renders again with the batch it sent: nothing changes
	click("#rename")
	p.WaitText("h1", "Feed!")
	click("#rename")
	p.WaitText("h1", "Feed!!")
	want("104 1 2 3 101")

	click(`#feed > li[data-vk-row="3"] .del`)
	want("104 1 2 101")
	if got := log.got(); !slices.Equal(got, []string{"remove 3"}) {
		t.Fatalf("the row's push carried %v", got)
	}
	click(`#feed > li[data-vk-row="1"] .move`) // a delete and an insert in one batch
	want("104 2 101 1")

	click("#reload") // a reset keeps the rows it names
	want("1 2")
	p.Eval(`document.querySelector('#feed > li[data-vk-row="2"]').__mark`, &kept.Mark)
	if kept.Mark != 1 {
		t.Fatal("the reset replaced a row it keeps")
	}
	click("#clear")
	want("")
	var empty bool
	p.Eval(`document.querySelector("#feed").hasAttribute("data-vk-empty")`, &empty)
	if !empty {
		t.Fatal("an empty container has no data-vk-empty")
	}
	click("#add")
	want("105")

	// a reconnect mounts afresh: a new generation, reset to the mount's rows
	p.Drop()
	p.Wait(`window.__wire.sockets.length === 2 && document.documentElement.dataset.vkState === "connected"`)
	want("1 2 3")
	click("#add")
	want("1 2 3 101")
	if keys() != "1 2 3 101" {
		t.Fatal(keys())
	}
}
