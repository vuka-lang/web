package web_test

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/viewtest"
)

var seenShelfRenders atomic.Int32

// seenShelfPage is a live page whose lazy region, shown from the first
// render but far below the fold, loads when seen.
type seenShelfPage struct{ ui.Live }

func (p *seenShelfPage) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("p", []ui.Attr{{Name: "id", Value: "top"}}, ui.Text("top")),
		ui.El("div", []ui.Attr{{Name: "style", Value: "height:4000px"}}),
		ui.El("div", []ui.Attr{{Name: "id", Value: "wrap"}},
			ui.Lazy(ui.LazyProps{When: true, Seen: true, Children: ui.Ctx(func(context.Context) ui.Node {
				n := seenShelfRenders.Add(1)
				return ui.El("p", []ui.Attr{{Name: "id", Value: "content"}}, ui.Text("shelf "+strconv.Itoa(int(n))))
			})})))
}

func seenShelfApp() *web.App {
	a := web.New(web.Only("none"))
	a.Live("/shelf", func() *seenShelfPage { return &seenShelfPage{} })
	return a
}

// A lazy region loading when seen (ui.Lazy's seen) waits, shown, for the
// browser to see it: the join renders the shell, marked data-vk-when-seen,
// and scrolling to it asks for the content once.
func TestLazySeen(t *testing.T) {
	seenShelfRenders.Store(0)
	p := viewtest.Open(t, seenShelfApp(), "/shelf")
	html := p.HTML()
	if !strings.Contains(html, "data-vk-when-seen") || !strings.Contains(html, "data-vk-init") {
		t.Fatalf("the region isn't marked:\n%s", html)
	}
	if strings.Contains(html, "shelf 1") || seenShelfRenders.Load() != 0 {
		t.Fatalf("the region rendered before being seen: %d renders\n%s", seenShelfRenders.Load(), html)
	}
	p.ScrollTo("#wrap")
	p.Expect("#content").Text("shelf 1")
	if strings.Contains(p.HTML(), "data-vk-when-seen") {
		t.Errorf("the mark stayed:\n%s", p.HTML())
	}
	p.ScrollTo("#wrap")
	if seenShelfRenders.Load() != 1 {
		t.Errorf("the region rendered %d times", seenShelfRenders.Load())
	}
}
