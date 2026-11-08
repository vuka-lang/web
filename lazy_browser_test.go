package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/uigen"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

var (
	shelfOpen    = uigen.State("shelfOpen", false)
	shelfRenders atomic.Int32
	shelfEdits   atomic.Int32
)

// shelf is a lazy region's content, its render the server work it holds
// back.
func shelfContent() ui.Node {
	return ui.Ctx(func(context.Context) ui.Node {
		n := shelfRenders.Add(1)
		return ui.El("p", []ui.Attr{{Name: "id", Value: "content"}}, ui.Text("shelf "+strconv.Itoa(int(n))))
	})
}

// shelfPage is a live page whose region a browser state shows.
type shelfPage struct {
	ui.Live
	each bool
}

func (p *shelfPage) Edit(context.Context) error { shelfEdits.Add(1); return nil }

func (p *shelfPage) Render() ui.Node {
	button := func(id string, c ui.Commands) ui.Node {
		return ui.El("button", []ui.Attr{{Name: "id", Value: id}, {Name: "onClick", Value: c}}, ui.Text(id))
	}
	return ui.El("main", nil,
		button("open", ui.JS.Set(shelfOpen, true)),
		button("close", ui.JS.Set(shelfOpen, false)),
		button("edit", ui.JS.Set(shelfOpen, true).Push(p.Edit)),
		ui.Lazy(ui.LazyProps{When: shelfOpen, Each: p.each, Children: shelfContent()}))
}

// sentLog records the messages the page's socket sends.
const sentLog = `(() => { window.sent = []; const send = WebSocket.prototype.send; WebSocket.prototype.send = function (d) { window.sent.push(JSON.parse(d)); return send.call(this, d); }; return true; })()`

func sent(t *testing.T, p *chrome.Page, kind string) int {
	t.Helper()
	var n int
	p.Eval(`window.sent.filter((m) => `+kind+`).length`, &n)
	return n
}

func openShelf(t *testing.T, each bool) *chrome.Page {
	t.Helper()
	shelfRenders.Store(0)
	shelfEdits.Store(0)
	app := New(Only("none"))
	app.Live("/shelf", func() *shelfPage { return &shelfPage{each: each} })
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Open(srv.URL + "/shelf")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	var ok bool
	p.Eval(sentLog, &ok)
	return p
}

// A real browser: a live page's hidden region isn't rendered until the
// browser first shows it, which sends one vkinit; the content then stays,
// reopening sends nothing; Each sends one at each opening.
func TestBrowserLazyLive(t *testing.T) {
	p := openShelf(t, false)
	var busy bool
	p.Eval(`!document.querySelector("#content") && document.querySelector("vk-lazy").getAttribute("aria-busy") === "true"`, &busy)
	if !busy || shelfRenders.Load() != 0 {
		t.Fatalf("the hidden region: busy %v, %d renders", busy, shelfRenders.Load())
	}
	p.Click("#open")
	p.Wait(`document.querySelector("#content") && !document.querySelector("vk-lazy").hasAttribute("data-vk-init")`)
	p.Click("#close")
	p.Wait(`document.querySelector("vk-lazy").hidden`)
	p.Click("#open")
	p.Wait(`!document.querySelector("vk-lazy").hidden`)
	p.Click("#edit") // an event: a render, the region's content with it
	p.Wait(`document.querySelector("#content").textContent === "shelf 2"`)
	if n := sent(t, p, `m.event === "vkinit"`); n != 1 {
		t.Errorf("%d vkinit events", n)
	}

	p = openShelf(t, true)
	for i := range 2 {
		p.Click("#open")
		p.Wait(`document.querySelector("#content") && document.querySelector("#content").textContent === "shelf ` + strconv.Itoa(i+1) + `"`)
		p.Click("#close")
		p.Wait(`document.querySelector("vk-lazy").hidden`)
	}
	if n := sent(t, p, `m.event === "vkinit"`); n != 2 || shelfRenders.Load() != 2 {
		t.Errorf("each: %d vkinit events, %d renders", n, shelfRenders.Load())
	}
}

// A real browser: an event whose chain shows the region carries it, so its
// reply renders the content and no vkinit follows.
func TestBrowserLazyShownWithPush(t *testing.T) {
	p := openShelf(t, false)
	p.Click("#edit")
	p.Wait(`document.querySelector("#content") !== null`)
	var shown bool
	p.Eval(`window.sent.some((m) => m.event === "click" && m.shown && m.shown.length === 1)`, &shown)
	if n := sent(t, p, `m.event === "vkinit"`); n != 0 || !shown || shelfEdits.Load() != 1 || shelfRenders.Load() != 1 {
		t.Errorf("open with a push: %d vkinit events, shown %v, %d edits, %d renders", n, shown, shelfEdits.Load(), shelfRenders.Load())
	}
}

var (
	cartOpen    = uigen.State("cartOpen", false)
	cartRenders atomic.Int32
)

// cartPage is a page that isn't live: a region a browser state shows, and
// a form type's form in a closed modal.
func cartPage(f *lampForm) ui.Node {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Cart"))), ui.El("body", nil, ui.StyleTag(), ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "id", Value: "open"}, {Name: "onClick", Value: ui.JS.Set(cartOpen, true)}}, ui.Text("Cart")),
		ui.El("button", []ui.Attr{{Name: "id", Value: "add"}, {Name: "onClick", Value: ui.OpenModal("add")}}, ui.Text("Add")),
		ui.Lazy(ui.LazyProps{When: cartOpen, Children: ui.Ctx(func(context.Context) ui.Node {
			cartRenders.Add(1)
			return ui.El("p", []ui.Attr{{Name: "id", Value: "cart"}}, ui.Text("3 products"))
		})}),
		&ui.Modal{Ref: "add", Children: ui.Form{For: ui.For(f), OnSubmit: func(lamp) error { return nil }, Bound: []string{"Name", "Price"}, Children: ui.Fragment(
			ui.Field{For: ui.For(&f.Value.Name, f)},
			ui.Field{For: ui.For(&f.Value.Price, f)},
		)}})))
}

func cartApp() *App {
	a := New(Only("none"))
	a.Get("/cart", func() ui.Node { return cartPage(&lampForm{}) })
	return a
}

// A page that isn't live answers a GET naming a region (LazyHeader) with
// that region's HTML only, its content rendered; a region it hasn't is 404.
func TestLazyRegionHTTP(t *testing.T) {
	cartRenders.Store(0)
	srv := httptest.NewServer(cartApp().Handler())
	defer srv.Close()
	get := func(region string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/cart", nil)
		if region != "" {
			req.Header.Set(LazyHeader, region)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, string(b)
	}
	_, page := get("")
	if cartRenders.Load() != 0 || strings.Contains(page, "data-vk-token") || !strings.Contains(page, `data-vk-region="0" data-vk-init`) || !strings.Contains(page, "/_vuka/live.js") {
		t.Fatalf("the page: %d renders\n%s", cartRenders.Load(), page)
	}
	res, body := get("0")
	if res.StatusCode != 200 || res.Header.Get(LazyHeader) == "" || res.Header.Get("Cache-Control") != "no-store" || body != `<vk-lazy data-vk-show="{&#34;i&#34;:false,&#34;s&#34;:&#34;c0:cartOpen:1&#34;}" hidden data-vk-region="0"><p id="cart">3 products</p></vk-lazy>` || cartRenders.Load() != 1 {
		t.Errorf("region 0: %d %v, %d renders: %s", res.StatusCode, res.Header, cartRenders.Load(), body)
	}
	if res, _ := get("9"); res.StatusCode != http.StatusNotFound {
		t.Errorf("a region the page hasn't: %d", res.StatusCode)
	}
}

// A real browser, a page that isn't live: a region asks the page for its
// content once shown, and a form type's form in a closed modal for its
// Init, each over HTTP, without a connection.
func TestBrowserLazyHTTP(t *testing.T) {
	cartRenders.Store(0)
	lampInits.Store(0)
	srv := httptest.NewServer(cartApp().Handler())
	defer srv.Close()
	p := chrome.Launch(t).Visit(srv.URL + "/cart")
	if cartRenders.Load() != 0 || lampInits.Load() != 0 {
		t.Fatalf("the page rendered hidden content: %d renders, %d inits", cartRenders.Load(), lampInits.Load())
	}
	p.Click("#open")
	p.Wait(`document.querySelector("#cart") && !document.querySelector("vk-lazy").hidden`)
	p.Click("#add")
	p.Wait(`document.querySelector("#lamp-price") && document.querySelector("#lamp-price").value === "25" && !document.querySelector("form").hasAttribute("data-vk-init")`)
	var state string
	p.Eval(`document.documentElement.getAttribute("data-vk-state") || ""`, &state)
	if state != "" || cartRenders.Load() != 1 || lampInits.Load() != 1 {
		t.Errorf("state %q, %d renders, %d inits", state, cartRenders.Load(), lampInits.Load())
	}
}
