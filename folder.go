package web

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2/diag"
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
// validate: tag answers 404. Its unexported fields of provided types are
// its dependencies, injected after the constructor runs, as a page-backed
// island's are, so a field users *users.Service needs no NewT, and its other
// unexported fields, its tracked state among them, are left as the
// constructor made them; a dependency field the URL would bind too (named
// as a path parameter, tagged url:) is E0419 at start-up. A page whose
// pointer has Title() string titles the document: ui.PageTitle reads it in
// the layouts. One with Crumbs() []ui.Crumb gives them its breadcrumbs,
// read with ui.PageCrumbs. A stateless page's Title and Crumbs are read
// after its mount hook; a Live page's once its fields are bound, before
// its mount hook, which its session runs as it renders. On
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
// Per request each layout is a zero T, its path parameters bound and its
// unexported fields of provided types injected (as PageRoute's page's),
// then its Load run, if it has one: Load(ctx, deps…) returning nothing or an error,
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

// rootLayout is the route's root layout: its first LayoutOf, nil when it
// has none.
func rootLayout(s *routeSpec) reflect.Type {
	for _, l := range s.layouts {
		if !l.guard {
			return l.typ
		}
	}
	return nil
}

type rootKey struct{}

// withRoot has h's requests carry their route's root layout t: a join or
// navigation records it (join.root), and the page's script tag names it
// (data-vk-root).
func withRoot(t reflect.Type, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if j, ok := r.Context().Value(joinKey{}).(*join); ok {
			j.root = t
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), rootKey{}, t)))
	})
}

// rootID is the identity of a root layout in a page's HTML: its type's
// package path and name, hashed, so the page names it without naming the
// app's packages.
func rootID(t reflect.Type) string {
	h := fnv.New64a()
	io.WriteString(h, t.PkgPath()+"."+t.Name())
	return strconv.FormatUint(h.Sum64(), 36)
}

// ReloadLayout has the layout T of a route's LayoutOf load again on every
// in-app navigation, what vuka's folder routes register for a layout whose
// Load or markup reads the request — web.Request, its URL or query. Other
// layouts persist: navigating over a page's live connection to another page
// under the same layouts, their path parameters the same, keeps them as
// they are — their Load doesn't run again, and the elements, scroll, client
// state and live components they render stay — and renders the page below
// them. Their markup renders again for the new page, the page getting only
// what changed of it: a header of ui.PageTitle and ui.PageCrumbs read
// there follows the page, where one Load stored stays the first page's
// (vuka registers a layout reading them in Load with the option). A layout
// reloading persists all the same in the page, its Load running again. A
// layout whose Load takes the *http.Request reloads without the option.
// Guards (GuardOf) run on every navigation.
func ReloadLayout[T any]() RouteOption {
	t := typeOf[T]()
	return routeOpt(func(s *routeSpec) { s.reloads = append(s.reloads, t) })
}

// layoutRef is a layout or guard a route's options name.
type layoutRef struct {
	typ   reflect.Type
	guard bool
}

// Receiver builds the receiver of a method handler for each request with
// ctor, a page's constructor, binding the route's path parameters and
// injecting its dependency fields as PageRoute does a page's (404 when a
// parameter doesn't parse):
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
	reload   bool          // it loads on every navigation (ReloadLayout)
}

func (a *App) compileLayouts(s *routeSpec, what string) ([]*layoutSpec, error) {
	var out []*layoutSpec
	pathNames := pathParams(s.path)
	for _, t := range s.reloads {
		if !slices.ContainsFunc(s.layouts, func(l layoutRef) bool { return l.typ == t && !l.guard }) {
			return nil, failf("E0321", what, s.where, "web.ReloadLayout[%s]: the route has no web.LayoutOf[%[1]s]", t).
				WithHelp("give the route the layout, or drop the option")
		}
	}
	for _, ref := range s.layouts {
		t := ref.typ
		opt := "web.LayoutOf"
		if ref.guard {
			opt = "web.GuardOf"
		}
		fail := func(format string, args ...any) *diag.Diagnostic {
			return failf("E0321", what, s.where, opt+"[%s]: "+format, append([]any{t}, args...)...)
		}
		l := &layoutSpec{typ: t, guard: ref.guard, reload: slices.Contains(s.reloads, t)}
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
		deps, err := a.depFields(t, s.path, s.where)
		if err != nil {
			return nil, failure("E0419", what, s.where, err)
		}
		l.props = append(l.props, deps...)
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
				if mt.In(i) == requestType {
					l.reload = true
				}
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

// heldLayout is a layout a page renders in, loaded: what a navigation of its
// live connection may keep (ReloadLayout).
type heldLayout struct {
	typ reflect.Type
	x   reflect.Value // *T, bound and loaded
	gen int           // the connection's navigation it was first loaded on, keying its components
}

// loadLayouts builds the layouts and guards for the request in their
// options' order: each bound and loaded. On a navigation of a live
// connection, the layouts the page it leaves renders in, from the
// outermost down to the first that differs — another type, or other path
// parameters — are kept as they were loaded, their Load not run again,
// unless they reload (ReloadLayout); guards always run. A join or a
// navigation records the layouts loaded for the next.
func (a *App) loadLayouts(rq *request, layouts []*layoutSpec, pathNames []string) ([]reflect.Value, error) {
	xs := make([]reflect.Value, len(layouts))
	j, _ := rq.r.Context().Value(joinKey{}).(*join)
	var held []heldLayout
	gen := 0
	if j != nil && j.why == "navigate" {
		held, gen = j.held, j.nav
	}
	k := 0 // the layout of held next in line, -1 past the first that differs
	var keep []heldLayout
	for i, l := range layouts {
		x := reflect.New(l.typ)
		if err := a.bindPage(x.Elem(), l.props, pathNames, rq.r); err != nil {
			return nil, err
		}
		if !l.guard {
			if k >= 0 && k < len(held) && held[k].typ == l.typ && sameProps(held[k].x.Elem(), x.Elem(), l.props) {
				k++
				if !l.reload {
					x.Elem().Set(held[k-1].x.Elem()) // its Load's work, kept
					xs[i] = x
					keep = append(keep, heldLayout{l.typ, x, held[k-1].gen})
					continue
				}
				keep = append(keep, heldLayout{l.typ, x, held[k-1].gen})
			} else {
				k = -1
				keep = append(keep, heldLayout{l.typ, x, gen})
			}
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
	if j != nil {
		j.layouts = keep
	}
	return xs, nil
}

// sameProps reports whether a and b, two of a layout, bind props alike.
func sameProps(a, b reflect.Value, props []prop) bool {
	for _, p := range props {
		if !reflect.DeepEqual(field(a, p.index).Interface(), field(b, p.index).Interface()) {
			return false
		}
	}
	return true
}

// slotKey keys the page below a route's layouts (ui.Keyed): the live
// connection's navigation it was rendered for, read as the page renders —
// a patch keeps it — so the components of the layouts kept keep their
// instances across a navigation and those of the page mount afresh. Each
// layout is keyed alike by the navigation it was loaded on. A page's HTTP
// render and its join count 0 alike, so the join adopts the render's
// instances.
type slotKey struct{ n *int }

func (k slotKey) String() string {
	if k.n == nil {
		return "0"
	}
	return strconv.Itoa(*k.n)
}

// nestLayouts is page inside the loaded layouts xs, keyed for the
// request's live connection (slotKey).
func (a *App) nestLayouts(rq *request, layouts []*layoutSpec, xs []reflect.Value, page ui.Node) ui.Node {
	var key slotKey
	j, _ := rq.r.Context().Value(joinKey{}).(*join)
	if j != nil {
		key.n = j.slot
	}
	n := ui.Keyed(key, page)
	k := 0 // the layouts outside layouts[i]
	for _, l := range layouts {
		if !l.guard {
			k++
		}
	}
	for i := len(layouts) - 1; i >= 0; i-- {
		if layouts[i].guard {
			continue
		}
		k--
		field(xs[i].Elem(), layouts[i].children).Set(reflect.ValueOf(&n).Elem())
		if layouts[i].ptr {
			n = xs[i].Interface().(ui.Node)
		} else {
			n = xs[i].Elem().Interface().(ui.Node)
		}
		if k > 0 { // the outermost, the document, isn't: Keep keeps it, or nothing
			gen := 0
			if j != nil && k < len(j.layouts) {
				gen = j.layouts[k].gen
			}
			n = ui.Keyed(gen, n)
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

// pageProps are the bindings of a folder page's fields (PageRoute,
// Receiver): its exported fields as a Live page's (liveProps), and its
// unexported fields of provided types, its dependencies, injected as a
// page-backed island's are (depFields).
func (a *App) pageProps(t reflect.Type, path, where string) ([]prop, error) {
	props, err := a.liveProps(t, where)
	if err != nil {
		return nil, err
	}
	deps, err := a.depFields(t, path, where)
	return append(props, deps...), err
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
// Title() string, and carrying its breadcrumbs (crumbed).
func titled(ctx context.Context, x reflect.Value) context.Context {
	if t, ok := x.Interface().(interface{ Title() string }); ok {
		ctx = ui.WithPageTitle(ctx, t.Title())
	}
	return crumbed(ctx, x)
}

// crumbed is ctx carrying the breadcrumbs of the page x (a pointer to its
// struct) when it has Crumbs() []ui.Crumb, for its layouts to read
// (ui.PageCrumbs) as they do its title.
func crumbed(ctx context.Context, x reflect.Value) context.Context {
	if c, ok := x.Interface().(interface{ Crumbs() []ui.Crumb }); ok {
		return ui.WithPageCrumbs(ctx, c.Crumbs())
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
	if err := ui.CheckParts(t, false); err != nil {
		return nil, failure("E0347", what, s.where, err)
	}
	if err := a.checkParams(t, what, s.where); err != nil {
		return nil, err
	}
	pathNames := pathParams(s.path)
	props, err := a.pageProps(t, s.path, s.where)
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
		rq := a.newRequest(w, r, s)
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
			page = a.nestLayouts(rq, layouts, loaded, x.Interface().(ui.Node))
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
	props, err := a.pageProps(t, s.path, s.where)
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
