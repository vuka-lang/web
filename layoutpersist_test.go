package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
)

var frameLoads, pathLoads atomic.Int64

// frameLayout persists across navigations.
type frameLayout struct{ Children ui.Node }

func (l *frameLayout) Load(ctx context.Context) error { frameLoads.Add(1); return nil }
func (l *frameLayout) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("body", nil, l.Children)).Render(ctx, w)
}

// pathLayout's Load takes the request: it loads on every navigation.
type pathLayout struct {
	Children ui.Node
	path     string
}

func (l *pathLayout) Load(ctx context.Context, r *http.Request) error {
	pathLoads.Add(1)
	l.path = r.URL.Path
	return nil
}
func (l *pathLayout) Render(ctx context.Context, w io.Writer) error {
	return ui.El("div", nil, ui.El("p", nil, ui.Text("at "+l.path)), l.Children).Render(ctx, w)
}

// TestLayoutsPersist: a navigation over the connection keeps the layouts
// the pages share, their Load not run again, unless one takes the request;
// a guard still runs, and refuses.
func TestLayoutsPersist(t *testing.T) {
	frameLoads.Store(0)
	pathLoads.Store(0)
	app := New(Only("none"))
	frame, path := LayoutOf[frameLayout](), LayoutOf[pathLayout]()
	page := func(text string) func() *titledPage {
		return func() *titledPage { return &titledPage{title: text, text: text} }
	}
	app.Live("/a", page("a"), frame, path)
	app.Live("/b", page("b"), frame, path)
	app.Live("/c", page("c"), frame, GuardOf[denyGuard]())
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	c, _ := joined(t, srv, "/a")
	defer c.CloseNow()
	frame0, path0 := frameLoads.Load(), pathLoads.Load()
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/b"})
	m := must(t, c)
	if m.Type != live.PatchMsg || !strings.Contains(m.Updates[0].HTML, "at /b") {
		t.Fatalf("navigate: %+v", m)
	}
	if n := frameLoads.Load(); n != frame0 {
		t.Errorf("the shared layout loaded again: %d, want %d", n, frame0)
	}
	if n := pathLoads.Load(); n != path0+1 {
		t.Errorf("the layout taking the request: %d loads, want %d", n, path0+1)
	}
	send(t, c, map[string]any{"type": "navigate", "ref": 3, "url": "/c"})
	if m := must(t, c); m.Type != live.Redirect || m.URL != "/about" {
		t.Errorf("a guard under a kept layout: %+v, want a redirect to /about", m)
	}
}

func TestReloadLayoutWithoutTheLayout(t *testing.T) {
	app := New(Only("none"))
	app.Live("/a", func() *titledPage { return &titledPage{} }, LayoutOf[frameLayout](), ReloadLayout[pathLayout]())
	if err := app.Build(); err == nil || !strings.Contains(err.Error(), "web.ReloadLayout[web.pathLayout]: the route has no web.LayoutOf[web.pathLayout]") {
		t.Errorf("Build: %v", err)
	}
}

// suffixLayout titles the document with the page's title and the site's.
type suffixLayout struct{ Children ui.Node }

func (l *suffixLayout) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text(ui.PageTitle(ctx)+" · Shop"))), ui.El("body", nil, l.Children)).Render(ctx, w)
}

// untitledLayout's head has no title: a load gives it the page's.
type untitledLayout struct{ Children ui.Node }

func (l *untitledLayout) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("head", nil), ui.El("body", nil, l.Children)).Render(ctx, w)
}

// TestNavigationTitle: a navigation's reply titles the document as a load
// of its URL does, a patch of the same URL included: the layout's <title>,
// else the page's own.
func TestNavigationTitle(t *testing.T) {
	app := New(Only("none"))
	page := func(title string) func() *titledPage {
		return func() *titledPage { return &titledPage{title: title, text: title} }
	}
	app.Live("/orders", page("Orders"), LayoutOf[suffixLayout]())
	app.Live("/products", page("Products"), LayoutOf[suffixLayout]())
	app.Live("/plain", page("Plain"), LayoutOf[untitledLayout]())
	app.Live("/other", page("Other"), LayoutOf[untitledLayout]())
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	title := func(c *websocket.Conn) string {
		t.Helper()
		_, b, err := c.Read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Type, Title string
		}
		if err := json.Unmarshal(b, &m); err != nil || m.Type != live.PatchMsg {
			t.Fatalf("%s: %v", b, err)
		}
		return m.Title
	}
	for _, tt := range []struct{ from, to, want string }{
		{"/orders", "/orders", "Orders · Shop"},
		{"/orders", "/orders?page=2", "Orders · Shop"},
		{"/orders", "/products", "Products · Shop"},
		{"/plain", "/plain", "Plain"},
		{"/plain", "/other", "Other"},
	} {
		html, _ := pageToken(t, srv, tt.to)
		if !strings.Contains(html, "<title>"+tt.want+"</title>") {
			t.Fatalf("a load of %s isn't titled %q: %s", tt.to, tt.want, html)
		}
		c, _ := joined(t, srv, tt.from)
		send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": tt.to, "patch": true})
		if got := title(c); got != tt.want {
			t.Errorf("%s → %s: title %q, want %q", tt.from, tt.to, got, tt.want)
		}
		c.CloseNow()
	}
}

func TestDocumentTitle(t *testing.T) {
	for _, tt := range []struct{ doc, want string }{
		{`<html><head><meta charset="utf-8"><title>A &amp; B · Shop</title></head><body><title>no</title></body></html>`, "A & B · Shop"},
		{`<html><head lang="en"><title data-x="1">T</title></head></html>`, "T"},
		{`<html><head></head><body><title>no</title></body></html>`, "page"},
		{`<html><body>x</body></html>`, ""},
		{`<div>x</div>`, ""},
	} {
		if got := documentTitle(tt.doc, "page"); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.doc, got, tt.want)
		}
	}
}
