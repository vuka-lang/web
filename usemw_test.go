package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/vuka/v2/diag"
)

// noteMW is middleware noting its name going in and out of the route.
func noteMW(l *accessLog, name string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			l.note(name)
			next.ServeHTTP(w, r)
			l.note(name + " out")
		})
	}
}

// newNoteMW is the constructor form: func(deps…) Middleware.
func newNoteMW(l *accessLog) Middleware { return noteMW(l, "built") }

// blockMW answers ?block=1 itself, never calling next.
func blockMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("block") == "1" {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "maintenance")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestUseOfOrder(t *testing.T) {
	l := &accessLog{errs: map[int]error{3: Forbidden}}
	app := New(Only("none"), Provide(func() *accessLog { return l }))
	app.Get("/orgs/{org}", newGuardedPage,
		UseOf(noteMW(l, "root mw")), UseOf(noteMW(l, "orgs mw")),
		GuardOf[rootGuard](), LayoutOf[noteLayout](), GuardOf[orgGuard]())
	h := app.Handler()
	expect(t, h, "GET", "/orgs/1", "", 200, "<main><p>org 1</p></main>")
	if got := l.take(); got != "root mw,orgs mw,root,layout,guard 1,mount,orgs mw out,root mw out" {
		t.Errorf("ran %s; want middleware root-first around the guards, layouts and page", got)
	}
	// A refusing guard answers inside the middleware.
	code, _, _ := do(t, h, "GET", "/orgs/3", "")
	if code != 403 {
		t.Errorf("refused page: %d", code)
	}
	if got := l.take(); got != "root mw,orgs mw,root,layout,guard 3,orgs mw out,root mw out" {
		t.Errorf("ran %s on a refusal", got)
	}
}

func TestUseOfConstructor(t *testing.T) {
	l := &accessLog{errs: map[int]error{}}
	app := New(Only("none"), Provide(func() *accessLog { return l }))
	app.Get("/api/orgs/{org}", (*orgAPI).Get, Receiver(newOrgAPI), UseOf(newNoteMW), GuardOf[orgGuard]())
	h := app.Handler()
	expect(t, h, "GET", "/api/orgs/1", "", 200, `{"id":1,"name":"Rex"}`, "Content-Type", "application/json")
	if got := l.take(); got != "built,guard 1,get 1,built out" {
		t.Errorf("ran %s; want the built middleware around the guard and handler", got)
	}
}

func TestUseOfShortCircuit(t *testing.T) {
	l := &accessLog{errs: map[int]error{}}
	app := New(Only("none"), Provide(func() *accessLog { return l }))
	app.Get("/orgs/{org}", newGuardedPage, UseOf(blockMW), UseOf(noteMW(l, "inner")), GuardOf[orgGuard]())
	h := app.Handler()
	code, body, _ := do(t, h, "GET", "/orgs/1?block=1", "")
	if code != 503 || body != "maintenance" {
		t.Errorf("blocked: %d %s", code, body)
	}
	if got := l.take(); got != "" {
		t.Errorf("ran %s behind a middleware that answered itself", got)
	}
	expect(t, h, "GET", "/orgs/1", "", 200, "<p>org 1</p>")
	if got := l.take(); got != "inner,guard 1,mount,inner out" {
		t.Errorf("ran %s without the block", got)
	}
}

func TestUseOfWrapsNotFound(t *testing.T) {
	l := &accessLog{errs: map[int]error{}}
	app := New(Only("none"), Provide(func() *accessLog { return l }))
	app.Get("/orgs/{org}", newGuardedPage, UseOf(noteMW(l, "mw")), GuardOf[orgGuard]())
	h := app.Handler()
	// {org} doesn't parse: the 404 happens inside the middleware, before the guard.
	code, _, _ := do(t, h, "GET", "/orgs/x", "")
	if code != 404 {
		t.Errorf("bad parameter: %d", code)
	}
	if got := l.take(); got != "mw,mw out" {
		t.Errorf("ran %s; want the middleware around the 404, nothing else", got)
	}
}

func TestUseOfLivePage(t *testing.T) {
	var runs atomic.Int64
	count := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			runs.Add(1)
			next.ServeHTTP(w, r)
		})
	}
	l := &accessLog{errs: map[int]error{}}
	app := New(Only("none"), Provide(func() *accessLog { return l }))
	app.Live("/live/{org}", newGuardedLive, UseOf(count), GuardOf[orgGuard]())
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	html, tok := pageToken(t, srv, "/live/1")
	if tok == "" || !strings.Contains(html, "live 1") {
		t.Fatalf("live page: %s", html)
	}
	if runs.Load() != 1 {
		t.Fatalf("GET ran the middleware %d times", runs.Load())
	}
	c, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1})
	m := must(t, c)
	if m.Type != live.Render || m.Ref != 1 {
		t.Fatalf("join: %+v", m)
	}
	if runs.Load() != 2 {
		t.Errorf("the join ran the middleware %d times in all; want 2", runs.Load())
	}
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, m.HTML, "click", 0), "event": "click"})
	if m := must(t, c); m.Ref != 2 || m.Error != "" {
		t.Fatalf("event: %+v", m)
	}
	if runs.Load() != 2 {
		t.Errorf("a socket event ran the middleware: %d runs", runs.Load())
	}
}

func TestUseOfBuildErrors(t *testing.T) {
	for _, c := range []struct {
		code diag.Code
		opt  RouteOption
		want string
	}{
		{"E0410", UseOf(42), "web.UseOf takes middleware"},
		{"E0410", UseOf(func() {}), "neither middleware nor its constructor"},
		{"E0407", UseOf(newNoteMW), "no provider for *web.accessLog"},
	} {
		a := New(Only("none"))
		a.Get("/x", newFilesPage, c.opt)
		err := a.Build()
		if diag.Of(err) != c.code || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.code, err)
		}
	}
}

// mark is a typed attribute RegisterType puts on a page's type; the gate
// below lets only marked routes through.
type mark struct{ name string }

// markedPage is a stateless folder page whose type carries a registered
// attribute, as vuka.RegisterType[markedPage](…) gives it.
type markedPage struct{ orgParams }

func newMarkedPage() *markedPage { return &markedPage{} }

func (p *markedPage) Render(ctx context.Context, w io.Writer) error {
	_, err := io.WriteString(w, "<p>m</p>")
	return err
}

func (p *markedPage) Post(ctx context.Context) error { return nil }

// unmarkedPage is its control: same shape, no registered attributes.
type unmarkedPage struct{ orgParams }

func newUnmarkedPage() *unmarkedPage { return &unmarkedPage{} }

func (p *unmarkedPage) Render(ctx context.Context, w io.Writer) error {
	_, err := io.WriteString(w, "<p>u</p>")
	return err
}

func (p *unmarkedPage) Post(ctx context.Context) error { return nil }

var registerMarked = sync.OnceFunc(func() { vuka.RegisterType[markedPage](mark{"public"}) })

func TestTypeAttrsReachFolderRoutes(t *testing.T) {
	registerMarked()
	infos := map[string][]any{}
	app := New(Only("none"), WrapRoutes("probe", func(i RouteInfo) Middleware {
		infos[i.Method+" "+i.Path] = i.Attrs
		return nil
	}))
	app.Get("/m/{org}", newMarkedPage, PageRoute(), Attrs(mark{"route"}))
	app.Post("/m/{org}", (*markedPage).Post, Receiver(newMarkedPage))
	app.Get("/u/{org}", newUnmarkedPage, PageRoute())
	app.Get("/plain/{org}", newMarkedPage) // a 1.2 route without page options: unchanged
	if err := app.Build(); err != nil {
		t.Fatal(err)
	}
	marks := func(key string) []string {
		var ms []string
		for _, x := range infos[key] {
			if m, ok := x.(mark); ok {
				ms = append(ms, m.name)
			}
		}
		return ms
	}
	if got := marks("GET /m/{org}"); len(got) != 2 || got[0] != "public" || got[1] != "route" {
		t.Errorf("GET attrs %v; want the type's before the route's own", got)
	}
	if got := marks("POST /m/{org}"); len(got) != 1 || got[0] != "public" {
		t.Errorf("POST attrs %v; want the receiver type's", got)
	}
	if got := marks("GET /u/{org}"); len(got) != 0 {
		t.Errorf("unmarked page attrs %v", got)
	}
	if got := marks("GET /plain/{org}"); len(got) != 0 {
		t.Errorf("a plain route took the type's attrs: %v", got)
	}
}

// TestTypeAttrsGateFolderRoutes is the auth shape: a gate refusing every
// route without the registered mark, so marking a page's type opens its GET
// and its method handlers, as auth.Public would.
func TestTypeAttrsGateFolderRoutes(t *testing.T) {
	registerMarked()
	gate := func(i RouteInfo) Middleware {
		for _, x := range i.Attrs {
			if _, ok := x.(mark); ok {
				return nil
			}
		}
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "forbidden", http.StatusForbidden)
			})
		}
	}
	app := New(Only("none"), WrapRoutes("gate", gate))
	app.Get("/m/{org}", newMarkedPage, PageRoute())
	app.Post("/m/{org}", (*markedPage).Post, Receiver(newMarkedPage))
	app.Get("/u/{org}", newUnmarkedPage, PageRoute())
	app.Post("/u/{org}", (*unmarkedPage).Post, Receiver(newUnmarkedPage))
	h := app.Handler()
	for path, want := range map[string]int{"/m/1": 200, "/u/1": 403} {
		if code, _, _ := do(t, h, "GET", path, ""); code != want {
			t.Errorf("GET %s: %d; want %d", path, code, want)
		}
	}
	for path, want := range map[string]int{"/m/1": 204, "/u/1": 403} {
		if code, _, _ := do(t, h, "POST", path, ""); code != want {
			t.Errorf("POST %s: %d; want %d", path, code, want)
		}
	}
}
