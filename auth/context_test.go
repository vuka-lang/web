package auth_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/auth"
	"github.com/vuka-lang/web/v2/auth/authtest"
)

func TestPermsHas(t *testing.T) {
	p := auth.Perms{"orders.*", "users.read"}
	for perm, want := range map[string]bool{"orders.write": true, "orders": false, "users.read": true, "users.write": false} {
		if got := p.Has(perm); got != want {
			t.Errorf("Has(%q) = %v", perm, got)
		}
	}
	if !(auth.Perms{"*"}).Has("anything") || (auth.Perms(nil)).Has("x") {
		t.Error("wildcard or nil")
	}
}

func TestContextVars(t *testing.T) {
	v, err := auth.Context(authtest.Context(context.Background(), &bob))
	if err != nil || v.User == nil || v.User.ID != "2" || !v.Perms.Has("orders.read") || v.Actor != nil {
		t.Errorf("Context = %+v, %v", v, err)
	}
	if v, _ := auth.Context(context.Background()); v.User != nil || v.Perms != nil {
		t.Errorf("anonymous: %+v", v)
	}
}

var canRenders atomic.Int64

// canWrite shows whether the visitor may write orders, from the variable
// perms; its state is all Assigns (none), so only a change of what it
// read renders it again.
type canWrite struct{ ui.Live }

func (c *canWrite) Render() ui.Node {
	canRenders.Add(1)
	return ui.El("p", nil, ui.NodeFunc(func(ctx context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, "can write: %v", web.Var[auth.Perms](ctx, "perms").Has("orders.write"))
		return err
	}))
}

type permsPage struct{ ui.Live }

func (p *permsPage) Poke() {}
func (p *permsPage) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Poke)}}, ui.Text("poke")),
		ui.Component("can", nil, &canWrite{}))
}

// TestPermissionVersionLive: once the app changes a user's perms and says
// so (Refresh), a live page showing them renders that spot again at its
// next event, without reloading.
func TestPermissionVersionLive(t *testing.T) {
	defer func(v bool) { live.Verify = v }(live.Verify)
	live.Verify = false
	u := users()
	app := newApp(t, u, nil, web.ContextProcessors(auth.Context))
	app.Live("/perms", &permsPage{})
	c := serve(t, app)
	c.login("bob@example.com", "hunter22", "")
	ws, m := c.socket("/perms", c.srv.URL)
	if !strings.Contains(m.HTML, "can write: false") {
		t.Fatalf("join: %+v", m)
	}
	poke := handler(t, m.HTML, 0)
	before := canRenders.Load()
	if r := click(t, ws, poke, 2); r.Error != "" || len(r.Updates) != 0 {
		t.Fatalf("nothing changed: %+v", r)
	}
	if canRenders.Load() != before {
		t.Error("the spot rendered again though nothing it read changed")
	}
	u.Add("bob@example.com", "hunter22", auth.Identity{ID: "2", Perms: []string{"orders.*"}})
	a, _ := web.Resolve[*auth.Auth](app)
	v := a.Version("2")
	a.Refresh("2")
	if a.Version("2") == v {
		t.Fatal("Refresh didn't move the version")
	}
	r := click(t, ws, poke, 3)
	if r.Error != "" || !strings.Contains(patched(r), "can write: true") {
		t.Fatalf("after Refresh: %+v", r)
	}
	if r := click(t, ws, poke, 4); r.Type != live.PatchMsg {
		t.Errorf("the page closed: %+v", r)
	}
}

type meVars struct{ Me string }

func meContext(ctx context.Context) (meVars, error) {
	if id := auth.Current(ctx); id != nil {
		return meVars{Me: "user " + id.ID}, nil
	}
	return meVars{}, nil
}

// TestAuthRequiredVars: a public page reading a variable of a processor
// wrapped in web.Auth(p, auth.Required) sends an anonymous visitor to the
// login page.
func TestAuthRequiredVars(t *testing.T) {
	app := newApp(t, users(), nil, web.ContextProcessors(web.Auth(meContext, auth.Required)))
	app.Get("/me", func(ctx context.Context) ui.Node { return ui.Text("me: " + web.Var[string](ctx, "me")) }, web.Attrs(auth.Public()))
	c := serve(t, app)
	if r := c.page("/me"); r.status != 302 || r.header.Get("Location") != "/login?next=%2Fme" {
		t.Errorf("anonymous: %d %v %s", r.status, r.header, r.body)
	}
	c.login("bob@example.com", "hunter22", "")
	if r := c.page("/me"); r.status != 200 || r.body != "me: user 2" {
		t.Errorf("signed in: %d %q", r.status, r.body)
	}
}

// TestImpersonate: an admin views the app as another user, Actor naming
// them, and stops.
func TestImpersonate(t *testing.T) {
	app := newApp(t, users(), func(c *auth.Config) { c.ImpersonatePerm = "admin" }, web.ContextProcessors(auth.Context))
	app.Post("/as/{uid}", func(ctx context.Context, uid string) error { return auth.Impersonate(ctx, uid) })
	app.Post("/stop", func(ctx context.Context) error { return auth.StopImpersonating(ctx) })
	app.Get("/vars", func(ctx context.Context) ui.Node {
		v := web.Var[*auth.Identity](ctx, "user")
		a := web.Var[*auth.Identity](ctx, "actor")
		s := "user=" + v.ID
		if a != nil {
			s += " actor=" + a.ID
		}
		return ui.Text(s)
	})
	c := serve(t, app)
	c.login("bob@example.com", "hunter22", "")
	if r := c.do("POST", "/as/1", "", "Origin", c.srv.URL); r.status != 403 {
		t.Errorf("bob impersonating: %d %s", r.status, r.body)
	}
	c.login("ada@example.com", "correct horse", "")
	if r := c.do("POST", "/as/2", "", "Origin", c.srv.URL); r.status >= 300 {
		t.Fatalf("ada impersonating: %d %s", r.status, r.body)
	}
	if r := c.do("GET", "/vars", ""); r.body != "user=2 actor=1" {
		t.Errorf("impersonating: %q", r.body)
	}
	if r := c.do("POST", "/as/1", "", "Origin", c.srv.URL); r.status != 409 {
		t.Errorf("nested: %d", r.status)
	}
	if r := c.do("POST", "/stop", "", "Origin", c.srv.URL); r.status >= 300 {
		t.Fatalf("stop: %d %s", r.status, r.body)
	}
	if r := c.do("GET", "/vars", ""); r.body != "user=1" {
		t.Errorf("stopped: %q", r.body)
	}
}

// TestBrowserAuthNeedsPersonal: auth's processor never reaches the
// browser unless Personal.
func TestBrowserAuthNeedsPersonal(t *testing.T) {
	err := newApp(t, users(), nil, web.ContextProcessors(web.Browser(auth.Context))).Build()
	if err == nil || !strings.Contains(err.Error(), "web.Browser(auth.Context)") {
		t.Errorf("Build: %v", err)
	}
	if err := newApp(t, users(), nil, web.ContextProcessors(web.Browser(web.Personal(auth.Context)))).Build(); err != nil {
		t.Errorf("Personal: %v", err)
	}
}

// TestBrowserRefusesWhatReadsAuth: a Browser processor that reads the
// signed-in identity at run time sends the browser nothing, unless
// Personal.
func TestBrowserRefusesWhatReadsAuth(t *testing.T) {
	for _, personal := range []bool{false, true} {
		p := web.Browser(meContext)
		if personal {
			p = web.Browser(web.Personal(meContext))
		}
		app := newApp(t, users(), nil, web.ContextProcessors(p))
		app.Get("/vars", func(ctx context.Context) ui.Node { return ui.Text(fmt.Sprint(ui.BrowserVars(ctx))) }, web.Attrs(auth.Public()))
		c := serve(t, app)
		c.login("bob@example.com", "hunter22", "")
		want := "map[]"
		if personal {
			want = "map[me:user 2]"
		}
		if r := c.page("/vars"); r.body != want {
			t.Errorf("personal %v: %q, want %q", personal, r.body, want)
		}
	}
}
