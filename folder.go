package web

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"strings"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka/diag"
)

// Rest is a catch-all path parameter: a field of type Rest binds the rest
// of the path a "{name...}" segment matches, slashes included.
//
//	type FileParams struct{ Path web.Rest } // GET /files/{path...}
type Rest string

// PageRoute makes a route a page as a pages/ folder declares it, what vuka's
// folder routes register:
//
//	web.Routes.Get("/employers/{employer}", employer.NewIndex, web.PageRoute())
//
// The page's struct is built by its constructor, its parameters of
// provided types injected and the others zero; its exported fields,
// promoted ones from embedded structs included, bind as a Live page's do
// (a provided type injected, text from the path segment, else the query
// parameter, of its name); a path parameter that doesn't parse or fails its
// validate: tag answers 404. A page whose pointer has Title() string titles
// the document: ui.PageTitle reads it in the layouts. A stateless page's
// Title is read after its mount hook; a Live page's once its fields are
// bound, before its mount hook, which its session runs as it renders. On
// Get, the constructor's struct is a stateless page, a ui.Node: the mount
// hook its LiveSchema declares (ui.MountPage), if any, runs once its fields
// are bound, and the page renders. LayoutOf implies PageRoute. On a
// function handler (a folder's method func, GET in [id].vuka) a scalar
// parameter bound from a path segment that doesn't parse answers 404 too.
func PageRoute() RouteOption { return routeOpt(func(s *routeSpec) { s.page = true }) }

// LayoutOf wraps a route's page in the layout T, a stateless struct
// component (T or *T a ui.Node) with a field Children ui.Node, the page or
// the next layout. Several nest in the order given, the first outermost,
// and replace Layout and LiveLayout for the route:
//
//	type Layout struct {
//		Params            // path parameters, bound as a page's are
//		Children ui.Node
//		Employer Employer
//	}
//
//	func (l *Layout) Load(ctx context.Context, store *Store) error { … }
//
// Per request each layout is a zero T, its path parameters bound, then its
// Load run, if it has one: Load(ctx, deps…) returning nothing or an error,
// its dependencies provided types; its error answers the request. Layouts
// and guards (GuardOf) load in the order their options are given, before a
// stateless page's mount hook. A Live
// page's layouts render around it on the request's render; the page's
// patches render the page. On a handler's route (Post, …) every layout is
// loaded before the handler runs, whatever it answers, a Load error
// answering instead; a ui.Node the handler returns for a whole page is
// wrapped in them. LayoutOf implies PageRoute.
func LayoutOf[T any]() RouteOption {
	t := typeOf[T]()
	return routeOpt(func(s *routeSpec) { s.page, s.layouts = true, append(s.layouts, layoutRef{t, false}) })
}

// GuardOf runs the guard T before a route's page or handler: a struct,
// rendering nothing, whose Load(ctx context.Context, deps…) error, its
// dependencies provided types, checks the request:
//
//	type Guard struct{ Params }
//
//	func (g *Guard) Load(ctx context.Context, perms *Perms) error {
//		if !perms.Can(ctx, "employers.view", g.Employer) {
//			return web.Forbidden
//		}
//		return nil
//	}
//
// Per request it is a zero T, its path parameters bound as a page's are,
// then its Load run; guards and layouts load in the order their options are
// given, before the page mounts or the handler runs, and the first error
// answers the request as the handler's would: NotFound a 404, Forbidden a
// 403, a Redirect redirects, as a page or as JSON. A Live page's guards run
// on its request and on its join. GuardOf implies PageRoute.
func GuardOf[T any]() RouteOption {
	t := typeOf[T]()
	return routeOpt(func(s *routeSpec) { s.page, s.layouts = true, append(s.layouts, layoutRef{t, true}) })
}

// layoutRef is a layout or guard a route's options name.
type layoutRef struct {
	typ   reflect.Type
	guard bool
}

// Receiver builds the receiver of a method handler for each request with
// ctor, a page's constructor, binding the route's path parameters into it
// as PageRoute does a page's (404 when one doesn't parse):
//
//	web.Routes.Post("/employers/{employer}/edit", (*edit.Edit).Post, web.Receiver(edit.NewEdit))
//
// The handler's other parameters bind as any handler's.
func Receiver(ctor any) RouteOption {
	v := reflect.ValueOf(ctor)
	return routeOpt(func(s *routeSpec) { s.page, s.recvCtor = true, v })
}

// UseOf wraps the route in mw, HTTP middleware: a func(next http.Handler)
// http.Handler, or its constructor, func(deps…) web.Middleware, its
// dependencies provided types, resolved when the app is built; what vuka's
// folder routes register for a folder's Middleware. Several wrap in the
// order given, the first outermost, and they wrap the whole route — outside
// its Use middleware, its guards and layouts, and its binding, so a path
// parameter's 404 happens inside them. Not calling next answers the request
// itself. On a Live page it wraps the GET and the join, never the socket's
// events.
func UseOf(mw any) RouteOption { return useOf{fn: reflect.ValueOf(mw)} }

// useOf is UseOf's middleware, resolved when the app is built.
type useOf struct{ fn reflect.Value }

func (u useOf) Attrs() []any { return []any{u} }

var middlewareType = typeOf[Middleware]()

// middlewareOf resolves a route's UseOf middleware, the first given
// outermost: each a Middleware as given, or built by its constructor, its
// provided dependencies injected.
func (a *App) middlewareOf(s *routeSpec) ([]Middleware, error) {
	if len(s.mwOf) == 0 {
		return nil, nil
	}
	what := s.method + " " + s.path + " (" + whereName(s.where) + ")"
	out := make([]Middleware, 0, len(s.mwOf))
	for _, u := range s.mwOf {
		fn := u.fn
		if !fn.IsValid() || fn.Kind() != reflect.Func || fn.IsNil() {
			return nil, failf("E0410", what, s.where, "web.UseOf takes middleware, not %v", valueName(fn)).
				WithHelp("pass a func(next http.Handler) http.Handler, or its constructor: func(deps…) web.Middleware")
		}
		t := fn.Type()
		if t.ConvertibleTo(middlewareType) {
			out = append(out, fn.Convert(middlewareType).Interface().(Middleware))
			continue
		}
		if t.IsVariadic() || t.NumOut() != 1 || !t.Out(0).ConvertibleTo(middlewareType) {
			return nil, failf("E0410", what, s.where, "web.UseOf(%s): neither middleware nor its constructor", funcName(fn)).
				WithHelp("pass a func(next http.Handler) http.Handler, or its constructor: func(deps…) web.Middleware")
		}
		in := make([]reflect.Value, t.NumIn())
		for i := range in {
			pt := t.In(i)
			if !a.c.has(pt) {
				return nil, failf("E0407", what, s.where, "no provider for %s, needed by %s", pt, funcName(fn)).
					WithHelp(provideHelp)
			}
			if err := a.c.check(pt, s.where); err != nil {
				return nil, err
			}
			a.needs = append(a.needs, pt)
			v, err := a.c.get(pt)
			if err != nil {
				return nil, err
			}
			in[i] = v
		}
		mw := fn.Call(in)[0]
		if mw.IsNil() {
			return nil, failf("E0410", what, s.where, "%s returned a nil middleware", funcName(fn))
		}
		out = append(out, mw.Convert(middlewareType).Interface().(Middleware))
	}
	return out, nil
}

func valueName(v reflect.Value) string {
	if !v.IsValid() {
		return "nil"
	}
	return v.Type().String()
}

// attrType is the type whose registered attributes (vuka.RegisterType) the
// route takes, beyond a Live page's or a controller's: a stateless folder
// page's struct, or a method handler's receiver built by Receiver.
func (s *routeSpec) attrType() reflect.Type {
	if !s.page {
		return nil
	}
	if s.recvCtor.IsValid() {
		if t, err := ctorOf(s.recvCtor.Type()); err == nil {
			return t
		}
		return nil
	}
	if s.recv == nil && s.method == http.MethodGet {
		return nodePage(s.fn)
	}
	return nil
}

// layoutSpec is a layout of LayoutOf, or a guard of GuardOf, compiled.
type layoutSpec struct {
	typ      reflect.Type
	guard    bool
	ptr      bool // *T is the ui.Node, not T
	children []int
	props    []prop
	load     reflect.Value // (*T).Load, a func taking the receiver first
	loadIn   []binder      // its parameters after the context's
}

func (a *App) compileLayouts(s *routeSpec, what string) ([]*layoutSpec, error) {
	var out []*layoutSpec
	pathNames := pathParams(s.path)
	for _, ref := range s.layouts {
		t := ref.typ
		opt := "web.LayoutOf"
		if ref.guard {
			opt = "web.GuardOf"
		}
		fail := func(format string, args ...any) *diag.Diagnostic {
			return failf("E0321", what, s.where, opt+"[%s]: "+format, append([]any{t}, args...)...)
		}
		l := &layoutSpec{typ: t, guard: ref.guard}
		switch {
		case ref.guard && t.Kind() != reflect.Struct:
			return nil, fail("a guard is a struct").WithHelp("declare type Guard struct { … } with a Load(ctx context.Context, deps…) error method")
		case ref.guard:
		case t.Kind() != reflect.Struct:
			return nil, fail("a layout is a struct").WithHelp("declare type Layout struct { Children ui.Node; … }")
		case t.Implements(nodeType):
		case reflect.PointerTo(t).Implements(nodeType):
			l.ptr = true
		default:
			return nil, fail("%s isn't a ui.Node", t).WithHelp("give it a Render(ctx context.Context, w io.Writer) error method, or a view")
		}
		if !ref.guard {
			f, ok := t.FieldByName("Children")
			if !ok || f.Type != nodeType || !f.IsExported() {
				return nil, fail("%s has no field Children ui.Node", t).WithHelp("add Children ui.Node: the page, or the next layout, renders there")
			}
			l.children = f.Index
		}
		for _, f := range reflect.VisibleFields(t) {
			if f.IsExported() && !f.Anonymous && scalar(f.Type) && matchName(pathNames, f.Name) != "" {
				l.props = append(l.props, prop{index: f.Index, name: f.Name, typ: f.Type, path: true})
			}
		}
		if err := a.checkPathProps(t, l.props, pathNames); err != nil {
			return nil, failure("E0328", what, s.where, err)
		}
		m, ok := reflect.PointerTo(t).MethodByName("Load")
		if !ok && ref.guard {
			return nil, fail("%s has no Load method", t).WithHelp("declare func (g *%s) Load(ctx context.Context, deps…) error", t.Name())
		}
		if ok {
			mt := m.Type
			if mt.NumIn() < 2 || mt.In(1) != contextType || mt.NumOut() > 1 || mt.NumOut() == 1 && mt.Out(0) != errType {
				return nil, fail("its Load is %s", m.Func.Type()).WithHelp("declare Load(ctx context.Context, deps…) error")
			}
			for i := 2; i < mt.NumIn(); i++ {
				b, err := a.requestParam(mt.In(i), s.where)
				if err != nil {
					return nil, failure("E0405", fmt.Sprintf("%s: %s.Load's %s parameter, %s", what, t, ordinal(i), mt.In(i)), loadWhere(m.Func, s.where), err)
				}
				l.loadIn = append(l.loadIn, b)
			}
			l.load = m.Func
		}
		out = append(out, l)
	}
	return out, nil
}

// loadWhere is where a Load method is declared, "Load at file:line", else
// the route's place.
func loadWhere(fn reflect.Value, where string) string {
	if f := runtime.FuncForPC(fn.Pointer()); f != nil {
		if file, line := f.FileLine(f.Entry()); file != "<autogenerated>" {
			return fmt.Sprintf("Load at %s:%d", shortFile(file), line)
		}
	}
	return where
}

// loadLayouts builds the layouts and guards for the request in their
// options' order: each bound and loaded.
func (a *App) loadLayouts(rq *request, layouts []*layoutSpec, pathNames []string) ([]reflect.Value, error) {
	xs := make([]reflect.Value, len(layouts))
	for i, l := range layouts {
		x := reflect.New(l.typ)
		if err := a.bindPage(x.Elem(), l.props, pathNames, rq.r); err != nil {
			return nil, err
		}
		if l.load.IsValid() {
			in := []reflect.Value{x, reflect.ValueOf(&rq.ctx).Elem()}
			for _, b := range l.loadIn {
				d, err := b(rq)
				if err != nil {
					return nil, err
				}
				in = append(in, d)
			}
			end := a.span(rq.ctx, "handler", l.typ.String()+".Load")
			out := l.load.Call(in)
			end()
			if len(out) == 1 && !out[0].IsNil() {
				return nil, out[0].Interface().(error)
			}
		}
		xs[i] = x
	}
	return xs, nil
}

// nestLayouts is page inside the loaded layouts xs.
func nestLayouts(layouts []*layoutSpec, xs []reflect.Value, page ui.Node) ui.Node {
	n := page
	for i := len(layouts) - 1; i >= 0; i-- {
		if layouts[i].guard {
			continue
		}
		field(xs[i].Elem(), layouts[i].children).Set(reflect.ValueOf(&n).Elem())
		if layouts[i].ptr {
			n = xs[i].Interface().(ui.Node)
		} else {
			n = xs[i].Elem().Interface().(ui.Node)
		}
	}
	return n
}

// bindPage binds v's props for r as bindProps does, except that a path
// parameter that doesn't parse, or fails its validate: tag, is NotFound.
func (a *App) bindPage(v reflect.Value, props []prop, pathNames []string, r *http.Request) error {
	for i := range props {
		p := &props[i]
		n := matchName(pathNames, p.name)
		if p.dep || n == "" || p.query {
			if err := a.bindProps(v, props[i:i+1], pathNames, r); err != nil {
				return err
			}
			continue
		}
		f := field(v, p.index)
		if err := setText(f, []string{a.router.Param(r, n)}); err != nil {
			return NotFound
		}
		if p.check != nil && p.check(f) != nil {
			return NotFound
		}
	}
	return nil
}

// checkPathProps gives the props of t bound from the path their validate:
// tags' checks.
func (a *App) checkPathProps(t reflect.Type, props []prop, pathNames []string) error {
	for i := range props {
		p := &props[i]
		if p.dep || p.query || matchName(pathNames, p.name) == "" {
			continue
		}
		tag := t.FieldByIndex(p.index).Tag.Get("validate")
		if tag == "" {
			continue
		}
		one := reflect.StructOf([]reflect.StructField{{Name: "V", Type: p.typ, Tag: reflect.StructTag(`json:"` + p.name + `" validate:"` + strings.ReplaceAll(tag, `"`, `\"`) + `"`)}})
		if err := ui.CheckValidate(one); err != nil {
			return err
		}
		p.check = func(f reflect.Value) error {
			x := reflect.New(one).Elem()
			x.Field(0).Set(f)
			return ui.Validate(x.Interface())
		}
	}
	return nil
}

// titled is ctx titled by the page x (a pointer to its struct) when it has
// Title() string.
func titled(ctx context.Context, x reflect.Value) context.Context {
	if t, ok := x.Interface().(interface{ Title() string }); ok {
		return ui.WithPageTitle(ctx, t.Title())
	}
	return ctx
}

// nodePage is the struct a PageRoute route's handler constructs when it is a
// stateless page: func(deps…) *T, *T a ui.Node and not stateful.
func nodePage(fn reflect.Value) reflect.Type {
	t, err := ctorOf(fn.Type())
	if err != nil {
		return nil
	}
	if p := reflect.PointerTo(t); !p.Implements(nodeType) || p.Implements(statefulType) {
		return nil
	}
	return t
}

// compileNodePage is a stateless page's GET (PageRoute): its struct built by its
// constructor, bound, mounted, titled and rendered in its layouts.
func (a *App) compileNodePage(s *routeSpec, t reflect.Type) (http.Handler, error) {
	what := s.method + " " + s.path + " (" + whereName(s.where) + ")"
	if err := a.checkCtor(s.fn, s.where); err != nil {
		return nil, err
	}
	if err := ui.CheckHooks(t); err != nil {
		return nil, failure("E0337", what, s.where, err)
	}
	if err := a.checkParams(t, what, s.where); err != nil {
		return nil, err
	}
	pathNames := pathParams(s.path)
	props, err := a.liveProps(t, s.where)
	if err == nil {
		err = a.checkPathProps(t, props, pathNames)
	}
	if err != nil {
		return nil, failure("E0328", what, s.where, err)
	}
	layouts, err := a.compileLayouts(s, what)
	if err != nil {
		return nil, err
	}
	route := s.method + " " + s.path
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rq := a.newRequest(w, r)
		if a.tb != nil {
			noteRoute(rq.ctx, route, s.where)
		}
		end := a.span(rq.ctx, "handler", shortName(s.where))
		x, err := a.newComponent(t, reflect.Value{}, s.fn)
		if err == nil {
			err = a.bindPage(x.Elem(), props, pathNames, r)
		}
		var loaded []reflect.Value
		if err == nil {
			loaded, err = a.loadLayouts(rq, layouts, pathNames)
		}
		if err == nil {
			err = ui.MountPage(rq.ctx, x.Interface())
		}
		var page ui.Node
		if err == nil {
			rq.ctx = titled(rq.ctx, x)
			page = nestLayouts(layouts, loaded, x.Interface().(ui.Node))
		}
		end()
		if err != nil {
			a.writeError(w, r, err, true)
			return
		}
		a.page(rq, page)
	}), nil
}

// pageReceiver is the binder of a method handler's receiver built by its
// page's constructor (Receiver).
func (a *App) pageReceiver(s *routeSpec, what string) (binder, error) {
	ctor := s.recvCtor
	if !ctor.IsValid() || ctor.Kind() != reflect.Func || ctor.IsNil() {
		return nil, failf("E0410", what, s.where, "web.Receiver takes the page's constructor, not %v", ctor).WithHelp("pass NewPage, a func(deps…) *Page")
	}
	t, err := ctorOf(ctor.Type())
	if err != nil {
		return nil, failure("E0410", what, s.where, err)
	}
	recv, deref := s.recv, false
	if recv.Kind() != reflect.Pointer {
		recv, deref = reflect.PointerTo(recv), true
	}
	if recv.Elem() != t {
		return nil, failf("E0410", what, s.where, "web.Receiver(%s) builds a *%s, and the handler's receiver is %s", funcName(ctor), t, s.recv).WithHelp("pass the constructor of the handler's type")
	}
	if err := a.checkCtor(ctor, s.where); err != nil {
		return nil, err
	}
	pathNames := pathParams(s.path)
	props, err := a.liveProps(t, s.where)
	if err == nil {
		err = a.checkPathProps(t, props, pathNames)
	}
	if err != nil {
		return nil, failure("E0328", what, s.where, err)
	}
	return func(rq *request) (reflect.Value, error) {
		x, err := a.newComponent(t, reflect.Value{}, ctor)
		if err == nil {
			err = a.bindPage(x.Elem(), props, pathNames, rq.r)
		}
		if deref {
			x = x.Elem()
		}
		return x, err
	}, nil
}

// pageResult is a handler's result when it is a page for layouts to wrap:
// a ui.Node that isn't a Redirect.
func pageResult(v reflect.Value) (ui.Node, bool) {
	if !v.IsValid() || v.Kind() == reflect.Interface && v.IsNil() || v.Kind() == reflect.Pointer && v.IsNil() {
		return nil, false
	}
	n, ok := v.Interface().(ui.Node)
	if _, redirect := n.(Redirect); !ok || redirect {
		return nil, false
	}
	return n, true
}

// framed reports whether layouts holds a layout, not only guards.
func framed(layouts []*layoutSpec) bool {
	for _, l := range layouts {
		if !l.guard {
			return true
		}
	}
	return false
}
