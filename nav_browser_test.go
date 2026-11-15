package web

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
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

// navHTTPApp is the app of the test of in-place navigation over HTTP
// (TestBrowserNavigationOverHTTP): pages /h/a, /h/b and /h/c that aren't
// live, each with an island, and a live one, /h/live, in one layout; navs
// counts the navigations fetched. The app stops as the test ends.
func navHTTPApp(t *testing.T) (app *App, navs *atomic.Int32) {
	islandAssets(t)
	meterMounts.Store(0)
	navs = new(atomic.Int32)
	app = New(Only("none"), NoTailwind())
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
	t.Cleanup(func() { app.Stop(context.Background()) }) // ends the assets' event stream
	return app, navs
}
