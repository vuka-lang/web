package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// onceStore is storage a read empties: what an action left for the next
// render of the page, as a flash or a just-made secret is kept.
var onceStore atomic.Pointer[string]

// oncePage shows a token with ui.ShowOnce: the one its mount took from
// onceStore, or one Gen makes. Inc renders the page again having forgotten
// the token, as a page that doesn't keep a secret does.
type oncePage struct {
	ui.Live
	token string
	key   any
	n     int
	gens  int
}

var onceMounts atomic.Int32

func (c *oncePage) LiveSchema(l *ui.LiveSchema) { l.Mount(c.mount) }
func (c *oncePage) mount(context.Context) error {
	onceMounts.Add(1)
	if s := onceStore.Swap(nil); s != nil {
		c.token, c.key = *s, ui.ShowOnceKey()
	}
	return nil
}
func (c *oncePage) Inc() { c.n++; c.token, c.key = "", nil }
func (c *oncePage) Gen() {
	c.gens++
	c.token, c.key = fmt.Sprintf("second%d", c.gens), ui.ShowOnceKey()
}
func (c *oncePage) Render() ui.Node {
	return ui.El("div", nil,
		ui.El("span", []ui.Attr{{Name: "id", Value: "n"}}, ui.Text(fmt.Sprintf("n%d", c.n))),
		ui.El("button", []ui.Attr{{Name: "id", Value: "inc"}, {Name: "onClick", Value: ui.On(c.Inc)}}, ui.Text("+")),
		ui.El("button", []ui.Attr{{Name: "id", Value: "gen"}, {Name: "onClick", Value: ui.On(c.Gen)}}, ui.Text("gen")),
		ui.ShowOnce{ID: "token", Key: c.key, Children: ui.El("code", nil, ui.Text(c.token))})
}

// onceOther is another page with a ShowOnce of the same ID, never given a
// value.
type onceOther struct{ ui.Live }

func (c *onceOther) Render() ui.Node {
	return ui.El("div", nil,
		ui.El("p", []ui.Attr{{Name: "id", Value: "other"}}, ui.Text("other")),
		ui.ShowOnce{ID: "token"})
}

// The value a ShowOnce shows stays on screen through the join's render
// (the session adopted or mounted afresh, the stored value gone), an
// event's render of a page that forgot it, and the page recovering in
// place; a second value replaces it; leaving the page takes it off, the
// snapshot back shows included.
func TestBrowserShowOnce(t *testing.T) {
	b := chrome.Launch(t)
	for _, adopt := range []bool{true, false} {
		t.Run(fmt.Sprintf("adopt=%v", adopt), func(t *testing.T) {
			opts := []Option{Only("none"), NoTailwind()}
			if !adopt {
				opts = append(opts, LiveAdopt(0))
			}
			app := New(opts...)
			frame := LayoutOf[navFrame]()
			app.Live("/once", &oncePage{}, frame)
			app.Live("/other", &onceOther{}, frame)
			srv := httptest.NewServer(app.Handler())
			defer srv.Close()

			onceMounts.Store(0)
			first := "first-secret"
			onceStore.Store(&first)
			const token = `(document.querySelector("#vk-once-token") || {}).textContent`
			p := b.Open(srv.URL + "/once")
			p.Wait(`document.documentElement.dataset.vkState === "connected"`)
			p.Wait(token + ` === "first-secret"`)
			want := int32(1)
			if !adopt {
				want = 2 // the join mounted again, the stored value gone
			}
			if m := onceMounts.Load(); m != want {
				t.Fatalf("mounts = %d, want %d", m, want)
			}

			p.Click("#inc")
			p.WaitText("#n", "n1")
			if got := p.Text("#vk-once-token"); got != "first-secret" {
				t.Fatalf("an event's render took the value off: %q", got)
			}

			p.Click("#gen")
			p.Wait(token + ` === "second1"`)
			p.Click("#inc")
			p.WaitText("#n", "n2")
			if got := p.Text("#vk-once-token"); got != "second1" {
				t.Fatalf("the second value went: %q", got)
			}

			closeAll(t, app) // in place: the page fetched again, its new session holding no value
			p.WaitText("#n", "n0")
			p.Wait(`document.documentElement.dataset.vkState === "connected"`)
			if got := p.Text("#vk-once-token"); got != "second1" {
				t.Fatalf("recovering in place took the value off: %q", got)
			}

			p.Eval(`window.__shown = false;
new MutationObserver(() => {
	const e = document.querySelector("#vk-once-token");
	if (e && e.textContent) window.__shown = true;
}).observe(document.documentElement, { childList: true, subtree: true, characterData: true });
window.vuka.navigate("/other"); true`, nil)
			p.WaitText("#other", "other")
			p.Wait(`document.querySelector("#vk-once-token") && document.querySelector("#vk-once-token").textContent === ""`)

			p.Eval(`history.back(); true`, nil)
			p.Wait(`location.pathname === "/once" && !document.documentElement.hasAttribute("data-vk-preview") && document.querySelector("#n") && document.querySelector("#n").textContent === "n0"`)
			if got := p.Text("#vk-once-token"); got != "" {
				t.Fatalf("back shows the value again: %q", got)
			}
			var shown bool
			if err := p.Eval(`window.__shown`, &shown); err != nil || shown {
				t.Fatalf("the value showed again after the page was left (%v)", err)
			}
		})
	}
}
