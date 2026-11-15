package web

import (
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// portalPage is a live page with a ui.Teleport written inside a small
// container that clips what overflows it, two levels down, and one into
// an element of the page.
type portalPage struct {
	ui.Live
	hits  *atomic.Int32
	Count int
	Open  bool
}

func (p *portalPage) Inc()  { p.Count++ }
func (p *portalPage) Hit()  { p.hits.Add(1) }
func (p *portalPage) Hide() { p.Open = false }

func (p *portalPage) Render() ui.Node {
	var dialog ui.Node
	if p.Open {
		dialog = ui.Teleport{ID: "pt", Children: ui.HTML(`<div id="dialog" style="position:fixed;left:20px;top:20px;width:200px;height:120px">
<p id="pcount">{count}</p>
<button id="phit" type="button" onClick={hit}>hit</button>
</div>`, p.Count, p.Hit)}
	}
	return ui.HTML(`<div id="layer"></div>
<div id="outer" style="overflow:hidden;width:10px;height:10px;position:relative">
<div id="inner" style="overflow:hidden;width:10px;height:10px">{dialog}</div>
</div>
<button id="pinc" type="button" onClick={inc}>inc</button>
<button id="phide" type="button" onClick={hide}>hide</button>
{inlayer}`, dialog, p.Inc, p.Hide, ui.Teleport{To: "#layer", Children: ui.HTML(`<span id="lcount">{count}</span>`, p.Count)})
}

func portalServer(t *testing.T) (*chrome.Page, *atomic.Int32) {
	t.Helper()
	hits := new(atomic.Int32)
	meterMounts.Store(0)
	a := New(Only("none"), NoTailwind())
	frame := LayoutOf[navFrame]()
	a.Live("/pt/a", func() *portalPage { return &portalPage{hits: hits, Open: true} }, frame)
	a.Live("/pt/b", &meter{}, frame)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Visit(srv.URL + "/pt/a")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	return p, hits
}

// portalAt says where #dialog is and how many there are: "BODY:1" for one,
// a direct child of the teleport moved to the end of <body>.
const portalAt = `(() => {
	const all = document.querySelectorAll("#dialog");
	if (!all.length) return "none";
	return all[0].parentElement.parentElement.tagName + ":" + all.length;
})()`

// TestBrowserPortal: content in a ui.Teleport written in a clipping
// container is moved to <body> and shows whole; the server's updates reach
// it there without copies piling up, a click in it reaches its handler, and
// it goes when the server stops rendering it.
func TestBrowserPortal(t *testing.T) {
	p, hits := portalServer(t)
	if got := evalString(t, p, portalAt); got != "BODY:1" {
		t.Fatalf("the teleported content is at %q", got)
	}
	var shown bool
	p.Eval(`(() => {
		const r = document.querySelector("#dialog").getBoundingClientRect();
		const at = document.elementFromPoint(r.left + 150, r.top + 100);
		return r.width === 200 && r.height === 120 && document.querySelector("#dialog").contains(at);
	})()`, &shown)
	if !shown {
		t.Fatal("the teleported content is clipped")
	}

	for i, want := range []string{"1", "2", "3"} {
		clickJS(p, "#pinc")
		p.WaitText("#pcount", want)
		if got := evalString(t, p, portalAt); got != "BODY:1" {
			t.Fatalf("after update %d the content is at %q", i+1, got)
		}
	}
	if got := evalString(t, p, `document.querySelectorAll("vk-teleport").length + ":" + document.querySelectorAll("#lcount").length + ":" + document.querySelector("#lcount").parentElement.parentElement.id`); got != "2:1:layer" {
		t.Fatalf("teleports after the updates: %q", got)
	}

	clickJS(p, "#phit")
	for i := 0; i < 50 && hits.Load() == 0; i++ {
		p.Eval(`new Promise((r) => setTimeout(r, 20))`, nil)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the click in the teleported content reached its handler %d times", n)
	}

	clickJS(p, "#phide")
	p.Wait(`document.querySelector("#dialog") === null`)
	if got := evalString(t, p, `String(document.querySelectorAll("vk-teleport").length)`); got != "1" {
		t.Fatalf("%s teleports left after hiding, want the one into #layer", got)
	}
}

// TestBrowserPortalNavigate: a navigation to another page takes the
// teleported content with it. Back, the page's snapshot shows what was
// moved into an element of the page (a copy of that element) but not what
// was moved to <body>, the runtime's, and once the fresh render is in
// there is one copy of each.
func TestBrowserPortalNavigate(t *testing.T) {
	p, _ := portalServer(t)
	clickJS(p, "#pinc")
	p.WaitText("#pcount", "1")
	p.Eval(`window.vuka.navigate("/pt/b"); true`, nil)
	p.WaitText("main span", "m1 n0")
	if got := evalString(t, p, portalAt+` + ":" + document.querySelectorAll("vk-teleport").length + ":" + document.querySelectorAll("#lcount").length`); got != "none:0:0" {
		t.Fatalf("after the navigation: %q", got)
	}

	p.Eval(`window.__previewed = []; new MutationObserver(() => {
		if (document.documentElement.hasAttribute("data-vk-preview")) window.__previewed.push(document.querySelectorAll("#dialog").length + ":" + document.querySelectorAll("#lcount").length);
	}).observe(document.documentElement, { attributes: true, childList: true, subtree: true }); history.back(); true`, nil)
	p.Wait(`location.pathname === "/pt/a" && !document.documentElement.hasAttribute("data-vk-preview") && document.querySelector("#pcount") !== null`)
	if got := evalString(t, p, `window.__previewed.length ? window.__previewed[0] : "no preview"`); got != "0:1" {
		t.Fatalf("the snapshot showed %q (dialogs:layer copies)", got)
	}
	if got := evalString(t, p, portalAt+` + ":" + document.querySelectorAll("vk-teleport").length + ":" + document.querySelectorAll("#lcount").length`); got != "BODY:1:2:1" {
		t.Fatalf("back on the page: %q", got)
	}
}
