package viewtest

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// Browser is a page a test drives in a headless Chrome: the real runtime
// runs it, so everything a visitor's browser does happens — JS commands,
// islands, hooks, morphing, focus, what the runtime keeps across patches,
// loading when seen, prefetching on hover. Actions are a person's: a
// click is the mouse's, Fill types key by key. They don't wait for the
// server; an Expect does, re-checking until it holds.
//
//	p := viewtest.Browse(t, app, "/todos")
//	p.Fill("title", "Buy milk")
//	p.Click("Add")
//	p.Expect("#todos li").Text("Buy milk")
//
// Locators read as they do for Live. A test without Chrome installed is
// skipped (failed when CI is set); VUKA_CHROME names the binary when it
// isn't in a usual place.
type Browser struct {
	t       testing.TB
	b       *chrome.Browser
	p       *chrome.Page
	base    string
	scripts []string
}

// EventStream runs Browse's page with no WebSocket, as behind a proxy
// refusing the upgrade: it goes live over the server-sent event stream.
func EventStream() Option { return func(o *options) { o.stream = true } }

// Browse serves app, launches Chrome and opens the page at path in it,
// returning once the page is loaded and, when it is live, connected. The
// browser and the server close as the test ends.
func Browse(t testing.TB, app *web.App, path string, opts ...Option) *Browser {
	t.Helper()
	var o options
	for _, f := range opts {
		f(&o)
	}
	if o.plain {
		t.Fatalf("viewtest: Plain applies to Mount, not Browse")
	}
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close) // after Chrome closes: a page on the event stream holds a request open
	b := &Browser{t: t, b: chrome.Launch(t), base: srv.URL}
	if o.stream {
		b.scripts = append(b.scripts, `delete window.WebSocket`)
	}
	b.p = b.open(path)
	return b
}

// ready is true once a page has loaded and, when live, connected.
const ready = `document.readyState === "complete" && (!document.querySelector("script[data-vk-token]") || document.documentElement.dataset.vkState === "connected")`

func (b *Browser) open(path string) *chrome.Page {
	b.t.Helper()
	return b.b.Start(b.base+path, ready, b.scripts...)
}

// NewTab opens path in another tab of the same browser, sharing its
// cookies: a second window of the visitor, for what one tab's actions do
// to another's page (a broadcast).
func (b *Browser) NewTab(path string) *Browser {
	b.t.Helper()
	return &Browser{t: b.t, b: b.b, p: b.open(path), base: b.base, scripts: b.scripts}
}

// finder is a JavaScript function finding the element a locator names, as
// find does.
const finder = `((loc, kind) => {
	const q = () => { try { return document.querySelector(loc); } catch (_) { return null; } };
	const sq = (s) => (s || "").replace(/\s+/g, " ").trim(), want = sq(loc);
	const byName = () => [...document.querySelectorAll("input,textarea,select")].find((e) => e.getAttribute("name") === loc) || null;
	const sel = "button,a,summary,label,input[type=submit],input[type=button],input[type=reset],[role=button],[role=link],[role=tab],[role=menuitem]";
	const byText = () => [...document.querySelectorAll(sel)].find((e) => sq(e.textContent) === want || e.tagName === "INPUT" && e.value === want || e.getAttribute("aria-label") === want) || null;
	for (const f of { any: [q, byName, byText], field: [byName, q], click: [q, byText] }[kind]) { const e = f(); if (e) return e; }
	return null;
})`

var kinds = map[int]string{anyKind: "any", fieldKind: "field", clickKind: "click"}

// el is a JavaScript expression for the element loc names.
func el(loc string, kind int) string { return fmt.Sprintf("%s(%q, %q)", finder, loc, kinds[kind]) }

// found waits for loc to name an element, failing the test when it
// doesn't in time; it returns the expression for the element.
func (b *Browser) found(what, loc string, kind int) string {
	b.t.Helper()
	expr := el(loc, kind)
	b.until(fmt.Sprintf("%s(%q) to find an element", what, loc), func() bool {
		var ok bool
		return b.p.Eval("!!"+expr, &ok) == nil && ok
	}, func() string { return "no element matches " + quote(loc) })
	return expr
}

// Click clicks the element loc names with the mouse, in its middle.
func (b *Browser) Click(loc string) *Browser {
	b.t.Helper()
	b.p.ClickElement(b.found("Click", loc, clickKind))
	return b
}

// Fill types value into a field, replacing what it held: the page sees
// each key. The field keeps the focus, as after typing — a Press goes to
// it; the next Click moves the focus on, as a person's does, and the
// field's change and focusout follow.
func (b *Browser) Fill(loc, value string) *Browser {
	b.t.Helper()
	expr := b.found("Fill", loc, fieldKind)
	var had string
	b.Eval(fmt.Sprintf(`(() => { const e = %s; e.focus(); if (e.select) e.select(); return e.value || ""; })()`, expr), &had)
	if value == "" {
		if had != "" {
			b.p.Key("Backspace")
		}
	} else {
		b.p.Keys(value)
	}
	return b
}

// Check sets a checkbox, or chooses a radio button (on is then true), by
// clicking it when it isn't so already.
func (b *Browser) Check(loc string, on bool) *Browser {
	b.t.Helper()
	expr := b.found("Check", loc, fieldKind)
	var now bool
	b.Eval(fmt.Sprintf(`!!(%s).checked`, expr), &now)
	if now != on {
		b.p.ClickElement(expr)
	}
	return b
}

// Select chooses the option of a select whose value, or else text, is
// option, as choosing it from the list does (input and change events).
func (b *Browser) Select(loc, option string) *Browser {
	b.t.Helper()
	expr := b.found("Select", loc, fieldKind)
	var ok bool
	b.Eval(fmt.Sprintf(`(() => { const e = %s, want = %q;
		const o = [...e.options].find((o) => o.value === want) || [...e.options].find((o) => o.textContent.replace(/\s+/g, " ").trim() === want);
		if (!o) return false;
		e.focus(); e.value = o.value;
		e.dispatchEvent(new Event("input", { bubbles: true })); e.dispatchEvent(new Event("change", { bubbles: true }));
		return true; })()`, expr, option), &ok)
	if !ok {
		b.t.Fatalf("viewtest: Select(%q): no option %q", loc, option)
	}
	return b
}

// Submit submits the form loc names — the form, a button submitting it
// or any element in it — as pressing its submit button does.
func (b *Browser) Submit(loc string) *Browser {
	b.t.Helper()
	expr := b.found("Submit", loc, clickKind)
	var ok bool
	b.Eval(fmt.Sprintf(`(() => { const e = %s, f = e.closest("form"); if (!f) return false;
		const s = e.matches("button:not([type=button]):not([type=reset]),input[type=submit]") && e.form === f ? e : undefined;
		f.requestSubmit(s); return true; })()`, expr), &ok)
	if !ok {
		b.t.Fatalf("viewtest: Submit(%q): the element is in no form", loc)
	}
	return b
}

// Press presses key in the element with the focus — "Enter", "Escape",
// "Tab", "ArrowDown", a letter — with modifiers held: "ctrl", "shift",
// "alt", "meta".
func (b *Browser) Press(key string, mods ...string) *Browser {
	b.t.Helper()
	b.p.Key(key, mods...)
	return b
}

// Hover moves the pointer onto the element loc names: what a hover
// handler, a tooltip or a link's prefetch waits for.
func (b *Browser) Hover(loc string) *Browser {
	b.t.Helper()
	b.p.HoverElement(b.found("Hover", loc, clickKind))
	return b
}

// ScrollTo scrolls the element loc names into view, the tab in front:
// what a deferred group loading when seen waits for.
func (b *Browser) ScrollTo(loc string) *Browser {
	b.t.Helper()
	expr := b.found("ScrollTo", loc, anyKind)
	b.p.Front()
	b.Eval(fmt.Sprintf(`(%s).scrollIntoView({ block: "center" }), true`, expr), nil)
	return b
}

// Navigate moves the page to url as a link does — in place, over the
// connection when the page is live — and waits for it to arrive.
func (b *Browser) Navigate(url string) *Browser {
	b.t.Helper()
	var want string
	b.Eval(fmt.Sprintf(`(() => { const u = new URL(%q, location.href);
		if (window.vuka && window.vuka.navigate) window.vuka.navigate(u.pathname + u.search); else location.href = u.href;
		return u.pathname + u.search; })()`, url), &want)
	b.p.Wait(fmt.Sprintf(`location.pathname + location.search === %q && %s`, want, ready))
	return b
}

// URL is the page's URL, its path and query.
func (b *Browser) URL() string {
	var s string
	b.Eval(`location.pathname + location.search`, &s)
	return s
}

// HTML is the document as it stands.
func (b *Browser) HTML() string {
	var s string
	b.Eval(`document.documentElement.outerHTML`, &s)
	return s
}

// Text is the text of the element loc names now, white space squashed;
// "" when none matches.
func (b *Browser) Text(loc string) string {
	return b.look(loc, anyKind).Text
}

// Eval evaluates a JavaScript expression in the page, awaiting a promise,
// and decodes its value into out when out isn't nil. A script error fails
// the test.
func (b *Browser) Eval(js string, out any) {
	b.t.Helper()
	if err := b.p.Eval(js, out); err != nil {
		b.t.Fatalf("viewtest: %v", err)
	}
}

// Expect is the element loc names, to check as the page changes.
func (b *Browser) Expect(loc string) *Expectation { return &Expectation{s: b, loc: loc} }

// Wait waits for cond to hold, polling it, and fails the test naming what
// it waited for once Timeout passes.
//
//	p.Wait("the chart drawn", func() bool { var n int; p.Eval(`document.querySelectorAll("svg path").length`, &n); return n > 0 })
func (b *Browser) Wait(what string, cond func() bool) {
	b.t.Helper()
	b.until(what, cond, func() string { return "the page:\n" + clip(b.HTML()) })
}

func (b *Browser) tb() testing.TB { return b.t }

func (b *Browser) look(loc string, kind int) look {
	var l look
	b.p.Eval(fmt.Sprintf(`(() => { const e = %s; if (!e) return { found: false };
		const sq = (s) => (s || "").replace(/\s+/g, " ").trim();
		const box = e.type === "checkbox" || e.type === "radio";
		const value = box ? (e.checked ? e.value || "on" : "") : ("value" in e && e.tagName !== "BUTTON" && e.tagName !== "LI" ? String(e.value) : "");
		const visible = e.checkVisibility ? e.checkVisibility() : !!(e.offsetWidth || e.offsetHeight || e.getClientRects().length);
		return { found: true, text: sq(e.textContent), value, visible, markup: e.outerHTML }; })()`, el(loc, kind)), &l)
	return l
}

func (b *Browser) until(what string, ok func() bool, last func() string) {
	b.t.Helper()
	for end := time.Now().Add(Timeout); ; time.Sleep(25 * time.Millisecond) {
		if ok() {
			return
		}
		if time.Now().After(end) {
			var at string
			b.p.Eval(`location.pathname + location.search`, &at)
			b.t.Fatalf("viewtest: waiting for %s on %s: %s", what, at, last())
		}
	}
}
