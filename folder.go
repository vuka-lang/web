package web

import (
	"context"
	"net/http"
	"reflect"
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
// Title is read after its Mount; a Live page's once its fields are bound,
// before Mount, which its session runs as it renders. On Get, the
// constructor's struct is a stateless page, a ui.Node: its Mount, Mount()
// or Mount(ctx) returning nothing or an error, runs once its fields are
// bound, and the page renders. LayoutOf implies PageRoute.
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
// its dependencies provided types; its error answers the request. A Live
// page's layouts render around it on the request's render; the page's
// patches render the page. On a handler's route (Post, …) they wrap a
// ui.Node it returns for a whole page. LayoutOf implies PageRoute.
func LayoutOf[T any]() RouteOption {
	t := typeOf[T]()
	return routeOpt(func(s *routeSpec) { s.page, s.layouts = true, append(s.layouts, t) })
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

// layoutSpec is a layout of LayoutOf, compiled.
type layoutSpec struct {
	typ      reflect.Type
	ptr      bool // *T is the ui.Node, not T
	children []int
	props    []prop
	load     reflect.Value // (*T).Load, a func taking the receiver first
	loadIn   []reflect.Type
}

func (a *App) compileLayouts(s *routeSpec, what string) ([]*layoutSpec, error) {
	var out []*layoutSpec
	pathNames := pathParams(s.path)
	for _, t := range s.layouts {
		fail := func(format string, args ...any) *diag.Diagnostic {
			return failf("E0321", what, s.where, "web.LayoutOf[%s]: "+format, append([]any{t}, args...)...)
		}
		if t.Kind() != reflect.Struct {
			return nil, fail("a layout is a struct").WithHelp("declare type Layout struct { Children ui.Node; … }")
		}
		l := &layoutSpec{typ: t}
		switch {
		case t.Implements(nodeType):
		case reflect.PointerTo(t).Implements(nodeType):
			l.ptr = true
		default:
			return nil, fail("%s isn't a ui.Node", t).WithHelp("give it a Render(ctx context.Context, w io.Writer) error method, or a view")
		}
		f, ok := t.FieldByName("Children")
		if !ok || f.Type != nodeType || !f.IsExported() {
			return nil, fail("%s has no field Children ui.Node", t).WithHelp("add Children ui.Node: the page, or the next layout, renders there")
		}
		l.children = f.Index
		for _, f := range reflect.VisibleFields(t) {
			if f.IsExported() && !f.Anonymous && scalar(f.Type) && matchName(pathNames, f.Name) != "" {
				l.props = append(l.props, prop{index: f.Index, name: f.Name, typ: f.Type, path: true})
			}
		}
		if err := a.checkPathProps(t, l.props, pathNames); err != nil {
			return nil, failure("E0328", what, s.where, err)
		}
		if m, ok := reflect.PointerTo(t).MethodByName("Load"); ok {
			mt := m.Type
			if mt.NumIn() < 2 || mt.In(1) != contextType || mt.NumOut() > 1 || mt.NumOut() == 1 && mt.Out(0) != errType {
				return nil, fail("its Load is %s", m.Func.Type()).WithHelp("declare Load(ctx context.Context, deps…) error")
			}
			for i := 2; i < mt.NumIn(); i++ {
				pt := mt.In(i)
				if !a.c.has(pt) {
					return nil, failf("E0407", what, s.where, "no provider for %s, needed by %s.Load", pt, t).WithHelp("provide it: web.Provide(NewX), or @web.Service on the type")
				}
				if err := a.c.check(pt, s.where); err != nil {
					return nil, err
				}
				a.needs = append(a.needs, pt)
				l.loadIn = append(l.loadIn, pt)
			}
			l.load = m.Func
		}
		out = append(out, l)
	}
	return out, nil
}

// wrapLayouts is page inside layouts for the request: each built, bound
// and loaded, outermost first.
func (a *App) wrapLayouts(rq *request, layouts []*layoutSpec, pathNames []string, page ui.Node) (ui.Node, error) {
	xs := make([]reflect.Value, len(layouts))
	for i, l := range layouts {
		x := reflect.New(l.typ)
		if err := a.bindPage(x.Elem(), l.props, pathNames, rq.r); err != nil {
			return nil, err
		}
		if l.load.IsValid() {
			in := []reflect.Value{x, reflect.ValueOf(&rq.ctx).Elem()}
			for _, t := range l.loadIn {
				d, err := a.c.get(t)
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
	n := page
	for i := len(layouts) - 1; i >= 0; i-- {
		field(xs[i].Elem(), layouts[i].children).Set(reflect.ValueOf(&n).Elem())
		if layouts[i].ptr {
			n = xs[i].Interface().(ui.Node)
		} else {
			n = xs[i].Elem().Interface().(ui.Node)
		}
	}
	return n, nil
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
	if m, ok := reflect.PointerTo(t).MethodByName("Mount"); ok {
		mt := m.Type
		if mt.NumIn() > 2 || mt.NumIn() == 2 && mt.In(1) != contextType || mt.NumOut() > 1 || mt.NumOut() == 1 && mt.Out(0) != errType {
			return nil, failf("E0333", what, s.where, "%s's Mount is %s", t, m.Func.Type()).WithHelp("declare Mount() or Mount(ctx context.Context), returning nothing or an error")
		}
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
		if err == nil {
			err = mountPage(rq.ctx, x)
		}
		var page ui.Node
		if err == nil {
			rq.ctx = titled(rq.ctx, x)
			page, err = a.wrapLayouts(rq, layouts, pathNames, x.Interface().(ui.Node))
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
