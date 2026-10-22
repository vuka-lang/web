package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka/diag"
)

// The hooks and handlers of a live page take what they need by type: the
// context, the event's form, the request, the app's ParamFrom types and
// its services.

type injBooks struct{ list []string }

func newInjBooks() *injBooks { return &injBooks{list: []string{"Dune"}} }

type injUser struct{ Name string }

func injUserFrom(r *http.Request) (*injUser, error) {
	return &injUser{Name: strings.ToUpper(r.URL.Query().Get("as"))}, nil
}

type bookForm struct {
	Title string `form:"title" validate:"required"`
}

type injShelf struct {
	ui.Live
	shown string
}

func (s *injShelf) LiveSchema(l *ui.LiveSchema) { l.Mount(s.mount) }

func (s *injShelf) mount(ctx context.Context, b *injBooks, r *http.Request, u *injUser) error {
	s.shown = strings.Join(b.list, ",") + " for " + u.Name + " at " + r.URL.Path
	return nil
}

func (s *injShelf) Add(ctx context.Context, f bookForm, b *injBooks, u *injUser) error {
	b.list = append(b.list, f.Title)
	s.shown = strings.Join(b.list, ",") + " by " + u.Name
	return nil
}

func (s *injShelf) Render() ui.Node {
	return ui.El("form", []ui.Attr{{Name: "onSubmit", Value: ui.On(s.Add)}}, ui.El("p", nil, ui.Text(s.shown)))
}

type injGuard struct{}

func (g *injGuard) Load(ctx context.Context, r *http.Request, u *injUser, b *injBooks) error {
	if u.Name == "" {
		return Forbidden
	}
	return nil
}

func injApp() *App {
	a := New(Only("none"), ParamFrom(injUserFrom))
	a.Service(newInjBooks)
	a.Live("/shelf", &injShelf{})
	a.Post("/form", func(v url.Values, r *http.Request) string { return v.Get("a") + r.Method })
	a.Get("/guarded", func() string { return "in" }, GuardOf[injGuard]())
	return a
}

func TestInjectedLivePage(t *testing.T) {
	srv := httptest.NewServer(injApp().Handler())
	defer srv.Close()
	html, _ := pageToken(t, srv, "/shelf?as=ada")
	if !strings.Contains(html, "<p>Dune for ADA at /shelf</p>") {
		t.Fatalf("the mount hook's parameters: %s", html)
	}
	c, html := joined(t, srv, "/shelf?as=ada")
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "submit", 0), "event": "submit", "form": map[string][]string{"title": {"Emma"}}})
	if m := must(t, c); m.Error != "" || len(m.Updates) != 1 || !strings.Contains(m.Updates[0].HTML, "<p>Dune,Emma by ADA</p>") {
		t.Fatalf("the handler's parameters: %+v", m)
	}
	send(t, c, map[string]any{"type": "event", "ref": 3, "target": handlerOf(t, html, "submit", 0), "event": "submit", "form": map[string][]string{}})
	if m := must(t, c); m.Error == "" {
		t.Errorf("an invalid form: %+v", m)
	}
}

func TestInjectedRoutes(t *testing.T) {
	h := injApp().Handler()
	expect(t, h, "POST", "/form?a=1", "", 200, `"1POST"`)
	expect(t, h, "POST", "/form", "a=2", 200, `"2POST"`, "Content-Type", "application/x-www-form-urlencoded")
	expect(t, h, "GET", "/guarded?as=x", "", 200, `"in"`)
	expect(t, h, "GET", "/guarded", "", 403, "")
}

type injDB struct{ *sql.DB }

type badShelf struct {
	ui.Live
}

func (s *badShelf) LiveSchema(l *ui.LiveSchema) { l.Mount(s.mount) }

func (s *badShelf) mount(ctx context.Context, n chan int) error { return nil }

func (s *badShelf) Add(f bookForm, db *sql.DB) {}

func (s *badShelf) Pick(id int, book *injBooks) {}

func (s *badShelf) Render() ui.Node { return nil }

// A parameter nothing fills is a Build error at its method's place.
func TestInjectedBuildErrors(t *testing.T) {
	a := New(Only("none"))
	a.Live("/bad", &badShelf{})
	err := a.Build()
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"error[E0405]: GET /bad (live page web.badShelf): badShelf.mount's 2nd parameter, chan int, can't come from a hook's call, which gives the context\n --> inject_test.go:",
		"= help: a hook takes the context (OnChange: and the ui.Changed set) and what the app provides",
		"error[E0407]: GET /bad (live page web.badShelf): badShelf.Add's 2nd parameter, *sql.DB, is not provided\n --> inject_test.go:",
		"= help: register it in main with app.Service(NewX), or web.Provide(NewX)",
		"badShelf.Pick's 2nd parameter, *web.injBooks, is not provided",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	if diag.Of(err) != "E0405" {
		t.Errorf("code %q", diag.Of(err))
	}
	if err := New(Only("none")).Service(newInjBooks).Live("/bad", &badShelf{}).Build(); strings.Contains(err.Error(), "Pick") {
		t.Errorf("a provided type is reported: %v", err)
	}
}
