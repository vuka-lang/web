package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// typingTick is a broadcast to typingPage and typingBox: they render
// again, their count of ticks changed, Q set when it carries one (another
// visitor's edit).
type typingTick struct{ Q string }

type typingForm struct {
	ui.FormState
	Title string `validate:"max=40"`
}

type typingNote struct {
	Text string `form:"text"`
}

type typingItem struct {
	ID   int
	Text string
}

// typingBox is a component in typingPage, rendered again on its own.
type typingBox struct {
	ui.Live
	q     string
	ticks int
}

func (b *typingBox) LiveSchema(l *ui.LiveSchema) { l.Mount(b.mount) }
func (b *typingBox) mount() error                { b.Subscribe("box"); return nil }
func (b *typingBox) Set(q string)                { b.q = q }
func (b *typingBox) Update(m typingTick) {
	b.ticks++
	if m.Q != "" {
		b.q = m.Q
	}
}
func (b *typingBox) Render() ui.Node {
	return ui.HTML(`<div><span id="bticks">{ticks}</span><input id="bq" value={q} onInput={set}/></div>`, b.ticks, b.q, b.Set)
}

// typingPage is a live page with the fields a visitor types in while the
// server renders them: inputs and a textarea whose onInput sets what the
// server renders as their value, a debounced one, a contenteditable, keyed
// rows with an input each, a stream's rows, a form reset by its submit, a
// struct form and a component.
type typingPage struct {
	ui.Live
	Q, Note string
	Notes   []string
	Rows    []string
	Ticks   int
	Form    typingForm
	next    int
	items   ui.Stream[typingItem]
}

func (p *typingPage) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }
func (p *typingPage) mount() error {
	p.Subscribe("typing")
	p.Rows = []string{"r1", "r2", "r3"}
	p.items.Reset(typingItem{1, "s1"}, typingItem{2, "s2"})
	p.next = 2
	return nil
}

func (p *typingPage) Update(m typingTick) {
	p.Ticks++
	if m.Q != "" {
		p.Q = m.Q
	}
}

func (p *typingPage) Set(q string)           { p.Q = q }
func (p *typingPage) SetNote(q string)       { p.Note = q }
func (p *typingPage) Add(n typingNote) error { p.Notes = append(p.Notes, n.Text); return nil }
func (p *typingPage) Save(typingForm) error  { return nil }
func (p *typingPage) Tick()                  { p.Ticks++ }
func (p *typingPage) DropFirst()             { p.Rows = p.Rows[1:] }
func (p *typingPage) PrependRow()            { p.Rows = append([]string{fmt.Sprint("n", len(p.Rows))}, p.Rows...) }
func (p *typingPage) Rotate()                { p.Rows = append(p.Rows[1:], p.Rows[0]) }
func (p *typingPage) StreamTop() {
	p.next++
	p.items.Prepend(typingItem{p.next, fmt.Sprint("s", p.next)})
}
func (p *typingPage) StreamDrop(id int) { p.items.Delete(id) }
func (p *typingPage) StreamEdit(id int) { p.items.Insert(typingItem{id, "edited"}) }

func (p *typingPage) Render() ui.Node {
	var rows, idrows, items []ui.Node
	for _, r := range p.Rows {
		rows = append(rows, ui.HTML(`<li key={k}><span>{r}</span><input class="row" name={n}/></li>`, r, r, r))
		idrows = append(idrows, ui.HTML(`<li id={id}><span>{r}</span><input class="row" name={n}/></li>`, "row-"+r, r, "id-"+r))
	}
	for _, it := range p.items.Rows() {
		items = append(items, ui.HTML(`<li key={id}><span>{text}</span><input class="item" name={name}/></li>`, it.ID, it.Text, fmt.Sprint("s", it.ID)))
	}
	block := p.items.Block(items)
	return ui.HTML(`<main>
<p id="ticks">{ticks}</p>
<button id="tick" onClick={tick}>tick</button>
<button id="dropfirst" onClick={drop}>drop</button>
<button id="prepend" onClick={prepend}>prepend</button>
<button id="rotate" onClick={rotate}>rotate</button>
<button id="stop" onClick={stop}>stream top</button>
<button id="sdrop" onClick={sdrop}>stream drop</button>
<button id="sedit" onClick={sedit}>stream edit</button>
<input id="q" value={q} onInput={set}/>
<input id="d" value={q2} onInput={set2} data-vk-debounce="500"/>
<textarea id="note" onInput={setNote}>{note}</textarea>
<div id="ed" contenteditable="true">hello</div>
<ul id="rows">{rows}</ul>
<ul id="idrows">{idrows}</ul>
<ul id="items" {attrs...}>{block}</ul>
<form id="f" onSubmit={add}><input id="text" name="text"/></form>
<p id="notes">{notes}</p>
{form}
{box}
</main>`, p.Ticks, p.Tick, p.DropFirst, p.PrependRow, p.Rotate, p.StreamTop,
		ui.JS.Push(p.StreamDrop, 1), ui.JS.Push(p.StreamEdit, 3),
		p.Q, p.Set, p.Q, p.Set, p.SetNote, p.Note, rows, idrows, block.Attrs(), block, p.Add, strings.Join(p.Notes, "|"),
		ui.Form{For: ui.For(&p.Form), OnSubmit: p.Save},
		ui.Component("box", nil, &typingBox{}))
}

// typingServer serves a typingPage whose events each take delay (a slow
// server: the replies come after the visitor typed more) and opens it.
func typingServer(t *testing.T) (*chrome.Page, *App, *atomic.Int64) {
	t.Helper()
	delay := &atomic.Int64{}
	a := New(Only("none"), GuardLiveEvents(func(context.Context, LiveHandler) error {
		time.Sleep(time.Duration(delay.Load()))
		return nil
	}))
	a.Live("/typing", func() *typingPage { return &typingPage{} })
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Open(srv.URL + "/typing")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	// what the focused field shows, and its caret, after each patch
	p.Eval(`window.seen = []; document.addEventListener("vk:patched", () => {
		const f = document.activeElement;
		window.seen.push((f.id || f.name || f.tagName) + ":" + (f.isContentEditable ? f.textContent + ":" + getSelection().focusOffset : f.value + ":" + f.selectionStart));
	}); 0`, nil)
	return p, a, delay
}

func seen(t *testing.T, p *chrome.Page) []string {
	t.Helper()
	var s []string
	if err := p.Eval(`window.seen.splice(0)`, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// waitSeen waits for n patches and returns what the focused field showed
// after each.
func waitSeen(t *testing.T, p *chrome.Page, n int) []string {
	t.Helper()
	p.Wait(fmt.Sprintf(`window.seen.length >= %d`, n))
	time.Sleep(50 * time.Millisecond)
	return seen(t, p)
}

func wantAll(t *testing.T, got []string, want string) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("no patch, want the field showing %q", want)
	}
	for _, g := range got {
		if g != want {
			t.Fatalf("after the patches the focused field showed %q, want each %q", got, want)
		}
	}
}

func value(t *testing.T, p *chrome.Page, sel string) string {
	t.Helper()
	var v string
	p.Eval(fmt.Sprintf(`document.querySelector(%q).value`, sel), &v)
	return v
}

// clickJS clicks a button from a script: the focus stays where it is.
func clickJS(p *chrome.Page, sel string) {
	p.Eval(fmt.Sprintf(`document.querySelector(%q).click()`, sel), nil)
}

func caret(p *chrome.Page, sel string, at int) {
	p.Eval(fmt.Sprintf(`document.querySelector(%q).setSelectionRange(%d, %d)`, sel, at, at), nil)
}

// TestBrowserTypingStaleReply: the reply to a keystroke's event renders the
// value the server had then; it comes after the visitor typed the next
// key, whose event is on its way. The field keeps what was typed and its
// caret — in an input, a textarea and a component's input.
func TestBrowserTypingStaleReply(t *testing.T) {
	for _, sel := range []string{"#q", "#note", "#bq"} {
		t.Run(sel[1:], func(t *testing.T) {
			p, _, delay := typingServer(t)
			delay.Store(int64(250 * time.Millisecond))
			p.Click(sel)
			p.Keys("ab")
			id := sel[1:]
			wantAll(t, waitSeen(t, p, 2), id+":ab:2")
			// typing in the middle: the caret stays where the visitor put it
			caret(p, sel, 1)
			p.Keys("XY")
			wantAll(t, waitSeen(t, p, 2), id+":aXYb:3")
			// a steady stream of keys, the replies lagging behind: none is lost
			delay.Store(int64(90 * time.Millisecond))
			caret(p, sel, 4)
			for _, r := range "steady" {
				p.Keys(string(r))
				time.Sleep(40 * time.Millisecond)
			}
			time.Sleep(900 * time.Millisecond) // the replies still on their way
			if v := value(t, p, sel); v != "aXYbsteady" {
				t.Fatalf("typed aXYbsteady, the field shows %q (patches: %q)", v, seen(t, p))
			}
		})
	}
}

// TestBrowserTypingBroadcast: patches the visitor's events didn't cause —
// a broadcast, another visitor's edit — while they type: a field typed in
// since the page last sent its event keeps what was typed (here a
// debounce waiting to send it); one showing what was sent takes the
// server's changed value, focused or not.
func TestBrowserTypingBroadcast(t *testing.T) {
	p, a, _ := typingServer(t)
	p.Click("#d")
	p.Keys("a")
	waitSeen(t, p, 1) // the debounced event sent, and its reply
	p.Keys("b")       // its event waits for the debounce
	a.Broadcast(context.Background(), "typing", typingTick{})
	wantAll(t, waitSeen(t, p, 1), "d:ab:2")
	a.Broadcast(context.Background(), "typing", typingTick{Q: "zzz"})
	wantAll(t, waitSeen(t, p, 1), "d:ab:2")
	if v := value(t, p, "#q"); v != "zzz" {
		t.Fatalf("#q, not focused, shows %q, want the server's zzz", v)
	}
	wantAll(t, waitSeen(t, p, 1), "d:ab:2") // the debounce's own event
	if v := value(t, p, "#q"); v != "ab" {
		t.Fatalf("#q shows %q, want the server's ab", v)
	}
	// nothing typed since the event was sent: the server's new value
	a.Broadcast(context.Background(), "typing", typingTick{Q: "zzz"})
	wantAll(t, waitSeen(t, p, 1), "d:zzz:3")
	// a component rendered again on its own while its field is typed in
	p.Click("#bq")
	p.Keys("xy")
	waitSeen(t, p, 2)
	caret(p, "#bq", 1)
	a.Broadcast(context.Background(), "box", typingTick{})
	wantAll(t, waitSeen(t, p, 1), "bq:xy:1")
	// a field no handler sets
	p.Click(`input[name="r2"]`)
	p.Keys("typed")
	caret(p, `input[name="r2"]`, 2)
	a.Broadcast(context.Background(), "typing", typingTick{})
	wantAll(t, waitSeen(t, p, 1), "r2:typed:2")
}

// TestBrowserTypingSiblings: rows around the one being typed in come, go
// and move; its field keeps the focus, what was typed and its caret — keyed
// rows and a stream's.
func TestBrowserTypingSiblings(t *testing.T) {
	p, _, _ := typingServer(t)
	for _, c := range []struct{ field, button string }{
		{`input[name="id-r3"]`, "#prepend"},
		{`input[name="id-r3"]`, "#dropfirst"},
		{`input[name="id-r3"]`, "#rotate"},
		{`input[name="id-r3"]`, "#tick"},
		{`input[name="r3"]`, "#prepend"},
		{`input[name="r3"]`, "#dropfirst"},
		{`input[name="r3"]`, "#rotate"},
		{`input[name="s2"]`, "#stop"},
		{`input[name="s2"]`, "#sdrop"},
		{`input[name="s2"]`, "#sedit"},
	} {
		t.Run(c.field[12:len(c.field)-2]+"/"+c.button[1:], func(t *testing.T) {
			p.Click(c.field)
			p.Eval(fmt.Sprintf(`document.querySelector(%q).value = ""`, c.field), nil)
			p.Keys("hey")
			caret(p, c.field, 1)
			seen(t, p)
			clickJS(p, c.button)
			name := strings.TrimSuffix(strings.TrimPrefix(c.field, `input[name="`), `"]`)
			wantAll(t, waitSeen(t, p, 1), name+":hey:1")
		})
	}
}

// TestBrowserTypingAfterSubmit: a form's submit resets it when its reply
// comes; a field typed in since the submit keeps what was typed.
func TestBrowserTypingAfterSubmit(t *testing.T) {
	p, _, delay := typingServer(t)
	delay.Store(int64(300 * time.Millisecond))
	p.Click("#text")
	p.Keys("milk")
	p.Press("Enter")
	p.Keys(" eggs")
	p.WaitText("#notes", "milk")
	time.Sleep(50 * time.Millisecond)
	if v := value(t, p, "#text"); v != "milk eggs" {
		t.Fatalf("typed milk eggs after the submit, the field shows %q", v)
	}
	// nothing typed since the submit: reset
	p.Eval(`document.querySelector("#text").value = "bread"`, nil)
	p.Press("Enter")
	p.WaitText("#notes", "milk|bread")
	p.Wait(`document.querySelector("#text").value === ""`)
}

// TestBrowserTypingStructForm: a struct form's fields, their changes sent
// as they are typed, the replies lagging behind.
func TestBrowserTypingStructForm(t *testing.T) {
	p, _, delay := typingServer(t)
	delay.Store(int64(400 * time.Millisecond))
	sel := `#typing-form-title`
	p.Click(sel)
	p.Keys("ab")
	time.Sleep(300 * time.Millisecond) // its change sent (the form's debounce is 250ms)
	p.Keys("cd")                       // and this one before the reply
	time.Sleep(1500 * time.Millisecond)
	got := seen(t, p)
	if len(got) == 0 {
		t.Fatal("no patch: the struct form sends no input events")
	}
	for i, g := range got {
		if !strings.HasPrefix(g, "typing-form-title:abcd") && !(i == 0 && strings.HasPrefix(g, "typing-form-title:ab")) {
			t.Fatalf("the field went back: %q", got)
		}
	}
	if v := value(t, p, sel); v != "abcd" {
		t.Fatalf("typed abcd, the field shows %q", v)
	}
}

// TestBrowserTypingComposition: a field being composed with an input method
// (compositionstart … compositionend) isn't touched by a patch, though it
// shows what its event sent.
func TestBrowserTypingComposition(t *testing.T) {
	p, a, _ := typingServer(t)
	p.Click("#q")
	p.Keys("x")
	waitSeen(t, p, 1)
	if err := p.Compose("k"); err != nil {
		t.Skip("no input method composition: ", err)
	}
	wantAll(t, waitSeen(t, p, 1), "q:xk:2") // the composition's input event, and its reply
	p.Compose("か")
	wantAll(t, waitSeen(t, p, 1), "q:xか:2")
	a.Broadcast(context.Background(), "typing", typingTick{Q: "other"})
	wantAll(t, waitSeen(t, p, 1), "q:xか:2")
	p.Commit("か")
	wantAll(t, waitSeen(t, p, 1), "q:xか:2")
}

// TestBrowserTypingContentEditable: an element being edited in place keeps
// what was typed in its server-rendered text through a patch.
func TestBrowserTypingContentEditable(t *testing.T) {
	p, a, _ := typingServer(t)
	p.Click("#ed")
	p.Eval(`(() => { const t = document.querySelector("#ed").firstChild; getSelection().collapse(t, 5); })()`, nil)
	p.Keys(" world")
	a.Broadcast(context.Background(), "typing", typingTick{})
	wantAll(t, waitSeen(t, p, 1), "ed:hello world:11")
}
