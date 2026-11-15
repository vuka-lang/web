package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/web/v2/internal/chrome"
)

// closeAll closes every joined live page of app with 4001, once one is
// joined to close.
func closeAll(t *testing.T, app *App) {
	t.Helper()
	if app.CloseLive(func(context.Context) bool { return true }) == 0 {
		t.Fatal("no live page to close")
	}
}

// A page the server closes with 4001 (must load again) recovers in place:
// it is fetched again over HTTP, patched and joined with its fresh token,
// the document — and a value a script left on window — still there. A
// second close within 10s loads the document.
func TestBrowserRecoverInPlace(t *testing.T) {
	app := meterApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/meter")
	p.Wait(`document.documentElement.dataset.vkState === "connected"`)
	p.WaitText("span", "m1 n0")
	p.Click("button")
	p.WaitText("span", "m1 n1")
	if err := p.Eval(`window.__sentinel = 1`, nil); err != nil {
		t.Fatal(err)
	}

	closeAll(t, app)
	p.WaitText("span", "m2 n0") // the route ran again, its session adopted by the join
	p.Wait(`document.documentElement.dataset.vkState === "connected"`)
	var s int
	if err := p.Eval(`window.__sentinel || 0`, &s); err != nil || s != 1 {
		t.Fatalf("the document loaded again (sentinel %d, %v)", s, err)
	}
	p.Click("button")
	p.WaitText("span", "m2 n1") // the new session answers

	closeAll(t, app) // within 10s of the first: a load
	p.Wait(`window.__sentinel === undefined && document.documentElement.dataset.vkState === "connected"`)
	p.WaitText("span", "m3 n0")
}

// A recovery whose reply can't be patched (here: another build, Vk-Reload)
// loads the document.
func TestBrowserRecoverFallsBack(t *testing.T) {
	app := meterApp()
	var rebuilt atomic.Bool
	h := app.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rebuilt.Load() && r.Header.Get(NavigateHeader) != "" {
			w.Header().Set(navReload, "build")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/meter")
	p.Wait(`document.documentElement.dataset.vkState === "connected"`)
	if err := p.Eval(`window.__sentinel = 1`, nil); err != nil {
		t.Fatal(err)
	}

	rebuilt.Store(true)
	closeAll(t, app)
	p.Wait(`window.__sentinel === undefined && document.documentElement.dataset.vkState === "connected"`)
	p.WaitText("span", "m2 n0")
}
