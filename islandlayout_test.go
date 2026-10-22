package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/ui/live"
	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/vuka/diag"
	"github.com/vuka-lang/web/internal/chrome"
)

// Pages backed by a component, routed with Live, and their layouts.

type shelf struct {
	ui.Live
	Q     string   `json:"q" url:"q" bind:"q"`
	Books []string `json:"books"`
	db    *applicantDB
}

func (s *shelf) Render() ui.Node { return ui.IslandPage(s) }
func (s *shelf) LiveSchema(l *ui.LiveSchema) {
	l.Mount(s.mount)
	l.OnChange(s.onChange)
}

func (s *shelf) mount(_ context.Context) error                  { s.Books = s.db.find(s.Q); return nil }
func (s *shelf) onChange(_ context.Context, _ ui.Changed) error { s.Books = s.db.find(s.Q); return nil }
func (s *shelf) Search(q string)                                { s.Q, s.Books = q, s.db.find(q) }
func (s *shelf) Title() string                                  { return "Shelf " + s.Q }

// plainShelf has no layout of its own.
type plainShelf struct {
	ui.Live
	Q string `json:"q" url:"q"`
}

func (s *plainShelf) Render() ui.Node { return ui.IslandPage(s) }

// statelessShelf is a stateless page with no layout of its own.
type statelessShelf struct {
	Q string `json:"q" url:"q"`
}

// notLive is routed with Live but doesn't embed ui.Live.
type notLive struct {
	Q string `json:"q"`
}

func titledDoc(title string) func(ui.Node) ui.Node {
	return func(page ui.Node) ui.Node {
		return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text(title))), ui.El("body", nil, page))
	}
}

// bare is a document whose head has no title.
func bare(page ui.Node) ui.Node {
	return ui.El("html", nil, ui.El("head", nil, ui.El("meta", []ui.Attr{{Name: "charset", Value: "utf-8"}})), ui.El("body", nil, page))
}

// shell reads the request: its path, and the page's title.
func shell(ctx context.Context, page ui.Node) ui.Node {
	return ui.El("main", []ui.Attr{{Name: "data-path", Value: Request(ctx).URL.Path}, {Name: "data-title", Value: ui.PageTitle(ctx)}}, page)
}

func init() {
	ui.Island("components/Shelf.vue", ui.Layout(titledDoc("island"), shell))(vuka.TypeOf[shelf]("web.shelf"))
	ui.Island("components/Shelf.vue")(vuka.TypeOf[plainShelf]("web.plainShelf"))
	ui.Island("components/Shelf.vue")(vuka.TypeOf[statelessShelf]("web.statelessShelf"))
	ui.Island("components/Shelf.vue")(vuka.TypeOf[notLive]("web.notLive"))
}

func shelfApp(opts ...Option) *App {
	db := &applicantDB{names: []string{"Ada", "Alan", "Grace"}}
	return New(append([]Option{Only("none"), Provide(func() *applicantDB { return db })}, opts...)...)
}

func TestLiveIslandPage(t *testing.T) {
	a := shelfApp(LiveLayout(titledDoc("app")))
	a.Live("/shelf", &shelf{}, Layout(titledDoc("route")))
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	html, tok := pageToken(t, srv, "/shelf?q=gr")
	for _, want := range []string{`<title>island</title>`, `<main data-path="/shelf" data-title="Shelf gr">`, `data-vk-island="shelf"`,
		`data-vk-methods="[&#34;search&#34;]"`} {
		if !strings.Contains(html, want) {
			t.Errorf("no %s in %s", want, html)
		}
	}
	if tok == "" || strings.Contains(html, "route") || strings.Contains(html, ">app<") {
		t.Errorf("the island's layout doesn't win, or the page isn't live: %s", html)
	}

	c, joinHTML := joined(t, srv, "/shelf?q=a")
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, joinHTML, "m-search", 0), "event": "emit", "data": []any{"grace"}})
	m := must(t, c)
	if m.Type != live.PatchMsg || len(m.Updates) != 1 {
		t.Fatalf("call: %+v", m)
	}
	if props, _, _ := rendered(t, m.Updates[0].HTML); props["q"] != "grace" || len(props["books"].([]any)) != 1 {
		t.Errorf("after the call: %v", props)
	}
	send(t, c, map[string]any{"type": "event", "ref": 3, "target": handlerOf(t, joinHTML, "m-_set", 0), "event": "emit", "data": map[string]any{"q": "a"}})
	m = must(t, c)
	if props, _, _ := rendered(t, m.Updates[0].HTML); m.Type != live.PatchMsg || props["q"] != "a" || len(props["books"].([]any)) != 3 {
		t.Errorf("after the set: %+v", m)
	}
}

func TestIslandLayoutPrecedence(t *testing.T) {
	a := shelfApp(LiveLayout(titledDoc("app")))
	a.Live("/route", &plainShelf{}, Layout(titledDoc("route")))
	a.Live("/app", &plainShelf{})
	a.Page("/stateless", &statelessShelf{})
	h := a.Handler()
	expect(t, h, "GET", "/route", "", 200, "<title>route</title>")
	expect(t, h, "GET", "/app", "", 200, "<title>app</title>")
	expect(t, h, "GET", "/stateless", "", 200, "<title>app</title>")

	// No layout: a bare document titled by the type's Title, else its name;
	// a layout without a title gets the page's.
	b := shelfApp()
	b.Live("/default", &plainShelf{})
	b.Live("/titled", &shelf{})
	b.Live("/bare", &plainShelf{}, Layout(bare))
	b.Page("/stateless", &statelessShelf{})
	h = b.Handler()
	expect(t, h, "GET", "/default", "", 200, "<title>plainShelf</title>")
	expect(t, h, "GET", "/stateless", "", 200, "<title>statelessShelf</title>")
	expect(t, h, "GET", "/titled?q=x", "", 200, "<title>island</title>")
	expect(t, h, "GET", "/bare", "", 200, `<head><title>plainShelf</title><meta charset="utf-8">`)
}

func TestLiveIslandNeedsLive(t *testing.T) {
	a := shelfApp()
	a.specs = append(a.specs, (&liveSpec{path: "/n", typ: vuka.TypeOf[notLive]("web.notLive").Reflect, name: "web.notLive"}).route())
	if err := a.Build(); err == nil || !strings.Contains(err.Error(), "embed ui.Live") {
		t.Errorf("got %v", err)
	}
}

// noRender embeds ui.Live but lacks Render: the message names the method.
type noRender struct {
	ui.Live
	Q string `json:"q"`
}

func TestLiveIslandNeedsRender(t *testing.T) {
	ui.Island("components/Shelf.vue")(vuka.TypeOf[noRender]("web.noRender"))
	a := shelfApp()
	a.specs = append(a.specs, (&liveSpec{path: "/r", typ: vuka.TypeOf[noRender]("web.noRender").Reflect, name: "web.noRender"}).route())
	if err := a.Build(); err == nil || !strings.Contains(err.Error(), "has no Render() ui.Node method") {
		t.Errorf("got %v", err)
	}
}

func TestIslandLayoutBad(t *testing.T) {
	type badShelf struct {
		ui.Live
	}
	ui.Island(ui.Layout("not a layout"))(vuka.TypeOf[badShelf]("web.badShelf"))
	if err := shelfApp().Page("/b", &badShelf{}).Build(); err == nil || diag.Of(err) != "E0321" || !strings.Contains(err.Error(), "ui.Layout is given a string, which isn't a layout") {
		t.Errorf("got %v", err)
	}
}

// Two pages backed by components share a layout: moving between them over
// the live connection keeps the layout's elements, the sidebar's marked
// link following the path.
type navA struct {
	ui.Live
	N string `json:"n"`
}

type navB struct {
	ui.Live
	N string `json:"n"`
}

func (p *navA) Render() ui.Node               { return ui.IslandPage(p) }
func (p *navB) Render() ui.Node               { return ui.IslandPage(p) }
func (p *navA) LiveSchema(l *ui.LiveSchema)   { l.Mount(p.mount) }
func (p *navA) mount(_ context.Context) error { p.N = "a"; return nil }
func (p *navB) LiveSchema(l *ui.LiveSchema)   { l.Mount(p.mount) }
func (p *navB) mount(_ context.Context) error { p.N = "b"; return nil }

func sidebar(ctx context.Context, page ui.Node) ui.Node {
	path := Request(ctx).URL.Path
	link := func(href, id string) ui.Node {
		attrs := []ui.Attr{}
		if href == path {
			attrs = append(attrs, ui.Attr{Name: "aria-current", Value: "page"})
		}
		return ui.Link(ui.LinkProps{Href: href, ID: id, Attrs: attrs, Children: ui.Text(id)})
	}
	return ui.El("div", nil, ui.El("nav", []ui.Attr{{Name: "id", Value: "side"}}, link("/a", "to-a"), link("/b", "to-b")),
		ui.El("section", []ui.Attr{{Name: "id", Value: "content"}}, page))
}

func init() {
	ui.Island("components/A.vue", ui.Layout(fullPage, sidebar))(vuka.TypeOf[navA]("web.navA"))
	ui.Island("components/B.vue", ui.Layout(fullPage, sidebar))(vuka.TypeOf[navB]("web.navB"))
}

func TestBrowserIslandLayoutNavigate(t *testing.T) {
	a := New(Only("none"))
	a.Live("/a", &navA{})
	a.Live("/b", &navB{})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)

	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/a")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	p.Eval(`document.querySelector("#side").__mark = 7; document.querySelector("#to-a").__mark = 8; true`, nil)
	p.Click("#to-b")
	p.Wait(`location.pathname === "/b" && document.querySelector('vk-island[data-vk-island="navB"]') !== null`)
	var kept bool
	p.Eval(`document.querySelector("#side").__mark === 7 && document.querySelector("#to-a").__mark === 8 &&
		document.querySelector("#to-b").getAttribute("aria-current") === "page" && !document.querySelector("#to-a").hasAttribute("aria-current") &&
		document.querySelector('vk-island[data-vk-island="navA"]') === null`, &kept)
	if !kept {
		var h string
		p.Eval(`document.body.innerHTML`, &h)
		t.Fatalf("the layout's elements weren't kept: %s", h)
	}
	p.Click("#to-a")
	p.Wait(`location.pathname === "/a" && document.querySelector('vk-island[data-vk-island="navA"]') !== null`)
	p.Eval(`document.querySelector("#side").__mark === 7 && document.querySelector("#to-a").getAttribute("aria-current") === "page"`, &kept)
	if !kept {
		t.Error("the layout's elements weren't kept on the way back")
	}
}

// What the page's scripts put in the body — a component's teleported
// content and its anchors — outlives a navigation morphing the body, so the
// component can still take it out (Vue's unmount walks a teleported
// fragment from its first anchor to its last).
func TestBrowserNavigateKeepsForeignBody(t *testing.T) {
	a := New(Only("none"))
	a.Live("/a", &navA{})
	a.Live("/b", &navB{})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)

	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/a")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	p.Eval(`(() => {
		window.__start = document.createTextNode("");
		window.__end = document.createTextNode("");
		const portal = document.createElement("div");
		portal.id = "portal";
		document.body.append(window.__start, portal, document.createComment("teleport"), window.__end);
		return true;
	})()`, nil)
	inBody := `window.__start.parentNode === document.body && window.__end.parentNode === document.body &&
		window.__start.nextSibling === document.querySelector("#portal") && document.querySelector("#portal").nextSibling.nodeType === 8 &&
		document.querySelector("#portal").nextSibling.nextSibling === window.__end`
	p.Click("#to-b")
	p.Wait(`location.pathname === "/b" && document.querySelector('vk-island[data-vk-island="navB"]') !== null`)
	var ok bool
	p.Eval(inBody, &ok)
	if !ok {
		var h string
		p.Eval(`document.body.innerHTML`, &h)
		t.Fatalf("the scripts' nodes in the body didn't outlive the navigation: %s", h)
	}
	p.Click("#to-a")
	p.Wait(`location.pathname === "/a" && document.querySelector('vk-island[data-vk-island="navA"]') !== null`)
	p.Eval(inBody+` && document.querySelectorAll("#side").length === 1`, &ok)
	if !ok {
		t.Error("the scripts' nodes in the body didn't outlive the way back")
	}
}
