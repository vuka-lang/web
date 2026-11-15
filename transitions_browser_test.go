package web

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// transitionsFrame is a root layout opting in to view transitions: its
// <html> carries data-vk-transitions, links to two pages that aren't live
// and two live ones, the page in <main>.
type transitionsFrame struct{ Children ui.Node }

func (l *transitionsFrame) Render(ctx context.Context, w io.Writer) error {
	link := func(href, id string) ui.Node { return ui.Link(ui.LinkProps{Href: href, ID: id, Children: ui.Text(id)}) }
	return ui.El("html", attrs("data-vk-transitions", ""),
		ui.El("head", nil, ui.El("title", nil, ui.Text(ui.PageTitle(ctx)))),
		ui.El("body", nil,
			ui.El("header", nil, link("/vt/a", "to-a"), link("/vt/b", "to-b"), link("/vt/l1", "to-l1"), link("/vt/l2", "to-l2")),
			ui.El("main", nil, l.Children))).Render(ctx, w)
}

// Navigations in Chrome, the app opted in: a move to another page over
// HTTP, over the socket and back goes through document.startViewTransition
// and lands; a patch of the same page doesn't; without data-vk-transitions
// on <html> no move does.
func TestBrowserViewTransitions(t *testing.T) {
	meterMounts.Store(0)
	app := New(Only("none"), NoTailwind())
	frame := LayoutOf[transitionsFrame]()
	text := func(s string) func() ui.Node {
		return func() ui.Node { return ui.Safe(`<p id="at">` + s + `</p>`) }
	}
	app.Get("/vt/a", text("a"), frame)
	app.Get("/vt/b", text("b"), frame)
	app.Live("/vt/l1", &meter{}, frame)
	app.Live("/vt/l2", &meter{}, frame)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	defer app.Stop(context.Background())

	p := chrome.Launch(t).Visit(srv.URL + "/vt/a")
	// The stub counts the transitions started, then starts the browser's
	// own (counting those finished: while one runs, the pointer reaches
	// only the document), or runs the update at once where Chrome has none.
	p.Eval(`window.__vt = 0; window.__done = 0;
		const own = document.startViewTransition && document.startViewTransition.bind(document);
		document.startViewTransition = (cb) => {
			window.__vt++;
			if (!own) { cb(); window.__done++; return; }
			const v = own(cb);
			v.finished.finally(() => window.__done++);
			return v;
		};
		window.__navs = 0;
		window.addEventListener("vk:navigate", () => window.__navs++);
		true`, nil)
	settled := `window.__done === window.__vt`
	calls := func() int {
		var n int
		if err := p.Eval(`window.__vt`, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Over HTTP, to another page.
	p.Click("#to-b")
	p.Wait(`location.pathname === "/vt/b" && document.querySelector("#at").textContent === "b" && window.__navs === 1`)
	if n := calls(); n != 1 {
		t.Fatalf("over HTTP: %d transitions, want 1", n)
	}

	// A patch stays on the page: no transition.
	p.Eval(`window.vuka.navigate("/vt/b?x=1", { patch: true }); true`, nil)
	p.Wait(`location.search === "?x=1" && window.__navs === 2`)
	if n := calls(); n != 1 {
		t.Fatalf("a patch: %d transitions, want 1", n)
	}

	// To a live page, which joins; then over the socket to another.
	p.Wait(settled)
	p.Click("#to-l1")
	p.Wait(`location.pathname === "/vt/l1" && document.documentElement.getAttribute("data-vk-state") === "connected"`)
	p.WaitText("main span", "m1 n0")
	p.Wait(settled)
	p.Click("#to-l2")
	p.Wait(`location.pathname === "/vt/l2" && window.__navs === 4`)
	p.WaitText("main span", "m2 n0")
	if n := calls(); n != 3 {
		t.Fatalf("over the socket: %d transitions, want 3", n)
	}
	// The page works after its transition: an event's patch lands.
	p.Wait(settled)
	p.Click("main button")
	p.WaitText("main span", "m2 n1")

	// Back, to another page: a transition too.
	p.Eval(`history.back(); true`, nil)
	p.Wait(`location.pathname === "/vt/l1" && window.__navs === 5`)
	if n := calls(); n != 4 {
		t.Fatalf("back: %d transitions, want 4", n)
	}

	// Without the attribute, a move to another page applies as before.
	p.Eval(`document.documentElement.removeAttribute("data-vk-transitions"); true`, nil)
	p.Wait(settled)
	p.Click("#to-l2")
	p.Wait(`location.pathname === "/vt/l2" && window.__navs === 6`)
	if n := calls(); n != 4 {
		t.Fatalf("without data-vk-transitions: %d transitions, want 4", n)
	}
}
