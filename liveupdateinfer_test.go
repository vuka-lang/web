package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
)

// urlTabsPage keeps its tab in the URL and a count of its own that a remount
// resets; it has no Update.
type urlTabsPage struct {
	ui.Live
	tab    string
	n      int
	mounts *atomic.Int32
}

func (p *urlTabsPage) LiveSchema(l *ui.LiveSchema) {
	l.Field(&p.tab, ui.URL("tab"))
	l.Mount(func(context.Context) error { p.mounts.Add(1); return nil })
}

func (p *urlTabsPage) Bump() { p.n++ }

func (p *urlTabsPage) Render() ui.Node {
	return ui.El("main", nil, ui.Text(fmt.Sprintf("tab %s n %d", p.tab, p.n)),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Bump)}}, ui.Text("bump")))
}

// urlTabsUpdating is urlTabsPage with an Update taking web.URLParams.
type urlTabsUpdating struct {
	ui.Live
	tab     string
	n       int
	mounts  *atomic.Int32
	updates *atomic.Int32
}

func (p *urlTabsUpdating) LiveSchema(l *ui.LiveSchema) {
	l.Field(&p.tab, ui.URL("tab"))
	l.Mount(func(context.Context) error { p.mounts.Add(1); return nil })
}

func (p *urlTabsUpdating) Update(URLParams) { p.updates.Add(1) }
func (p *urlTabsUpdating) Bump()            { p.n++ }

func (p *urlTabsUpdating) Render() ui.Node {
	return ui.El("main", nil, ui.Text(fmt.Sprintf("tab %s n %d", p.tab, p.n)),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Bump)}}, ui.Text("bump")))
}

// A patch keeps a live page without Update when it changes only what the
// page's @ui.URL fields keep, and says nothing; a patch changing anything
// else mounts it again and, in development, says once for the route what
// kept it from staying and the Update that would; a page with an Update
// stays mounted across both, its Update running, as before.
func TestLivePatchInfersUpdate(t *testing.T) {
	withToolbar(t)
	logs := &warnLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var mounts, updates atomic.Int32
	start := func(t *testing.T) *httptest.Server {
		mounts.Store(0)
		updates.Store(0)
		logs.take()
		a := New(Only("none"), Dev())
		a.Live("/tabs", func() *urlTabsPage { return &urlTabsPage{mounts: &mounts} })
		a.Live("/updating", func() *urlTabsUpdating {
			return &urlTabsUpdating{mounts: &mounts, updates: &updates}
		})
		srv := httptest.NewServer(a.Handler())
		t.Cleanup(srv.Close)
		return srv
	}
	// open joins from, bumps the count once and returns a patch function.
	open := func(t *testing.T, srv *httptest.Server, from string) func(ref int, to string) string {
		t.Helper()
		c, html := joined(t, srv, from)
		t.Cleanup(func() { c.CloseNow() })
		send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", 0), "event": "click"})
		if got := fmt.Sprint(must(t, c)); !strings.Contains(got, "n 1") {
			t.Fatalf("the bump didn't render: %s", got)
		}
		return func(ref int, to string) string {
			t.Helper()
			send(t, c, map[string]any{"type": "navigate", "ref": ref, "url": to, "patch": true})
			return fmt.Sprint(must(t, c))
		}
	}

	t.Run("only @ui.URL fields change: stays mounted, no warning", func(t *testing.T) {
		srv := start(t)
		patch := open(t, srv, "/tabs?tab=a")
		got := patch(3, "/tabs?tab=b")
		if !strings.Contains(got, "tab b n 1") {
			t.Fatalf("the patch didn't keep the page's count: %s", got)
		}
		if mounts.Load() != 1 {
			t.Fatalf("mounts %d, want 1", mounts.Load())
		}
		if got := logs.take(); len(got) != 0 {
			t.Fatalf("warned: %q", got)
		}
	})

	t.Run("a parameter nothing keeps: mounts again, warns once", func(t *testing.T) {
		srv := start(t)
		patch := open(t, srv, "/tabs?tab=a")
		got := patch(3, "/tabs?tab=b&other=1")
		if !strings.Contains(got, "tab b n 0") {
			t.Fatalf("the page wasn't mounted again: %s", got)
		}
		if mounts.Load() != 2 {
			t.Fatalf("mounts %d, want 2", mounts.Load())
		}
		w := logs.take()
		if len(w) != 1 || !strings.HasPrefix(w[0], "WARN a patch link targets a live page without Update") ||
			!strings.Contains(w[0], "changes ?other=, which no @ui.URL field") ||
			!strings.Contains(w[0], "func (p *urlTabsPage) Update(web.URLParams)") || !strings.Contains(w[0], "route=/tabs") {
			t.Fatalf("logged %q", w)
		}
		patch(4, "/tabs?tab=c&other=2")
		patch(5, "/tabs?tab=c&other=2")
		if w := logs.take(); len(w) != 0 {
			t.Fatalf("said again for the same route: %q", w)
		}
	})

	t.Run("nothing changes: mounts again, says so", func(t *testing.T) {
		srv := start(t)
		patch := open(t, srv, "/tabs?tab=a")
		if got := patch(3, "/tabs?tab=a"); !strings.Contains(got, "tab a n 0") {
			t.Fatalf("the page wasn't mounted again: %s", got)
		}
		if w := logs.take(); len(w) != 1 || !strings.Contains(w[0], "the patch changes no parameter") {
			t.Fatalf("logged %q", w)
		}
	})

	t.Run("an explicit Update: stays mounted, Update runs", func(t *testing.T) {
		srv := start(t)
		patch := open(t, srv, "/updating?tab=a")
		if got := patch(3, "/updating?tab=b"); !strings.Contains(got, "tab b n 1") {
			t.Fatalf("the patch didn't keep the page: %s", got)
		}
		if got := patch(4, "/updating?tab=b&other=1"); strings.Contains(got, "n 0") {
			t.Fatalf("the page was mounted again: %s", got)
		}
		if mounts.Load() != 1 || updates.Load() != 2 {
			t.Fatalf("mounts %d, updates %d, want 1 and 2", mounts.Load(), updates.Load())
		}
		if w := logs.take(); len(w) != 0 {
			t.Fatalf("warned: %q", w)
		}
	})
}
