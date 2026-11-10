package web

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// stockDB is what the stock pages show and their methods change,
// outside the pages' state.
type stockDB struct {
	mu    sync.Mutex
	stock int
}

func (d *stockDB) touch() { d.mu.Lock(); d.stock++; d.mu.Unlock() }
func (d *stockDB) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stock
}

// stockMarkup is what both stock pages render: the stock, the page
// of the query, a field to type in, a button changing the stock without a
// reload, one reloading in its Success and one whose method returns the
// reload. The notes are tall enough to scroll.
func stockMarkup(page, stock int, touch, touchReload any) ui.Node {
	b := func(id string, ch ui.Commands) ui.Node {
		return ui.El("button", []ui.Attr{attr("id", id), attr("type", "button"), attr("onClick", ui.On(ch))}, ui.Text(id))
	}
	return ui.El("main", nil,
		ui.El("p", []ui.Attr{attr("id", "stock")}, ui.Textf("stock %d page %d", stock, page)),
		ui.El("input", []ui.Attr{attr("id", "q")}),
		b("stale", ui.JS.Push(touch)),
		b("touch", ui.JS.Push(touch).Success(ui.JS.Reload())),
		b("returned", ui.JS.Push(touchReload)),
		ui.El("div", []ui.Attr{attr("id", "notes"), attr("style", "height: 3000px")}, ui.Text("notes")),
	)
}

// stockPage isn't live: its methods run on fresh copies over HTTP.
type stockPage struct {
	Page int
	DB   *stockDB
}

func (p *stockPage) Title() string                       { return "Products" }
func (p *stockPage) Touch(db *stockDB)                   { db.touch() }
func (p *stockPage) TouchReload(db *stockDB) ui.Commands { db.touch(); return ui.JS.Reload() }
func (p *stockPage) Render(ctx context.Context, w io.Writer) error {
	return stockMarkup(p.Page, p.DB.count(), p.Touch, p.TouchReload).Render(ctx, w)
}

// liveStock is the same markup on a live page whose state is its props:
// a method changing only the stock renders nothing again by itself.
type liveStock struct {
	ui.Live
	Page int
	DB   *stockDB
}

func (p *liveStock) Title() string            { return "Products" }
func (p *liveStock) Touch()                   { p.DB.touch() }
func (p *liveStock) TouchReload() ui.Commands { p.DB.touch(); return ui.JS.Reload() }
func (p *liveStock) Render() ui.Node {
	return stockMarkup(p.Page, p.DB.count(), p.Touch, p.TouchReload)
}

func init() {
	for _, m := range []any{(*stockPage).Touch, (*stockPage).TouchReload} {
		ui.RegisterPush(m)
	}
}

// TestBrowserReload: on a page that isn't live and on a live one, a push
// whose method changes only data outside the page's state shows the new
// value once Reload runs, from its Success or from the commands the method
// returns; the query and the #hash stay, and so do scroll, focus and what
// was typed; the title is the layout's, as on a first load.
func TestBrowserReload(t *testing.T) {
	defer func(v bool) { live.Verify = v }(live.Verify)
	live.Verify = false // the pages read the stock, which isn't their state
	db := &stockDB{}
	a := New(Only("none"), Provide(func() *stockDB { return db }))
	a.Get("/stock", func() *stockPage { return &stockPage{} }, PageRoute(), LayoutOf[suffixLayout]())
	a.Live("/live-stock", func() *liveStock { return &liveStock{} }, LayoutOf[suffixLayout]())
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	for _, path := range []string{"/stock", "/live-stock"} {
		t.Run(strings.Trim(path, "/"), func(t *testing.T) {
			url := srv.URL + path + "?page=2#notes"
			var p *chrome.Page
			if path == "/live-stock" {
				p = b.Open(url)
			} else {
				p = b.Visit(url)
			}
			p.Wait(`document.title === "Products · Shop"`)
			stock := func() string { return fmt.Sprintf("stock %d page 2", db.count()) }
			p.WaitText("#stock", stock())
			p.Eval(`document.querySelector("#q").focus(); document.querySelector("#q").value = "typed"; window.scrollTo(0, 1200); window.marked = true`, nil)
			var entries float64
			p.Eval(`history.length`, &entries)
			click := func(button string) {
				t.Helper()
				n := db.count()
				p.Eval(`document.querySelector("`+button+`").click()`, nil)
				for deadline := time.Now().Add(5 * time.Second); db.count() == n; time.Sleep(10 * time.Millisecond) {
					if time.Now().After(deadline) {
						t.Fatalf("%s: the method didn't run", button)
					}
				}
			}
			if path == "/stock" {
				click("#stale")
				time.Sleep(300 * time.Millisecond)
				if got := p.Text("#stock"); got == stock() {
					t.Fatalf("the stock shows %q without a reload", got)
				}
			}
			for _, button := range []string{"#touch", "#returned"} {
				click(button)
				p.WaitText("#stock", stock())
				var got []any
				p.Eval(`[location.pathname + location.search + location.hash, document.title, document.activeElement.id, document.querySelector("#q").value, Math.round(scrollY), window.marked === true, history.length]`, &got)
				if want := fmt.Sprint([]any{path + "?page=2#notes", "Products · Shop", "q", "typed", float64(1200), true, entries}); fmt.Sprint(got) != want {
					t.Errorf("%s: after the reload %v, want %s", button, got, want)
				}
			}
		})
	}
}
