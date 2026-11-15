package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/web/v2/internal/chrome"
)

// A page's slow part in Chrome arrives in the page's own response: the
// fallback shows while the document is still loading, the part moves in
// before the load event, and the runtime, running after it, asks for no
// parts and reports no error.
func TestBrowserPartsStreamed(t *testing.T) {
	var asked atomic.Int32
	app := streamApp(func() *streamPage {
		a := make(chan struct{})
		time.AfterFunc(300*time.Millisecond, func() { close(a) })
		return &streamPage{a: a}
	}, NoTailwind())
	app.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(PartsHeader) != "" {
				asked.Add(1)
			}
			h.ServeHTTP(w, r)
		})
	})
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		srv.Close()
		app.Stop(context.Background())
	})
	const watch = `(() => {
		window.__errors = [];
		const err = console.error;
		console.error = (...a) => { window.__errors.push(a.join(" ")); err.apply(console, a); };
		addEventListener("error", (e) => window.__errors.push(String(e.message)));
		addEventListener("load", () => { window.__load = performance.now(); });
		document.addEventListener("vk:part", (e) => { (window.__parts ||= []).push([e.detail.id, document.readyState]); });
		new MutationObserver(() => {
			if (!window.__fallback && document.body && document.body.textContent.includes("loading alpha")) window.__fallback = document.readyState;
		}).observe(document, { childList: true, subtree: true, characterData: true });
	})()`
	b := chrome.Launch(t)
	p := b.Start(srv.URL+"/s", `document.readyState === "complete" && window.vuka`, watch)
	var got struct {
		Fallback string
		Alpha    string
		Parts    [][]string
		Left     bool
		Errors   []string
	}
	p.Eval(`({fallback: window.__fallback || "", alpha: (document.getElementById("alpha") || {}).textContent || "", parts: window.__parts || [],
		left: /vk-part:|loading alpha/.test(document.body.innerHTML) || !!document.querySelector("template, script:not([src])"), errors: window.__errors})`, &got)
	if got.Fallback != "loading" || got.Alpha != "alpha" || len(got.Parts) != 2 || got.Left || len(got.Errors) > 0 {
		t.Fatalf("the page: %+v", got)
	}
	for _, pt := range got.Parts {
		if pt[1] != "loading" {
			t.Errorf("part %s arrived once the document had loaded: not streamed", pt[0])
		}
	}
	time.Sleep(100 * time.Millisecond) // the runtime had its chance to ask
	if n := asked.Load(); n != 0 {
		t.Errorf("the runtime asked for the parts %d times", n)
	}
}
