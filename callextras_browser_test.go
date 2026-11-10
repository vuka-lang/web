package web

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// orderNote is opened by both extras pages; from inside it a ConfirmBox
// opens over it.
type orderNote struct{ ID int64 }

func (n orderNote) View() ui.Node {
	return ui.El("section", nil,
		ui.El("p", []ui.Attr{attr("id", "note")}, ui.Text(fmt.Sprintf("order %d", n.ID))),
		ui.El("button", []ui.Attr{attr("id", "ask"), attr("type", "button"), attr("onClick", ui.On(ui.JS.Call(ui.ConfirmBox{Text: "Sure?"})))}, ui.Text("ask")))
}

func (n orderNote) Render(ctx context.Context, w io.Writer) error { return n.View().Render(ctx, w) }

func init() { ui.RegisterCall(orderNote{}) }

// tally keeps what the extras pages' typed Success saw.
type tally struct{ last chan int }

func extrasMarkup() ui.Node {
	b := func(id string, ch ui.Commands) ui.Node {
		return ui.El("button", []ui.Attr{attr("id", id), attr("type", "button"), attr("onClick", ui.On(ch))}, ui.Text(id))
	}
	return ui.Fragment(
		b("confirm", ui.JS.Confirm("Delete it?").Then(openLog("yes"))),
		b("native", ui.JS.Confirm("Native?", ui.NativeConfirm()).Then(openLog("native yes"))),
		b("toast", ui.JS.Call(ui.Notice{Text: "Saved", Timeout: 400 * time.Millisecond}).Closed(openLog("toast gone"))),
		b("pop", ui.JS.Call(orderNote{ID: 1}, ui.As(ui.Popover{})).Closed(openLog("pop gone"))),
		b("drawer", ui.JS.Call(orderNote{ID: 2}, ui.As(ui.Drawer{}), ui.SizeLg)),
		b("side", ui.JS.Call(orderNote{ID: 3}, ui.Into("#side"))),
		b("side2", ui.JS.Call(orderNote{ID: 4}, ui.Into("#side"))),
		b("modal", ui.JS.Call(orderNote{ID: 5}).Closed(openLog("modal gone"))),
		ui.El("div", []ui.Attr{attr("id", "side")}),
		ui.El("p", []ui.Attr{attr("id", "outside")}, ui.Text("outside")),
	)
}

type extrasPage struct{}

func (p *extrasPage) Count() (int, error)     { return 7, nil }
func (p *extrasPage) Counted(n int, t *tally) { t.last <- n }

func (p *extrasPage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("body", nil, ui.El("main", nil, extrasMarkup(),
		ui.El("button", []ui.Attr{attr("id", "count"), attr("type", "button"), attr("onClick", ui.On(ui.JS.Push(p.Count).Success(p.Counted)))}, ui.Text("count"))))).Render(ctx, w)
}

func init() {
	ui.RegisterPush((*extrasPage).Count)
	ui.RegisterPush((*extrasPage).Counted)
}

type liveExtras struct {
	ui.Live
	T *tally
}

func (p *liveExtras) Count() (int, error) { return 7, nil }
func (p *liveExtras) Counted(n int)       { p.T.last <- n }

func (p *liveExtras) Render() ui.Node {
	return ui.El("main", nil, extrasMarkup(),
		ui.El("button", []ui.Attr{attr("id", "count"), attr("type", "button"), attr("onClick", ui.On(ui.JS.Push(p.Count).Success(p.Counted)))}, ui.Text("count")))
}

// TestBrowserCallExtras: Confirm in a ConfirmBox (and the browser's with
// NativeConfirm), a Notice that closes itself, a Popover Escape and a press
// elsewhere close, a Drawer, Into, two modals stacked with the
// focus going back down, and a typed Success — on a page that isn't live
// and on a live one.
func TestBrowserCallExtras(t *testing.T) {
	tl := &tally{last: make(chan int, 4)}
	a := New(Only("none"), Provide(func() *tally { return tl }))
	a.Get("/extras", func() *extrasPage { return &extrasPage{} }, PageRoute())
	a.Live("/live-extras", &liveExtras{T: tl})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	for _, path := range []string{"/extras", "/live-extras"} {
		t.Run(strings.Trim(path, "/"), func(t *testing.T) {
			live := path == "/live-extras"
			var p *chrome.Page
			if live {
				p = b.Open(srv.URL + path)
			} else {
				p = b.Visit(srv.URL + path)
			}
			p.Eval(`window.steps = []; window.log = (ctx, s) => { window.steps.push(s); }; window.asked = []; window.confirm = (m) => (window.asked.push(m), true)`, nil)
			var got any
			eval := func(js string) string {
				t.Helper()
				p.Eval(js, &got)
				return fmt.Sprint(got)
			}

			// Confirm asks in a ConfirmBox; No stops the chain, Yes goes on.
			p.Click("#confirm")
			p.Wait(`document.querySelector("dialog[data-vk-confirm]")`)
			if s := eval(`(() => { const d = document.querySelector("dialog[data-vk-confirm]"); return d.matches(":modal") + " " + d.textContent; })()`); s != "true Delete it?CancelOK" {
				t.Errorf("a ConfirmBox: %s", s)
			}
			p.Click("[data-vk-no]")
			p.Wait(`!document.querySelector("dialog[data-vk-confirm]")`)
			p.Click("#confirm")
			p.Wait(`document.querySelector("dialog[data-vk-confirm]")`)
			p.Click("[data-vk-yes]")
			p.Wait(`window.steps.includes("yes")`)
			p.Click("#native")
			p.Wait(`window.steps.includes("native yes")`)
			if s := eval(`window.steps.join(",") + " " + window.asked.join(",")`); s != "yes,native yes Native?" {
				t.Errorf("No stopped, Yes went on, NativeConfirm asked the browser: %s", s)
			}

			// A Notice: in the host, not modal, gone by itself.
			p.Click("#toast")
			p.Wait(`document.querySelector("#vk-overlays [data-vk-present=toast]")`)
			p.Wait(`window.steps.includes("toast gone")`)

			// A Popover: shown, not modal, by its button; Escape, then a press elsewhere.
			p.Click("#pop")
			p.Wait(`document.querySelector("dialog.vk-popover[open]")`)
			if s := eval(`(() => { const d = document.querySelector("dialog.vk-popover"), r = document.querySelector("#pop").getBoundingClientRect(); return d.matches(":modal") + " " + (Math.abs(d.getBoundingClientRect().top - r.bottom - 4) < 2); })()`); s != "false true" {
				t.Errorf("a popover by its button: %s", s)
			}
			p.Press("Escape")
			p.Wait(`!document.querySelector("dialog.vk-popover") && window.steps.includes("pop gone")`)
			p.Click("#pop")
			p.Wait(`document.querySelector("dialog.vk-popover[open]")`)
			p.Click("#outside")
			p.Wait(`!document.querySelector("dialog.vk-popover")`)

			// A Drawer, its size.
			p.Click("#drawer")
			p.Wait(`document.querySelector("dialog.vk-drawer")`)
			if s := eval(`(() => { const d = document.querySelector("dialog.vk-drawer"); return d.matches(":modal") + " " + d.getAttribute("data-vk-size"); })()`); s != "true lg" {
				t.Errorf("a drawer: %s", s)
			}
			p.Press("Escape")
			p.Wait(`!document.querySelector("dialog.vk-drawer")`)

			// Into a region: in place, the second replacing the first.
			p.Click("#side")
			p.Wait(`document.querySelector("#side [data-vk-present=inline] #note")`)
			p.Click("#side2")
			p.WaitText("#side #note", "order 4")
			if s := eval(`document.querySelectorAll("#side [data-vk-call]").length`); s != "1" {
				t.Errorf("a region shows one: %s", s)
			}

			// Two modals stacked: Escape closes the top, the focus goes back down.
			p.Eval(`window.steps = []; document.querySelector("#modal").focus()`, nil)
			p.Click("#modal")
			p.Wait(`document.querySelector("dialog.vk-call:modal")`)
			p.Eval(`document.querySelector("dialog.vk-call #ask").focus()`, nil)
			p.Click("dialog.vk-call #ask")
			p.Wait(`document.querySelector("dialog.vk-confirm:modal")`)
			p.Press("Escape")
			p.Wait(`!document.querySelector("dialog.vk-confirm")`)
			if s := eval(`document.activeElement.id + " " + !!document.querySelector("dialog.vk-call:modal")`); s != "ask true" {
				t.Errorf("the top one closed, the focus back on its button: %s", s)
			}
			p.Press("Escape")
			p.Wait(`window.steps.includes("modal gone")`)
			if s := eval(`document.activeElement.id`); s != "modal" {
				t.Errorf("the focus back on the page's button: %s", s)
			}

			// A typed Success: the method gets the push's result.
			p.Click("#count")
			select {
			case n := <-tl.last:
				if n != 7 {
					t.Errorf("the result: %d", n)
				}
			case <-time.After(5 * time.Second):
				t.Error("Success's method wasn't called with the result")
			}
		})
	}
}
