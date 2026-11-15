package web

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2"
)

// streamPage has two deferred parts, each waiting for its own channel.
type streamPage struct{ a, b chan struct{} }

func wait(ctx context.Context, c chan struct{}) error {
	if c == nil {
		return nil
	}
	select {
	case <-c:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *streamPage) Alpha(ctx context.Context) (string, error) { return "alpha", wait(ctx, p.a) }
func (p *streamPage) Beta(ctx context.Context) (string, error)  { return "beta", wait(ctx, p.b) }

func init() {
	vuka.RegisterFunc((*streamPage).Alpha, ui.Defer(""))
	vuka.RegisterFunc((*streamPage).Beta, ui.Defer(""))
}

func (p *streamPage) Render(ctx context.Context, w io.Writer) error {
	show := func(s string) ui.Node { return ui.El("p", []ui.Attr{{Name: "id", Value: s}}, ui.Text(s)) }
	return fullPage(ui.Group{
		ui.El("h1", nil, ui.Text("Stream")),
		ui.Part(p, (*streamPage).Alpha, ui.Text("loading alpha"), show),
		ui.Part(p, (*streamPage).Beta, ui.Text("loading beta"), show),
	}).Render(ctx, w)
}

func streamApp(page func() *streamPage, opts ...Option) *App {
	a := New(append([]Option{Only("none")}, opts...)...)
	a.Get("/s", page)
	return a
}

func loadDocument(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Sec-Fetch-Dest", "document")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A browser loading the page as its document gets its parts in the same
// response: the shell first, while both methods run, then each part as its
// method returns — Beta's, released first, before Alpha's — and the
// runtime's tag after the last, with no parts token; compressed, each
// flush reaching the client.
func TestPartsStreamed(t *testing.T) {
	a, b := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(streamApp(func() *streamPage { return &streamPage{a: a, b: b} }).Handler())
	defer srv.Close()
	res := loadDocument(t, srv.URL+"/s")
	defer res.Body.Close()
	if !res.Uncompressed { // the transport asked for gzip: each flush goes through the compression
		t.Errorf("the page wasn't compressed: %v", res.Header)
	}
	if res.Header.Get("Content-Length") != "" || res.Header.Get("X-Accel-Buffering") != "no" || res.Header.Get("Cache-Control") != "private" {
		t.Errorf("headers: %v", res.Header)
	}
	r := bufio.NewReader(res.Body)
	shell := readUntil(t, r, `(document.currentScript)</script>`) // the mover's script ends the shell
	ids := partIDs(shell)
	if len(ids) != 2 || !strings.Contains(shell, "loading alpha") || !strings.Contains(shell, "window.__vkS=") || strings.Contains(shell, "<template") || strings.Contains(shell, "/_vuka/live.js") {
		t.Fatalf("the shell: %s", shell)
	}
	if len(shell) < streamPartsPad {
		t.Errorf("the shell is %d bytes, unpadded", len(shell))
	}
	close(b)
	beta := readUntil(t, r, `__vkS("`+ids[1]+`")</script>`)
	if want := `<template data-vk-part="` + ids[1] + `"><p id="beta">beta</p></template><script>__vkS("` + ids[1] + `")</script>`; beta != want {
		t.Errorf("Beta's part, first: %s", beta)
	}
	close(a)
	rest, _ := io.ReadAll(r)
	if !strings.HasPrefix(string(rest), `<template data-vk-part="`+ids[0]+`"><p id="alpha">alpha</p></template><script>__vkS("`+ids[0]+`")</script><script src="/_vuka/live.js`) ||
		strings.Contains(string(rest), "data-vk-parts=") || !strings.HasSuffix(string(rest), "</body></html>") {
		t.Errorf("Alpha's part, then the runtime's tag: %s", rest)
	}
}

// noFlush is a ResponseWriter that can't flush.
type noFlush struct{ w http.ResponseWriter }

func (n noFlush) Header() http.Header         { return n.w.Header() }
func (n noFlush) Write(b []byte) (int, error) { return n.w.Write(b) }
func (n noFlush) WriteHeader(code int)        { n.w.WriteHeader(code) }

// Through a writer that can't flush, behind a policy refusing inline
// scripts, or to a client not loading a document, the page goes whole, its
// parts parked for the runtime's request: no template, a token.
func TestPartsStreamFallback(t *testing.T) {
	for name, wrap := range map[string]func(http.Handler) http.Handler{
		"no flush": func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(noFlush{w}, r) })
		},
		"csp": func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'nonce-x' 'unsafe-inline'")
				h.ServeHTTP(w, r)
			})
		},
		"fetch": func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Header.Set("Sec-Fetch-Dest", "empty")
				h.ServeHTTP(w, r)
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			app := streamApp(func() *streamPage { return &streamPage{a: make(chan struct{}), b: make(chan struct{})} })
			app.Use(wrap)
			srv := httptest.NewServer(app.Handler())
			defer srv.Close()
			res := loadDocument(t, srv.URL+"/s")
			done := make(chan string)
			go func() { b, _ := io.ReadAll(res.Body); res.Body.Close(); done <- string(b) }()
			var body string
			select {
			case body = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the page's response stayed open")
			}
			if len(partIDs(body)) != 2 || strings.Contains(body, "<template") || strings.Contains(body, "__vkS") || !partsTokRE.MatchString(body) {
				t.Errorf("the page: %s", body)
			}
		})
	}
}

func TestInlineScripts(t *testing.T) {
	for policy, want := range map[string]bool{
		"":                                  true,
		"img-src 'self'":                    true,
		"default-src 'self'":                false,
		"default-src 'unsafe-inline'":       true,
		"script-src 'self' 'unsafe-inline'": true,
		"default-src 'unsafe-inline'; script-src 'self'":     false,
		"script-src 'unsafe-inline' 'sha256-x'":              false,
		"script-src-elem 'unsafe-inline'; script-src 'self'": true,
	} {
		h := http.Header{}
		if policy != "" {
			h.Set("Content-Security-Policy", policy)
		}
		if got := inlineScripts(h); got != want {
			t.Errorf("%q: %v, want %v", policy, got, want)
		}
	}
}

// A part still running when PartsWindow runs out is canceled and keeps its
// fallback and its marks, for the runtime to ask for; the response ends.
func TestPartsStreamWindow(t *testing.T) {
	srv := httptest.NewServer(streamApp(func() *streamPage { return &streamPage{a: make(chan struct{})} }, PartsWindow(100*time.Millisecond)).Handler())
	defer srv.Close()
	res := loadDocument(t, srv.URL+"/s")
	done := make(chan string)
	go func() { b, _ := io.ReadAll(res.Body); res.Body.Close(); done <- string(b) }()
	var body string
	select {
	case body = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the response outlived the parts' window")
	}
	ids := partIDs(body)
	if len(ids) != 2 || !strings.Contains(body, `<template data-vk-part="`+ids[1]+`">`) || strings.Contains(body, `<template data-vk-part="`+ids[0]+`">`) ||
		!strings.Contains(body, "loading alpha") || strings.Contains(body, "data-vk-parts=") || !strings.HasSuffix(body, "</body></html>") {
		t.Errorf("the page: %s", body)
	}
}
