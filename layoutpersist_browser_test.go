package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/uigen"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// shellLoads counts the Loads of the shell layout, crumbLoads the
// path-reading one's, deptLoads the departments layout's.
var shellLoads, crumbLoads, deptLoads atomic.Int64

// shellLayout is the document: a scrolling sidebar with a client state, a
// live counter and links marking the current one.
type shellLayout struct {
	Children ui.Node
	loads    int64
}

func (l *shellLayout) Load(ctx context.Context) error {
	l.loads = shellLoads.Add(1)
	return nil
}

func (l *shellLayout) Render(ctx context.Context, w io.Writer) error {
	open := uigen.State("shell.open", false)
	var items []ui.Node
	for i := range 40 {
		items = append(items, ui.El("p", nil, ui.Text(fmt.Sprint("item ", i))))
	}
	link := func(id, href string, mode ui.LinkActive) ui.Node {
		return ui.Link(ui.LinkProps{Href: href, ID: id, Active: mode, ActiveClass: "on", Children: ui.Text(id)})
	}
	return ui.El("html", nil,
		ui.El("head", nil, ui.El("title", nil, ui.Text(ui.PageTitle(ctx)))),
		ui.El("body", nil,
			ui.El("nav", attrs("id", "side", "style", "height: 120px; overflow: auto"),
				ui.El("button", attrs("id", "tog", "type", "button", "onClick", ui.On(uigen.Toggle(ui.JS, open)),
					"aria-pressed", ui.BindExpr(open.Get(), `{"$":0}`, open)), ui.Text("menu")),
				ui.El("span", attrs("id", "loads"), ui.Text(fmt.Sprint(l.loads))),
				ui.Component("shell.counter", nil, &sideCounter{}),
				link("to-d1", "/depts/1", ui.ActivePrefix),
				link("to-d2", "/depts/2", ui.ActivePrefix),
				link("to-about", "/about", ui.ActiveExact),
				link("to-secret", "/secret", ui.ActiveExact),
				ui.Fragment(items...)),
			ui.El("main", attrs("id", "page"), l.Children))).Render(ctx, w)
}

// sideCounter is a live component of the shell: its count is the session's.
type sideCounter struct {
	ui.Live
	n int
}

func (c *sideCounter) Inc() { c.n++ }
func (c *sideCounter) Render() ui.Node {
	return ui.El("button", attrs("id", "count", "type", "button", "onClick", ui.On(c.Inc)), ui.Text(fmt.Sprint("count ", c.n)))
}

// deptLayout wraps a department's pages: shared by /depts/{dept}/… under
// one {dept}.
type deptLayout struct {
	Dept     int
	Children ui.Node
	loads    int64
}

func (l *deptLayout) Load(ctx context.Context) error {
	l.loads = deptLoads.Add(1)
	return nil
}

func (l *deptLayout) Render(ctx context.Context, w io.Writer) error {
	return ui.El("section", attrs("id", "dept", "data-loads", fmt.Sprint(l.loads)),
		ui.El("h2", nil, ui.Text(fmt.Sprint("dept ", l.Dept))),
		ui.Link(ui.LinkProps{Href: fmt.Sprint("/depts/", l.Dept, "/people"), ID: "to-people", Children: ui.Text("people")}),
		l.Children).Render(ctx, w)
}

// crumbLayout reads the request's path in its Load: it loads on every
// navigation.
type crumbLayout struct {
	Children ui.Node
	path     string
}

func (l *crumbLayout) Load(ctx context.Context) error {
	crumbLoads.Add(1)
	l.path = Request(ctx).URL.Path
	return nil
}

func (l *crumbLayout) Render(ctx context.Context, w io.Writer) error {
	return ui.El("div", nil, ui.El("p", attrs("id", "crumb"), ui.Text(l.path)), l.Children).Render(ctx, w)
}

// denyGuard sends the visitor to /about.
type denyGuard struct{}

func (denyGuard) Load(ctx context.Context) error { return Redirect("/about") }

type titledPage struct {
	ui.Live
	title, text string
}

func (p *titledPage) Title() string { return p.title }
func (p *titledPage) Render() ui.Node {
	return ui.El("div", attrs("id", "content"), ui.Text(p.text))
}

type deptPage struct {
	ui.Live
	Dept int
}

func (p *deptPage) Title() string { return fmt.Sprint("Dept ", p.Dept) }
func (p *deptPage) Render() ui.Node {
	return ui.El("div", attrs("id", "content"), ui.Text(fmt.Sprint("dept page ", p.Dept)))
}

func persistApp() *App {
	app := New(Only("none"))
	shell, dept, crumb := LayoutOf[shellLayout](), LayoutOf[deptLayout](), LayoutOf[crumbLayout]()
	app.Live("/depts/{dept}", func() *deptPage { return &deptPage{} }, shell, dept)
	app.Live("/depts/{dept}/people", func() *titledPage { return &titledPage{title: "People", text: "people"} }, shell, dept)
	app.Live("/about", func() *titledPage { return &titledPage{title: "About", text: "about"} }, shell, crumb, ReloadLayout[crumbLayout]())
	app.Live("/help", func() *titledPage { return &titledPage{title: "Help", text: "help"} }, shell, crumb, ReloadLayout[crumbLayout]())
	app.Live("/secret", func() *titledPage { return &titledPage{title: "Secret", text: "secret"} }, shell, GuardOf[denyGuard]())
	return app
}

// TestBrowserPersistentLayouts: navigating between pages sharing layouts
// keeps them — their DOM, scroll, client state and live components — and
// doesn't load them again; the page below them is swapped, titled, focused
// and scrolled to its top.
func TestBrowserPersistentLayouts(t *testing.T) {
	shellLoads.Store(0)
	deptLoads.Store(0)
	crumbLoads.Store(0)
	srv := httptest.NewServer(persistApp().Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/depts/1")
	current := func(id string) string {
		return fmt.Sprintf(`document.querySelector("#%s").getAttribute("aria-current") === "page" && document.querySelector("#%[1]s").classList.contains("on")`, id)
	}
	p.Wait(current("to-d1") + ` && !document.querySelector("#to-d2").hasAttribute("aria-current")`)
	loads0 := shellLoads.Load()
	// a click with no scrolling: the sidebar's scroll is what it keeps
	follow := func(sel string) { p.Eval(fmt.Sprintf(`document.querySelector(%q).click()`, sel), nil) }

	// The sidebar's scroll, client state and live counter; a marker on its node.
	p.Click("#tog")
	p.Wait(`document.querySelector("#tog").getAttribute("aria-pressed") === "true"`)
	p.Click("#count")
	p.WaitText("#count", "count 1")
	p.Eval(`document.querySelector("#side").scrollTop = 200; document.querySelector("#side").marked = true`, nil)
	p.Wait(`document.querySelector("#side").scrollTop === 200`)

	// Into the same department: both layouts stay.
	dept0 := deptLoads.Load()
	follow("#to-people")
	p.Wait(`location.pathname === "/depts/1/people" && document.title === "People"`)
	p.WaitText("#content", "people")
	kept := `document.querySelector("#side").marked === true && document.querySelector("#side").scrollTop === 200 &&
		document.querySelector("#tog").getAttribute("aria-pressed") === "true" && document.querySelector("#count").textContent === "count 1"`
	p.Wait(kept)
	if n := shellLoads.Load(); n != loads0 {
		t.Errorf("the shell loaded again: %d loads, want %d", n, loads0)
	}
	if n := deptLoads.Load(); n != dept0 {
		t.Errorf("the department layout loaded again: %d loads, want %d", n, dept0)
	}
	if got := p.Text("#loads"); got != fmt.Sprint(loads0) {
		t.Errorf("the shell rendered loads %s, want %d", got, loads0)
	}
	p.Wait(current("to-d1"))

	// Another department: the department layout loads again, the shell stays.
	follow("#to-d2")
	p.Wait(`location.pathname === "/depts/2" && document.title === "Dept 2"`)
	p.WaitText("#dept h2", "dept 2")
	p.Wait(kept)
	p.Wait(current("to-d2") + ` && !document.querySelector("#to-d1").hasAttribute("aria-current")`)
	if n := deptLoads.Load(); n != dept0+1 {
		t.Errorf("department layout loads: %d, want %d", n, dept0+1)
	}
	p.Wait(`document.activeElement === document.querySelector("main")`)

	// A layout reading the path loads on every navigation.
	crumb0 := crumbLoads.Load()
	follow("#to-about")
	p.Wait(`location.pathname === "/about"`)
	p.WaitText("#crumb", "/about")
	p.Eval(`vuka.navigate("/help")`, nil)
	p.Wait(`location.pathname === "/help"`)
	p.WaitText("#crumb", "/help")
	p.Wait(kept)
	if n := crumbLoads.Load(); n != crumb0+2 {
		t.Errorf("path-reading layout loads: %d, want %d", n, crumb0+2)
	}
	if n := shellLoads.Load(); n != loads0 {
		t.Errorf("the shell loaded again: %d loads, want %d", n, loads0)
	}

	// Back and forward.
	p.Eval(`history.back()`, nil)
	p.Wait(`location.pathname === "/about" && document.title === "About"`)
	p.WaitText("#crumb", "/about")
	p.Wait(current("to-about"))
	p.Eval(`history.back()`, nil)
	p.Wait(`location.pathname === "/depts/2"`)
	p.WaitText("#content", "dept page 2")
	p.Wait(kept + ` && ` + current("to-d2"))

	// A guard runs on the navigation even though the shell stays: it
	// sends the visitor elsewhere.
	follow("#to-secret")
	p.Wait(`location.pathname === "/about"`)
	p.WaitText("#content", "about")
}

// TestBrowserPrefetch: hovering a link on a page without a live connection
// fetches its page ahead (Sec-Purpose: prefetch); following it opens that
// response, its route run once and its live page's Mount once — the join
// adopts the session the prefetch rendered. A link marked off, or to a
// download, isn't fetched.
func TestBrowserPrefetch(t *testing.T) {
	var gets, prefetches, mounts atomic.Int64
	app := New(Only("none"))
	app.Get("/", func() ui.Node {
		return ui.El("html", nil, ui.El("body", nil,
			ui.Link(ui.LinkProps{Href: "/target", ID: "go", Children: ui.Text("target")}),
			ui.Link(ui.LinkProps{Href: "/other", ID: "off", Prefetch: ui.PrefetchOff, Children: ui.Text("other")}),
			ui.El("a", attrs("id", "dl", "href", "/file", "download", "", "data-vk-prefetch", ""), ui.Text("file")),
			ui.El("button", attrs("type", "button", "onClick", ui.On(ui.JS.AddClass(ui.This, "x"))), ui.Text("js"))))
	})
	count := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/target" || r.URL.Path == "/other" || r.URL.Path == "/file" {
				if prefetching(r) {
					prefetches.Add(1)
				} else if r.Header.Get("X-Vuka-Live") == "" {
					gets.Add(1)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
	app.Use(count)
	app.Live("/target", func() *mountCounter { return &mountCounter{n: &mounts} })
	app.Live("/other", func() *mountCounter { return &mountCounter{n: &mounts} })
	app.Get("/file", func() string { return "file" })
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Visit(srv.URL + "/")
	p.Hover("#off")
	p.Hover("#dl")
	p.Eval(`new Promise((r) => setTimeout(r, 300))`, nil)
	if n := prefetches.Load(); n != 0 {
		t.Fatalf("%d prefetches of links marked off or downloads", n)
	}
	p.Hover("#go")
	deadline := time.Now().Add(5 * time.Second)
	for prefetches.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if prefetches.Load() != 1 {
		t.Fatalf("hovering the link: %d prefetches", prefetches.Load())
	}
	time.Sleep(200 * time.Millisecond) // the response arrives
	p.Click("#go")
	p.Wait(`location.pathname === "/target" && document.documentElement.dataset.vkState === "connected"`)
	if g, m := gets.Load(), mounts.Load(); g != 0 || m != 1 {
		t.Errorf("following a prefetched link: %d more GETs, %d Mounts; want 0 and 1", g, m)
	}
}

type mountCounter struct {
	ui.Live
	n *atomic.Int64
}

func (c *mountCounter) LiveSchema(l *ui.LiveSchema) { l.Mount(c.mount) }
func (c *mountCounter) mount(context.Context) error { c.n.Add(1); return nil }
func (c *mountCounter) Render() ui.Node {
	return ui.El("main", nil, ui.Text("target"))
}
