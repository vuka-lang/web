package web

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/vuka-lang/ui"
)

type onewayPage struct{ Title string }

func (p *onewayPage) Render(ctx context.Context, w io.Writer) error {
	_, err := io.WriteString(w, p.Title)
	return err
}

// TestDecoratedPage: a route decorator on the constructor of a struct that
// renders declares a page outside pages/ (E0720); one on a function — data
// or a ui.Node it builds — declares a handler.
func TestDecoratedPage(t *testing.T) {
	p := pkgOf(t)
	route(Get("/about/{id}"), decl(p, "newAbout", func() *onewayPage { return &onewayPage{} }))
	route(Post("/fragment"), decl(p, "fragment", func() ui.Node { return ui.Text("ok") }))
	route(Get("/api/n"), decl(p, "count", func() int { return 1 }))
	err := New(Only(p)).Build()
	if err == nil {
		t.Fatal("built")
	}
	msg := err.Error()
	for _, want := range []string{"E0720", "@web.Get on main.newAbout (its struct onewayPage renders) declares a page outside pages/", "pages/about/[id].vuka.html"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "fragment") || strings.Contains(msg, "count") {
		t.Errorf("a handler is reported:\n%s", msg)
	}
}

func TestPageFile(t *testing.T) {
	for path, want := range map[string]string{"/": "pages/index.vuka.html", "/pets/{id}": "pages/pets/[id].vuka.html", "/files/{path...}": "pages/files/[path...].vuka.html"} {
		if got := pageFile(path); got != want {
			t.Errorf("pageFile(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestFolderIslandPage: a stateless island page under pages/ is registered
// as pages/route.go does, its folder's layout around it and its calls
// served.
func TestFolderIslandPage(t *testing.T) {
	db := &applicantDB{names: []string{"Ada", "Alan", "Grace"}}
	h := New(Only("none"), Provide(func() *applicantDB { return db })).
		Get("/applicants", func() *applicants { return &applicants{} }, PageRoute(), LayoutOf[siteLayout]()).Handler()
	code, body, _ := do(t, h, "GET", "/applicants?q=a", "")
	if code != 200 || !strings.Contains(body, `<div id="site">`) || !strings.Contains(body, `data-vk-island="applicants"`) {
		t.Fatalf("%d %s", code, body)
	}
	props, raw, sig := rendered(t, body)
	if props["q"] != "a" {
		t.Errorf("props %v", props)
	}
	expect(t, h, "POST", "/applicants", callBody("search", []any{"gr"}, raw, sig), 200, `\"q\":\"gr\"`, "Content-Type", "application/json")
}

// onewayDeny is a guard refusing every request.
type onewayDeny struct{}

func (onewayDeny) Load(ctx context.Context) error { return Forbidden }

// TestFolderIslandPageGuard: a folder's guard checks the island page's
// calls as it does its GET.
func TestFolderIslandPageGuard(t *testing.T) {
	db := &applicantDB{names: []string{"Ada"}}
	open := New(Only("none"), Provide(func() *applicantDB { return db })).
		Get("/applicants", func() *applicants { return &applicants{} }, PageRoute()).Handler()
	_, body, _ := do(t, open, "GET", "/applicants?q=a", "")
	_, raw, sig := rendered(t, body)
	h := New(Only("none"), Provide(func() *applicantDB { return db })).
		Get("/applicants", func() *applicants { return &applicants{} }, PageRoute(), GuardOf[onewayDeny]()).Handler()
	if code, _, _ := do(t, h, "GET", "/applicants?q=a", ""); code != 403 {
		t.Errorf("GET: %d", code)
	}
	if code, _, _ := do(t, h, "POST", "/applicants", callBody("search", []any{"gr"}, raw, sig), "Content-Type", "application/json"); code != 403 {
		t.Errorf("call: %d", code)
	}
}
