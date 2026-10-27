package web

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/vuka/v2/diag"
)

// This file holds what a library guarding an app's routes builds on — an
// authentication package, say: middleware for each route from its
// declaration's attributes, parameters bound from the request, checks run
// on live connections, the live event a goroutine handles, and notes for the
// development toolbar's Request panel.

// RouteInfo describes a route for the function WrapRoutes registers.
type RouteInfo struct {
	Method, Path string
	Where        string // "main.ShowPet at pages.vuka:24", "live page main.Counter"
	// Attrs are the typed attributes of the route's declaration, after those
	// of its controller's type: the attributes of a @web.Live page's type,
	// or those given with web.Attrs from Go.
	Attrs []any
	Live  bool // a Live page
	// LiveType is a Live page's component type (the struct, not a
	// pointer); nil for other routes.
	LiveType reflect.Type
	// HTML says the route answers with a page (a ui.Node, a view or a Live
	// page): its errors are pages, and a browser may be sent elsewhere.
	HTML bool
	// RootLayout is the route's root layout, the outermost of its
	// web.LayoutOf layouts (the struct type); nil when it has none. Pages
	// under one root layout share a document: in-app navigation between
	// them patches the page, and between two roots is a full page load.
	RootLayout reflect.Type
	// Calls says the route gates each call it serves itself, by the
	// call's declaration (GuardCalls): web.GraphQL's operations and
	// fields. A library gating routes leaves such a route open unless its
	// attributes carry a gate.
	Calls bool
}

// A RouteAttr is a typed attribute that only a WrapRoutes function gives a
// meaning to — a permission a route requires, say. Build fails when a route
// carries one and the app has no WrapRoutes of the name RouteWrapper returns,
// so a route never goes unguarded because the library guarding it wasn't
// installed.
type RouteAttr interface{ RouteWrapper() string }

type routeWrap struct {
	name string
	f    func(RouteInfo) Middleware
}

// WrapRoutes has Build call f with each route of the app (decorated or
// added in code, Live pages included); the middleware f returns, if not
// nil, wraps the route outside its own (@web.Use) middleware and inside the
// app's. Functions added first wrap outermost. name is what a RouteAttr
// names.
func WrapRoutes(name string, f func(RouteInfo) Middleware) Option {
	return OptionFunc(func(a *App) {
		a.mutable("WrapRoutes")
		a.wraps = append(a.wraps, routeWrap{name, f})
	})
}

// Attrs gives a route registered in code typed attributes, as attributes
// written on a decorated handler are: they reach WrapRoutes. A vuka.Bundle
// (a composed decorator, such as one a library's attribute function
// returns) gives its attributes. It also goes to App.Live.
func Attrs(attrs ...any) RouteOption {
	return routeOpt(func(s *routeSpec) { s.attrs = append(s.attrs, flattenAttrs(attrs)...) })
}

func flattenAttrs(attrs []any) []any {
	var out []any
	for _, x := range attrs {
		if b, ok := x.(vuka.Bundle); ok {
			out = append(out, b.Attrs()...)
			continue
		}
		out = append(out, x)
	}
	return out
}

// liveOption makes a route's option one for a live page: Attrs and Wrap
// apply to the page's route.
func (o routeOpt) liveOption(s *liveSpec) { s.routeOpts = append(s.routeOpts, o) }

// ParamFrom binds handler parameters of type T with f, before any other
// rule: f's error answers the request as a handler's would. A library
// gives handlers its own values this way — the signed-in user, say.
func ParamFrom[T any](f func(r *http.Request) (T, error)) Option {
	t := typeOf[T]()
	return OptionFunc(func(a *App) {
		a.mutable("ParamFrom")
		if a.binders == nil {
			a.binders = map[reflect.Type]func(*http.Request) (reflect.Value, error){}
		}
		a.binders[t] = func(r *http.Request) (reflect.Value, error) {
			v, err := f(r)
			return reflect.ValueOf(&v).Elem(), err
		}
	})
}

// Implementation returns the app's value implementing the interface T: the
// one provided as T, else the one provided type implementing it. It is an
// error when none does, or several do. Unlike Resolve it doesn't build the
// app, so a Check may call it.
func Implementation[T any](a *App) (T, error) {
	var zero T
	t := typeOf[T]()
	if t.Kind() != reflect.Interface {
		return zero, diag.New("E0414", "Implementation[%s]: not an interface", t).WithHelp("T names an interface the provided types implement")
	}
	if !a.c.has(t) {
		var found []reflect.Type
		for pt := range a.c.providers {
			if pt.Implements(t) {
				found = append(found, pt)
			}
		}
		switch len(found) {
		case 0:
			return zero, diag.New("E0414", "nothing provided implements %s", t).WithHelp("provide a type implementing it")
		case 1:
			t = found[0]
		default:
			names := make([]string, len(found))
			for i, f := range found {
				names[i] = f.String()
			}
			return zero, diag.New("E0414", "several provided types implement %s: %s", typeOf[T](), strings.Join(names, ", ")).WithHelp("provide one of them as %s", typeOf[T]())
		}
	}
	v, err := a.c.get(t)
	if err != nil {
		return zero, err
	}
	return v.Interface().(T), nil
}

// WriteError answers r with err as a handler's error is answered: its
// status and message (a *web.Error), an HTML page when html is set, JSON
// otherwise, or what OnError writes.
func (a *App) WriteError(w http.ResponseWriter, r *http.Request, err error, html bool) {
	var to Redirect
	if errors.As(err, &to) {
		redirect(w, r, to)
		return
	}
	a.writeError(w, r, err, html)
}

// LiveCheck has each joined live page checked with f every LiveCheckEvery
// (30 seconds by default), given the context the page's route ran with: an
// error closes the connection with code 4001 and the page reloads, its
// route running again for the browser. A page is checked when it mounts by
// its route itself — the join and each navigation run it through the app's
// middleware and gates — and its events and broadcasts trust what it
// mounted with; LiveCheck is the net for what changes while it sits open
// without the app hearing of it (a session deleted by another process). A
// library that ends sessions itself closes their pages at once with
// CloseLive.
func LiveCheck(f func(ctx context.Context) error) Option {
	return OptionFunc(func(a *App) {
		a.mutable("LiveCheck")
		a.liveChecks = append(a.liveChecks, f)
	})
}

// CloseLive closes the app's joined live pages whose context — the one
// their route ran with, as LiveCheck gets it — match reports true for,
// with code 4001: each page reloads, its route running again for the
// browser. It returns how many it closes. A page closes before it handles
// another message or broadcast; one handling a message closes once that
// is done. match runs on the caller's goroutine, once per joined page: a
// library ending a session closes the pages signed in with it this way.
func (a *App) CloseLive(match func(ctx context.Context) bool) int {
	a.hub.mu.Lock()
	conns := make([]*conn, 0, len(a.hub.conns))
	for c := range a.hub.conns {
		conns = append(conns, c)
	}
	a.hub.mu.Unlock()
	n := 0
	for _, c := range conns {
		ctx := c.pageCtx()
		if ctx == nil || c.ending.Load() || !match(ctx) {
			continue
		}
		c.ending.Store(true)
		select {
		case c.wake <- struct{}{}:
		default:
		}
		n++
	}
	return n
}

// LiveHandler describes the handler a live event targets, for a guard
// (GuardLiveEvents): what Vuka's registry knows of its declaration and of
// the component it belongs to.
type LiveHandler struct {
	Name string // "Board.Clear"; "Board.Render.func1" for a function literal
	// Component is the struct type of the component whose event it is; nil
	// for a handler of a page that is no component.
	Component reflect.Type
	// Recv and Method are a method handler's receiver type (Board for
	// *Board) and name; Recv is nil for a type Vuka registered nothing for.
	Recv   reflect.Type
	Method string
	// Attrs are the handler's typed attributes, written on its declaration
	// (vuka.FuncOf); none for a function literal or a function without.
	// The types' own are vuka.TypeAttrs(Component) and
	// vuka.TypeAttrs(Recv).
	Attrs []any
	// Literal says the handler is a function literal (onClick={func() { … }}):
	// it has no attributes, and the methods it calls are checked by their
	// own decorators when it runs.
	Literal bool
}

// GuardLiveEvents has f decide each live event before its handler runs,
// given the page's context (the identity it joined with) and the handler:
// an error refuses the event — the handler doesn't run, and the page
// receives the error as a handler's (vk:error). A library gating events
// reads the handler's attributes and its component's type's here, where
// web dispatches, rather than only inside the handler's call.
func GuardLiveEvents(f func(ctx context.Context, h LiveHandler) error) Option {
	return OptionFunc(func(a *App) {
		a.mutable("GuardLiveEvents")
		a.liveGuards = append(a.liveGuards, f)
	})
}

// liveHandler describes fn, the handler target names, for the guards.
func (c *conn) liveHandler(target string, fn any, name string) LiveHandler {
	h := LiveHandler{Name: name}
	if s, ok := c.s.(interface{ Component(string) ui.Stateful }); ok {
		if comp := s.Component(target); comp != nil {
			h.Component = reflect.TypeOf(comp)
			if h.Component.Kind() == reflect.Pointer {
				h.Component = h.Component.Elem()
			}
		}
	}
	if _, method, _ := strings.Cut(target, ":"); method != "" && (method[0] < '0' || method[0] > '9') {
		// A push by name ("c1:Inc"): the component's method, a value made
		// by reflection, which names no declaration of its own.
		h.Recv, h.Method, h.Attrs = h.Component, method, vuka.MethodAttrs(h.Component, method)
		return h
	}
	ref, err := vuka.FuncOf(fn)
	if err != nil {
		return h
	}
	h.Recv, h.Method, h.Attrs, h.Literal = ref.Recv, ref.Method, ref.Attrs, ref.Literal
	return h
}

// LiveEvent is the live event a goroutine is handling: CurrentLiveEvent
// returns it to the event's handler and to its decorators, which, unlike
// the handler, may have no context.Context to read.
type LiveEvent struct {
	ctx       context.Context
	handler   string
	rendering atomic.Bool // the handler returned, and the page renders
	mu        sync.Mutex
	refused   error
}

// Context is the context the page's route ran with (the join's, or the
// latest navigation's): its request's values, cookies and middleware's.
func (e *LiveEvent) Context() context.Context { return e.ctx }

// Handler names the handler the event targets: "Admin.Delete", or
// "Admin.Render.func1" for a function literal.
func (e *LiveEvent) Handler() string { return e.handler }

// Handling reports whether the event's handler is running — the handler
// itself, or anything it calls, a function literal's calls included — rather
// than the render of what it changed, which follows it on the same
// goroutine. A gate refusing an event checks calls made while Handling.
func (e *LiveEvent) Handling() bool { return !e.rendering.Load() }

// Refuse makes err the event's error, which the page receives as a
// handler's (vk:error); for a decorator that doesn't let a handler without
// an error result run.
func (e *LiveEvent) Refuse(err error) {
	e.mu.Lock()
	if e.refused == nil {
		e.refused = err
	}
	e.mu.Unlock()
}

var (
	liveGoroutines sync.Map     // goroutine id → *conn, while the connection serves on it
	liveConnsN     atomic.Int64 // how many: none, and CurrentLiveEvent costs a load
)

// CurrentLiveEvent is the live event the calling goroutine handles, nil
// outside one: web's connections handle an event's handler, and every
// decorator around it, on the connection's goroutine.
func CurrentLiveEvent() *LiveEvent {
	if liveConnsN.Load() == 0 {
		return nil
	}
	c, _ := liveGoroutines.Load(goid())
	if c, ok := c.(*conn); ok {
		return c.ev
	}
	return nil
}

// LivePage is the context of the live page whose connection the calling
// goroutine serves — the page's route's, carrying its identity — whatever
// it is doing: handling an event, rendering, delivering a broadcast. It is
// nil on any other goroutine, and before the connection's first join.
func LivePage() context.Context {
	if liveConnsN.Load() == 0 {
		return nil
	}
	if c, ok := liveGoroutines.Load(goid()); ok {
		return c.(*conn).pageCtx()
	}
	return nil
}

// LiveOrigin is the context of the live page whose connection started the
// calling goroutine — the page's route's, carrying its identity — when a
// goroutine the connection's own goroutine started calls it: a goroutine an
// event's handler starts (go b.Clear()), or one a decorator runs a call on.
// It is nil on any other goroutine: one started further down (by such a
// goroutine), a pool's worker, a request's, a cron's; and once the page's
// connection has closed. It reads the goroutine's stack (a few
// microseconds): a check that has a context.Context asks it instead.
func LiveOrigin() context.Context {
	if liveConnsN.Load() == 0 {
		return nil
	}
	id := parentGoid()
	if id == 0 {
		return nil
	}
	if c, ok := liveGoroutines.Load(id); ok {
		return c.(*conn).pageCtx()
	}
	return nil
}

// bindConn makes c the connection served on the calling goroutine until
// unbind. The binding is per connection, not per event: the goroutine's id
// costs a stack read, paid once for the connection.
func bindConn(c *conn) (unbind func()) {
	id := goid()
	liveGoroutines.Store(id, c)
	liveConnsN.Add(1)
	return func() {
		liveGoroutines.Delete(id)
		liveConnsN.Add(-1)
	}
}

// IsLiveJoin reports whether ctx is a live page's route running again for
// its connection's join: the page was served already (its GET), and the
// connection rebuilds it — on the first join, and on each reconnect. A
// handler with side effects — sending mail, counting a visit — skips them
// then, so they happen once per visit:
//
//	if !web.IsLiveJoin(ctx) {
//		mailer.Send(ctx, welcome)
//	}
//
// It is false for the GET itself, for a navigation over the connection (a
// visit of its own, replacing an HTTP load), and for the page's events and
// renders after the join, though their context descends from the join's. A
// client can't make it true: web marks its own run, not a header.
//
// The route runs for every join whether or not the join adopts the GET's
// session (LiveAdopt) — middleware, gates and the handler itself, which
// sees IsLiveJoin true either way. Adoption only skips mounting the page's
// components again, so Mount runs once per view when the join arrives in
// time; an expired window, a restart or a reconnect mounts them again.
func IsLiveJoin(ctx context.Context) bool {
	j, ok := ctx.Value(joinKey{}).(*join)
	return ok && j.why == "join" && !j.done.Load()
}

// RequestNote is a row of the development toolbar's Request panel.
type RequestNote struct {
	Section string // the heading it goes under: "Auth"
	Name    string
	Value   string
	Refused bool // shown as a failure
}

// NoteRequest adds a row to the development toolbar's Request panel for the
// request (or live message) of ctx: what a library decided about it — who is
// signed in, which gate let it through. It does nothing for a context that no
// toolbar watches. Never note a secret.
func NoteRequest(ctx context.Context, n RequestNote) {
	if e := entryOf(ctx); e != nil {
		e.mu.Lock()
		e.Facts = append(e.Facts, n)
		e.mu.Unlock()
	}
}
