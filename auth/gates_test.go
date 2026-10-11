package auth_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/ui/live"
	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/web"
	"github.com/vuka-lang/web/auth"
)

// vault is what Vuka generates for a live page whose gated methods are
// reached through every shape of handler:
//
//	@auth.Requires("admin") func (v *vault) Clear()
//	@auth.Requires("admin") func (v *vault) ClearCtx(ctx context.Context) error
//	@elsewhere @auth.Requires("admin") func (v *vault) Far()
//
// elsewhere runs the rest of the call on another goroutine. Its init
// registers what the generated code does (vuka.RegisterFunc).
type vault struct{ ui.Live }

// locked is a component gated on its type: @auth.Requires("admin").
type locked struct {
	ui.Live
	n int
}

var (
	gateRuns atomic.Int64              // gated bodies run
	gateDone = make(chan struct{}, 64) // a shape's call returned, on whatever goroutine

	vaultClear    = vuka.Func{Name: "vault.Clear", ErrIndex: -1, CtxIndex: -1, Zero: func() []any { return nil }, Attrs: adminOnly.Attrs()}
	vaultClearCtx = vuka.Func{Name: "vault.ClearCtx", ErrIndex: 0, CtxIndex: 0, Zero: func() []any { return []any{error(nil)} }, Attrs: adminOnly.Attrs()}
	vaultFar      = vuka.Func{Name: "vault.Far", ErrIndex: -1, CtxIndex: -1, Zero: func() []any { return nil }, Attrs: adminOnly.Attrs()}
)

func init() {
	vuka.RegisterFunc((*vault).Clear, adminOnly.Attrs()...)
	vuka.RegisterFunc((*vault).ClearCtx, adminOnly.Attrs()...)
	vuka.RegisterFunc((*vault).Far, adminOnly.Attrs()...)
	vuka.RegisterType[locked](adminOnly.Attrs()...)
	live.AllowPush("github.com/vuka-lang/web/auth_test.vault", "Clear")
}

func elsewhere(c *vuka.Call) {
	done := make(chan struct{})
	go func() { c.Next(); close(done) }()
	<-done
}

func (v *vault) Clear() {
	vuka.NewCall(&vaultClear, adminOnly.Calls(), v, []any{}, func(*vuka.Call) []any { gateRuns.Add(1); return nil }).Run()
}

func (v *vault) ClearCtx(ctx context.Context) error {
	c := vuka.NewCall(&vaultClearCtx, adminOnly.Calls(), v, []any{ctx}, func(*vuka.Call) []any { gateRuns.Add(1); return []any{error(nil)} })
	c.Run()
	err, _ := c.Results[0].(error)
	return err
}

func (v *vault) Far() {
	vuka.NewCall(&vaultFar, append([]vuka.Decorator{elsewhere}, adminOnly.Calls()...), v, []any{}, func(*vuka.Call) []any { gateRuns.Add(1); return nil }).Run()
}

func signal() { gateDone <- struct{}{} }

// The handlers, by shape.
func (v *vault) Spawn()                       { go func() { v.Clear(); signal() }() }
func (v *vault) SpawnCtx(ctx context.Context) { go func() { v.ClearCtx(ctx); signal() }() }
func (v *vault) SpawnBackground()             { go func() { v.ClearCtx(context.Background()); signal() }() }
func (v *vault) SpawnDeeper()                 { go func() { go func() { v.Clear(); signal() }() }() }

func (v *vault) Render() ui.Node {
	on := func(fn any) []ui.Attr { return []ui.Attr{{Name: "onClick", Value: ui.On(fn)}} }
	return ui.El("main", nil,
		ui.El("button", on(v.Clear)),               // 0 a gated method
		ui.El("button", on(v.ClearCtx)),            // 1 a gated method taking the page's context
		ui.El("button", on(func() { v.Clear() })),  // 2 a function literal calling it
		ui.El("button", on(func() { v.Far() })),    // 3 … through a decorator moving to another goroutine
		ui.El("button", on(v.Spawn)),               // 4 a goroutine the handler starts, no context
		ui.El("button", on(v.SpawnCtx)),            // 5 … given the page's context
		ui.El("button", on(v.SpawnBackground)),     // 6 … given context.Background()
		ui.El("button", on(v.SpawnDeeper)),         // 7 a goroutine a goroutine of the handler starts
		ui.Component("vault.locked", 0, &locked{})) // its button: 8
}

func (l *locked) Render() ui.Node {
	return ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(func() { l.n++; gateRuns.Add(1) })}}, ui.Text(fmt.Sprint(l.n)))
}

type shape struct {
	name  string
	i     int
	async bool // the handler returns before the gated call: wait for it
	// deeper: no live page attributes the call — refused by default
	// (strict gates), run under LaxGates (the documented limit).
	deeper bool
	// elsewhere: refused on another goroutine, the event's reply carries no
	// error (the call's caller gets it).
	elsewhere bool
}

var shapes = []shape{
	{"method handler", 0, false, false, false},
	{"method handler with ctx", 1, false, false, false},
	{"function literal", 2, false, false, false},
	{"decorator on another goroutine", 3, false, false, true},
	{"goroutine, no ctx", 4, true, false, false},
	{"goroutine, page ctx", 5, true, false, false},
	{"goroutine, Background ctx", 6, true, false, false},
	{"goroutine of a goroutine", 7, true, true, false},
	{"type-gated component, function literal", 8, false, false, false},
}

func TestGateShapes(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprint("strict=", strict), func(t *testing.T) {
			defer auth.SetStrictGates(true)
			app := newApp(t, users(), func(c *auth.Config) { c.LaxGates = !strict })
			app.Live("/vault", &vault{})
			for _, who := range []struct{ login, pw string }{{"bob@example.com", "hunter22"}, {"ada@example.com", "correct horse"}} {
				admin := who.login == "ada@example.com"
				c := serve(t, app)
				c.login(who.login, who.pw, "")
				ws, m := c.socket("/vault", c.srv.URL)
				ref := 2
				try := func(name, target string, async, deeper, elsewhere bool) {
					t.Helper()
					before := gateRuns.Load()
					r := click(t, ws, target, ref)
					ref++
					if async {
						select {
						case <-gateDone:
						case <-time.After(5 * time.Second):
							t.Fatalf("%s: the goroutine never returned", name)
						}
					}
					ran := gateRuns.Load() > before
					want := admin && !(strict && deeper) || deeper && !strict
					if ran != want {
						t.Errorf("%s as %s: ran %v, want %v (reply %+v)", name, who.login, ran, want, r)
					}
					if !async && !elsewhere && !admin && r.Error != "forbidden" {
						t.Errorf("%s as %s: reply %q, want forbidden", name, who.login, r.Error)
					}
				}
				for _, s := range shapes {
					try(s.name, handler(t, m.HTML, s.i), s.async, s.deeper, s.elsewhere)
				}
				// A push by name reaches the method by reflection: the
				// registry has its gate all the same.
				id, _, _ := strings.Cut(handler(t, m.HTML, 0), ":")
				try("push by name", id+":Clear", false, false, false)
			}
		})
	}
}

// Outside any request, a gated call needs an identity or auth.System —
// unless LaxGates, when it is the app's own work.
func TestGatesOutsideEvents(t *testing.T) {
	defer auth.SetStrictGates(true)
	v := &vault{}
	ctx := context.Background()
	run := func(name string, f func() error, want bool, wantErr error) {
		t.Helper()
		before := gateRuns.Load()
		err := f()
		if ran := gateRuns.Load() > before; ran != want || wantErr != nil && !errorsIs(err, wantErr) {
			t.Errorf("%s: ran %v, err %v", name, ran, err)
		}
	}
	plain := func() error { v.Clear(); return nil }
	inGoroutine := func() error {
		done := make(chan struct{})
		go func() { v.Clear(); close(done) }() // a cron's or a job's goroutine
		<-done
		return nil
	}
	withCtx := func(ctx context.Context) func() error { return func() error { return v.ClearCtx(ctx) } }
	bobCtx := auth.WithIdentity(ctx, &bob)
	adaCtx := auth.WithIdentity(ctx, &auth.Identity{ID: "1", Perms: []string{"admin"}})

	auth.SetStrictGates(false)
	run("lax: a plain call", plain, true, nil)
	run("lax: a call on a goroutine", inGoroutine, true, nil)
	run("lax: a call with a background context", withCtx(ctx), true, nil)
	run("lax: a call as bob", withCtx(bobCtx), false, web.Forbidden)
	run("lax: a call as an admin", withCtx(adaCtx), true, nil)

	auth.SetStrictGates(true)
	run("a plain call", plain, false, nil)
	run("a call on a goroutine", inGoroutine, false, nil)
	run("a background context", withCtx(ctx), false, web.Unauthorized)
	run("auth.System", withCtx(auth.System(ctx)), true, nil)
	run("as bob", withCtx(bobCtx), false, web.Forbidden)
	run("as an admin", withCtx(adaCtx), true, nil)
}

// A gated method given a request's context checks its identity, whatever
// the route's own gate.
func TestGatesInRequests(t *testing.T) {
	app := newApp(t, users(), nil)
	app.Get("/vault/clear", func(ctx context.Context) error { return (&vault{}).ClearCtx(ctx) }, web.Params("ctx"), web.Attrs(auth.Public()))
	c := serve(t, app)
	if r := c.do("GET", "/vault/clear", ""); r.status != 401 {
		t.Errorf("anonymous: %d", r.status)
	}
	c.login("bob@example.com", "hunter22", "")
	if r := c.do("GET", "/vault/clear", ""); r.status != 403 {
		t.Errorf("bob: %d", r.status)
	}
	c.login("ada@example.com", "correct horse", "")
	if r := c.do("GET", "/vault/clear", ""); r.status != 204 {
		t.Errorf("an admin: %d %s", r.status, r.body)
	}
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
