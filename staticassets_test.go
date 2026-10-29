package web

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func registerStaticForTest(t *testing.T, m map[string]string) {
	t.Helper()
	staticMu.Lock()
	staticHashes, staticPlains = nil, nil
	staticMu.Unlock()
	RegisterStatic(m)
	t.Cleanup(func() {
		staticMu.Lock()
		staticHashes, staticPlains = nil, nil
		staticMu.Unlock()
		assetWarned.Clear()
		lastApp.Store(nil)
	})
}

var assetFiles = fstest.MapFS{
	"images/logo.png": {Data: []byte("\x89PNG logo")},
	"site.css":        {Data: []byte(`body{background:url(/static/images/logo.png)} .a{background:url("images/logo.png?v=1")} .b{background:url(https://example.com/x.png)} .c{background:url(missing.png)}`)},
}

func assetApp(t *testing.T, opts ...Option) http.Handler {
	a := New(append([]Option{Only("none")}, opts...)...)
	a.Static("/static", assetFiles)
	a.Get("/asset", func(ctx context.Context, r *http.Request) string {
		return Asset(ctx, r.URL.Query().Get("p"))
	})
	a.Get("/asset-url", func(ctx context.Context, r *http.Request) string {
		return AssetURL(ctx, r.URL.Query().Get("p"))
	})
	return a.Handler()
}

func TestAssetHashed(t *testing.T) {
	registerStaticForTest(t, map[string]string{
		"/static/images/logo.png": "/static/images/logo.3f9a1c2b.png",
		"/static/site.css":        "/static/site.0d1e2f3a.css",
	})
	h := assetApp(t)

	expect(t, h, "GET", "/asset?p=/static/images/logo.png", "", 200, "/static/images/logo.3f9a1c2b.png")
	expect(t, h, "GET", "/asset?p=/static/images/logo.png%23top", "", 200, "/static/images/logo.3f9a1c2b.png#top")

	rec := get(h, "GET", "/static/images/logo.3f9a1c2b.png")
	if rec.Code != 200 || rec.Body.String() != "\x89PNG logo" || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("the hashed name: %d %q, Cache-Control %q", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
	rec = get(h, "GET", "/static/images/logo.png")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != staticRevalidate {
		t.Errorf("the plain name: %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := get(h, "GET", "/static/images/logo.00000000.png"); rec.Code != 404 {
		t.Errorf("an unregistered hashed name: %d", rec.Code)
	}

	rec = get(h, "GET", "/static/site.0d1e2f3a.css")
	css := rec.Body.String()
	if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") {
		t.Errorf("the hashed stylesheet: Cache-Control %q, type %q", rec.Header().Get("Cache-Control"), rec.Header().Get("Content-Type"))
	}
	for _, want := range []string{"url(/static/images/logo.3f9a1c2b.png)", `url("images/logo.3f9a1c2b.png?v=1")`, "url(https://example.com/x.png)", "url(missing.png)"} {
		if !strings.Contains(css, want) {
			t.Errorf("the stylesheet lacks %s:\n%s", want, css)
		}
	}
	rec = get(h, "GET", "/static/site.css")
	if rec.Body.String() != css || rec.Header().Get("Cache-Control") != staticRevalidate {
		t.Errorf("the plain stylesheet: Cache-Control %q\n%s", rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	if rec := get(h, "GET", "/static/site.css", "If-None-Match", rec.Header().Get("ETag")); rec.Code != http.StatusNotModified {
		t.Errorf("revalidating the stylesheet: %d", rec.Code)
	}
}

func TestAssetPlain(t *testing.T) {
	registerStaticForTest(t, nil)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	h := assetApp(t)
	expect(t, h, "GET", "/asset?p=/static/images/logo.png", "", 200, "/static/images/logo.png")
	if logs.Len() != 0 {
		t.Errorf("a file the mount serves logged:\n%s", logs.String())
	}
	expect(t, h, "GET", "/asset?p=/static/images/lgo.png", "", 200, "/static/images/lgo.png")
	expect(t, h, "GET", "/asset?p=/static/images/lgo.png", "", 200, "/static/images/lgo.png")
	if n := strings.Count(logs.String(), "no app.Static mount serves the asset"); n != 1 || !strings.Contains(logs.String(), "/static/images/lgo.png") {
		t.Errorf("a missing file logged %d times:\n%s", n, logs.String())
	}
	rec := get(h, "GET", "/static/site.css")
	if !strings.Contains(rec.Body.String(), "url(/static/images/logo.png)") {
		t.Errorf("the stylesheet without a manifest:\n%s", rec.Body.String())
	}
}

func TestAssetPrefixAndURL(t *testing.T) {
	registerStaticForTest(t, map[string]string{"/static/images/logo.png": "/static/images/logo.3f9a1c2b.png"})
	h := assetApp(t)
	mux := http.NewServeMux()
	mux.Handle("/shop/", http.StripPrefix("/shop", h))

	expect(t, mux, "GET", "/shop/asset?p=/static/images/logo.png", "", 200, "/shop/static/images/logo.3f9a1c2b.png")
	if rec := get(mux, "GET", "/shop/static/images/logo.3f9a1c2b.png"); rec.Code != 200 {
		t.Errorf("the hashed name under the prefix: %d", rec.Code)
	}

	req := httptest.NewRequest("GET", "http://example.com/shop/asset-url?p=/static/images/logo.png", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if got := rec.Body.String(); !strings.Contains(got, `"http://example.com/shop/static/images/logo.3f9a1c2b.png"`) {
		t.Errorf("AssetURL in a request: %q", got)
	}
}

func TestAssetURLBase(t *testing.T) {
	registerStaticForTest(t, map[string]string{"/static/images/logo.png": "/static/images/logo.3f9a1c2b.png"})
	assetApp(t, BaseURL("https://example.com/shop/"))
	ctx := context.Background()
	if got := AssetURL(ctx, "/static/images/logo.png"); got != "https://example.com/shop/static/images/logo.3f9a1c2b.png" {
		t.Errorf("AssetURL without a request: %q", got)
	}
	if got := Asset(ctx, "/static/images/logo.png"); got != "/shop/static/images/logo.3f9a1c2b.png" {
		t.Errorf("Asset without a request: %q", got)
	}

	a := New(Only("none"), BaseURL("example.com"))
	if err := a.Build(); err == nil || !strings.Contains(err.Error(), "not an absolute URL") {
		t.Errorf("a relative BaseURL: %v", err)
	}
}

func TestAppCSSRewritten(t *testing.T) {
	registerStaticForTest(t, map[string]string{"/static/images/logo.png": "/static/images/logo.3f9a1c2b.png"})
	cssMu.Lock()
	old, had := cssReg[cssName]
	cssReg[cssName] = []byte(`.hero{background-image:url('/static/images/logo.png')}`)
	cssMu.Unlock()
	RegisterEncoded(cssPath, "br", []byte("stale"))
	defer func() {
		cssMu.Lock()
		if had {
			cssReg[cssName] = old
		} else {
			delete(cssReg, cssName)
		}
		cssMu.Unlock()
		dropEncodings(cssPath)
	}()
	h := New(Only("none")).Handler()
	rec := get(h, "GET", cssPath)
	if !strings.Contains(rec.Body.String(), "url('/static/images/logo.3f9a1c2b.png')") {
		t.Errorf("app.css: %q", rec.Body.String())
	}
	if registeredEncoding(cssPath, "br") != nil {
		t.Error("app.css kept the encoding made before its url()s changed")
	}
}
