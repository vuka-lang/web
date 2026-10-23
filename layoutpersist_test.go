package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
