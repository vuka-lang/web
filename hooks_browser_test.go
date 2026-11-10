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
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// basketDB is what the cart pages' methods change: a dependency, so a fresh
// copy of the page over HTTP and the live instance share it.
type basketDB struct {
	mu                 sync.Mutex
	removes, refreshes int
}

func (d *basketDB) counts() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fmt.Sprintf("%d %d", d.removes, d.refreshes)
}

func (d *basketDB) refreshCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.refreshes
}

func (d *basketDB) remove(id int64) error {
	if id == 0 {
		return BadRequest("no such item")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removes++
	return nil
}

func (d *basketDB) refresh() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refreshes++
}

func hookLog(s string) ui.Commands { return ui.JS.Call("log", s) }

// basketMarkup is what both cart pages render, pushing to their methods.
func basketMarkup(remove, slow, refresh any) ui.Node {
	b := func(id string, ch ui.Commands) ui.Node {
		return ui.El("button", []ui.Attr{attr("id", id), attr("type", "button"), attr("onClick", ui.On(ch))}, ui.Text(id))
	}
	full := func(id int64) ui.Commands {
		return ui.JS.Push(remove, id).
			Before(hookLog("before")).
			Success(hookLog("success")).
			Error(ui.JS.SetText("#err", ui.LoadError).Then(hookLog("error"))).
			After(hookLog("after"))
	}
	return ui.Fragment(
		b("ok", full(1)),
		b("bad", full(0)),
		b("confirm", ui.JS.Busy(ui.Sel("#list")).Push(remove, int64(1)).Before(ui.JS.Confirm("Remove?").Then(hookLog("before"))).After(hookLog("after"))),
		b("gate", ui.JS.Push(remove, int64(1)).Before(ui.JS.If(ui.Sel("#agree").Checked()).Call("log", "agreed")).After(hookLog("after"))),
		b("slow", ui.JS.Push(slow).Timeout(300*time.Millisecond).Success(hookLog("success")).Error(hookLog("error")).After(hookLog("after"))),
		b("boom", ui.JS.Push(remove, int64(1)).Success(ui.JS.Call("boom").Then(hookLog("success"))).Error(hookLog("error")).After(hookLog("after"))),
		b("method", ui.JS.Push(remove, int64(1)).Success(refresh).After(hookLog("after"))),
		ui.El("input", []ui.Attr{attr("id", "agree"), attr("type", "checkbox")}),
		ui.El("ul", []ui.Attr{attr("id", "list")}),
		ui.El("p", []ui.Attr{attr("id", "err")}),
	)
}

// basketPage isn't live: its pushes and hooks' methods go over HTTP to fresh
// copies of it.
type basketPage struct{}

func (p *basketPage) Remove(id int64, db *basketDB) error { return db.remove(id) }
func (p *basketPage) Slow()                               { time.Sleep(2 * time.Second) }
func (p *basketPage) Refresh(db *basketDB)                { db.refresh() }

func (p *basketPage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Cart"))), ui.El("body", nil, ui.El("main", nil,
		basketMarkup(p.Remove, p.Slow, p.Refresh)))).Render(ctx, w)
}

// liveBasket is the same markup on a live page: over its socket.
type liveBasket struct {
	ui.Live
	DB *basketDB
}

func (p *liveBasket) Remove(id int64) error { return p.DB.remove(id) }
func (p *liveBasket) Slow()                 { time.Sleep(2 * time.Second) }
func (p *liveBasket) Refresh()              { p.DB.refresh() }
func (p *liveBasket) Render() ui.Node {
	return ui.El("main", nil, basketMarkup(p.Remove, p.Slow, p.Refresh))
}

func init() {
	for _, m := range []any{(*basketPage).Remove, (*basketPage).Slow, (*basketPage).Refresh} {
		ui.RegisterPush(m)
	}
}

// TestBrowserPushLifecycle: Before, the request, Success or Error, After,
// in the same order on a page that isn't live (over HTTP) and on a live
// one (over its socket): a Before that stops sends nothing and takes Busy
// off; a timeout is an error; a failing Success runs After but not Error;
// a method as a hook is pushed; on the live page, a connection lost while
// waiting fails the push.
func TestBrowserPushLifecycle(t *testing.T) {
	db := &basketDB{}
	a := New(Only("none"), Provide(func() *basketDB { return db }))
	a.Get("/cart", func() *basketPage { return &basketPage{} }, PageRoute())
	a.Live("/live-cart", &liveBasket{DB: db})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	for _, path := range []string{"/cart", "/live-cart"} {
		t.Run(strings.Trim(path, "/"), func(t *testing.T) {
			live := path == "/live-cart"
			var p *chrome.Page
			if live {
				p = b.Open(srv.URL + path)
			} else {
				p = b.Visit(srv.URL + path)
			}
			p.Eval(`window.log = (ctx, s) => { window.steps.push(s); }; window.boom = () => { throw new Error("boom"); }`, nil)
			steps := func(click string, want string) {
				t.Helper()
				p.Eval(`window.steps = []`, nil)
				p.Click(click)
				p.Wait(`window.steps.includes("after")`)
				var got any
				p.Eval(`window.steps.join(",")`, &got)
				if got != want {
					t.Errorf("%s: got %v, want %s", click, got, want)
				}
			}
			steps("#ok", "before,success,after")
			steps("#bad", "before,error,after")
			if s := p.Text("#err"); s != "no such item" {
				t.Errorf("ui.LoadError in Error: %q", s)
			}
			steps("#slow", "error,after")
			steps("#boom", "after")
			refreshes := db.refreshCount()
			steps("#method", "after")
			for deadline := time.Now().Add(5 * time.Second); db.refreshCount() == refreshes; time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the method given to Success wasn't pushed")
				}
			}

			// a Before that stops
			before := db.counts()
			p.Eval(`window.steps = []; window.confirm = () => false`, nil)
			p.Click("#confirm")
			p.Click("#gate")
			time.Sleep(300 * time.Millisecond)
			var got any
			p.Eval(`[window.steps.join(","), document.querySelector("#list").getAttribute("aria-busy")]`, &got)
			if fmt.Sprint(got) != "[ <nil>]" || db.counts() != before {
				t.Errorf("stopped: nothing sent, no hook, no busy: %v %s → %s", got, before, db.counts())
			}
			p.Eval(`window.confirm = () => true; document.querySelector("#agree").checked = true`, nil)
			steps("#confirm", "before,after")
			steps("#gate", "agreed,after")
			p.Eval(`document.querySelector("#list").getAttribute("aria-busy")`, &got)
			if got != nil {
				t.Errorf("busy after the reply: %v", got)
			}
			if !live {
				return
			}
			p.Eval(`window.steps = []; window.__wire.sockets.forEach((s) => s.close(4000, "dropped")); document.querySelector("#ok").click()`, nil)
			p.Wait(`window.steps.includes("after")`)
			p.Eval(`window.steps.join(",")`, &got)
			if got != "before,error,after" {
				t.Errorf("a lost connection: %v", got)
			}
		})
	}
}
