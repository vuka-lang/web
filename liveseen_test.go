package web

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// seenCtl steers a seenPage's comments loader: it counts its runs, waits
// for release when set, and reports a context ended first on ended.
type seenCtl struct {
	runs    atomic.Int32
	release chan struct{}
	ended   chan error
}

// seenPage defers stats, loaded at once, and comments, loaded once their
// fallback below a tall spacer is seen.
type seenPage struct {
	ui.Live
	ctl      *seenCtl
	stats    int
	comments []string
	clicks   int
}

func (p *seenPage) Click() { p.clicks++ }

func (p *seenPage) LiveSchema(l *ui.LiveSchema) {
	l.Field(&p.stats, ui.Defer("stats"))
	l.Field(&p.comments, ui.Defer("comments").WhenSeen())
	l.Loads("stats", p.loadStats)
	l.Loads("comments", p.loadComments)
}

func (p *seenPage) loadStats(ctx context.Context) error { p.stats = 7; return nil }

func (p *seenPage) loadComments(ctx context.Context) error {
	p.ctl.runs.Add(1)
	if p.ctl.release != nil {
		select {
		case <-p.ctl.release:
		case <-ctx.Done():
			p.ctl.ended <- ctx.Err()
			return ctx.Err()
		}
	}
	p.comments = []string{"first", "second"}
	return nil
}

func (p *seenPage) Render() ui.Node {
	stats := ui.Node(ui.Text("stats loading"))
	if !p.Loading("stats") {
		stats = ui.Text(fmt.Sprint("stats ", p.stats))
	}
	comments := ui.Node(ui.Skeleton{For: "comments", Lines: 3, Attrs: []ui.Attr{{Name: "id", Value: "comments-fallback"}}})
	if !p.Loading("comments") {
		comments = ui.Text("comments " + strings.Join(p.comments, ","))
	}
	return ui.El("main", nil,
		ui.El("p", []ui.Attr{{Name: "id", Value: "stats"}}, stats),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Click)}}, ui.Text(fmt.Sprint("clicks ", p.clicks))),
		ui.El("div", []ui.Attr{{Name: "style", Value: "height:4000px"}}),
		ui.El("section", []ui.Attr{{Name: "id", Value: "comments"}}, comments))
}

func seenApp(ctl *seenCtl) *App {
	a := New(Only("none"))
	a.Live("/post", func() *seenPage { return &seenPage{ctl: ctl} })
	a.Get("/elsewhere", func() ui.Node { return ui.El("main", nil, ui.Text("elsewhere")) })
	return a
}

// quiet fails unless the next message c sends is the reply to a click,
// sent after what came before it: nothing answered those.
func quiet(t *testing.T, c *websocket.Conn, html string, ref int, what string) {
	t.Helper()
	send(t, c, map[string]any{"type": "event", "ref": ref, "target": handlerOf(t, html, "click", 0), "event": "click"})
	if m := must(t, c); m.Ref != ref {
		t.Fatalf("%s: %+v", what, m)
	}
}

// A group loading when seen waits for the browser to name it: once, and
// only it.
func TestLiveDeferredSeen(t *testing.T) {
	ctl := &seenCtl{}
	srv := httptest.NewServer(seenApp(ctl).Handler())
	defer srv.Close()

	c, html := joined(t, srv, "/post")
	if !strings.Contains(html, `data-vk-seen="comments"`) {
		t.Fatalf("the fallback isn't marked: %s", html)
	}
	patchesUntil(t, c, "stats 7")
	time.Sleep(100 * time.Millisecond)
	if n := ctl.runs.Load(); n != 0 {
		t.Fatalf("comments loaded after the join: %d runs", n)
	}

	// Names that aren't the page's groups loading when seen do nothing.
	send(t, c, map[string]any{"type": "loads", "groups": []string{"stats", "nope", "", "bad name"}})
	quiet(t, c, html, 2, "an answer to groups not loading when seen")

	send(t, c, map[string]any{"type": "loads", "groups": []string{"comments"}})
	got := patchesUntil(t, c, "comments first,second")
	if strings.Contains(got, `data-vk-seen`) {
		t.Errorf("the fallback stayed: %s", got)
	}
	send(t, c, map[string]any{"type": "loads", "groups": []string{"comments", "comments"}})
	quiet(t, c, html, 3, "a repeat was answered")
	if n := ctl.runs.Load(); n != 1 {
		t.Errorf("comments loaded %d times", n)
	}
}

// Leaving the page ends a load the browser asked for, as it does the rest.
func TestLiveDeferredSeenNavigation(t *testing.T) {
	ctl := &seenCtl{release: make(chan struct{}), ended: make(chan error, 1)}
	srv := httptest.NewServer(seenApp(ctl).Handler())
	defer srv.Close()

	c, _ := joined(t, srv, "/post")
	patchesUntil(t, c, "stats 7")
	send(t, c, map[string]any{"type": "loads", "groups": []string{"comments"}})
	deadline := time.Now().Add(3 * time.Second)
	for ctl.runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/elsewhere"})
	select {
	case err := <-ctl.ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ended with %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leaving the page didn't end its load")
	}
}

func TestBrowserDeferredSeen(t *testing.T) {
	ctl := &seenCtl{}
	srv := httptest.NewServer(seenApp(ctl).Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Open(srv.URL + "/post")
	p.WaitText("#stats", "stats 7")
	time.Sleep(200 * time.Millisecond)
	if n := ctl.runs.Load(); n != 0 {
		t.Fatalf("comments loaded below the fold: %d runs", n)
	}
	if err := p.Eval(`document.querySelector("#comments-fallback").scrollIntoView(), true`, nil); err != nil {
		t.Fatal(err)
	}
	p.WaitText("#comments", "comments first,second")
	if n := ctl.runs.Load(); n != 1 {
		t.Errorf("comments loaded %d times", n)
	}
}

// Without IntersectionObserver a fallback asks as soon as the page joins:
// the group loads as an eager one does, rather than never.
func TestBrowserDeferredSeenNoObserver(t *testing.T) {
	ctl := &seenCtl{}
	srv := httptest.NewServer(seenApp(ctl).Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Start(srv.URL+"/post", `document.documentElement.dataset.vkState === "connected"`, `delete window.IntersectionObserver`)
	p.WaitText("#comments", "comments first,second")
	if n := ctl.runs.Load(); n != 1 {
		t.Errorf("comments loaded %d times", n)
	}
}
