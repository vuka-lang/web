package web

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka/diag"
)

// orgStore is the folder pages' data.
type orgStore struct{ names map[int]string }

// orgParams is a parameter folder's Params: {org}, at least 1.
type orgParams struct {
	Org int `validate:"min=1"`
}

// permitParams is a nested parameter folder's: {org}/permits/{permit}.
type permitParams struct {
	orgParams
	Permit int
}

// siteLayout is the outermost layout: the document, titled by the page.
type siteLayout struct{ Children ui.Node }

func (l siteLayout) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil,
		ui.El("head", nil, ui.El("title", nil, ui.Text(ui.PageTitle(ctx)))),
		ui.El("body", nil, ui.El("div", []ui.Attr{{Name: "id", Value: "site"}}, l.Children))).Render(ctx, w)
}

// orgLayout is a parameter folder's layout, loading what its {org} names.
type orgLayout struct {
	orgParams
	Children ui.Node
	name     string
}

func (l *orgLayout) Load(ctx context.Context, s *orgStore) error {
	n, ok := s.names[l.Org]
	if !ok {
		return NotFound
	}
	l.name = n
	return nil
}

func (l *orgLayout) Render(ctx context.Context, w io.Writer) error {
	return ui.El("section", []ui.Attr{{Name: "data-org", Value: l.name}}, l.Children).Render(ctx, w)
}

// orgIndex is a stateless page: built by its constructor, mounted once its
// path parameters are bound.
type orgIndex struct {
	orgParams
	store *orgStore
	name  string
}

func newOrgIndex(s *orgStore) *orgIndex { return &orgIndex{store: s} }

func (p *orgIndex) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }
func (p *orgIndex) mount(_ context.Context) error {
	p.name = p.store.names[p.Org]
	return nil
}

func (p *orgIndex) Title() string { return "Org " + p.name }

func (p *orgIndex) Render(ctx context.Context, w io.Writer) error {
	return ui.El("p", nil, ui.Text(fmt.Sprint("org ", p.Org, " ", p.name))).Render(ctx, w)
}

// permitPage is a live page whose parameters come from two folders.
type permitPage struct {
	ui.Live
	permitParams
	n int
}

func newPermitPage() *permitPage { return &permitPage{} }

func (p *permitPage) LiveSchema(l *ui.LiveSchema)   { l.Mount(p.mount) }
func (p *permitPage) mount(_ context.Context) error { p.n = p.Permit * 10; return nil }
func (p *permitPage) Inc()                          { p.n++ }
func (p *permitPage) Title() string                 { return "Permit " + itoa(p.Permit) }
func (p *permitPage) Render() ui.Node {
	return ui.El("div", nil, ui.El("span", nil, ui.Text(fmt.Sprint("permit ", p.Org, "/", p.Permit, " n=", p.n))),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Inc)}}, ui.Text("+")))
}

// filesPage is a Rest folder's page: {path...}.
type filesPage struct{ Path Rest }

func newFilesPage() *filesPage { return &filesPage{} }

func (p *filesPage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("pre", nil, ui.Text(string(p.Path))).Render(ctx, w)
}

// orgEdit is a page with method handlers, its receiver built per request.
type orgEdit struct {
	*orgParams
	store *orgStore
}

type renameForm struct {
	Name string `form:"name" validate:"required"`
}

func newOrgEdit(s *orgStore) *orgEdit { return &orgEdit{store: s} }

func (e *orgEdit) Render(ctx context.Context, w io.Writer) error {
	return ui.El("form", nil, ui.Text(e.store.names[e.Org])).Render(ctx, w)
}

func (e *orgEdit) Post(ctx context.Context, f renameForm) error {
	e.store.names[e.Org] = f.Name
	return Redirect("/orgs/" + itoa(e.Org))
}

func (e *orgEdit) Delete(ctx context.Context) (ui.Node, error) {
	return ui.El("p", nil, ui.Text(fmt.Sprint("deleted ", e.Org))), nil
}

func folderApp(opts ...Option) *App {
	s := &orgStore{names: map[int]string{1: "Acme", 2: "Globex"}}
	app := New(append([]Option{Only("none"), Provide(func() *orgStore { return s })}, opts...)...)
	site, org := LayoutOf[siteLayout](), LayoutOf[orgLayout]()
	app.Get("/orgs/{org}", newOrgIndex, site, org)
	app.Live("/orgs/{org}/permits/{permit}", newPermitPage, site, org)
	app.Get("/orgs/{org}/edit", newOrgEdit, PageRoute(), site, org)
	app.Post("/orgs/{org}/edit", (*orgEdit).Post, Receiver(newOrgEdit), site, org)
	app.Delete("/orgs/{org}/edit", (*orgEdit).Delete, Receiver(newOrgEdit), site, org)
	app.Get("/files/{path...}", newFilesPage, PageRoute())
	return app
}

func TestFolderStatelessPage(t *testing.T) {
	h := folderApp().Handler()
	code, body, _ := do(t, h, "GET", "/orgs/1", "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	want := `<title>Org Acme</title></head><body><div id="site"><section data-org="Acme"><p>org 1 Acme</p></section></div></body>`
	if !strings.Contains(body, want) {
		t.Errorf("page:\n%s\nwant %s", body, want)
	}
	expect(t, h, "GET", "/orgs/2/edit", "", 200, `<section data-org="Globex"><form>Globex</form></section>`)
	for _, path := range []string{"/orgs/x", "/orgs/0", "/orgs/-1", "/orgs/9", "/orgs/x/edit"} {
		expect(t, h, "GET", path, "", 404, "")
	}
}

func TestFolderLivePage(t *testing.T) {
	srv := httptest.NewServer(folderApp().Handler())
	defer srv.Close()
	html, tok := pageToken(t, srv, "/orgs/1/permits/3")
	if tok == "" {
		t.Fatalf("not a live page: %s", html)
	}
	want := `<title>Permit 3</title></head><body><div id="site"><section data-org="Acme"><div data-vk-id="c1"><span>permit 1/3 n=30</span>`
	if !strings.Contains(html, want) {
		t.Fatalf("page:\n%s\nwant %s", html, want)
	}
	c, joinHTML := joined(t, srv, "/orgs/2/permits/4")
	defer c.CloseNow()
	if !strings.Contains(joinHTML, `<section data-org="Globex">`) || !strings.Contains(joinHTML, "n=40") {
		t.Fatalf("join: %s", joinHTML)
	}
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, joinHTML, "click", 0), "event": "click"})
	m := must(t, c)
	if len(m.Updates) != 1 || m.Updates[0].ID != "c1" || !strings.Contains(m.Updates[0].HTML, "n=41") {
		t.Errorf("patch: %+v", m)
	}
	h := srv.Config.Handler
	for _, path := range []string{"/orgs/1/permits/x", "/orgs/x/permits/1", "/orgs/0/permits/1", "/orgs/9/permits/1"} {
		expect(t, h, "GET", path, "", 404, "")
	}
}

func TestFolderRest(t *testing.T) {
	h := folderApp().Handler()
	expect(t, h, "GET", "/files/a/b/c.txt", "", 200, "<pre>a/b/c.txt</pre>")
	expect(t, h, "GET", "/files/", "", 200, "<pre></pre>")
}

func TestFolderMethodHandlers(t *testing.T) {
	app := folderApp()
	h := app.Handler()
	code, _, hdr := do(t, h, "POST", "/orgs/1/edit", "name=Initech", "Content-Type", "application/x-www-form-urlencoded")
	if code != 303 || hdr.Get("Location") != "/orgs/1" {
		t.Fatalf("post: %d %v", code, hdr)
	}
	expect(t, h, "GET", "/orgs/1", "", 200, "<title>Org Initech</title>")
	expect(t, h, "POST", "/orgs/1/edit", "name=", 422, "", "Content-Type", "application/x-www-form-urlencoded")
	expect(t, h, "POST", "/orgs/x/edit", "name=A", 404, "", "Content-Type", "application/x-www-form-urlencoded")
	// A whole page is wrapped in the layouts, a fragment isn't.
	expect(t, h, "DELETE", "/orgs/2/edit", "", 200, `<section data-org="Globex"><p>deleted 2</p></section>`, "Accept", "text/html")
	code, body, _ := do(t, h, "DELETE", "/orgs/1/edit", "")
	if code != 200 || body != "<p>deleted 1</p>" {
		t.Errorf("fragment: %d %s", code, body)
	}
}

type badLayout struct{ Children ui.Node }

func (l *badLayout) Load(ctx context.Context, d *applicantDB) error { return nil }
func (l *badLayout) Render(ctx context.Context, w io.Writer) error  { return nil }

type childless struct{}

func (childless) Render(context.Context, io.Writer) error { return nil }

func TestFolderBuildErrors(t *testing.T) {
	for _, c := range []struct {
		code diag.Code
		reg  func(*App)
		want string
	}{
		{"E0321", func(a *App) { a.Get("/x", newFilesPage, LayoutOf[int]()) }, "a layout is a struct"},
		{"E0321", func(a *App) { a.Get("/x", newFilesPage, LayoutOf[childless]()) }, "no field Children ui.Node"},
		{"E0407", func(a *App) { a.Get("/x", newFilesPage, LayoutOf[badLayout]()) }, "no provider for *web.applicantDB"},
		{"E0410", func(a *App) { a.Post("/x/{org}", (*orgEdit).Post, Receiver(newOrgIndex)) }, "builds a *web.orgIndex"},
		{"E0328", func(a *App) {
			a.Get("/x/{bad}", func() *badParam { return &badParam{} }, PageRoute())
		}, "nope"},
	} {
		a := New(Only("none"), Provide(func() *orgStore { return &orgStore{} }))
		c.reg(a)
		err := a.Build()
		if diag.Of(err) != c.code || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.code, err)
		}
	}
}

type badParam struct {
	Bad int `validate:"nope"`
}

func (*badParam) Render(context.Context, io.Writer) error { return nil }

// Routes without the folder forms keep a path parameter that doesn't parse
// a 400, and a Get of a constructor its handler.
func TestFolderFormsAreOptIn(t *testing.T) {
	app := New(Only("none"))
	app.Live("/n/{start}", &counter{})
	app.Get("/f/{path...}", newFilesPage)
	h := app.Handler()
	expect(t, h, "GET", "/n/x", "", 400, "")
	expect(t, h, "GET", "/f/a/b", "", 200, "<pre></pre>")
}

// guardStore says what guardLayout's Load answers for each {org}, and
// records the handlers that ran.
type guardStore struct {
	errs map[int]error
	ran  []string
}

type guardLayout struct {
	orgParams
	Children ui.Node
}

func (l *guardLayout) Load(ctx context.Context, s *guardStore) error { return s.errs[l.Org] }

func (l *guardLayout) Render(ctx context.Context, w io.Writer) error {
	return ui.El("main", nil, l.Children).Render(ctx, w)
}

type orgAction struct {
	orgParams
	store *guardStore
}

func newOrgAction(s *guardStore) *orgAction { return &orgAction{store: s} }

func (g *orgAction) Render(context.Context, io.Writer) error { return nil }

func (g *orgAction) Post(ctx context.Context, f renameForm) error {
	g.store.ran = append(g.store.ran, "post "+itoa(g.Org)+" "+f.Name)
	return Redirect("/done")
}

func (g *orgAction) Delete(ctx context.Context) error {
	g.store.ran = append(g.store.ran, "delete "+itoa(g.Org))
	return nil
}

// A route's layouts load before its method handler, whatever it answers:
// a Load error answers instead, the handler not run.
func TestFolderLayoutsGuardMethodHandlers(t *testing.T) {
	s := &guardStore{errs: map[int]error{2: NotFound, 3: Forbidden, 4: Redirect("/login")}}
	app := New(Only("none"), Provide(func() *guardStore { return s }))
	app.Post("/g/{org}", (*orgAction).Post, Receiver(newOrgAction), LayoutOf[guardLayout]())
	app.Delete("/g/{org}", (*orgAction).Delete, Receiver(newOrgAction), LayoutOf[guardLayout]())
	h := app.Handler()
	form := []string{"Content-Type", "application/x-www-form-urlencoded"}
	for _, c := range []struct {
		org      string
		code     int
		location string
	}{
		{"2", 404, ""},
		{"3", 403, ""},
		{"4", 303, "/login"},
	} {
		for _, m := range []string{"POST", "DELETE"} {
			code, body, hdr := do(t, h, m, "/g/"+c.org, "name=x", form...)
			if code != c.code || hdr.Get("Location") != c.location {
				t.Errorf("%s /g/%s: %d %v %s; want %d to %q", m, c.org, code, hdr.Get("Location"), body, c.code, c.location)
			}
		}
	}
	if len(s.ran) != 0 {
		t.Fatalf("handlers ran behind a failing Load: %v", s.ran)
	}
	code, _, hdr := do(t, h, "POST", "/g/1", "name=x", form...)
	if code != 303 || hdr.Get("Location") != "/done" {
		t.Errorf("post: %d %v", code, hdr)
	}
	if code, _, _ := do(t, h, "DELETE", "/g/1", ""); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if strings.Join(s.ran, ",") != "post 1 x,delete 1" {
		t.Errorf("ran %v", s.ran)
	}
}

// orgGET is a folder's method func, GET in api/orgs/[org].vuka: its scalar
// binds the segment of its name.
func orgGET(ctx context.Context, org int, s *orgStore) (string, error) {
	n, ok := s.names[org]
	if !ok {
		return "", NotFound
	}
	return n, nil
}

// TestFolderMethodFuncs: vuka's pages/route.go registers a method func on
// the app with its parameters' names; a segment that doesn't parse names
// no record, 404, as a page's does, while a hand-written route's scalar
// that doesn't parse stays 400.
func TestFolderMethodFuncs(t *testing.T) {
	routes := OptionFunc(func(app *App) {
		app.Get("/api/orgs/{org}", orgGET, PageRoute(), Params("ctx", "org", "s"))
	})
	app := folderApp(routes)
	app.Get("/plain/{org}", orgGET, Params("ctx", "org", "s"))
	h := app.Handler()
	expect(t, h, "GET", "/api/orgs/2", "", 200, `"Globex"`)
	expect(t, h, "GET", "/api/orgs/9", "", 404, "")
	expect(t, h, "GET", "/api/orgs/x", "", 404, "")
	expect(t, h, "GET", "/plain/x", "", 400, "")
}
