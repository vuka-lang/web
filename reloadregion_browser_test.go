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

// invoiceDB is the invoices the invoice pages list.
type invoiceDB struct {
	mu  sync.Mutex
	ids []int
}

func (d *invoiceDB) list() []int { d.mu.Lock(); defer d.mu.Unlock(); return slices.Clone(d.ids) }
func (d *invoiceDB) add(id int)  { d.mu.Lock(); d.ids = append(d.ids, id); d.mu.Unlock() }
func (d *invoiceDB) delete(id int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids = slices.DeleteFunc(d.ids, func(x int) bool { return x == id })
}

// invoiceTable is the region: a table whose rows a delete reloads.
func invoiceTable(ids []int, del any) ui.Node {
	rows := make([]ui.Node, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, ui.El("tr", []ui.Attr{attr("key", id)}, ui.El("td", nil, ui.Textf("invoice %d", id)),
			ui.El("td", nil, ui.El("button", []ui.Attr{attr("id", "delete-"+strconv.Itoa(id)), attr("type", "button"),
				attr("onClick", ui.On(ui.JS.Push(del, id).Success(ui.JS.Reload("#invoices"))))}, ui.Text("delete")))))
	}
	return ui.El("table", []ui.Attr{attr("id", "invoices")}, ui.El("tbody", nil, rows...))
}

// invoiceFilter is an open filter dialog with a field to type in, and a
// button reloading the table.
func invoiceFilter(open bool, opener ui.JSChain) ui.Node {
	dialog := []ui.Attr{attr("id", "filter")}
	if open {
		dialog = append(dialog, attr("open", ""))
	}
	return ui.Fragment(
		ui.El("button", []ui.Attr{attr("id", "open"), attr("type", "button"), attr("onClick", ui.On(opener))}, ui.Text("filter")),
		ui.El("button", []ui.Attr{attr("id", "refresh"), attr("type", "button"), attr("onClick", ui.On(ui.JS.Reload("#invoices")))}, ui.Text("refresh")),
		ui.El("dialog", dialog, ui.El("input", []ui.Attr{attr("id", "q")})))
}

// invoicesPage isn't live: the dialog opens in the browser.
type invoicesPage struct{ DB *invoiceDB }

func (p *invoicesPage) Delete(id int, db *invoiceDB) { db.delete(id) }
func (p *invoicesPage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Invoices"))), ui.El("body", nil, ui.El("main", nil,
		invoiceFilter(false, ui.JS.SetAttr("#filter", "open", "")),
		invoiceTable(p.DB.list(), p.Delete)))).Render(ctx, w)
}

// liveInvoices is live: the dialog is open while its field says, and the
// table is a component of its own.
type liveInvoices struct {
	ui.Live
	DB   *invoiceDB
	open bool
}

func (p *liveInvoices) OpenFilter() { p.open = true }
func (p *liveInvoices) Render() ui.Node {
	return ui.El("main", nil, invoiceFilter(p.open, ui.JS.Push(p.OpenFilter)), ui.Component("invoices.table", nil, &liveInvoiceTable{DB: p.DB}))
}

type liveInvoiceTable struct {
	ui.Live
	DB *invoiceDB
}

func (t *liveInvoiceTable) Delete(id int)   { t.DB.delete(id) }
func (t *liveInvoiceTable) Render() ui.Node { return invoiceTable(t.DB.list(), t.Delete) }

func init() { ui.RegisterPush((*invoicesPage).Delete) }

// TestBrowserReloadRegion: on a page that isn't live and on a live one,
// reloading the table's region shows rows added since and drops a deleted
// one, while the filter dialog stays open and the text typed in it stays.
func TestBrowserReloadRegion(t *testing.T) {
	defer func(v bool) { live.Verify = v }(live.Verify)
	live.Verify = false // the table reads the database
	db := &invoiceDB{}
	a := New(Only("none"), Provide(func() *invoiceDB { return db }))
	a.Get("/invoices", func() *invoicesPage { return &invoicesPage{} }, PageRoute())
	a.Live("/live-invoices", func() *liveInvoices { return &liveInvoices{} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	for _, path := range []string{"/invoices", "/live-invoices"} {
		t.Run(strings.Trim(path, "/"), func(t *testing.T) {
			db.mu.Lock()
			db.ids = []int{1, 2}
			db.mu.Unlock()
			var p *chrome.Page
			if path == "/live-invoices" {
				p = b.Open(srv.URL + path + "?page=2")
			} else {
				p = b.Visit(srv.URL + path + "?page=2")
			}
			rows := func() string {
				var got string
				p.Eval(`Array.from(document.querySelectorAll("#invoices tr td:first-child"), (td) => td.textContent).join(",")`, &got)
				return got
			}
			p.Click("#open")
			p.Wait(`document.querySelector("#filter").hasAttribute("open")`)
			p.Eval(`document.querySelector("#q").focus(); document.querySelector("#q").value = "overdue"`, nil)
			kept := func(what string) {
				t.Helper()
				var got []any
				p.Eval(`[document.querySelector("#filter").hasAttribute("open"), document.querySelector("#q").value, location.pathname + location.search]`, &got)
				if fmt.Sprint(got) != fmt.Sprintf("[true overdue %s?page=2]", path) {
					t.Errorf("%s: %v", what, got)
				}
			}
			db.add(3)
			p.Eval(`document.querySelector("#refresh").click()`, nil)
			p.Wait(`document.querySelectorAll("#invoices tr").length === 3`)
			if got := rows(); got != "invoice 1,invoice 2,invoice 3" {
				t.Errorf("the new row: %s", got)
			}
			kept("after a refresh")
			p.Eval(`document.querySelector("#delete-1").click()`, nil)
			p.Wait(`document.querySelectorAll("#invoices tr").length === 2`)
			time.Sleep(100 * time.Millisecond)
			if got := rows(); got != "invoice 2,invoice 3" {
				t.Errorf("after a delete: %s", got)
			}
			kept("after a delete")
		})
	}
}

type invoicesChanged struct{ reload, inReply bool }

// watchedDB is what watchedTable lists: read from a package variable, so
// the table has no props, and with no state of its own, is skipped by the
// page's renders until something renders its region.
var watchedDB = &invoiceDB{}

// watchedInvoices is a page whose watcher, a component beside its table,
// hears of new invoices by a broadcast. With reload the watcher's Update
// has the table's region render again.
type watchedInvoices struct{ ui.Live }

func (p *watchedInvoices) Render() ui.Node {
	return ui.El("main", nil, ui.El("input", []ui.Attr{attr("id", "q")}),
		ui.Component("invoices.watcher", nil, &invoiceWatcher{}),
		ui.Component("invoices.table", nil, &watchedTable{}))
}

type invoiceWatcher struct{ ui.Live }

func (w *invoiceWatcher) LiveSchema(l *ui.LiveSchema) { l.Mount(w.mount) }
func (w *invoiceWatcher) mount(context.Context) error { w.Subscribe("invoices"); return nil }
func (w *invoiceWatcher) Update(m invoicesChanged) {
	if m.inReply {
		w.Reload("#invoices")
	} else if m.reload {
		w.PushJS(ui.JS.Reload("#invoices"))
	}
}
func (w *invoiceWatcher) Render() ui.Node { return ui.El("span", nil) }

type watchedTable struct{ ui.Live }

func (t *watchedTable) Delete(id int)   { watchedDB.delete(id) }
func (t *watchedTable) Render() ui.Node { return invoiceTable(watchedDB.list(), t.Delete) }

// TestBrowserReloadRegionFromServer: a handler on the server (here a
// broadcast's Update, with no event from the browser) re-renders one region
// of the page, which the page's renders otherwise skip, by pushing
// Reload("#id"): the region shows the rows added since, and what the
// visitor typed elsewhere stays.
func TestBrowserReloadRegionFromServer(t *testing.T) {
	defer func(v bool) { live.Verify = v }(live.Verify)
	live.Verify = false // the table reads the database
	watchedDB.ids = []int{1}
	a := New(Only("none"))
	a.Live("/watched", func() *watchedInvoices { return &watchedInvoices{} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/watched")
	p.Eval(`document.querySelector("#q").value = "overdue"`, nil)
	watchedDB.add(2)

	if err := a.Broadcast(context.Background(), "invoices", invoicesChanged{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	var n int
	if p.Eval(`document.querySelectorAll("#invoices tr").length`, &n); n != 1 {
		t.Fatalf("a broadcast without the reload rendered the region: %d rows", n)
	}

	if err := a.Broadcast(context.Background(), "invoices", invoicesChanged{reload: true}); err != nil {
		t.Fatal(err)
	}
	p.Wait(`document.querySelectorAll("#invoices tr").length === 2`)
	var q string
	if p.Eval(`document.querySelector("#q").value`, &q); q != "overdue" {
		t.Errorf("the field beside the region after its reload: %q", q)
	}
}

// TestBrowserReloadInReply: a broadcast's Update has the region's render
// carried in the broadcast's own patch (ui.Live.Reload): the region shows
// the rows added since, what the visitor typed elsewhere stays, and the
// browser sends nothing — no vkreload round trip.
func TestBrowserReloadInReply(t *testing.T) {
	defer func(v bool) { live.Verify = v }(live.Verify)
	live.Verify = false // the table reads the database
	watchedDB.ids = []int{1}
	a := New(Only("none"))
	a.Live("/watched", func() *watchedInvoices { return &watchedInvoices{} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/watched")
	var ok bool
	p.Eval(sentLog, &ok)
	p.Eval(`document.querySelector("#q").value = "overdue"`, nil)
	watchedDB.add(2)

	if err := a.Broadcast(context.Background(), "invoices", invoicesChanged{inReply: true}); err != nil {
		t.Fatal(err)
	}
	p.Wait(`document.querySelectorAll("#invoices tr").length === 2`)
	var q string
	if p.Eval(`document.querySelector("#q").value`, &q); q != "overdue" {
		t.Errorf("the field beside the region after its reload: %q", q)
	}
	if n := sent(t, p, `m.event === "vkreload"`); n != 0 {
		t.Errorf("the render came in the reply, yet %d vkreload events were sent", n)
	}
}
