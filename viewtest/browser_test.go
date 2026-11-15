package viewtest_test

import (
	"testing"

	"github.com/vuka-lang/web/v2/viewtest"
)

func TestBrowse(t *testing.T) {
	p := viewtest.Browse(t, app(nil), "/counter")
	p.Click("Add one").Click("Add one")
	p.Expect("#count").Text("2")

	p.Click("Todos")
	p.Expect("#page form").Visible()
	if p.URL() != "/todos" {
		t.Fatalf("URL %q", p.URL())
	}
	p.Fill("title", "Milk").Check("done", true).Select("kind", "Work")
	p.Expect("title").Value("Milk")
	p.Expect("done").Value("true")
	p.Submit("Add")
	p.Expect("#todos li").Text("Milk/true/work")

	// A hover renders the page ahead: its Mount runs then, not on the click.
	counterMounts.Store(0)
	p.Hover("#to-mounted")
	p.Wait("the hover's mount", func() bool { return counterMounts.Load() == 1 })
	p.Click("#to-mounted").Expect("#m").Text("mount 1")
	p.Navigate("/counter").Expect("#count").Text("0")
}

func TestBrowseDeferred(t *testing.T) {
	release := make(chan struct{})
	p := viewtest.Browse(t, app(release), "/feed", viewtest.EventStream())
	p.Expect("#stats").Text("stats loading")
	close(release)
	p.Expect("#stats").Text("stats 7")
	p.ScrollTo("#comments")
	p.Expect("#comments").Text("comments first")
	var ws bool
	p.Eval(`"WebSocket" in window`, &ws)
	if ws {
		t.Error("EventStream left the WebSocket")
	}
	p.Click("away")
	p.Expect("main p").Text("a plain page")
	p.Fill("q", "cats").Press("Enter")
	p.Expect("#found").Text("found cats")
}

func TestBrowseTabs(t *testing.T) {
	p := viewtest.Browse(t, app(nil), "/counter")
	q := p.NewTab("/todos")
	q.Expect("#page form").Visible()
	p.Click("Add one").Expect("#count").Text("1")
}
