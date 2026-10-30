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

// popups is a page whose popup a script opens as a component library's
// select does: the popup goes to the body, a backdrop goes beside its
// positioner and focus guards beside the place it was declared.
type popups struct {
	ui.Live
	n int
}

func (c *popups) Inc() { c.n++ }
func (c *popups) Render() ui.Node {
	at := func(name string, v any) ui.Attr { return ui.Attr{Name: name, Value: v} }
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{at("id", "trigger")}, ui.Text("open")),
		ui.El("div", []ui.Attr{at("id", "home")},
			ui.El("div", []ui.Attr{at("class", "portal"), at("hidden", true)},
				ui.El("div", []ui.Attr{at("class", "positioner"), at("hidden", true)},
					ui.El("p", []ui.Attr{at("id", "pop")}, ui.Text(fmt.Sprintf("count %d", c.n))),
					ui.El("button", []ui.Attr{at("id", "inner"), at("onClick", ui.On(c.Inc))}, ui.Text("+"))))),
		ui.El("output", []ui.Attr{at("id", "n")}, ui.Text(fmt.Sprint(c.n))),
		ui.El("button", []ui.Attr{at("id", "inc"), at("onClick", ui.On(c.Inc))}, ui.Text("inc")))
}

// Elements a script added beside the server's — a popup's backdrop in the
// portal, focus guards where the popup was declared — stay where it put
// them through patches, while the popup is open and while it closes: the
// server's elements are matched past them, so the positioner the script
// holds is patched, not replaced by the backdrop made over.
func TestBrowserScriptAddedSiblings(t *testing.T) {
	app := New(Only("none"))
	app.Live("/popups", &popups{})
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)

	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/popups")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	p.Eval(`(() => {
		const portal = document.querySelector(".portal"), pos = portal.firstElementChild, home = portal.parentElement;
		document.body.appendChild(portal);
		portal.hidden = false;
		pos.hidden = false;
		pos.setAttribute("data-open", "");
		const backdrop = document.createElement("div");
		backdrop.className = "backdrop";
		pos.before(backdrop);
		const guards = [0, 1].map(() => { const g = document.createElement("span"); g.className = "guard"; g.tabIndex = 0; return g; });
		home.before(guards[0]);
		home.after(guards[1]);
		Object.assign(window, { __portal: portal, __pos: pos, __backdrop: backdrop, __guards: guards });
		return true;
	})()`, nil)
	kept := `document.querySelectorAll(".positioner").length === 1 && document.querySelector(".positioner") === window.__pos &&
		window.__pos.parentNode === window.__portal && window.__portal.parentNode === document.body &&
		window.__pos.previousSibling === window.__backdrop && window.__portal.children.length === 2 &&
		window.__guards[0].nextSibling === document.querySelector("#home") && window.__guards[1].previousSibling === document.querySelector("#home") &&
		document.querySelector("#home").children.length === 0`
	check := func(when, extra string) {
		t.Helper()
		var ok bool
		p.Eval(kept+" && "+extra, &ok)
		if !ok {
			var h string
			p.Eval(`document.body.innerHTML`, &h)
			t.Fatalf("%s the script's elements or the popup it holds are gone: %s", when, h)
		}
	}
	for i := 1; i <= 2; i++ {
		p.Click("#inner")
		p.WaitText("#pop", fmt.Sprintf("count %d", i))
		check(fmt.Sprintf("open, after %d patches,", i), `!window.__pos.hidden && window.__pos.hasAttribute("data-open")`)
	}
	// Closing: an animation plays before the script takes its elements out.
	p.Eval(`(() => {
		window.__pos.removeAttribute("data-open");
		window.__pos.setAttribute("data-ending-style", "");
		window.__backdrop.inert = true;
		return true;
	})()`, nil)
	p.Click("#inc")
	p.WaitText("#pop", "count 3")
	check("closing, after a patch,", `window.__pos.hasAttribute("data-ending-style") && window.__backdrop.inert`)
	p.Eval(`(() => {
		window.__pos.removeAttribute("data-ending-style");
		window.__pos.hidden = true;
		window.__portal.hidden = true;
		window.__backdrop.remove();
		window.__guards.forEach((g) => g.remove());
		return true;
	})()`, nil)
	p.Click("#inc")
	p.WaitText("#n", "4")
	var ok bool
	p.Eval(`document.querySelectorAll(".positioner, .backdrop, .guard").length === 1 && document.querySelector(".positioner") === window.__pos &&
		window.__pos.hidden && window.__portal.hidden && document.querySelector("#pop").textContent === "count 4"`, &ok)
	if !ok {
		var h string
		p.Eval(`document.body.innerHTML`, &h)
		t.Fatalf("closed, the popup isn't the one the script holds: %s", h)
	}
}
