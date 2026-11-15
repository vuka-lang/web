package viewtest_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/viewtest"
)

type counter struct {
	ui.Live
	n int
}

func (c *counter) Inc() { c.n++ }

func (c *counter) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("p", []ui.Attr{{Name: "id", Value: "count"}}, ui.Text(fmt.Sprint(c.n))),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(c.Inc)}}, ui.Text("Add one")))
}

type todoForm struct {
	Title string `form:"title"`
	Done  bool   `form:"done"`
	Kind  string `form:"kind"`
}

type todos struct {
	ui.Live
	items []string
}

func (t *todos) Add(f todoForm) error {
	if f.Title == "" {
		return web.BadRequest("a title, please")
	}
	t.items = append(t.items, fmt.Sprintf("%s/%v/%s", f.Title, f.Done, f.Kind))
	return nil
}

func (t *todos) Render() ui.Node {
	var lis []ui.Node
	for _, it := range t.items {
		lis = append(lis, ui.El("li", nil, ui.Text(it)))
	}
	return ui.El("main", nil,
		ui.El("form", []ui.Attr{{Name: "onSubmit", Value: ui.On(t.Add)}},
			ui.El("input", []ui.Attr{{Name: "name", Value: "title"}}),
			ui.El("input", []ui.Attr{{Name: "type", Value: "checkbox"}, {Name: "name", Value: "done"}, {Name: "value", Value: "true"}}),
			ui.El("select", []ui.Attr{{Name: "name", Value: "kind"}},
				ui.El("option", []ui.Attr{{Name: "value", Value: "home"}}, ui.Text("Home")),
				ui.El("option", []ui.Attr{{Name: "value", Value: "work"}}, ui.Text("Work"))),
			ui.El("button", nil, ui.Text("Add"))),
		ui.El("ul", []ui.Attr{{Name: "id", Value: "todos"}}, lis...))
}

// feed defers stats, loaded at once, and comments, loaded when seen.
type feed struct {
	ui.Live
	release  chan struct{}
	stats    int
	comments string
}

func (p *feed) LiveSchema(l *ui.LiveSchema) {
	l.Field(&p.stats, ui.Defer("stats"))
	l.Field(&p.comments, ui.Defer("comments").WhenSeen())
	l.Loads("stats", func(ctx context.Context) error { <-p.release; p.stats = 7; return nil })
	l.Loads("comments", func(ctx context.Context) error { p.comments = "first"; return nil })
}

func (p *feed) Away() error { return web.Redirect("/plain") }

func (p *feed) Render() ui.Node {
	stats, comments := "stats loading", ui.Node(ui.Skeleton{For: "comments", Lines: 1})
	if !p.Loading("stats") {
		stats = fmt.Sprint("stats ", p.stats)
	}
	if !p.Loading("comments") {
		comments = ui.Text("comments " + p.comments)
	}
	return ui.El("main", nil,
		ui.El("p", []ui.Attr{{Name: "id", Value: "stats"}}, ui.Text(stats)),
		ui.El("section", []ui.Attr{{Name: "id", Value: "comments"}}, comments),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Away)}}, ui.Text("away")))
}

type frame struct{ Children ui.Node }

func (l *frame) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("body", nil,
		ui.El("nav", nil,
			ui.Link(ui.LinkProps{Href: "/counter", ID: "to-counter", Children: ui.Text("Counter")}),
			ui.Link(ui.LinkProps{Href: "/todos", ID: "to-todos", Children: ui.Text("Todos")}),
			ui.Link(ui.LinkProps{Href: "/mounted", ID: "to-mounted", Children: ui.Text("Mounted")})),
		ui.El("div", []ui.Attr{{Name: "id", Value: "page"}}, l.Children))).Render(ctx, w)
}

var counterMounts atomic.Int32

type mounted struct {
	ui.Live
	m int32
}

func (c *mounted) LiveSchema(l *ui.LiveSchema) {
	l.Mount(func(context.Context) error { c.m = counterMounts.Add(1); return nil })
}
func (c *mounted) Render() ui.Node {
	return ui.El("p", []ui.Attr{{Name: "id", Value: "m"}}, ui.Text(fmt.Sprint("mount ", c.m)))
}

func app(release chan struct{}) *web.App {
	a := web.New(web.Only("none"), web.NoDotEnv(), web.NoTailwind())
	f := web.LayoutOf[frame]()
	a.Live("/counter", &counter{}, f)
	a.Live("/todos", func() *todos { return &todos{} }, f)
	a.Live("/feed", func() *feed { return &feed{release: release} })
	a.Live("/mounted", &mounted{}, f)
	a.Get("/plain", func() ui.Node {
		return ui.El("main", nil, ui.El("p", nil, ui.Text("a plain page")),
			ui.El("form", []ui.Attr{{Name: "method", Value: "get"}, {Name: "action", Value: "/search"}},
				ui.El("input", []ui.Attr{{Name: "name", Value: "q"}}), ui.El("button", nil, ui.Text("Search"))))
	})
	a.Get("/search", func(q struct {
		Q string `query:"q"`
	}) ui.Node {
		return ui.El("p", []ui.Attr{{Name: "id", Value: "found"}}, ui.Text("found "+q.Q))
	})
	return a
}

func TestLiveClick(t *testing.T) {
	p := viewtest.Open(t, app(nil), "/counter")
	p.Click("Add one").Click("button")
	p.Expect("#count").Text("2")
	if p.Text("#count") != "2" {
		t.Fatal(p.HTML())
	}
}

func TestLiveForm(t *testing.T) {
	p := viewtest.Open(t, app(nil), "/todos")
	p.Submit("Add")
	if p.Err() != "a title, please" {
		t.Errorf("Err %q", p.Err())
	}
	p.Fill("title", "Milk").Check("done", true).Select("kind", "Work")
	p.Expect("title").Value("Milk")
	p.Expect("done").Value("true")
	p.Click("Add")
	p.Expect("#todos li").Text("Milk/true/work")
	if p.Err() != "" {
		t.Errorf("Err %q", p.Err())
	}
}

func TestLiveDeferredAndSeen(t *testing.T) {
	release := make(chan struct{})
	p := viewtest.Open(t, app(release), "/feed")
	p.Expect("#stats").Text("stats loading")
	close(release)
	p.Expect("#stats").Text("stats 7")
	p.Expect("#comments [data-vk-seen]").Visible()
	p.ScrollTo("#comments")
	p.Expect("#comments").Text("comments first")
	p.Expect("#comments [data-vk-seen]").Absent()

	// A redirect from an event loads the page it names, a plain one; its
	// form gets the next page.
	p.Click("away")
	p.Expect("main p").Text("a plain page")
	p.Fill("q", "cats").Submit("Search")
	p.Expect("#found").Text("found cats")
	if p.URL() != "/search?q=cats" {
		t.Errorf("URL %q", p.URL())
	}
}

func TestLiveNavigation(t *testing.T) {
	p := viewtest.Open(t, app(nil), "/counter")
	p.Click("Add one")
	p.Click("Todos")
	if p.URL() != "/todos" {
		t.Fatalf("URL %q", p.URL())
	}
	p.Expect("#page form").Visible()
	p.Navigate("/counter").Expect("#count").Text("0")

	// A hover renders the page ahead: its Mount runs then, not on the click.
	counterMounts.Store(0)
	p.Hover("#to-mounted")
	p.Wait("the hover's mount", func() bool { return counterMounts.Load() == 1 })
	p.Click("#to-mounted").Expect("#m").Text("mount 1")
}

func TestLiveFailures(t *testing.T) {
	for _, c := range []struct {
		do   func(p *viewtest.Live)
		want string
	}{
		{func(p *viewtest.Live) { p.Click("Nope") }, `Click: no element matches "Nope"`},
		{func(p *viewtest.Live) { p.Click("#count") }, "nothing answers a click"},
		{func(p *viewtest.Live) { p.Expect("#count").Text("9") }, `waiting for #count to have text "9" on /counter: it shows text "0"`},
	} {
		var ft fakeT
		func() {
			defer func() { recover() }()
			saved := viewtest.Timeout
			viewtest.Timeout = 200e6
			defer func() { viewtest.Timeout = saved }()
			c.do(viewtest.Open(&ft, app(nil), "/counter"))
		}()
		for _, f := range ft.cleanup {
			f()
		}
		if !strings.Contains(ft.msg, c.want) {
			t.Errorf("got %q, want %q", ft.msg, c.want)
		}
	}
}
