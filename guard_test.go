package web

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/ui/live"
	"github.com/vuka-lang/vuka/diag"
)

// accessLog says what orgGuard answers for each {org}, and records the
// loads, mounts and handlers that ran.
type accessLog struct {
	mu   sync.Mutex
	errs map[int]error
	ran  []string
}

func (l *accessLog) note(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ran = append(l.ran, s)
}

func (l *accessLog) refuse(org int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs[org] = err
}

func (l *accessLog) take() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := strings.Join(l.ran, ",")
	l.ran = nil
	return s
}

// orgGuard is a parameter folder's guard.
type orgGuard struct{ orgParams }

func (g *orgGuard) Load(ctx context.Context, l *accessLog) error {
	l.note("guard " + itoa(g.Org))
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.errs[g.Org]
}

// rootGuard is an outer folder's guard, without parameters.
type rootGuard struct{}

func (rootGuard) Load(ctx context.Context, l *accessLog) error {
	l.note("root")
	return nil
}

// noteLayout is a layout noting its Load.
type noteLayout struct {
	Children ui.Node
}

func (n *noteLayout) Load(ctx context.Context, l *accessLog) error {
	l.note("layout")
	return nil
}

func (n *noteLayout) Render(ctx context.Context, w io.Writer) error {
	return ui.El("main", nil, n.Children).Render(ctx, w)
}

// guardedPage is a stateless page noting its Mount.
type guardedPage struct {
	orgParams
	log *accessLog
}

func newGuardedPage(l *accessLog) *guardedPage { return &guardedPage{log: l} }

func (p *guardedPage) LiveSchema(l *ui.LiveSchema)   { l.Mount(p.mount) }
func (p *guardedPage) mount(_ context.Context) error { p.log.note("mount"); return nil }

func (p *guardedPage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("p", nil, ui.Text("org "+itoa(p.Org))).Render(ctx, w)
}

// guardedLive is a live page.
type guardedLive struct {
	ui.Live
	orgParams
}

func newGuardedLive() *guardedLive { return &guardedLive{} }

func (p *guardedLive) Inc() {}
func (p *guardedLive) Render() ui.Node {
	return ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Inc)}}, ui.Text("live "+itoa(p.Org)))
}

// orgAPI is an endpoint: method handlers answering JSON.
type orgAPI struct {
	orgParams
	log *accessLog
}

func newOrgAPI(l *accessLog) *orgAPI { return &orgAPI{log: l} }

func (a *orgAPI) Get(ctx context.Context) (Pet, error) {
	a.log.note("get " + itoa(a.Org))
	return Pet{ID: a.Org, Name: "Rex"}, nil
}

type petInput struct {
	Name string `json:"name" validate:"required"`
}

func (a *orgAPI) Post(ctx context.Context, in petInput) (Pet, error) {
	a.log.note("post " + itoa(a.Org))
	return Pet{ID: a.Org, Name: in.Name}, nil
}

func guardApp() (*App, *accessLog) {
	l := &accessLog{errs: map[int]error{2: NotFound, 3: Forbidden, 4: Redirect("/login")}}
	app := New(Only("none"), Provide(func() *accessLog { return l }))
	root, org := GuardOf[rootGuard](), GuardOf[orgGuard]()
	app.Get("/orgs/{org}", newGuardedPage, root, LayoutOf[noteLayout](), org)
	app.Get("/bare/{org}", newGuardedPage, org)
	app.Live("/live/{org}", newGuardedLive, org)
	app.Get("/api/orgs/{org}", (*orgAPI).Get, Receiver(newOrgAPI), root, org)
	app.Post("/api/orgs/{org}", (*orgAPI).Post, Receiver(newOrgAPI), root, org)
	return app, l
}

func TestGuardOrder(t *testing.T) {
	app, l := guardApp()
	h := app.Handler()
	expect(t, h, "GET", "/orgs/1", "", 200, "<main><p>org 1</p></main>")
	if got := l.take(); got != "root,layout,guard 1,mount" {
		t.Errorf("ran %s; want guards and layouts in their options' order, then Mount", got)
	}
	// Guards alone wrap nothing.
	code, body, _ := do(t, h, "GET", "/bare/1", "")
	if code != 200 || body != "<p>org 1</p>" {
		t.Errorf("bare: %d %s", code, body)
	}
}

func TestGuardStopsPages(t *testing.T) {
	app, l := guardApp()
	h := app.Handler()
	for _, c := range []struct {
		path, location string
		code           int
	}{
		{"/orgs/2", "", 404},
		{"/orgs/3", "", 403},
		{"/orgs/4", "/login", 302},
		{"/orgs/x", "", 404},
	} {
		code, _, hdr := do(t, h, "GET", c.path, "")
		if code != c.code || hdr.Get("Location") != c.location {
			t.Errorf("%s: %d to %q; want %d to %q", c.path, code, hdr.Get("Location"), c.code, c.location)
		}
	}
	if got := l.take(); strings.Contains(got, "mount") {
		t.Errorf("ran %s behind a refusing guard", got)
	}
}

func TestGuardLivePage(t *testing.T) {
	app, l := guardApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	html, tok := pageToken(t, srv, "/live/1")
	if tok == "" || !strings.Contains(html, "<!DOCTYPE html>") || !strings.Contains(html, "live 1") {
		t.Fatalf("a guarded live page keeps its document: %s", html)
	}
	expect(t, srv.Config.Handler, "GET", "/live/3", "", 403, "")
	l.take()

	// The join runs the guard again.
	l.refuse(1, Forbidden)
	c, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1})
	m := must(t, c)
	if m.Type != live.Error || !strings.Contains(m.Error, "403") {
		t.Errorf("join of a refused page: %+v", m)
	}
	if got := l.take(); got != "guard 1" {
		t.Errorf("ran %s on the join", got)
	}
}

func TestGuardEndpoint(t *testing.T) {
	app, l := guardApp()
	h := app.Handler()
	code, body, hdr := do(t, h, "GET", "/api/orgs/1", "")
	if code != 200 || !strings.Contains(hdr.Get("Content-Type"), "application/json") || !strings.Contains(body, `{"id":1,"name":"Rex"}`) {
		t.Fatalf("get: %d %v %s", code, hdr, body)
	}
	expect(t, h, "POST", "/api/orgs/1", `{"name":"Tom"}`, 200, `{"id":1,"name":"Tom"}`, "Content-Type", "application/json")
	if got := l.take(); got != "root,guard 1,get 1,root,guard 1,post 1" {
		t.Errorf("ran %s", got)
	}
	for _, m := range []string{"GET", "POST"} {
		for _, c := range []struct {
			path, want, location string
			code                 int
		}{
			{"/api/orgs/2", `"not found"`, "", 404},
			{"/api/orgs/3", `"forbidden"`, "", 403},
			{"/api/orgs/4", "", "/login", map[string]int{"GET": 302, "POST": 303}[m]},
			{"/api/orgs/x", `"not found"`, "", 404},
		} {
			code, body, hdr := do(t, h, m, c.path, `{"name":"Tom"}`, "Content-Type", "application/json")
			if code != c.code || !strings.Contains(body, c.want) || hdr.Get("Location") != c.location ||
				c.code != 303 && c.code != 302 && !strings.Contains(hdr.Get("Content-Type"), "application/json") {
				t.Errorf("%s %s: %d %v %s; want %d %s", m, c.path, code, hdr, body, c.code, c.want)
			}
		}
	}
	if got := l.take(); strings.Contains(got, "get") || strings.Contains(got, "post") {
		t.Errorf("handlers ran behind a refusing guard: %s", got)
	}
}

type loadless struct{}

type guardNeeds struct{}

func (guardNeeds) Load(ctx context.Context, d *applicantDB) error { return nil }

type guardBadLoad struct{}

func (guardBadLoad) Load() error { return nil }

func TestGuardBuildErrors(t *testing.T) {
	for _, c := range []struct {
		code diag.Code
		opt  RouteOption
		want string
	}{
		{"E0321", GuardOf[int](), "web.GuardOf[int]: a guard is a struct"},
		{"E0321", GuardOf[loadless](), "has no Load method"},
		{"E0321", GuardOf[guardBadLoad](), "its Load is"},
		{"E0407", GuardOf[guardNeeds](), "no provider for *web.applicantDB"},
	} {
		a := New(Only("none"))
		a.Get("/x", newFilesPage, c.opt)
		err := a.Build()
		if diag.Of(err) != c.code || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.code, err)
		}
	}
}
