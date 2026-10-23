package web

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2/diag"
)

// A folder page built with &T{} gets its unexported fields of provided
// types injected, as a page-backed island does: on its GET, as a method
// handler's receiver, as a Live page, and in its layouts.

type pdStore struct{ names map[int]string }

type pdGreeter interface{ Greet(string) string }

type pdHello struct{}

func (pdHello) Greet(n string) string { return "hello " + n }

type pdUnprovided struct{}

type pdPage struct {
	Org     int
	store   *pdStore
	greeter pdGreeter
	own     *pdUnprovided // not provided: the page's own, left nil
	count   ui.State[int] // client state: never touched
	shown   string
}

func (p *pdPage) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }

func (p *pdPage) mount(ctx context.Context) error {
	p.shown = p.greeter.Greet(p.store.names[p.Org])
	if p.own != nil {
		p.shown += " (own set)"
	}
	return nil
}

func (p *pdPage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("p", nil, ui.Text(p.shown)).Render(ctx, w)
}

type pdRename struct {
	Name string `form:"name"`
}

func (p *pdPage) Post(ctx context.Context, f pdRename) (string, error) {
	p.store.names[p.Org] = f.Name
	return p.greeter.Greet(p.store.names[p.Org]), nil
}

type pdLive struct {
	ui.Live
	Org   int
	store *pdStore
	shown string
}

func (p *pdLive) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }

func (p *pdLive) mount(ctx context.Context) error {
	p.shown = "live " + p.store.names[p.Org]
	return nil
}

func (p *pdLive) Render() ui.Node { return ui.El("p", nil, ui.Text(p.shown)) }

type pdLayout struct {
	Children ui.Node
	greeter  pdGreeter
	title    string
}

func (l *pdLayout) Load(ctx context.Context) error {
	l.title = l.greeter.Greet("layout")
	return nil
}

func (l *pdLayout) Render(ctx context.Context, w io.Writer) error {
	return ui.El("main", nil, ui.El("h1", nil, ui.Text(l.title)), l.Children).Render(ctx, w)
}

func pdApp() *App {
	s := &pdStore{names: map[int]string{1: "acme"}}
	a := New(Only("none"), Provide(func() *pdStore { return s }), Provide(func() pdGreeter { return pdHello{} }))
	a.Get("/orgs/{org}", func() *pdPage { return &pdPage{} }, PageRoute(), LayoutOf[pdLayout]())
	a.Post("/orgs/{org}", (*pdPage).Post, Receiver(func() *pdPage { return &pdPage{} }))
	a.Live("/live/{org}", func() *pdLive { return &pdLive{} }, PageRoute())
	return a
}

func TestPageDependencyFields(t *testing.T) {
	h := pdApp().Handler()
	expect(t, h, "GET", "/orgs/1", "", 200, "<main><h1>hello layout</h1><p>hello acme</p></main>")
	expect(t, h, "POST", "/orgs/1", "name=globex", 200, `"hello globex"`, "Content-Type", "application/x-www-form-urlencoded")
	expect(t, h, "GET", "/orgs/1", "", 200, "<p>hello globex</p>")
	srv := httptest.NewServer(h)
	defer srv.Close()
	if html, _ := pageToken(t, srv, "/live/1"); !strings.Contains(html, ">live globex</p>") {
		t.Errorf("a Live folder page: %s", html)
	}
}

type pdPathDep struct {
	store *pdStore
}

func (p *pdPathDep) Render(ctx context.Context, w io.Writer) error { return nil }

type pdQueryDep struct {
	store *pdStore `url:"s"`
}

func (p *pdQueryDep) Render(ctx context.Context, w io.Writer) error { return nil }

// A dependency field the URL would bind too is a Build error.
func TestPageDependencyFieldBoundFromURL(t *testing.T) {
	for _, c := range []struct {
		name string
		reg  func(a *App)
		want string
	}{
		{"path", func(a *App) { a.Get("/x/{store}", func() *pdPathDep { return &pdPathDep{} }, PageRoute()) }, "pdPathDep.store, a *web.pdStore, is a dependency the app provides, and the path parameter {store} binds it too"},
		{"query", func(a *App) { a.Get("/y", func() *pdQueryDep { return &pdQueryDep{} }, PageRoute()) }, "pdQueryDep.store, a *web.pdStore, is a dependency the app provides, and the query parameter s binds it too"},
	} {
		a := New(Only("none"), Provide(func() *pdStore { return &pdStore{} }))
		c.reg(a)
		err := a.Build()
		if err == nil || !strings.Contains(err.Error(), c.want) || diag.Of(err) != "E0419" {
			t.Errorf("%s: want E0419 %q, got %v", c.name, c.want, err)
		}
	}
	// Not provided, the same fields are the page's own.
	a := New(Only("none"))
	a.Get("/x/{store}", func() *pdPathDep { return &pdPathDep{} }, PageRoute())
	if err := a.Build(); err != nil {
		t.Errorf("an unprovided field: %v", err)
	}
}
