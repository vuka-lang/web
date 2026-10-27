package web

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/vuka-lang/web/v2/internal/chrome"
)

// A page's deferred part in Chrome: the page paints with the part's
// fallback before the part's method returns, then the part moves into
// place; a navigation over HTTP to the page shows it at once the same way.
func TestBrowserParts(t *testing.T) {
	release := make(chan struct{}, 2)
	app := partsApp(func() *partsPage { return &partsPage{release: release} }, NoTailwind())
	app.Get("/start", func() *partsHome { return &partsHome{} })
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(func() { // after Chrome closes, its connections with it
		srv.Close()
		app.Stop(context.Background())
	})

	// when the first paint was, and when each part arrived
	const watch = `addEventListener("vk:part", () => {}); new PerformanceObserver((l) => l.getEntries().forEach((e) => { if (e.name === "first-contentful-paint") window.__fcp = e.startTime; })).observe({ type: "paint", buffered: true }); document.addEventListener("vk:part", (e) => { (window.__parts ||= []).push([e.detail.id, performance.now()]); });`
	b := chrome.Launch(t)
	p := b.Start(srv.URL+"/apps", `window.__fcp > 0`, watch)
	var shown struct {
		H1       string
		Fallback bool
		Loading  string
	}
	p.Eval(`({h1: document.querySelector("h1").textContent, fallback: document.body.innerHTML.includes("<!--vk-part:vk-p1-->…") && !document.querySelector("p"), loading: document.readyState})`, &shown)
	if shown.H1 != "Apps" || !shown.Fallback || shown.Loading != "loading" {
		t.Fatalf("before the part's method returns: %+v", shown)
	}
	release <- struct{}{}
	p.Wait(`document.querySelector("p") && document.querySelector("p").textContent === "3 open" && document.readyState === "complete"`)
	var ok bool
	p.Eval(`window.__parts.length === 3 && window.__parts.find(([id]) => id === "vk-p1")[1] > window.__fcp && !document.querySelector("template")`, &ok)
	if !ok {
		var got any
		p.Eval(`({fcp: window.__fcp, parts: window.__parts})`, &got)
		t.Fatalf("the waiting part arrived before the first paint, or the parts stayed in their templates: %v", got)
	}

	// In place over HTTP: the page shows before its part's method returns.
	p = b.Visit(srv.URL + "/start")
	p.Eval(`window.__doc = 1; true`, nil)
	p.Click("#apps")
	p.Wait(`location.pathname === "/apps" && document.querySelector("h1").textContent === "Apps" && window.__doc === 1`)
	p.Eval(`!document.querySelector("p") && document.body.innerHTML.includes("<!--vk-part:vk-p1-->")`, &ok)
	if !ok {
		t.Fatal("the navigation waited for the part")
	}
	release <- struct{}{}
	p.Wait(`document.querySelector("p") && document.querySelector("p").textContent === "3 open" && window.__doc === 1`)
}
