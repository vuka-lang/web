package web

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// overlays is a page whose markup a script changes, as a component
// library's does: it moves a popup to the body while open, marks its
// trigger open, shows a chosen value in place of the server's.
type overlays struct {
	ui.Live
	n int
}

func (c *overlays) Inc()            { c.n++ }
func (c *overlays) Render() ui.Node { return c.render(c.Inc) }
func (c *overlays) render(inc func()) ui.Node {
	at := func(name string, v any) ui.Attr { return ui.Attr{Name: name, Value: v} }
	var popup ui.Node
	if c.n < 4 {
		popup = ui.El("div", []ui.Attr{at("class", "portal"), at("hidden", true)},
			ui.El("p", []ui.Attr{at("id", "pop")}, ui.Text(fmt.Sprintf("count %d", c.n))),
			ui.El("button", []ui.Attr{at("id", "inner"), at("onClick", ui.On(inc))}, ui.Text("+")))
	}
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{at("id", "trigger"), at("aria-expanded", "false"), at("title", fmt.Sprintf("n%d", c.n/2))}, ui.Text("open")),
		ui.El("div", []ui.Attr{at("id", "home")}, popup),
		ui.El("span", []ui.Attr{at("id", "shown")}, ui.Text("lamps")),
		ui.El("input", []ui.Attr{at("id", "chosen"), at("type", "hidden"), at("value", "lamps")}),
		ui.El("output", []ui.Attr{at("id", "n")}, ui.Text(fmt.Sprint(c.n))),
		ui.El("button", []ui.Attr{at("id", "inc"), at("onClick", ui.On(inc))}, ui.Text("inc")))
}

// A real browser: what the page's scripts did to the server's markup
// outlives patches — a moved popup is patched where it is, never put back,
// and goes when the server stops rendering it; changed attributes and
// children stay the script's until the server renders something else.
func TestBrowserScriptOwnedState(t *testing.T) {
	app := New(Only("none"))
	app.Live("/overlays", &overlays{})
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)

	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/overlays")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	p.Eval(`(() => {
		const portal = document.querySelector(".portal");
		document.body.appendChild(portal);
		portal.hidden = false;
		portal.setAttribute("data-open", "");
		document.querySelector("#trigger").setAttribute("aria-expanded", "true");
		document.querySelector("#shown").textContent = "chairs";
		document.querySelector("#chosen").value = "chairs";
		window.__portal = portal;
		return true;
	})()`, nil)
	for i := 1; i <= 3; i++ {
		p.Click("#inner")
		p.WaitText("#n", fmt.Sprint(i))
		p.WaitText("#pop", fmt.Sprintf("count %d", i))
		var ok bool
		p.Eval(`document.querySelectorAll(".portal").length === 1 && document.querySelector(".portal") === window.__portal &&
			window.__portal.parentNode === document.body && !window.__portal.hidden && window.__portal.hasAttribute("data-open") &&
			document.querySelector("#home").children.length === 0 &&
			document.querySelector("#trigger").getAttribute("aria-expanded") === "true" &&
			document.querySelector("#shown").textContent === "chairs" && document.querySelector("#chosen").value === "chairs"`, &ok)
		if !ok {
			var h string
			p.Eval(`document.body.innerHTML`, &h)
			t.Fatalf("after %d patches the script's changes are gone: %s", i, h)
		}
	}
	var title string
	p.Eval(`document.querySelector("#trigger").title`, &title)
	if title != "n1" {
		t.Errorf("an attribute the server changed shows %q, want n1", title)
	}
	p.Click("#inc")
	p.WaitText("#n", "4")
	p.Wait(`document.querySelectorAll(".portal").length === 0 && !window.__portal.isConnected`)
}

// openOverlays is overlays with its popup moved by a script that runs
// before the live runtime: open as the page loads.
type openOverlays struct{ overlays }

func (c *openOverlays) Inc() { c.n++ }
func (c *openOverlays) Render() ui.Node {
	return ui.Group{c.overlays.render(c.Inc), ui.Safe(`<script type="module">
		const portal = document.querySelector(".portal");
		document.body.appendChild(portal);
		portal.hidden = false;
		window.__portal = portal;
	</script>`)}
}

// A popup moved before the live runtime ran is patched where it is too.
func TestBrowserMovedBeforeJoin(t *testing.T) {
	app := New(Only("none"))
	app.Live("/open", &openOverlays{})
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)

	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/open")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	for i := 1; i <= 2; i++ {
		p.Click("#inner")
		p.WaitText("#pop", fmt.Sprintf("count %d", i))
	}
	var ok bool
	p.Eval(`document.querySelectorAll(".portal").length === 1 && document.querySelector(".portal") === window.__portal &&
		window.__portal.parentNode === document.body && !window.__portal.hidden`, &ok)
	if !ok {
		var h string
		p.Eval(`document.body.innerHTML`, &h)
		t.Fatalf("the popup moved before the join was put back: %s", h)
	}
}
