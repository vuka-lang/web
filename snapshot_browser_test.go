package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// slowPage is a live page that renders which Mount made it; every Mount
// after the first takes a while, so a navigation back to it waits on the
// server.
var slowMounts atomic.Int32

type slowPage struct {
	ui.Live
	m int32
}

func (c *slowPage) LiveSchema(l *ui.LiveSchema) { l.Mount(c.mount) }
func (c *slowPage) mount(_ context.Context) error {
	if c.m = slowMounts.Add(1); c.m > 1 {
		time.Sleep(400 * time.Millisecond)
	}
	return nil
}
func (c *slowPage) Render() ui.Node {
	return ui.El("p", []ui.Attr{{Name: "id", Value: "a"}}, ui.Text(fmt.Sprintf("a%d", c.m)))
}

// snapshotApp serves the slow page at /s/a and meter at /s/b, each with
// links to the other.
func snapshotApp() *App {
	slowMounts.Store(0)
	meterMounts.Store(0)
	app := New(Only("none"), NoTailwind())
	frame := LayoutOf[navFrame]()
	app.Live("/s/a", &slowPage{}, frame)
	app.Live("/s/b", &meter{}, frame)
	return app
}

// seen records, on every change of the page, whether it is a preview and
// what #a says.
const recordSeen = `window.__seen = [];
new MutationObserver(() => {
	const a = document.querySelector("#a");
	window.__seen.push([document.documentElement.hasAttribute("data-vk-preview"), a ? a.textContent : ""]);
}).observe(document.documentElement, { attributes: true, childList: true, subtree: true, characterData: true });
true`

// Back to a live page shows its snapshot at once, marked data-vk-preview,
// while the server mounts it again; the fresh render then morphs in and
// the mark goes.
func TestBrowserSnapshotOnBack(t *testing.T) {
	app := snapshotApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	p := chrome.Launch(t).Visit(srv.URL + "/s/a")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	p.WaitText("#a", "a1")
	p.Eval(`window.vuka.navigate("/s/b"); true`, nil)
	p.WaitText("main span", "m1 n0")
	p.Eval(recordSeen, nil)

	p.Eval(`history.back(); true`, nil)
	p.Wait(`document.documentElement.hasAttribute("data-vk-preview") && document.querySelector("#a") && document.querySelector("#a").textContent === "a1"`)
	var early bool
	p.Eval(`document.documentElement.hasAttribute("data-vk-navigating")`, &early)
	if !early {
		t.Error("the preview showed after the navigation landed")
	}
	p.Wait(`location.pathname === "/s/a" && !document.documentElement.hasAttribute("data-vk-preview") && document.querySelector("#a").textContent === "a2"`)

	// What showed: the snapshot first, then the fresh render, never the two mixed.
	var ok bool
	p.Eval(`(() => {
		const s = window.__seen, first = s.findIndex((x) => x[0]);
		return first >= 0 && s[first][1] === "a1" && !s[s.length - 1][0] && s[s.length - 1][1] === "a2" &&
			!s.slice(first).some((x) => x[0] && x[1] === "a2");
	})()`, &ok)
	if !ok {
		var seen any
		p.Eval(`window.__seen`, &seen)
		t.Fatalf("the page didn't go preview a1 → fresh a2: %v", seen)
	}
}

// A page marked data-vk-no-snapshot keeps none: back waits on the server,
// no preview shown, and lands as before.
func TestBrowserSnapshotOptOut(t *testing.T) {
	app := snapshotApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	p := chrome.Launch(t).Visit(srv.URL + "/s/a")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	p.WaitText("#a", "a1")
	p.Eval(`document.documentElement.setAttribute("data-vk-no-snapshot", ""); window.vuka.navigate("/s/b"); true`, nil)
	p.WaitText("main span", "m1 n0")
	p.Eval(recordSeen, nil)

	p.Eval(`history.back(); true`, nil)
	p.Wait(`location.pathname === "/s/a" && document.querySelector("#a") && document.querySelector("#a").textContent === "a2"`)
	var ok bool
	p.Eval(`!document.documentElement.hasAttribute("data-vk-preview") && !window.__seen.some((x) => x[0]) && !window.__seen.some((x) => x[1] === "a1")`, &ok)
	if !ok {
		var seen any
		p.Eval(`window.__seen`, &seen)
		t.Fatalf("a page kept no snapshot, yet one showed: %v", seen)
	}
}

// A submit while the preview shows is dropped: the form is the page as it
// was.
func TestBrowserSnapshotHoldsSubmit(t *testing.T) {
	app := snapshotApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	p := chrome.Launch(t).Visit(srv.URL + "/s/a")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	p.WaitText("#a", "a1")
	p.Eval(`window.vuka.navigate("/s/b"); true`, nil)
	p.WaitText("main span", "m1 n0")

	p.Eval(`history.back(); true`, nil)
	p.Wait(`document.documentElement.hasAttribute("data-vk-preview")`)
	var prevented bool
	p.Eval(`(() => {
		const f = document.createElement("form");
		document.querySelector("main").append(f);
		const e = new SubmitEvent("submit", { bubbles: true, cancelable: true });
		f.dispatchEvent(e);
		f.remove();
		return e.defaultPrevented;
	})()`, &prevented)
	if !prevented {
		t.Error("a submit during the preview went on")
	}
	p.Wait(`!document.documentElement.hasAttribute("data-vk-preview") && document.querySelector("#a").textContent === "a2"`)
}
