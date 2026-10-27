package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// navFrame is the browser test's root layout: a header with a search field
// and links, the page in <main>.
type navFrame struct{ Children ui.Node }

func (l *navFrame) Render(ctx context.Context, w io.Writer) error {
	link := func(href, id string) ui.Node { return ui.Link(ui.LinkProps{Href: href, ID: id, Children: ui.Text(id)}) }
	return ui.El("html", nil,
		ui.El("head", nil, ui.El("title", nil, ui.Text(ui.PageTitle(ctx)))),
		ui.El("body", nil,
			ui.El("header", nil, ui.El("input", attrs("id", "q")), link("/h/a", "to-a"), link("/h/b", "to-b"), link("/h/c", "to-c"), link("/h/live", "to-live")),
			ui.El("main", nil, l.Children))).Render(ctx, w)
}

// islandAssets is an assets directory with two vanilla islands, Clock and
// Ticker, each noting its mounts and unmounts on window.
func islandAssets(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "i"), 0o755)
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", `{"islands":{"Clock":{"name":"Clock","module":"clock.js"},"Ticker":{"name":"Ticker","module":"ticker.js"}}}`)
	for _, name := range []string{"Clock", "Ticker"} {
		write("i/"+map[string]string{"Clock": "clock.js", "Ticker": "ticker.js"}[name], `export function mount(el, props) {
	(window.mounts ||= []).push("`+name+` " + props.n);
	el.textContent = "`+name+` " + props.n;
	return { unmount() { (window.unmounts ||= []).push("`+name+` " + props.n); } };
}
`)
	}
	t.Setenv(AssetsEnv, dir)
}

// In-place navigation over HTTP in Chrome: a page that isn't live moves to
// another in place, the layout's elements, its field's focus and what was
// typed kept, the islands of the page it leaves unmounted and the new
// page's mounted; back fetches again; a live page reached joins without
// mounting twice. The document is never loaded again.
func TestBrowserNavigationOverHTTP(t *testing.T) {
	islandAssets(t)
	meterMounts.Store(0)
	var navs atomic.Int32
	app := New(Only("none"), NoTailwind())
	app.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(NavigateHeader) != "" {
				navs.Add(1)
			}
			h.ServeHTTP(w, r)
		})
	})
	frame := LayoutOf[navFrame]()
	island := func(name, n string) func() ui.Node {
		return func() ui.Node {
			return ui.Safe(`<p id="at">` + n + `</p><vk-island data-vk-island="` + name + `" data-vk-client="load" data-vk-props="{&#34;n&#34;:&#34;` + n + `&#34;}"></vk-island>`)
		}
	}
	app.Get("/h/a", island("Clock", "a"), frame)
	app.Get("/h/b", island("Ticker", "b"), frame)
	app.Get("/h/c", island("Clock", "c"), frame)
	app.Live("/h/live", &meter{}, frame)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	defer app.Stop(context.Background()) // ends the assets' event stream

	p := chrome.Launch(t).Visit(srv.URL + "/h/a")
	p.Wait(`(window.mounts || []).join() === "Clock a"`)
	p.Eval(`window.__doc = 1; document.querySelector("header").__mark = 7; true`, nil)
	p.Type("#q", "typed")

	// A patch link's move, the field focused: focus and typing stay.
	p.Eval(`window.vuka.navigate("/h/b", { patch: true }); true`, nil)
	p.Wait(`location.pathname === "/h/b" && document.querySelector("#at").textContent === "b"`)
	p.Wait(`window.mounts.join() === "Clock a,Ticker b" && window.unmounts.join() === "Clock a"`)
	kept := `window.__doc === 1 && document.querySelector("header").__mark === 7 && document.querySelector("#q").value === "typed"`
	var ok bool
	p.Eval(kept+` && document.activeElement.id === "q" && history.scrollRestoration === "manual"`, &ok)
	if !ok {
		t.Fatal("the patch didn't keep the layout, the field's focus and what was typed")
	}

	// A link: the next page's island mounts where the last one's was.
	p.Click("#to-c")
	p.Wait(`location.pathname === "/h/c" && window.mounts.join() === "Clock a,Ticker b,Clock c" && window.unmounts.join() === "Clock a,Ticker b"`)
	p.Eval(kept+` && document.activeElement.tagName === "MAIN"`, &ok)
	if !ok {
		t.Fatal("the navigation didn't keep the layout, or focus didn't move to main")
	}

	// Back: fetched again, its island mounted again.
	p.Eval(`history.back(); true`, nil)
	p.Wait(`location.pathname === "/h/b" && window.mounts.join() === "Clock a,Ticker b,Clock c,Ticker b"`)

	// A live page: it joins, Mount having run once.
	p.Click("#to-live")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	p.WaitText("main span", "m1 n0")
	p.Click("main button")
	p.WaitText("main span", "m1 n1")
	p.Eval(kept, &ok)
	if !ok {
		t.Fatal("the live page's arrival loaded the document")
	}
	if n := navs.Load(); n != 4 {
		t.Errorf("%d navigations fetched, want 4", n)
	}
}
