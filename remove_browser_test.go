package web

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// shipmentDB is the shipments the shipment pages list, and whether a
// delete fails.
type shipmentDB struct {
	mu   sync.Mutex
	ids  []int
	fail bool
}

func (d *shipmentDB) list() []int    { d.mu.Lock(); defer d.mu.Unlock(); return slices.Clone(d.ids) }
func (d *shipmentDB) setFail(v bool) { d.mu.Lock(); d.fail = v; d.mu.Unlock() }
func (d *shipmentDB) delete(id int) error {
	time.Sleep(300 * time.Millisecond) // the row is gone before the reply
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fail {
		return BadRequest("still on its way")
	}
	d.ids = slices.DeleteFunc(d.ids, func(x int) bool { return x == id })
	return nil
}

// shipmentsMarkup is a keyed table whose rows a delete removes at once.
func shipmentsMarkup(ids []int, del any) ui.Node {
	rows := make([]ui.Node, 0, len(ids))
	for _, id := range ids {
		ch := ui.JS.Push(del, id).
			Before(ui.JS.Remove(ui.Closest("tr"))).
			Error(ui.JS.SetText("#err", ui.LoadError)).
			After(ui.JS.Call("log", "after"))
		rows = append(rows, ui.El("tr", []ui.Attr{attr("id", "row-"+strconv.Itoa(id)), attr("key", id)},
			ui.El("td", nil, ui.Textf("shipment %d", id)),
			ui.El("td", nil, ui.El("button", []ui.Attr{attr("class", "delete"), attr("type", "button"), attr("onClick", ui.On(ch))}, ui.Text("delete")))))
	}
	return ui.El("main", nil, ui.El("p", []ui.Attr{attr("id", "err")}), ui.El("table", nil, ui.El("tbody", nil, rows...)))
}

// shipmentsPage isn't live: its delete runs on a fresh copy over HTTP.
type shipmentsPage struct{ DB *shipmentDB }

func (p *shipmentsPage) Delete(id int, db *shipmentDB) error { return db.delete(id) }
func (p *shipmentsPage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Shipments"))), ui.El("body", nil,
		shipmentsMarkup(p.DB.list(), p.Delete))).Render(ctx, w)
}

// liveShipments is the same table on a live page.
type liveShipments struct {
	ui.Live
	DB *shipmentDB
}

func (p *liveShipments) Delete(id int) error { return p.DB.delete(id) }
func (p *liveShipments) Render() ui.Node     { return shipmentsMarkup(p.DB.list(), p.Delete) }

func init() { ui.RegisterPush((*shipmentsPage).Delete) }

// TestBrowserRemove: on a page that isn't live and on a live one, a row a
// delete's Before removes is gone at once; when the delete fails it comes
// back in its place before Error runs, and stays through the next render;
// when it succeeds it stays gone.
func TestBrowserRemove(t *testing.T) {
	defer func(v bool) { live.Verify = v }(live.Verify)
	live.Verify = false // the rows are read from the database
	db := &shipmentDB{}
	a := New(Only("none"), Provide(func() *shipmentDB { return db }))
	a.Get("/shipments", func() *shipmentsPage { return &shipmentsPage{} }, PageRoute())
	a.Live("/live-shipments", func() *liveShipments { return &liveShipments{} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	for _, path := range []string{"/shipments", "/live-shipments"} {
		t.Run(strings.Trim(path, "/"), func(t *testing.T) {
			db.mu.Lock()
			db.ids, db.fail = []int{1, 2, 3}, true
			db.mu.Unlock()
			var p *chrome.Page
			if path == "/live-shipments" {
				p = b.Open(srv.URL + path)
			} else {
				p = b.Visit(srv.URL + path)
			}
			p.Eval(`window.steps = []; window.log = (ctx, s) => { window.steps.push(s); }`, nil)
			rows := func() string {
				var got string
				p.Eval(`Array.from(document.querySelectorAll("tbody tr"), (r) => r.id).join(" ")`, &got)
				return got
			}
			p.Eval(`document.querySelector("#row-2 .delete").click()`, nil)
			if got := rows(); got != "row-1 row-3" {
				t.Fatalf("at once: %s", got)
			}
			p.Wait(`window.steps.includes("after")`)
			if got, err := rows(), p.Text("#err"); got != "row-1 row-2 row-3" || err != "still on its way" {
				t.Fatalf("after the error: %s, %q", got, err)
			}
			var failed bool
			p.Eval(`document.querySelector("#row-2 .delete").hasAttribute("data-vk-failed")`, &failed)
			if !failed {
				t.Error("the restored row's button isn't marked failed")
			}

			db.setFail(false)
			p.Eval(`window.steps = []; document.querySelector("#row-2 .delete").click()`, nil)
			if got := rows(); got != "row-1 row-3" {
				t.Fatalf("at once, again: %s", got)
			}
			p.Wait(`window.steps.includes("after")`)
			time.Sleep(200 * time.Millisecond)
			if got := rows(); got != "row-1 row-3" || fmt.Sprint(db.list()) != "[1 3]" {
				t.Fatalf("after the delete: %s, %v", got, db.list())
			}
		})
	}
}
