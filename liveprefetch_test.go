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
)

// A prefetch runs the target's route for the connection and holds the result,
// so the navigate that follows a click applies it without running the route
// again. A counting middleware stands for the route's work (auth gates, layout
// loads, a page handler's queries): it runs on the prefetch, not on the click.
// Messages are handled in order on the connection's one goroutine, so by the
// time the navigate's reply arrives the prefetch has already run.
func TestLivePrefetch(t *testing.T) {
	meterMounts.Store(0)
	var runs atomic.Int64
	count := Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Vuka-Live") == "navigate" {
				runs.Add(1)
			}
			h.ServeHTTP(w, r)
		})
	})
	app := New(Only("none"), Dev())
	app.Live("/pf/a", &meter{})
	app.Live("/pf/b", &meter{}, count)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	c, _ := joined(t, srv, "/pf/a")

	// Hover: the route runs once, ahead of any click.
	send(t, c, map[string]any{"type": "prefetch", "url": "/pf/b"})
	// Click: the warm page applies without running the route again.
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/pf/b"})
	if raw := reply(t, c); !strings.Contains(raw, "n0") || !strings.Contains(raw, `"url":"/pf/b"`) {
		t.Fatalf("navigate to the warmed page: %s", raw)
	}
	if n := runs.Load(); n != 1 {
		t.Errorf("route runs after a prefetched navigation = %d, want 1 (the hover's, not the click's)", n)
	}

	// A second move to /pf/b with no prefetch runs the route on the click.
	send(t, c, map[string]any{"type": "navigate", "ref": 3, "url": "/pf/a"})
	reply(t, c)
	send(t, c, map[string]any{"type": "navigate", "ref": 4, "url": "/pf/b"})
	reply(t, c)
	if n := runs.Load(); n != 2 {
		t.Errorf("route runs after an unwarmed navigation = %d, want 2", n)
	}
}

// A prefetch is used only for the exact URL held, and a navigation elsewhere
// drops it: moving to a page that wasn't warmed still runs its route and
// renders that page, not the warmed one.
func TestLivePrefetchStaleDropped(t *testing.T) {
	meterMounts.Store(0)
	app := New(Only("none"), Dev())
	app.Live("/pf/a", &meter{})
	app.Get("/pf/b", func() ui.Node { return ui.El("div", nil, ui.Text("PAGE-B")) })
	app.Get("/pf/c", func() ui.Node { return ui.El("div", nil, ui.Text("PAGE-C")) })
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	c, _ := joined(t, srv, "/pf/a")
	send(t, c, map[string]any{"type": "prefetch", "url": "/pf/b"})
	// Go to /pf/c instead: the warm /pf/b can't serve it, so /pf/c's own route
	// runs and its page shows — the stale /pf/b is dropped, not shown.
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/pf/c"})
	raw := reply(t, c)
	if !strings.Contains(raw, "PAGE-C") {
		t.Fatalf("navigate to an unwarmed page, want PAGE-C: %s", raw)
	}
	if strings.Contains(raw, "PAGE-B") {
		t.Fatalf("the dropped prefetch leaked into another page: %s", raw)
	}
}

// reply reads one server frame and returns it as JSON text, so a test can look
// for rendered content whether it arrives as html or as a tree update.
func reply(t testing.TB, c *websocket.Conn) string {
	t.Helper()
	b, _ := json.Marshal(must(t, c))
	return string(b)
}

// pfDoc is a document layout every /pf/sec page shares: kept across them.
type pfDoc struct{ Children ui.Node }

func (l *pfDoc) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("body", nil, l.Children)).Render(ctx, w)
}

// pfSec is a layout under it keyed by its path parameter, holding a live
// component: another {sec} is another layout, its component mounting afresh.
type pfSec struct {
	Sec      int
	Children ui.Node
}

func (l *pfSec) Render(ctx context.Context, w io.Writer) error {
	return ui.El("section", nil, ui.Component("pf.sec", nil, &meter{}), l.Children).Render(ctx, w)
}

// A prefetched navigation keys the layouts it loads to the navigation that
// uses it, as a fresh run does: moving from /pf/sec/1 to /pf/sec/2 keeps the
// document but mounts the section layout's component afresh, whether or not
// the link was hovered first.
func TestLivePrefetchLayoutKeys(t *testing.T) {
	app := New(Only("none"))
	doc, sec := LayoutOf[pfDoc](), LayoutOf[pfSec]()
	app.Live("/pf/sec/{sec}", func() *titledPage { return &titledPage{text: "page"} }, doc, sec)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	for _, hover := range []bool{false, true} {
		c, _ := joined(t, srv, "/pf/sec/1")
		if hover {
			send(t, c, map[string]any{"type": "prefetch", "url": "/pf/sec/2"})
		}
		before := meterMounts.Load()
		send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/pf/sec/2"})
		reply(t, c)
		if n := meterMounts.Load() - before; n != 1 {
			t.Errorf("hovered %v: the section layout's component mounted %d times on the move to another section, want 1", hover, n)
		}
		c.CloseNow()
	}
}
