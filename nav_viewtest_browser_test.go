package web_test

import (
	"testing"

	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/viewtest"
)

// In-place navigation over HTTP in Chrome: a page that isn't live moves to
// another in place, the layout's elements, its field's focus and what was
// typed kept, the islands of the page it leaves unmounted and the new
// page's mounted; back fetches again; a live page reached joins without
// mounting twice. The document is never loaded again.
func TestBrowserNavigationOverHTTP(t *testing.T) {
	app, navs := web.NavHTTPApp(t)
	p := viewtest.Browse(t, app, "/h/a")
	is := func(js string) func() bool {
		return func() bool {
			var ok bool
			p.Eval("!!("+js+")", &ok)
			return ok
		}
	}
	p.Wait("Clock a mounted", is(`(window.mounts || []).join() === "Clock a"`))
	p.Eval(`window.__doc = 1; document.querySelector("header").__mark = 7; true`, nil)
	p.Fill("#q", "typed")

	// A patch link's move, the field focused: focus and typing stay.
	p.Eval(`window.vuka.navigate("/h/b", { patch: true }); true`, nil)
	p.Expect("#at").Text("b")
	p.Wait("Ticker b mounted, Clock a unmounted", is(`window.mounts.join() === "Clock a,Ticker b" && window.unmounts.join() === "Clock a"`))
	if p.URL() != "/h/b" {
		t.Fatalf("URL %q", p.URL())
	}
	kept := `window.__doc === 1 && document.querySelector("header").__mark === 7 && document.querySelector("#q").value === "typed"`
	if !is(kept + ` && document.activeElement.id === "q" && history.scrollRestoration === "manual"`)() {
		t.Fatal("the patch didn't keep the layout, the field's focus and what was typed")
	}

	// A link: the next page's island mounts where the last one's was.
	p.Click("#to-c")
	p.Wait("Clock c mounted", is(`location.pathname === "/h/c" && window.mounts.join() === "Clock a,Ticker b,Clock c" && window.unmounts.join() === "Clock a,Ticker b"`))
	if !is(kept + ` && document.activeElement.tagName === "MAIN"`)() {
		t.Fatal("the navigation didn't keep the layout, or focus didn't move to main")
	}

	// Back: fetched again, its island mounted again.
	p.Eval(`history.back(); true`, nil)
	p.Wait("Ticker b mounted again", is(`location.pathname === "/h/b" && window.mounts.join() === "Clock a,Ticker b,Clock c,Ticker b"`))

	// A live page: it joins, Mount having run once.
	p.Click("#to-live")
	p.Wait("the live page connected", is(`document.documentElement.getAttribute("data-vk-state") === "connected"`))
	p.Expect("main span").Text("m1 n0")
	p.Click("main button")
	p.Expect("main span").Text("m1 n1")
	if !is(kept)() {
		t.Fatal("the live page's arrival loaded the document")
	}
	if n := navs.Load(); n != 4 {
		t.Errorf("%d navigations fetched, want 4", n)
	}
}
