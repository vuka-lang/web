package web

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vuka-lang/web/v2/internal/chrome"
)

// countSockets has the page count the WebSockets it opens from now on.
func countSockets(t *testing.T, p *chrome.Page) {
	t.Helper()
	if err := p.Eval(`window.__ws = 0; window.__sentinel = 1; (() => { const W = WebSocket; window.WebSocket = function (u, pr) { window.__ws++; return new W(u, pr); }; })()`, nil); err != nil {
		t.Fatal(err)
	}
}

func pageTransition(t *testing.T, p *chrome.Page, name string, persisted bool) {
	t.Helper()
	js := `window.dispatchEvent(new PageTransitionEvent("` + name + `", { persisted: ` + map[bool]string{true: "true", false: "false"}[persisted] + ` }))`
	if err := p.Eval(js, nil); err != nil {
		t.Fatal(err)
	}
}

func liveConns(app *App) int {
	return int(app.hub.open.Load())
}

// A live page being hidden (pagehide) closes its connection, as the
// back/forward cache needs, without retrying it as a lost one; shown again
// from the cache (pageshow, persisted) it connects again at once and its
// page answers. Real caching can't be driven from the test browser: the
// events are synthetic.
func TestBrowserBFCacheRejoin(t *testing.T) {
	app := meterApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/meter")
	p.Wait(`document.documentElement.dataset.vkState === "connected"`)
	p.WaitText("span", "m1 n0")
	countSockets(t, p)

	pageTransition(t, p, "pagehide", true)
	deadline := time.Now().Add(2 * time.Second)
	for liveConns(app) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the server still holds the connection after pagehide")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(time.Second) // a retry would have come by now
	var st string
	var n int
	p.Eval(`document.documentElement.dataset.vkState`, &st)
	p.Eval(`window.__ws`, &n)
	if st != "connecting" || n != 0 {
		t.Fatalf("after pagehide: state %q, %d sockets opened", st, n)
	}

	pageTransition(t, p, "pageshow", false) // not from the cache: nothing to do
	time.Sleep(300 * time.Millisecond)
	if p.Eval(`window.__ws`, &n); n != 0 {
		t.Fatalf("a pageshow not from the cache connected (%d)", n)
	}

	pageTransition(t, p, "pageshow", true)
	p.Wait(`document.documentElement.dataset.vkState === "connected" && window.__ws === 1 && window.__sentinel === 1`)
	p.WaitText("span", "m2 n0") // a fresh session: the render's was adopted once
	p.Click("button")
	p.WaitText("span", "m2 n1")
}

// A page shown from the cache after its token expired is closed with 4001
// by its join, and recovers in place: the document (and what a script left
// on window) kept, the page fetched again and joined with a fresh token.
func TestBrowserBFCacheExpiredRecovers(t *testing.T) {
	app := meterApp(LiveTokenTTL(2 * time.Second))
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/meter")
	p.Wait(`document.documentElement.dataset.vkState === "connected"`)
	p.WaitText("span", "m1 n0")
	countSockets(t, p)

	pageTransition(t, p, "pagehide", true)
	time.Sleep(3500 * time.Millisecond) // the token expires while the page is cached
	pageTransition(t, p, "pageshow", true)
	p.Wait(`document.documentElement.dataset.vkState === "connected" && window.__ws === 2 && window.__sentinel === 1`)
	p.WaitText("span", "m2 n0")
	p.Click("button")
	p.WaitText("span", "m2 n1")
}

// countLeave counts the window's beforeunload listeners.
const countLeave = `(() => {
	window.__leave = 0;
	const add = window.addEventListener, rm = window.removeEventListener;
	window.addEventListener = function (t, ...a) { if (t === "beforeunload") window.__leave++; return add.call(this, t, ...a); };
	window.removeEventListener = function (t, ...a) { if (t === "beforeunload") window.__leave--; return rm.call(this, t, ...a); };
})()`

// The runtime listens for beforeunload (which keeps a page out of some
// browsers' back/forward cache) only while a form asking before leaving is
// on the page; the browser's prompt still guards the form's changes.
func TestBrowserBFCacheLeaveListener(t *testing.T) {
	app := meterApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	var n int
	p := b.Start(srv.URL+"/meter", `document.documentElement.dataset.vkState === "connected"`, countLeave)
	if p.Eval(`window.__leave`, &n); n != 0 {
		t.Fatalf("a page without a form asking before leaving listens for beforeunload (%d)", n)
	}

	nsrv := httptest.NewServer(noteApp().Handler())
	defer nsrv.Close()
	p = b.Start(nsrv.URL+"/note", `document.documentElement.dataset.vkState === "connected"`, countLeave)
	if p.Eval(`window.__leave`, &n); n != 1 {
		t.Fatalf("a form asking before leaving: %d beforeunload listeners", n)
	}
	leave := `(() => { const e = new Event("beforeunload", { cancelable: true }); window.dispatchEvent(e); return e.defaultPrevented; })()`
	var asked bool
	if p.Eval(leave, &asked); asked {
		t.Fatal("the prompt shows for a form without changes")
	}
	p.Type(`input[name="title"]`, " draft")
	if p.Eval(leave, &asked); !asked {
		t.Fatal("no prompt for a form with changes")
	}
	p.Eval(`window.confirm = () => true`, nil)
	p.Click("#away")
	p.Wait(`location.pathname === "/away" && window.__leave === 0`)
}
