// Package web is a web framework for Vuka: routes, views and services are
// declared with decorators, and web.New gathers them into an app.
//
//	@web.Get("/pets/{id}")
//	@web.Template("pet.html")
//	func ShowPet(id int, store *PetStore) Result[Pet] { … }
//
//	func main() { web.New().Run(":8080") }
//
// Everything a decorator declares is a call of the Routes registry, and an
// App's methods of the same names register on it alone:
//
//	app.Get("/pets/{id}", ShowPet)
package web

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sync"

	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/vuka/v2/diag"
)

// Middleware wraps an http.Handler.
type Middleware = func(http.Handler) http.Handler

type declRoute struct {
	method, path string
	decl         *vuka.Decl
	opts         *routeOpts // registered with options (Registry, App): nil for a decorated handler's
}

type declView struct {
	file vuka.File
	name string
}

type declController struct {
	prefix     string
	typ        *vuka.Type
	own        bool
	registered bool // by Registry or App: its type's attributes are read at Build
}

type registry struct {
	routes      []declRoute
	views       map[string]declView
	uses        map[string][]Middleware
	controllers []declController
	lives       []*liveSpec
	pages       []typePage
	crons       []declCron
	errs        []error // the registrations' mistakes, which Build reports
}

// Registry holds what an app is made of: routes, live pages and
// controllers. Routes is the package's, filled at init by the declarations
// of every package the program links; New gives each app a copy, and an
// App's methods of the same names add to its own:
//
//	func init() {
//		web.Routes.Get("/pets/{id}", ShowPet)
//		web.Routes.Live("/board", NewBoard)
//	}
//
//	app.Get("/pets/{id}", ShowPet)
//
// A handler registered from Go binds its scalar parameters to the path's
// segments in order; one the compiler registers binds them by name, which
// it records (vuka.RegisterParams), from the path or the query. Services
// are the app's: app.Service in main.
//
// It is what the decorators are: @web.Get("/pets/{id}") on ShowPet is the
// call above.
type Registry struct {
	mu sync.Mutex
	r  registry
}

// Routes is the registry of the program's declared routes, pages and
// controllers.
var Routes = &Registry{r: registry{views: map[string]declView{}, uses: map[string][]Middleware{}}}

func (*Registry) vukaRegistry() {}

func (g *Registry) record(f func(*registry)) *Registry {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(&g.r)
	return g
}

// Handle registers fn for method and path: a handler, its parameters filled
// by type (see inject.go), or the constructor of an @ui.Island struct (a
// page backed by its island).
func (g *Registry) Handle(method, path string, fn any, opts ...RouteOption) *Registry {
	return g.record(func(r *registry) { r.handle(method, path, fn, opts, false) })
}

func (g *Registry) Get(path string, fn any, opts ...RouteOption) *Registry {
	return g.Handle(http.MethodGet, path, fn, opts...)
}
func (g *Registry) Post(path string, fn any, opts ...RouteOption) *Registry {
	return g.Handle(http.MethodPost, path, fn, opts...)
}
func (g *Registry) Put(path string, fn any, opts ...RouteOption) *Registry {
	return g.Handle(http.MethodPut, path, fn, opts...)
}
func (g *Registry) Patch(path string, fn any, opts ...RouteOption) *Registry {
	return g.Handle(http.MethodPatch, path, fn, opts...)
}
func (g *Registry) Delete(path string, fn any, opts ...RouteOption) *Registry {
	return g.Handle(http.MethodDelete, path, fn, opts...)
}

// Live serves a stateful component as a page at path (see the Live
// decorator): page is its constructor, NewBoard, which builds each page's
// component, its parameters of provided types injected and the others
// zero. With ui.Island among opts it is a page backed by its island.
func (g *Registry) Live(path string, page any, opts ...RouteOption) *Registry {
	return g.record(func(r *registry) { r.live(path, page, opts, false) })
}

// Controller makes the struct ctor builds a service whose route methods
// (their method expressions registered: (*PetAdmin).Index) are served
// under prefix.
func (g *Registry) Controller(prefix string, ctor any, opts ...RouteOption) *Registry {
	return g.record(func(r *registry) { r.controller(prefix, ctor, opts, false) })
}

func declKey(d *vuka.Decl) string {
	return fmt.Sprintf("%s\x00%s\x00%s:%d", d.Pkg, d.Name, d.File, d.Line)
}

func record(f func(*registry)) { Routes.record(f) }

func snapshot() registry {
	Routes.mu.Lock()
	defer Routes.mu.Unlock()
	reg := &Routes.r
	s := registry{
		routes:      append([]declRoute(nil), reg.routes...),
		controllers: append([]declController(nil), reg.controllers...),
		lives:       append([]*liveSpec(nil), reg.lives...),
		pages:       append([]typePage(nil), reg.pages...),
		crons:       append([]declCron(nil), reg.crons...),
		errs:        append([]error(nil), reg.errs...),
		views:       make(map[string]declView, len(reg.views)),
		uses:        make(map[string][]Middleware, len(reg.uses)),
	}
	for k, v := range reg.views {
		s.views[k] = v
	}
	for k, v := range reg.uses {
		s.uses[k] = append([]Middleware(nil), v...)
	}
	return s
}

// Handle declares a route for method and path: @web.Handle("OPTIONS", "/x").
// Path segments in braces bind parameters of the same name: "/pets/{id}";
// "{rest...}" matches the rest of the path.
//
// On a type — an @ui.Island struct, with @web.Get — it declares a
// stateless page backed by that component (see "Pages backed by a
// component" in the islands guide): its exported fields are the
// component's props, its exported methods what the component calls. A live
// one (embedding ui.Live) is declared with @web.Live.
func Handle(method, path string) vuka.Bundle {
	return vuka.Dual(func(d *vuka.Decl) {
		record(func(r *registry) { r.routes = append(r.routes, declRoute{method: method, path: path, decl: d}) })
	}, func(t *vuka.Type) {
		record(func(r *registry) { r.pages = append(r.pages, typePage{method, path, t, reflect.Value{}}) })
	})
}

func Get(path string) vuka.Bundle    { return Handle(http.MethodGet, path) }
func Post(path string) vuka.Bundle   { return Handle(http.MethodPost, path) }
func Put(path string) vuka.Bundle    { return Handle(http.MethodPut, path) }
func Patch(path string) vuka.Bundle  { return Handle(http.MethodPatch, path) }
func Delete(path string) vuka.Bundle { return Handle(http.MethodDelete, path) }

// Template renders a route's result with a view: a .templ file of the same
// package (its one exported component, or the one named) or an html/template
// file (its main template, or the one named). The order of @web.Template and
// the route's method decorator doesn't matter; from Go it is an option:
// app.Get("/pets/{id}", ShowPet, web.Template(petView)).
func Template(f vuka.File, name ...string) Templates {
	v := declView{file: f}
	if len(name) > 0 {
		v.name = name[0]
	}
	return func(d *vuka.Decl) {
		if sink, ok := d.Func.(*declView); ok {
			*sink = v
			return
		}
		record(func(r *registry) { r.views[declKey(d)] = v })
	}
}

// Templates is what Template returns: a declarer giving a route its view,
// and an option doing so.
type Templates func(*vuka.Decl)

// Attrs makes the view a route's option.
func (t Templates) Attrs() []any { return []any{t} }

// Uses is what Use returns: a declarer adding middleware to one route, a
// route's option doing so, and an Option adding it to the whole app.
type Uses func(*vuka.Decl)

// Attrs makes the middleware a route's option.
func (u Uses) Attrs() []any { return []any{u} }

func (u Uses) middleware() []Middleware {
	var mw []Middleware
	u(&vuka.Decl{Func: &mw})
	return mw
}

// Use adds HTTP middleware, the first given outermost: @web.Use(mw) on a
// route, or web.New(web.Use(mw)) for every request of the app.
func Use(mw ...Middleware) Uses {
	return func(d *vuka.Decl) {
		if sink, ok := d.Func.(*[]Middleware); ok {
			*sink = append(*sink, mw...)
			return
		}
		record(func(r *registry) { k := declKey(d); r.uses[k] = append(r.uses[k], mw...) })
	}
}

func (u Uses) apply(a *App) { a.mw = append(a.mw, u.middleware()...) }

// Controller makes a struct a service whose route methods are served under
// prefix.
func Controller(prefix string) func(*vuka.Type) {
	return func(t *vuka.Type) {
		if t.New == nil {
			panic(diag.New("E0418", "@web.Controller on %s, which isn't a struct", t.Name).WithHelp("put it on a struct type"))
		}
		record(func(r *registry) { r.controllers = append(r.controllers, declController{prefix: prefix, typ: t}) })
	}
}

// Option configures an App.
type Option interface{ apply(*App) }

// OptionFunc is an Option written as a function of the App, for libraries
// defining their own options: it runs inside New, before Build, and may call
// any of the App's methods (Provide, Use, Handle, OnStart, Check, …).
type OptionFunc func(*App)

func (f OptionFunc) apply(a *App) { f(a) }

// Options is one Option applying opts in order; nil options are skipped.
func Options(opts ...Option) Option {
	return OptionFunc(func(a *App) {
		for _, o := range opts {
			if o != nil {
				o.apply(a)
			}
		}
	})
}

// Provide adds constructors to the app's container: func(deps…) T or
// func(deps…) (T, error). A value that isn't a function is provided as is.
func Provide(ctors ...any) Option { return OptionFunc(func(a *App) { a.Provide(ctors...) }) }

// Eager provides constructors whose values are built and started by Start
// even when no route needs them (workers, schedulers).
func Eager(ctors ...any) Option { return OptionFunc(func(a *App) { a.Eager(ctors...) }) }

// OnStart adds a hook Start runs; see App.OnStart.
func OnStart(fn func(context.Context) error) Option {
	return OptionFunc(func(a *App) { a.OnStart(fn) })
}

// OnStop adds a hook Stop runs; see App.OnStop.
func OnStop(fn func(context.Context) error) Option { return OptionFunc(func(a *App) { a.OnStop(fn) }) }

// Check adds a check Build runs; see App.Check.
func Check(fn func(*App) error) Option { return OptionFunc(func(a *App) { a.Check(fn) }) }

// Only limits the declared routes and controllers an app takes to
// those of the given packages (import paths; "main" for the main package).
func Only(pkgs ...string) Option {
	return OptionFunc(func(a *App) {
		a.only = map[string]bool{}
		for _, p := range pkgs {
			a.only[p] = true
		}
	})
}

// Dev turns on development where the process runs in it: VUKA_DEV=1 or
// VUKA_ENV=development (`VUKA_DEV=1 go run .`). Responses then show
// internal errors' messages and the toolbar is on, and Start runs
// tailwindcss --watch and each frontend's Vite dev server, when installed,
// and, for an app registering islands, vuka build-islands -watch (the vuka
// command a tool of the module or on PATH), the app serving through them;
// Stop ends them. Without the tools the app serves the last build and logs
// one line saying how to get them. Under vuka run, which runs them itself,
// it starts nothing. In a test binary it turns on development without
// starting anything; elsewhere — a production binary — it does nothing, so
// main can keep it.
func Dev() Option { return OptionFunc(func(a *App) { a.devTools = true }) }

// WithRouter serves the app's routes with r instead of net/http's ServeMux.
func WithRouter(r Router) Option { return OptionFunc(func(a *App) { a.router = r }) }

// OnError replaces the error page and JSON error body.
func OnError(f func(w http.ResponseWriter, r *http.Request, e *Error, html bool)) Option {
	return OptionFunc(func(a *App) { a.onError = f })
}

func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }
