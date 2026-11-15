package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
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
// document but mounts the section layout's component afresh, once, whether or
// not the link was hovered first (hovered, the mount is the hover's render).
func TestLivePrefetchLayoutKeys(t *testing.T) {
	app := New(Only("none"))
	doc, sec := LayoutOf[pfDoc](), LayoutOf[pfSec]()
	app.Live("/pf/sec/{sec}", func() *titledPage { return &titledPage{text: "page"} }, doc, sec)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	for _, hover := range []bool{false, true} {
		c, _ := joined(t, srv, "/pf/sec/1")
		before := meterMounts.Load()
		if hover {
			send(t, c, map[string]any{"type": "prefetch", "url": "/pf/sec/2"})
		}
		send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/pf/sec/2"})
		reply(t, c)
		if n := meterMounts.Load() - before; n != 1 {
			t.Errorf("hovered %v: the section layout's component mounted %d times on the move to another section, want 1", hover, n)
		}
		c.CloseNow()
	}
}

// navHTML is the markup a v1 reply carries: its page, else its updates.
func navHTML(m live.ServerMessage) string {
	var b strings.Builder
	b.WriteString(m.HTML)
	for _, u := range m.Updates {
		b.WriteString(u.HTML)
	}
	return b.String()
}

// pfApp serves meters at /pf/a and /pf/b (whose navigate runs it counts) and
// whatever more says.
func pfApp(runs *atomic.Int64, opts ...Option) *App {
	meterMounts.Store(0)
	count := Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Vuka-Live") == "navigate" {
				runs.Add(1)
			}
			h.ServeHTTP(w, r)
		})
	})
	app := New(append([]Option{Only("none")}, opts...)...)
	app.Live("/pf/a", &meter{})
	app.Live("/pf/b", &meter{}, count)
	return app
}

// A hover renders the page whole, mounting its components, and the click
// takes them: Mount runs at the hover and not again, the page arriving as
// its Mount left it, live (its handlers answer).
func TestLivePrefetchRendersAhead(t *testing.T) {
	var runs atomic.Int64
	srv := httptest.NewServer(pfApp(&runs).Handler())
	defer srv.Close()
	c, html := joined(t, srv, "/pf/a")
	if n := meterMounts.Load(); n != 1 {
		t.Fatalf("mounts after the join = %d, want 1 (adopted)", n)
	}
	send(t, c, map[string]any{"type": "prefetch", "url": "/pf/b"})
	quiet(t, c, html, 2, "a click on the page after the hover")
	if n := meterMounts.Load(); n != 2 {
		t.Fatalf("mounts after the hover = %d, want 2: the hover renders the page", n)
	}
	send(t, c, map[string]any{"type": "navigate", "ref": 3, "url": "/pf/b"})
	m := must(t, c)
	page := navHTML(m)
	if m.Error != "" || !strings.Contains(page, "m2 n0") || m.URL != "/pf/b" {
		t.Fatalf("navigate to the page rendered ahead: %+v", m)
	}
	if n := meterMounts.Load(); n != 2 {
		t.Errorf("mounts after the click = %d, want 2: the click takes the hover's", n)
	}
	if n := runs.Load(); n != 1 {
		t.Errorf("route runs = %d, want 1", n)
	}
	send(t, c, map[string]any{"type": "event", "ref": 4, "target": handlerOf(t, page, "click", 0), "event": "click"})
	if m := must(t, c); !strings.Contains(navHTML(m), "m2 n1") {
		t.Errorf("a click on the page taken: %+v", m)
	}
}

// pfCloser counts its mounts and unmounts, and listens to a topic.
type pfCloser struct {
	ui.Live
	m int32
}

var pfCloserMounts, pfCloserGone atomic.Int32

func (p *pfCloser) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }
func (p *pfCloser) mount(context.Context) error {
	p.m = pfCloserMounts.Add(1)
	p.Subscribe("pf.closer")
	return nil
}
func (p *pfCloser) Unmount()        { pfCloserGone.Add(1) }
func (p *pfCloser) Render() ui.Node { return ui.El("p", nil, ui.Text(fmt.Sprintf("closer m%d", p.m))) }

// A page rendered ahead and never taken is unmounted: when a navigation
// goes elsewhere, when its window runs out, when the connection ends, and
// when a broadcast reaches what it subscribed to (it would show what that
// replaced). A click after any of them mounts the page afresh.
func TestLivePrefetchAheadClosed(t *testing.T) {
	ttl := aheadTTL
	defer func() { aheadTTL = ttl }()
	var runs atomic.Int64
	app := pfApp(&runs)
	app.Live("/pf/close", &pfCloser{})
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	hover := func(t *testing.T) (*websocket.Conn, string) {
		t.Helper()
		pfCloserMounts.Store(0)
		pfCloserGone.Store(0)
		c, html := joined(t, srv, "/pf/a")
		send(t, c, map[string]any{"type": "prefetch", "url": "/pf/close"})
		quiet(t, c, html, 2, "a click after the hover")
		if pfCloserMounts.Load() != 1 || pfCloserGone.Load() != 0 {
			t.Fatalf("after the hover: %d mounts, %d unmounts", pfCloserMounts.Load(), pfCloserGone.Load())
		}
		return c, html
	}
	clickFresh := func(t *testing.T, c *websocket.Conn) {
		t.Helper()
		send(t, c, map[string]any{"type": "navigate", "ref": 9, "url": "/pf/close"})
		if m := must(t, c); !strings.Contains(navHTML(m), "closer m2") {
			t.Errorf("the click after the page was dropped: %+v", m)
		}
	}

	t.Run("navigated elsewhere", func(t *testing.T) {
		c, _ := hover(t)
		send(t, c, map[string]any{"type": "navigate", "ref": 3, "url": "/pf/b"})
		reply(t, c)
		if n := pfCloserGone.Load(); n != 1 {
			t.Errorf("unmounts = %d, want 1", n)
		}
	})
	t.Run("expired", func(t *testing.T) {
		aheadTTL = 50 * time.Millisecond
		defer func() { aheadTTL = ttl }()
		c, _ := hover(t)
		waitFor(t, func() bool { return pfCloserGone.Load() == 1 })
		clickFresh(t, c)
		c.CloseNow() // the page the click mounted goes with it, before the next case counts
		waitFor(t, func() bool { return pfCloserGone.Load() == 2 })
	})
	t.Run("connection end", func(t *testing.T) {
		c, _ := hover(t)
		c.CloseNow()
		waitFor(t, func() bool { return pfCloserGone.Load() == 1 })
	})
	t.Run("broadcast", func(t *testing.T) {
		c, html := hover(t)
		if err := app.Broadcast(context.Background(), "pf.closer", "news"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return pfCloserGone.Load() == 1 })
		quiet(t, c, html, 3, "a click after the broadcast")
		clickFresh(t, c)
	})
}

// pfNoMsgs flashes and shows no messages; pfMsgs shows them, counting its
// mounts.
type pfNoMsgs struct{ ui.Live }

func (p *pfNoMsgs) Save(ctx context.Context) error { return Flash(ctx, "info", "Saved") }
func (p *pfNoMsgs) Render() ui.Node {
	return ui.El("main", nil, ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Save)}}, ui.Text("save")))
}

var pfMsgsMounts atomic.Int32

type pfMsgs struct {
	ui.Live
	m int32
}

func (p *pfMsgs) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }
func (p *pfMsgs) mount(context.Context) error { p.m = pfMsgsMounts.Add(1); return nil }
func (p *pfMsgs) Noop()                       {}
func (p *pfMsgs) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Noop)}}, ui.Text(fmt.Sprint("msgs m", p.m))),
		ui.NodeFunc(func(ctx context.Context, w io.Writer) error {
			io.WriteString(w, "messages ")
			for _, m := range Var[[]Message](ctx, "messages") {
				io.WriteString(w, m.Level+":"+m.Text+";")
			}
			return nil
		}))
}

// The hover's render reads a copy of the connection's context processor
// history: a message it shows is shown by the page the click commits, once,
// and no other; a message flashed after the hover makes the render stale,
// so the click mounts the page afresh, showing it.
func TestLivePrefetchMessages(t *testing.T) {
	app := New(Only("none"), ContextProcessors(MessagesContext))
	app.Live("/pf/save", func() *pfNoMsgs { return &pfNoMsgs{} })
	app.Live("/pf/msgs", func() *pfMsgs { return &pfMsgs{} })
	app.Live("/pf/other", func() *pfMsgs { return &pfMsgs{} })
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	save := func(t *testing.T, c *websocket.Conn, html string, ref int) {
		t.Helper()
		send(t, c, map[string]any{"type": "event", "ref": ref, "target": handlerOf(t, html, "click", 0), "event": "click"})
		if m := must(t, c); m.Error != "" {
			t.Fatalf("save: %+v", m)
		}
	}
	navigate := func(t *testing.T, c *websocket.Conn, ref int, url string) string {
		t.Helper()
		send(t, c, map[string]any{"type": "navigate", "ref": ref, "url": url})
		m := must(t, c)
		if m.Error != "" {
			t.Fatalf("navigate: %+v", m)
		}
		return navHTML(m)
	}
	noop := func(t *testing.T, c *websocket.Conn, page string, ref int) string {
		t.Helper()
		send(t, c, map[string]any{"type": "event", "ref": ref, "target": handlerOf(t, page, "click", 0), "event": "click"})
		return navHTML(must(t, c))
	}

	t.Run("flashed before the hover", func(t *testing.T) {
		pfMsgsMounts.Store(0)
		c, html := joined(t, srv, "/pf/save")
		save(t, c, html, 2)
		send(t, c, map[string]any{"type": "prefetch", "url": "/pf/msgs"})
		page := navigate(t, c, 3, "/pf/msgs")
		if !strings.Contains(page, "msgs m1") || !strings.Contains(page, "info:Saved;") {
			t.Fatalf("the page taken: %s", page)
		}
		if n := pfMsgsMounts.Load(); n != 1 {
			t.Errorf("mounts = %d, want 1", n)
		}
		if got := noop(t, c, page, 4); strings.Contains(got, "Saved") {
			t.Errorf("shown again: %s", got)
		}
	})
	t.Run("an unused hover shows nothing", func(t *testing.T) {
		c, html := joined(t, srv, "/pf/save")
		save(t, c, html, 2)
		send(t, c, map[string]any{"type": "prefetch", "url": "/pf/msgs"})
		if page := navigate(t, c, 3, "/pf/other"); !strings.Contains(page, "info:Saved;") {
			t.Errorf("the message the hover's render showed is gone: %s", page)
		}
	})
	t.Run("flashed after the hover", func(t *testing.T) {
		pfMsgsMounts.Store(0)
		c, html := joined(t, srv, "/pf/save")
		send(t, c, map[string]any{"type": "prefetch", "url": "/pf/msgs"})
		save(t, c, html, 2)
		page := navigate(t, c, 3, "/pf/msgs")
		if !strings.Contains(page, "msgs m2") || !strings.Contains(page, "info:Saved;") {
			t.Fatalf("after a flash the click mounts afresh, showing it: %s", page)
		}
		if got := noop(t, c, page, 4); strings.Contains(got, "Saved") {
			t.Errorf("shown again: %s", got)
		}
	})
}

// pfDefer defers stats, loaded at once, and comments, loaded when seen,
// counting each load and its mounts.
type pfDefer struct {
	ui.Live
	stats    int
	comments string
}

var pfDeferMounts, pfStatsRuns, pfCommentsRuns atomic.Int32

func (p *pfDefer) LiveSchema(l *ui.LiveSchema) {
	l.Mount(func(context.Context) error { pfDeferMounts.Add(1); return nil })
	l.Field(&p.stats, ui.Defer("stats"))
	l.Field(&p.comments, ui.Defer("comments").WhenSeen())
	l.Loads("stats", func(context.Context) error { pfStatsRuns.Add(1); p.stats = 7; return nil })
	l.Loads("comments", func(context.Context) error { pfCommentsRuns.Add(1); p.comments = "first"; return nil })
}

func (p *pfDefer) Render() ui.Node {
	stats := ui.Node(ui.Text("stats loading"))
	if !p.Loading("stats") {
		stats = ui.Text(fmt.Sprint("stats ", p.stats))
	}
	comments := ui.Node(ui.Skeleton{For: "comments"})
	if !p.Loading("comments") {
		comments = ui.Text("comments " + p.comments)
	}
	return ui.El("main", nil, ui.El("p", nil, stats), ui.El("section", nil, comments))
}

// A page rendered ahead starts no deferred load at the hover: the click
// starts its eager ones on the live session, as a navigation without a
// hover does, and those loading when seen wait for the browser to name them.
func TestLivePrefetchDeferred(t *testing.T) {
	var runs atomic.Int64
	app := pfApp(&runs)
	app.Live("/pf/defer", func() *pfDefer { return &pfDefer{} })
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	pfDeferMounts.Store(0)
	pfStatsRuns.Store(0)
	pfCommentsRuns.Store(0)
	c, html := joined(t, srv, "/pf/a")
	send(t, c, map[string]any{"type": "prefetch", "url": "/pf/defer"})
	quiet(t, c, html, 2, "a click after the hover")
	time.Sleep(50 * time.Millisecond)
	if pfDeferMounts.Load() != 1 || pfStatsRuns.Load() != 0 || pfCommentsRuns.Load() != 0 {
		t.Fatalf("after the hover: %d mounts, %d stats loads, %d comments loads; want 1, 0, 0", pfDeferMounts.Load(), pfStatsRuns.Load(), pfCommentsRuns.Load())
	}
	send(t, c, map[string]any{"type": "navigate", "ref": 3, "url": "/pf/defer"})
	if page := navHTML(must(t, c)); !strings.Contains(page, "stats loading") || !strings.Contains(page, `data-vk-seen="comments"`) {
		t.Fatalf("the page taken: %s", page)
	}
	patchesUntil(t, c, "stats 7")
	time.Sleep(50 * time.Millisecond)
	if pfStatsRuns.Load() != 1 || pfCommentsRuns.Load() != 0 {
		t.Fatalf("after the click: %d stats loads, %d comments loads; want 1, 0", pfStatsRuns.Load(), pfCommentsRuns.Load())
	}
	send(t, c, map[string]any{"type": "loads", "groups": []string{"comments"}})
	patchesUntil(t, c, "comments first")
	if pfDeferMounts.Load() != 1 || pfStatsRuns.Load() != 1 || pfCommentsRuns.Load() != 1 {
		t.Errorf("at the end: %d mounts, %d stats loads, %d comments loads; want 1 each", pfDeferMounts.Load(), pfStatsRuns.Load(), pfCommentsRuns.Load())
	}
}

// pfFeed fills a stream in Mount.
type pfFeed struct {
	ui.Live
	rows ui.Stream[feedItem]
}

var pfFeedMounts atomic.Int32

func (p *pfFeed) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }
func (p *pfFeed) mount(context.Context) error {
	pfFeedMounts.Add(1)
	p.rows.Reset(feedItem{1, "row-one"}, feedItem{2, "row-two"})
	return nil
}
func (p *pfFeed) Render() ui.Node {
	return ui.El("main", nil, p.rows.List("ul", nil, func(it feedItem) ui.Node {
		return ui.El("li", []ui.Attr{{Name: "key", Value: it.ID}}, ui.Text(it.Text))
	}))
}

// A stream its Mount filled at the hover arrives with the page the click
// takes: the render ahead keeps the rows for the reply.
func TestLivePrefetchStream(t *testing.T) {
	var runs atomic.Int64
	app := pfApp(&runs)
	app.Live("/pf/feed", func() *pfFeed { return &pfFeed{} })
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	pfFeedMounts.Store(0)
	_, tok := pageToken(t, srv, "/pf/a")
	c, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1, "v": 2, "ext": []string{"stream"}})
	must(t, c)
	send(t, c, map[string]any{"type": "prefetch", "url": "/pf/feed"})
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/pf/feed"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if raw := string(b); !strings.Contains(raw, "row-one") || !strings.Contains(raw, "row-two") {
		t.Errorf("the rows didn't arrive: %s", raw)
	}
	if n := pfFeedMounts.Load(); n != 1 {
		t.Errorf("mounts = %d, want 1", n)
	}
}

// Render-ahead falls back to the route's run alone — the click mounts the
// page — when adoption is off, when the visitor isn't who hovered, and when
// a LiveCheck fails at the click; the route runs again in the last two.
func TestLivePrefetchFallbacks(t *testing.T) {
	var who atomic.Value
	var failing atomic.Bool
	cases := []struct {
		name      string
		opts      []Option
		change    func()
		mounts    int32 // at the click
		routeRuns int64
	}{
		{"adoption off", []Option{LiveAdopt(0)}, func() {}, 1, 1},
		{"another identity", []Option{LiveIdentity(func(context.Context) string { return who.Load().(string) })}, func() { who.Store("bob") }, 1, 2},
		{"a check fails", []Option{LiveCheckEvery(0), LiveCheck(func(context.Context) error {
			if failing.Load() {
				return Unauthorized
			}
			return nil
		})}, func() { failing.Store(true) }, 1, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			who.Store("ann")
			failing.Store(false)
			var runs atomic.Int64
			srv := httptest.NewServer(pfApp(&runs, tc.opts...).Handler())
			defer srv.Close()
			c, html := joined(t, srv, "/pf/a")
			send(t, c, map[string]any{"type": "prefetch", "url": "/pf/b"})
			quiet(t, c, html, 2, "a click after the hover")
			before := meterMounts.Load()
			if tc.name != "adoption off" && before != 2 {
				t.Fatalf("mounts after the hover = %d, want 2", before)
			}
			tc.change()
			send(t, c, map[string]any{"type": "navigate", "ref": 3, "url": "/pf/b"})
			m := must(t, c)
			if want := fmt.Sprintf("m%d n0", before+tc.mounts); !strings.Contains(navHTML(m), want) {
				t.Fatalf("navigate: want %s in %+v", want, m)
			}
			if n := meterMounts.Load() - before; n != tc.mounts {
				t.Errorf("mounts at the click = %d, want %d", n, tc.mounts)
			}
			if n := runs.Load(); n != tc.routeRuns {
				t.Errorf("route runs = %d, want %d", n, tc.routeRuns)
			}
		})
	}
}
