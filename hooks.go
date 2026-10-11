package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/vuka-lang/vuka"
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
	// HTML says the route answers with a page (a ui.Node, a view or a Live
	// page): its errors are pages, and a browser may be sent elsewhere.
	HTML bool
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
	return func(s *routeSpec) { s.attrs = append(s.attrs, flattenAttrs(attrs)...) }
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

// liveOption makes a RouteOption one for App.Live: Attrs and Wrap apply to
// the page's route.
func (o RouteOption) liveOption(s *liveSpec) { s.routeOpts = append(s.routeOpts, o) }

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
		return zero, fmt.Errorf("web: Implementation[%s]: not an interface", t)
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
			return zero, fmt.Errorf("web: nothing provided implements %s", t)
		case 1:
			t = found[0]
		default:
			names := make([]string, len(found))
			for i, f := range found {
				names[i] = f.String()
			}
			return zero, fmt.Errorf("web: several provided types implement %s: %s", typeOf[T](), strings.Join(names, ", "))
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

// LiveCheck has every message of a live connection after its join checked
// with f, given the context the page's route ran with: an error closes the
// connection with code 4001 and the page reloads, its route running again
// for the browser. A library that ends sessions closes their pages this
// way.
func LiveCheck(f func(ctx context.Context) error) Option {
	return OptionFunc(func(a *App) {
		a.mutable("LiveCheck")
		a.liveChecks = append(a.liveChecks, f)
	})
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
	liveEvents  sync.Map // goroutine id → *LiveEvent
	liveEventsN atomic.Int64
)

// CurrentLiveEvent is the live event the calling goroutine handles, nil
// outside one: web's connections handle an event's handler, and every
// decorator around it, on one goroutine.
func CurrentLiveEvent() *LiveEvent {
	if liveEventsN.Load() == 0 {
		return nil
	}
	e, _ := liveEvents.Load(goid())
	ev, _ := e.(*LiveEvent)
	return ev
}

// bindLiveEvent makes e the goroutine's live event until unbind, which
// restores the one bound before, as bindGoroutine does the toolbar's sink.
//
// The binding is by goroutine because the gate needing it is a call
// decorator of the handler: a method the handler's attributes reach only
// from inside the call (Vuka registers no method's attributes for web to
// read where it dispatches, and a component type's only when a type
// decorator takes it), which may have no context.Context, and which the live
// session calls with the page's context, the same for an event as for Render
// or a call from Go. Production pays one runtime.Stack per event.
func bindLiveEvent(e *LiveEvent) (unbind func()) {
	id := goid()
	prev, had := liveEvents.Swap(id, e)
	liveEventsN.Add(1)
	return func() {
		if had {
			liveEvents.Store(id, prev)
		} else {
			liveEvents.Delete(id)
		}
		liveEventsN.Add(-1)
	}
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
