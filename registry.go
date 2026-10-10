// Package web is a web framework for Vuka: routes, views and services are
// declared with decorators, and web.New gathers them into an app.
//
//	@web.Get("/pets/{id}")
//	@web.Template("pet.html")
//	func ShowPet(id int, store *PetStore) Result[Pet] { … }
//
//	func main() { web.New().Run(":8080") }
//
// Everything a decorator declares can also be registered from plain Go
// (App.Get, App.Provide, …); see the README.
package web

import (
	"fmt"
	"net/http"
	"reflect"
	"sync"

	"github.com/vuka-lang/vuka"
)

// Middleware wraps an http.Handler.
type Middleware = func(http.Handler) http.Handler

type declRoute struct {
	method, path string
	decl         *vuka.Decl
}

type declView struct {
	file vuka.File
	name string
}

type declController struct {
	prefix string
	typ    *vuka.Type
}

type registry struct {
	routes      []declRoute
	views       map[string]declView
	uses        map[string][]Middleware
	services    []*vuka.Type
	controllers []declController
}

var (
	regMu sync.Mutex
	reg   = registry{views: map[string]declView{}, uses: map[string][]Middleware{}}
)

func declKey(d *vuka.Decl) string {
	return fmt.Sprintf("%s\x00%s\x00%s:%d", d.Pkg, d.Name, d.File, d.Line)
}

func record(f func(*registry)) {
	regMu.Lock()
	defer regMu.Unlock()
	f(&reg)
}

func snapshot() registry {
	regMu.Lock()
	defer regMu.Unlock()
	s := registry{
		routes:      append([]declRoute(nil), reg.routes...),
		services:    append([]*vuka.Type(nil), reg.services...),
		controllers: append([]declController(nil), reg.controllers...),
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
func Handle(method, path string) func(*vuka.Decl) {
	return func(d *vuka.Decl) {
		record(func(r *registry) { r.routes = append(r.routes, declRoute{method, path, d}) })
	}
}

func Get(path string) func(*vuka.Decl)    { return Handle(http.MethodGet, path) }
func Post(path string) func(*vuka.Decl)   { return Handle(http.MethodPost, path) }
func Put(path string) func(*vuka.Decl)    { return Handle(http.MethodPut, path) }
func Patch(path string) func(*vuka.Decl)  { return Handle(http.MethodPatch, path) }
func Delete(path string) func(*vuka.Decl) { return Handle(http.MethodDelete, path) }

// Template renders a route's result with a view: a .templ file of the same
// package (its one exported component, or the one named) or an html/template
// file (its main template, or the one named). The order of @web.Template and
// the route's method decorator doesn't matter.
func Template(f vuka.File, name ...string) func(*vuka.Decl) {
	v := declView{file: f}
	if len(name) > 0 {
		v.name = name[0]
	}
	return func(d *vuka.Decl) { record(func(r *registry) { r.views[declKey(d)] = v }) }
}

// Uses is what Use returns: a declarer adding middleware to one route, and an
// Option adding it to the whole app.
type Uses func(*vuka.Decl)

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

func (u Uses) apply(a *App) {
	var mw []Middleware
	u(&vuka.Decl{Func: &mw})
	a.mw = append(a.mw, mw...)
}

// Service makes a struct a dependency: a lazy singleton built from its fields,
// each injected by type.
func Service(t *vuka.Type) {
	if t.New == nil {
		panic(fmt.Sprintf("web: @web.Service needs a struct; %s isn't one", t.Name))
	}
	record(func(r *registry) { r.services = append(r.services, t) })
}

// Controller makes a struct a service whose route methods are served under
// prefix.
func Controller(prefix string) func(*vuka.Type) {
	return func(t *vuka.Type) {
		if t.New == nil {
			panic(fmt.Sprintf("web: @web.Controller needs a struct; %s isn't one", t.Name))
		}
		record(func(r *registry) { r.controllers = append(r.controllers, declController{prefix, t}) })
	}
}

// Option configures an App.
type Option interface{ apply(*App) }

type optionFunc func(*App)

func (f optionFunc) apply(a *App) { f(a) }

// Provide adds constructors to the app's container: func(deps…) T or
// func(deps…) (T, error). A value that isn't a function is provided as is.
func Provide(ctors ...any) Option { return optionFunc(func(a *App) { a.Provide(ctors...) }) }

// Only limits the declared routes, services and controllers an app takes to
// those of the given packages (import paths; "main" for the main package).
func Only(pkgs ...string) Option {
	return optionFunc(func(a *App) {
		a.only = map[string]bool{}
		for _, p := range pkgs {
			a.only[p] = true
		}
	})
}

// Dev shows the messages of internal errors in responses; so does the
// environment variable VUKA_ENV=development.
func Dev() Option { return optionFunc(func(a *App) { a.dev = true }) }

// WithRouter serves the app's routes with r instead of net/http's ServeMux.
func WithRouter(r Router) Option { return optionFunc(func(a *App) { a.router = r }) }

// OnError replaces the error page and JSON error body.
func OnError(f func(w http.ResponseWriter, r *http.Request, e *Error, html bool)) Option {
	return optionFunc(func(a *App) { a.onError = f })
}

func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }
