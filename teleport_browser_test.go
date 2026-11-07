package web

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

type vkTeleportSent struct {
	Outer string `form:"outer"`
	Inner string `form:"inner"`
}

// vkTeleportPage is a live page whose overlay is written inside a clipping
// ancestor (a transform and a mask-image) and a form, in a <vk-teleport>
// (what ui.Teleport renders), with a teleport nested in it and one into an
// element of the layout.
type vkTeleportPage struct {
	ui.Live
	mu    *sync.Mutex
	sent  *[]vkTeleportSent
	Count int
	Open  bool
}

func (p *vkTeleportPage) Inc()   { p.Count++ }
func (p *vkTeleportPage) Close() { p.Open = false }
func (p *vkTeleportPage) Show()  { p.Open = true }
func (p *vkTeleportPage) Send(s vkTeleportSent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	*p.sent = append(*p.sent, s)
	return nil
}

func (p *vkTeleportPage) Render() ui.Node {
	var overlay ui.Node
	if p.Open {
		overlay = ui.HTML(`<vk-teleport id="tp" data-vk-teleport="body" style="display:contents">
<div class="overlay" style="position:fixed;inset:0">
<p id="count">{count}</p>
<input id="inner" name="inner"/>
<button id="inc" type="button" onClick={inc}>inc</button>
<button id="close" type="button" onClick={close}>close</button>
<div ref="panel" id="panel" hidden>panel</div>
<vk-teleport data-vk-teleport="body" style="display:contents"><p id="nested">{again}</p></vk-teleport>
</div>
</vk-teleport>`, p.Count, p.Inc, p.Close, p.Count)
	}
	return ui.HTML(`<main>
<div id="layer"></div>
<div id="clip" onKeyDown={esc} style="transform:translateZ(0);mask-image:linear-gradient(black,transparent)">
<form onSubmit={send}>
<input id="outer" name="outer" value="o"/>
{overlay}
<button id="submit">send</button>
</form>
<button id="reveal" type="button" onClick={reveal}>reveal</button>
<button id="show" type="button" onClick={show}>show</button>
<vk-teleport data-vk-teleport="#layer" style="display:contents"><span id="inlayer">{count}</span></vk-teleport>
{box}
</div>
</main>`, p.Close, p.Send, overlay, ui.JS.Show(ui.Ref("panel")), p.Show, p.Count, ui.Component("box", nil, &vkTeleportBox{}))
}

// vkTeleportBox is a component that teleports itself: its markup's root is
// the teleport.
type vkTeleportBox struct {
	ui.Live
	n int
}

func (b *vkTeleportBox) Inc() { b.n++ }

func (b *vkTeleportBox) Render() ui.Node {
	return ui.HTML(`<vk-teleport data-vk-teleport="body" style="display:contents"><div id="box"><b id="boxn">{n}</b><button id="boxinc" type="button" onClick={inc}>+</button></div></vk-teleport>`, b.n, b.Inc)
}

func vkTeleportServer(t *testing.T) (*chrome.Page, func() []vkTeleportSent) {
	t.Helper()
	var mu sync.Mutex
	var sent []vkTeleportSent
	a := New(Only("none"))
	a.Live("/tp", func() *vkTeleportPage { return &vkTeleportPage{mu: &mu, sent: &sent, Open: true} })
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Open(srv.URL + "/tp")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	return p, func() []vkTeleportSent {
		mu.Lock()
		defer mu.Unlock()
		return append([]vkTeleportSent(nil), sent...)
	}
}

func evalString(t *testing.T, p *chrome.Page, js string) string {
	t.Helper()
	var s string
	if err := p.Eval(js, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestBrowserTeleport: the runtime moves a teleport's content to the end of
// its target, patches it there, keeps a focused field's focus and value
// across a patch, keeps its fields in the form they were written in, finds a
// command's ref in it, and removes it when the server stops rendering it.
func TestBrowserTeleport(t *testing.T) {
	p, sent := vkTeleportServer(t)
	where := `[...document.querySelectorAll("vk-teleport")].map((t) => (t.id || t.firstElementChild.id) + ">" + (t.parentElement.id || t.parentElement.tagName)).sort().join(" ")`
	if got := evalString(t, p, where); got != "box>BODY inlayer>layer nested>BODY tp>BODY" {
		t.Fatalf("teleports at %q", got)
	}

	// a patch reaches the moved content, a focused field in it keeps its focus
	p.Eval(`const f = document.querySelector("#inner"); f.focus(); f.value = "typed"; document.querySelector("#inc").click(); 0`, nil)
	p.WaitText("#count", "1")
	p.WaitText("#nested", "1")
	p.WaitText("#inlayer", "1")
	if got := evalString(t, p, `document.activeElement.id + ":" + document.activeElement.value`); got != "inner:typed" {
		t.Fatalf("focus after the patch: %q", got)
	}
	if got := evalString(t, p, where); got != "box>BODY inlayer>layer nested>BODY tp>BODY" {
		t.Fatalf("teleports after a patch at %q", got)
	}

	// a component that teleports itself is patched where its content is
	clickJS(p, "#boxinc")
	p.WaitText("#boxn", "1")
	if got := evalString(t, p, `document.querySelector("#box").parentElement.parentElement.tagName + ":" + document.querySelectorAll("#box").length`); got != "BODY:1" {
		t.Fatalf("the component's content: %q", got)
	}

	// a command's ref, in the instance's teleported content
	clickJS(p, "#reveal")
	p.Wait(`!document.querySelector("#panel").hidden && getComputedStyle(document.querySelector("#panel")).display !== "none"`)

	// the field written in the form is sent with it
	if got := evalString(t, p, `document.querySelector("#inner").form && document.querySelector("#inner").form.querySelector("#outer") ? "in" : "out"`); got != "in" {
		t.Fatalf("the teleported field is %s of its form", got)
	}
	clickJS(p, "#submit")
	p.Wait(`document.querySelector("#inner") && document.querySelector("#inner").form !== null`)
	for i := 0; i < 50 && len(sent()) == 0; i++ {
		p.Eval(`new Promise((r) => setTimeout(r, 20))`, nil)
	}
	if s := sent(); len(s) != 1 || s[0].Outer != "o" || s[0].Inner != "typed" {
		t.Fatalf("sent %+v", s)
	}

	// gone with its placeholder, nested content too — closed by a key handler
	// on an ancestor where the overlay is written; back without moving twice
	p.Eval(`document.querySelector("#inner").dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true })); 0`, nil)
	p.Wait(`document.querySelector("#count") === null`)
	if got := evalString(t, p, `String(document.querySelectorAll("vk-teleport").length) + ":" + String(document.querySelector("#nested"))`); got != "2:null" {
		t.Fatalf("after close: %q", got)
	}
	clickJS(p, "#show")
	p.WaitText("#count", "1")
	if got := evalString(t, p, where); got != "box>BODY inlayer>layer nested>BODY tp>BODY" {
		t.Fatalf("teleports after showing again at %q", got)
	}
	if got := evalString(t, p, `document.body.innerHTML`); strings.Count(got, `id="count"`) != 1 {
		t.Fatalf("the overlay is in the page more than once: %s", got)
	}
}

// TestTeleportInPlace: without the runtime the content is where it is written.
func TestTeleportInPlace(t *testing.T) {
	a := New(Only("none"))
	a.Live("/tp", func() *vkTeleportPage { return &vkTeleportPage{mu: &sync.Mutex{}, sent: new([]vkTeleportSent), Open: true} })
	html := get(a.Handler(), "GET", "/tp").Body.String()
	clip := strings.Index(html, `id="clip"`)
	tp := strings.Index(html, `<vk-teleport id="tp"`)
	if clip < 0 || tp < clip {
		t.Fatalf("the teleport isn't rendered in place:\n%s", html)
	}
}
