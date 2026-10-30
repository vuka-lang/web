package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/web/v2/internal/chrome"
)

// A page's deferred part in Chrome: the page loads whole — readyState
// complete, its load event fired — while the part's method, started by the
// page's request, still runs, its fallback in place; then the part, fetched
// on its own with the page's token, moves in, the route not run again. A
// navigation over HTTP to the page shows it at once the same way. The page
// refuses inline scripts: none is needed.
func TestBrowserParts(t *testing.T) {
	release := make(chan struct{}, 2)
	var made atomic.Int32
	app := partsApp(func() *partsPage { made.Add(1); return &partsPage{release: release} }, NoTailwind())
	app.Get("/start", func() *partsHome { return &partsHome{} })
	app.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", "script-src 'self'")
			h.ServeHTTP(w, r)
		})
	})
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(func() { // after Chrome closes, its connections with it
		srv.Close()
		app.Stop(context.Background())
	})

	// when the page loaded, and when each part arrived
	const watch = `addEventListener("load", () => { window.__load = performance.now(); }); document.addEventListener("vk:part", (e) => { (window.__parts ||= []).push([e.detail.id, performance.now(), document.readyState]); });`
	b := chrome.Launch(t)
	p := b.Start(srv.URL+"/apps", `document.readyState === "complete"`, watch)
	var shown struct {
		H1       string
		Fallback bool
		State    string
		Loaded   bool
		Parts    int
	}
	p.Eval(`({h1: document.querySelector("h1").textContent, fallback: /<!--vk-part:vk-p1-[0-9a-z]+-->…/.test(document.body.innerHTML) && !document.querySelector("p"), state: document.readyState, loaded: window.__load > 0, parts: (window.__parts || []).length})`, &shown)
	// Broken's part may have arrived: it doesn't wait for Stats.
	if shown.H1 != "Apps" || !shown.Fallback || shown.State != "complete" || !shown.Loaded || shown.Parts > 1 {
		t.Fatalf("before the part's method returns: %+v", shown)
	}
	release <- struct{}{}
	p.Wait(`document.querySelector("p") && document.querySelector("p").textContent === "3 open"`)
	var ok bool
	p.Eval(`window.__parts.length === 3 && window.__parts.every(([, at, state]) => at > window.__load && state === "complete") && !document.querySelector("template")`, &ok)
	if !ok {
		var got any
		p.Eval(`({load: window.__load, parts: window.__parts})`, &got)
		t.Fatalf("a part arrived before the page had loaded, or stayed in its template: %v", got)
	}
	if made.Load() != 1 {
		t.Errorf("the page was made %d times for one view: its parts' request ran the route again", made.Load())
	}

	// In place over HTTP: the page shows before its part's method returns.
	p = b.Visit(srv.URL + "/start")
	p.Eval(`window.__doc = 1; true`, nil)
	p.Click("#apps")
	p.Wait(`location.pathname === "/apps" && document.querySelector("h1").textContent === "Apps" && window.__doc === 1`)
	p.Eval(`!document.querySelector("p") && /<!--vk-part:vk-p1-/.test(document.body.innerHTML)`, &ok)
	if !ok {
		t.Fatal("the navigation waited for the part")
	}
	release <- struct{}{}
	p.Wait(`document.querySelector("p") && document.querySelector("p").textContent === "3 open" && window.__doc === 1`)
	if made.Load() != 2 {
		t.Errorf("the page was made %d times for two views", made.Load())
	}
}
