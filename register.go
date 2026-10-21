package web

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"strings"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/vuka/diag"
)

// routeOpts are what a registration's options give it.
type routeOpts struct {
	own   bool       // registered on an App: Only doesn't apply
	spec  []routeOpt // Params, Wrap, …
	live  []LiveOption
	types []func(*vuka.Type) // type decorators (ui.Island), for a page's type
	mw    []Middleware
	mwOf  []useOf
	view  *declView
	attrs []any
}

var typeDecoType = reflect.TypeFor[func(*vuka.Type)]()

func collect(opts []RouteOption, own bool) *routeOpts {
	o := &routeOpts{own: own}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		for _, x := range opt.Attrs() {
			switch x := x.(type) {
			case routeOpt:
				o.spec = append(o.spec, x)
				continue
			case Uses:
				o.mw = append(o.mw, x.middleware()...)
				continue
			case useOf:
				o.mwOf = append(o.mwOf, x)
				continue
			case Templates:
				var v declView
				x(&vuka.Decl{Func: &v})
				o.view = &v
				continue
			case liveOptionFunc:
				o.live = append(o.live, x)
				continue
			}
			if t := reflect.TypeOf(x); t != nil && t.Kind() == reflect.Func && t.ConvertibleTo(typeDecoType) {
				o.types = append(o.types, reflect.ValueOf(x).Convert(typeDecoType).Interface().(func(*vuka.Type)))
				continue
			}
			o.attrs = append(o.attrs, x)
		}
	}
	return o
}

// applyTo gives a route's spec what the options say; nothing for a
// decorated handler's (nil).
func (o *routeOpts) applyTo(s *routeSpec) {
	if o == nil {
		return
	}
	s.mw = append(s.mw, o.mw...)
	s.mwOf = append(s.mwOf, o.mwOf...)
	if o.view != nil {
		s.view = o.view
	}
	s.attrs = append(s.attrs, o.attrs...)
	for _, f := range o.spec {
		f(s)
	}
}

// pageType describes t, the struct ctor builds, for its type decorators
// and its page.
func (o *routeOpts) pageType(t reflect.Type, ctor any) *vuka.Type {
	attrs := slices.Clone(vuka.TypeAttrs(t))
	if o != nil {
		attrs = append(attrs, o.attrs...)
	}
	return &vuka.Type{Name: t.String(), Reflect: t, Attrs: attrs, New: ctor}
}

func (r *registry) fail(err error) { r.errs = append(r.errs, err) }

func (r *registry) handle(method, path string, fn any, opts []RouteOption, own bool) {
	d, err := funcDecl(fn)
	if err != nil {
		r.fail(diag.New("E0403", "%s %s: %v", method, path, err).WithHelp("pass a function"))
		return
	}
	o := collect(opts, own)
	if t := pageCtor(d); t != nil {
		for _, f := range o.types {
			f(o.pageType(t, fn))
		}
	}
	r.routes = append(r.routes, declRoute{method: method, path: path, decl: d, opts: o})
}

func (r *registry) live(path string, page any, opts []RouteOption, own bool) {
	o := collect(opts, own)
	v := reflect.ValueOf(page)
	ls := &liveSpec{path: path, own: own}
	switch {
	case v.Kind() == reflect.Pointer && v.Type().Elem().Kind() == reflect.Struct && !v.IsNil():
		ls.typ, ls.template = v.Type().Elem(), v.Elem()
	case v.Kind() == reflect.Func:
		t, err := ctorOf(v.Type())
		if err != nil {
			r.fail(diag.New("E0329", "Live(%q, %s): %v", path, v.Type(), err).WithHelp("pass the page's constructor: func(deps…) *Page"))
			return
		}
		ls.typ, ls.ctor = t, v
	default:
		r.fail(diag.New("E0329", "Live(%q, …) takes the page's constructor, not a %T", path, page).WithHelp("pass NewPage, a func(deps…) *Page"))
		return
	}
	ls.name, ls.pkg = ls.typ.String(), ls.typ.PkgPath()
	for _, f := range o.types {
		f(o.pageType(ls.typ, page))
	}
	ls.attrs, ls.registered = o.attrs, true
	ls.mw, ls.mwOf = o.mw, o.mwOf
	for _, lo := range o.live {
		lo.liveOption(ls)
	}
	for _, f := range o.spec {
		ls.routeOpts = append(ls.routeOpts, f)
	}
	r.lives = append(r.lives, ls)
}

func (r *registry) controller(prefix string, ctor any, opts []RouteOption, own bool) {
	t, err := ctorOf(reflect.TypeOf(ctor))
	if err != nil {
		r.fail(diag.New("E0418", "Controller(%q, %T): %v", prefix, ctor, err).WithHelp("pass the controller's constructor: func(deps…) *Controller"))
		return
	}
	o := collect(opts, own)
	ty := &vuka.Type{Name: t.String(), Reflect: t, Attrs: o.attrs, New: ctor}
	r.controllers = append(r.controllers, declController{prefix: prefix, typ: ty, own: own, registered: true})
}

func (r *registry) service(ctor any, own bool) {
	t, err := ctorOf(reflect.TypeOf(ctor))
	if err != nil {
		r.fail(diag.New("E0418", "Service(%T): %v", ctor, err).WithHelp("pass the service's constructor: func(deps…) *Service"))
		return
	}
	r.services = append(r.services, declService{typ: (*routeOpts)(nil).pageType(t, ctor), own: own})
}

// ctorOf is the struct a constructor builds: func(…) *T or func(…) (*T, error).
func ctorOf(t reflect.Type) (reflect.Type, error) {
	if t == nil || t.Kind() != reflect.Func {
		return nil, errors.New("not a function")
	}
	if n := t.NumOut(); n == 0 || n > 2 || n == 2 && t.Out(1) != errType {
		return nil, fmt.Errorf("a constructor returns *T or (*T, error), not %s", t)
	}
	out := t.Out(0)
	if out.Kind() != reflect.Pointer || out.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("a constructor returns a pointer to a struct, not %s", out)
	}
	return out.Elem(), nil
}

// pageCtor is the struct d builds when it is a constructor (func(…) *T, a
// struct), else nil.
func pageCtor(d *vuka.Decl) reflect.Type {
	if d.Recv != nil {
		return nil
	}
	t, err := ctorOf(reflect.TypeOf(d.Func))
	if err != nil {
		return nil
	}
	return t
}

// funcDecl describes a function registered from Go as the compiler
// describes a decorated one: its name, package, place and parameters (its
// attributes, those registered for it, are read when the app is built). A
// method expression's receiver is its Recv.
func funcDecl(fn any) (*vuka.Decl, error) {
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func || v.IsNil() {
		return nil, fmt.Errorf("the handler is %T, not a function", fn)
	}
	t := v.Type()
	d := &vuka.Decl{Func: fn, Variadic: t.IsVariadic(), Name: t.String()}
	ref, _ := vuka.FuncOf(fn)
	f := runtime.FuncForPC(v.Pointer())
	skip := 0
	if ref.Method != "" && !ref.Literal && t.NumIn() > 0 && f != nil && !strings.HasSuffix(f.Name(), "-fm") {
		d.Recv, skip = t.In(0), 1 // a method expression, not a method value
	}
	if f != nil {
		file, line := f.FileLine(f.Entry())
		d.File, d.Line = shortFile(file), line
		name := strings.TrimSuffix(f.Name(), "-fm")
		slash := strings.LastIndexByte(name, '/') + 1
		if dot := strings.IndexByte(name[slash:], '.'); dot >= 0 {
			d.Pkg = strings.ReplaceAll(name[:slash+dot], "%2e", ".")
			local := strings.NewReplacer("(*", "", ")", "").Replace(name[slash+dot+1:])
			d.Name = local
			if skip == 0 {
				d.Name = d.Pkg[strings.LastIndexByte(d.Pkg, '/')+1:] + "." + local
			}
		}
	}
	for i := skip; i < t.NumIn(); i++ {
		d.Params = append(d.Params, vuka.Param{Type: t.In(i)})
	}
	for i := 0; i < t.NumOut(); i++ {
		d.Results = append(d.Results, t.Out(i))
	}
	return d, nil
}

// newComponent builds a page's component: by its constructor, its
// parameters of provided types injected and the others zero, else a fresh
// struct holding template's values.
func (a *App) newComponent(t reflect.Type, template, ctor reflect.Value) (reflect.Value, error) {
	if !ctor.IsValid() {
		c := reflect.New(t)
		if template.IsValid() {
			c.Elem().Set(template)
		}
		return c, nil
	}
	ct := ctor.Type()
	args := make([]reflect.Value, ct.NumIn())
	for i := range args {
		pt := ct.In(i)
		if !a.c.has(pt) {
			args[i] = reflect.Zero(pt)
			continue
		}
		v, err := a.c.get(pt)
		if err != nil {
			return reflect.Value{}, err
		}
		args[i] = v
	}
	var out []reflect.Value
	if ct.IsVariadic() {
		out = ctor.CallSlice(args)
	} else {
		out = ctor.Call(args)
	}
	if len(out) == 2 && !out[1].IsNil() {
		return reflect.Value{}, out[1].Interface().(error)
	}
	if out[0].IsNil() {
		return reflect.Value{}, &Error{Status: http.StatusInternalServerError, Message: fmt.Sprintf("%s returned a nil %s", funcName(ctor), out[0].Type())}
	}
	if a.recheck[t] {
		if err := ui.CheckIsland(t); err != nil {
			return reflect.Value{}, err
		}
	}
	return out[0], nil
}

// checkIsland checks the island page t at start-up. A deferred group
// without a loader (E0324) waits for the page's constructor, which may
// declare it (l.Loads): its pages are checked as it builds them.
func (a *App) checkIsland(t reflect.Type, ctor reflect.Value) error {
	err := ui.CheckIsland(t)
	var d *diag.Diagnostic
	if err == nil || !ctor.IsValid() || !errors.As(err, &d) || d.Code != "E0324" {
		return err
	}
	if a.recheck == nil {
		a.recheck = map[reflect.Type]bool{}
	}
	a.recheck[t] = true
	return nil
}

// checkCtor checks the dependencies a page's constructor takes.
func (a *App) checkCtor(ctor reflect.Value, where string) error {
	if !ctor.IsValid() {
		return nil
	}
	for i := range ctor.Type().NumIn() {
		if pt := ctor.Type().In(i); a.c.has(pt) {
			if err := a.c.check(pt, where); err != nil {
				return err
			}
			a.needs = append(a.needs, pt)
		}
	}
	return nil
}
