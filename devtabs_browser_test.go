package web

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/web/v2/internal/chrome"
)

// devTabsApp is an app with all the development tooling on, on a port of its
// own: scoped components, the app's stylesheet, the toolbar under vuka run,
// and a plain route and an image to ask for from a tab.
func devTabsApp() *App {
	return scopedApp().
		Get("/ping", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			fmt.Fprint(w, "pong")
		}).
		Get("/dot.gif", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/gif")
			w.Header().Set("Cache-Control", "no-store")
			w.Write([]byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"))
		})
}

func serveOn(t *testing.T, addr string, a *App) (stop func()) {
	t.Helper()
	var l net.Listener
	var err error
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if l, err = net.Listen("tcp", addr); err == nil || time.Now().After(end) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: a.Handler()}
	go srv.Serve(l)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		srv.Close()
		a.Stop(ctx)
	}
}

// Any number of tabs of a development app works: the tooling's channels hold
// no connection against the browser's six per host, so a request and an image
// from any tab finish at once, and a new tab loads.
func TestDevManyTabsBrowser(t *testing.T) {
	withToolbar(t)
	devAssets(t)
	css := filepath.Join(t.TempDir(), "app.css")
	os.WriteFile(css, []byte(".one{}"), 0o644)
	t.Setenv(CSSEnv, css)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "build.json"), []byte(`{"state":"ok","took":1.5}`), 0o644)
	t.Setenv(DevDirEnv, dir)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	stop := serveOn(t, addr, devTabsApp())
	defer func() { stop() }()
	page := "http://" + addr + "/"

	const tabs = 10
	loaded := `document.readyState === "complete" && document.documentElement.dataset.vkState === "connected"`
	b := chrome.Launch(t)
	pages := []*chrome.Page{b.Open(page)}
	for range tabs - 1 {
		pages = append(pages, b.Start(page, "true"))
	}
	waitLoaded := func(p *chrome.Page, within time.Duration) time.Duration {
		t.Helper()
		start := time.Now()
		for {
			var ok bool
			if err := p.Eval("!!("+loaded+")", &ok); err == nil && ok {
				return time.Since(start)
			}
			if time.Since(start) > within {
				t.Fatalf("a tab didn't load within %s of %d being open: the browser's connections are held", within, tabs)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	for _, p := range pages {
		waitLoaded(p, 20*time.Second)
	}

	// probe asks from the page for what a page asks for, timed; it answers
	// by itself when the request sits queued.
	probe := func(p *chrome.Page, what, js string) {
		t.Helper()
		var ms float64
		expr := `(async () => {
			const start = performance.now();
			const queued = new Promise((_, no) => setTimeout(() => no(new Error("still queued after 8s")), 8000));
			await Promise.race([queued, (async () => { ` + js + ` })()]);
			return performance.now() - start;
		})()`
		if err := p.Eval(expr, &ms); err != nil {
			t.Fatalf("%s with %d tabs open: %v", what, tabs, err)
		}
		if ms > 4000 {
			t.Errorf("%s took %.0fms with %d tabs open", what, ms, tabs)
		}
		t.Logf("%s: %.0fms", what, ms)
	}
	for i, p := range []*chrome.Page{pages[0], pages[tabs/2], pages[tabs-1]} {
		probe(p, fmt.Sprintf("a request from tab %d", i), `const r = await fetch("/ping?n=" + Math.random()); await r.text();`)
		probe(p, fmt.Sprintf("an image from tab %d", i), `const im = new Image(); im.src = "/dot.gif?n=" + Math.random(); await im.decode();`)
	}
	waitLoaded(b.Start(page, "true"), 8*time.Second)

	// However many scripts use it, one socket a tab.
	last := pages[tabs-1]
	var sockets int
	if err := last.Eval(`__wire.sockets.filter((s) => s.url.endsWith("/_vuka/dev")).length`, &sockets); err != nil || sockets != 1 {
		t.Errorf("%d development sockets on a tab, %v", sockets, err)
	}

	// The channels reach a tab, and every other.
	if err := last.Eval(`void __vukaDev.on("assets", (v) => { window.manifest = v; })`, nil); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(os.Getenv(AssetsEnv), "manifest.json")
	old, _ := os.ReadFile(manifest)
	os.WriteFile(manifest, bytes.Replace(old, []byte("k1.0a1b2c3d.js"), []byte("k1.0a1b2c3e.js"), 1), 0o644)
	last.Wait(`window.manifest && window.manifest.includes("k1.0a1b2c3e.js")`)
	os.WriteFile(manifest, old, 0o644)
	cssIs := func(want string) string {
		return `document.querySelector("style[` + cssInlineMark + `]").textContent === "` + want + `"`
	}
	stateIs := func(want string) string {
		return `document.querySelector("vuka-toolbar").shadowRoot.querySelector(".state").textContent === "` + want + `"`
	}
	os.WriteFile(css, []byte(".two{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "build.json"), []byte(`{"state":"failed","output":"main.go:3: undefined: x"}`), 0o644)
	for _, p := range []*chrome.Page{pages[0], last} {
		p.Wait(cssIs(".two{}"))
		p.Wait(stateIs("build failed"))
	}

	// The server restarts: every tab hears the state of the new one, of what
	// changed while it was gone too.
	stop()
	os.WriteFile(css, []byte(".three{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "build.json"), []byte(`{"state":"ok","took":2}`), 0o644)
	time.Sleep(700 * time.Millisecond) // a few attempts refused
	stop = serveOn(t, addr, devTabsApp())
	for _, p := range []*chrome.Page{pages[0], last} {
		p.Wait(cssIs(".three{}"))
		p.Wait(`document.querySelector("vuka-toolbar").shadowRoot.querySelector(".state").textContent !== "build failed"`)
	}
	probe(last, "a request after the restart", `const r = await fetch("/ping?n=" + Math.random()); await r.text();`)
}

// A component module that fails to load, as during a restart, loads when the
// server answers again, without the page being reloaded.
func TestModuleRetryBrowser(t *testing.T) {
	devAssets(t)
	h := scopedApp().Handler()
	var failing atomic.Int32
	failing.Store(2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_vuka/c/k1.") && failing.Add(-1) >= 0 {
			http.Error(w, "restarting", http.StatusBadGateway)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close) // after Chrome, which Launch closes when the test ends, holding connections
	p := chrome.Launch(t).Open(srv.URL + "/")
	p.Wait(`window.hooks && window.hooks.length > 0`)
	var hooks []string
	if err := p.Eval(`window.hooks`, &hooks); err != nil || len(hooks) == 0 || !strings.HasPrefix(hooks[0], "mounted") {
		t.Errorf("hooks %v, %v", hooks, err)
	}
}
