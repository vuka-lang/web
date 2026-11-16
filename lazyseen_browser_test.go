package web_test

import (
	"context"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

var seenCartRenders atomic.Int32

// seenCartPage isn't live: its lazy region, shown but below the fold,
// loads when seen, over HTTP.
func seenCartPage() ui.Node {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Cart"))), ui.El("body", nil, ui.StyleTag(), ui.El("main", nil,
		ui.El("p", []ui.Attr{{Name: "id", Value: "top"}}, ui.Text("top")),
		ui.El("div", []ui.Attr{{Name: "style", Value: "height:4000px"}}),
		ui.El("div", []ui.Attr{{Name: "id", Value: "wrap"}},
			ui.Lazy(ui.LazyProps{When: true, Seen: true, Children: ui.Ctx(func(context.Context) ui.Node {
				n := seenCartRenders.Add(1)
				return ui.El("p", []ui.Attr{{Name: "id", Value: "content"}}, ui.Text("cart "+strconv.Itoa(int(n))))
			})})))))
}

// A real browser: a live page's region loading when seen, shown from the
// first render but below the fold, isn't rendered until it scrolls into
// view; then one vkinit asks for the content, once.
func TestBrowserLazySeen(t *testing.T) {
	seenShelfRenders.Store(0)
	srv := httptest.NewServer(seenShelfApp().Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Open(srv.URL + "/shelf")
	p.WaitText("#top", "top")
	time.Sleep(200 * time.Millisecond)
	if n := seenShelfRenders.Load(); n != 0 {
		t.Fatalf("the region rendered below the fold: %d renders", n)
	}
	if err := p.Eval(`document.querySelector("#wrap").scrollIntoView(), true`, nil); err != nil {
		t.Fatal(err)
	}
	p.WaitText("#content", "shelf 1")
	if n := seenShelfRenders.Load(); n != 1 {
		t.Errorf("the region rendered %d times", n)
	}
}

// Without IntersectionObserver a region loading when seen asks as soon as
// it shows, so the content loads as an unseen region's does rather than
// never.
func TestBrowserLazySeenNoObserver(t *testing.T) {
	seenShelfRenders.Store(0)
	srv := httptest.NewServer(seenShelfApp().Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Start(srv.URL+"/shelf", `document.documentElement.dataset.vkState === "connected"`, `delete window.IntersectionObserver`)
	p.WaitText("#content", "shelf 1")
	if n := seenShelfRenders.Load(); n != 1 {
		t.Errorf("the region rendered %d times", n)
	}
}

// A real browser, a page that isn't live: a region loading when seen asks
// the page for its content over HTTP once it scrolls into view.
func TestBrowserLazySeenHTTP(t *testing.T) {
	seenCartRenders.Store(0)
	a := web.New(web.Only("none"))
	a.Get("/cart", seenCartPage)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Visit(srv.URL + "/cart")
	p.WaitText("#top", "top")
	time.Sleep(200 * time.Millisecond)
	if n := seenCartRenders.Load(); n != 0 {
		t.Fatalf("the region rendered below the fold: %d renders", n)
	}
	if err := p.Eval(`document.querySelector("#wrap").scrollIntoView(), true`, nil); err != nil {
		t.Fatal(err)
	}
	p.WaitText("#content", "cart 1")
	if n := seenCartRenders.Load(); n != 1 {
		t.Errorf("the region rendered %d times", n)
	}
}
