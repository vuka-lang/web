package web

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
)

// App is a web application: the routes, services and controllers declared
// with decorators, and whatever was added in code.
type App struct {
	router  Router
	c       *container
	mw      []Middleware
	only    map[string]bool
	dev     bool
	onError func(http.ResponseWriter, *http.Request, *Error, bool)
	reg     registry
	specs   []*routeSpec
	statics []func()
	needs   []reflect.Type
	eager   []*provider
	checks  []func(*App) error

	wraps      []routeWrap
	binders    map[reflect.Type]func(*http.Request) (reflect.Value, error)
	liveChecks []func(context.Context) error
	csrf       *http.CrossOriginProtection

	envFiles []string // nil: the defaults
	env      *envLoad
	required []string

	liveLayout  func(ui.Node) ui.Node
	liveKey     []byte
	liveOrigins []string
	liveTTL     time.Duration
	liveMax     int // the newest live protocol version spoken
	relay       Relay
	hub         *hub

	tb        *toolbar // nil: no development toolbar
	noToolbar bool

	noCSS     bool
	cssSource string      // web.Tailwind's path, for the vuka command
	css       *stylesheet // nil: pages link no stylesheet

	once    sync.Once
	handler http.Handler
	err     error
}

// New makes an app of every route, service and controller declared so far
// (each app takes its own snapshot, with its own singletons). It first reads
// the env files into the process environment (see LoadEnv), so constructors
// and Start methods see their values through os.Getenv.
func New(opts ...Option) *App {
	a := &App{c: newContainer(), reg: snapshot(), liveTTL: 12 * time.Hour, liveMax: 2, hub: newHub()}
	for _, o := range opts {
		o.apply(a)
	}
	if a.env = loadEnv(a.envFiles); a.env.err != nil {
		a.c.errs = append(a.c.errs, a.env.err)
	}
	a.dev = a.dev || os.Getenv("VUKA_ENV") == "development"
	if a.liveKey == nil {
		a.liveKey = devLiveKey(a.dev)
	}
	if a.router == nil {
		a.router = NewServeMux()
	}
	a.css = a.appCSS()
	a.tb = a.newToolbar()
	return a
}

func (a *App) mutable(what string) {
	if a.handler != nil || a.err != nil {
		panic("web: " + what + " after the app was built")
	}
}

// Provide adds constructors (or ready values) to the app's container.
func (a *App) Provide(ctors ...any) *App {
	a.mutable("Provide")
	for _, c := range ctors {
		a.c.provide(c, "")
	}
	return a
}

// Eager adds constructors whose values Start builds (and starts, when they
// implement Starter) even when nothing needs them. Provided values that
// aren't functions always start; constructors otherwise only when needed.
func (a *App) Eager(ctors ...any) *App {
	a.mutable("Eager")
	for _, c := range ctors {
		if p := a.c.provide(c, ""); p != nil {
			a.eager = append(a.eager, p)
		}
	}
	return a
}

// OnStart adds fn to the app's start sequence, after the values supplied
// and the hooks added before it and before the values Start constructs.
func (a *App) OnStart(fn func(context.Context) error) *App {
	a.mutable("OnStart")
	a.c.hook(hook{name: funcName(reflect.ValueOf(fn)), start: fn})
	return a
}

// OnStop adds fn to the app's stop sequence, which runs in the reverse order
// of the start sequence: fn runs after everything started after it was added
// has stopped.
func (a *App) OnStop(fn func(context.Context) error) *App {
	a.mutable("OnStop")
	a.c.hook(hook{name: funcName(reflect.ValueOf(fn)), stop: fn})
	return a
}

// Check adds fn to the checks Build runs once the routes are compiled; its
// error is reported with Build's own.
func (a *App) Check(fn func(*App) error) *App {
	a.mutable("Check")
	a.checks = append(a.checks, fn)
	return a
}

// Use adds app-wide middleware; the first added runs first.
func (a *App) Use(mw ...Middleware) *App {
	a.mutable("Use")
	a.mw = append(a.mw, mw...)
	return a
}

// RouteOption configures a route registered in code.
type RouteOption func(*routeSpec)

// Params names a handler's parameters in order, which reflection can't see:
// scalar parameters bind by name.
func Params(names ...string) RouteOption {
	return func(s *routeSpec) {
		for i := range s.params {
			if i < len(names) {
				s.params[i].Name = names[i]
			}
		}
	}
}

// View renders the route's result with a view, as @web.Template does.
func View(f vuka.File, name ...string) RouteOption {
	return func(s *routeSpec) {
		s.view = &declView{file: f}
		if len(name) > 0 {
			s.view.name = name[0]
		}
	}
}

// Wrap adds middleware to the route, as @web.Use does.
func Wrap(mw ...Middleware) RouteOption { return func(s *routeSpec) { s.mw = append(s.mw, mw...) } }

// Handle registers fn for method and path, bound as a decorated handler is.
func (a *App) Handle(method, path string, fn any, opts ...RouteOption) *App {
	a.mutable("Handle")
	v := reflect.ValueOf(fn)
	s := &routeSpec{method: method, path: path, fn: v}
	if v.Kind() != reflect.Func {
		a.c.errs = append(a.c.errs, fmt.Errorf("web: %s %s: the handler is %T, not a function", method, path, fn))
		return a
	}
	s.where = funcName(v)
	for i := 0; i < v.Type().NumIn(); i++ {
		s.params = append(s.params, vuka.Param{Type: v.Type().In(i)})
	}
	for _, o := range opts {
		o(s)
	}
	a.specs = append(a.specs, s)
	return a
}

func (a *App) Get(path string, fn any, opts ...RouteOption) *App {
	return a.Handle(http.MethodGet, path, fn, opts...)
}
func (a *App) Post(path string, fn any, opts ...RouteOption) *App {
	return a.Handle(http.MethodPost, path, fn, opts...)
}
func (a *App) Put(path string, fn any, opts ...RouteOption) *App {
	return a.Handle(http.MethodPut, path, fn, opts...)
}
func (a *App) Patch(path string, fn any, opts ...RouteOption) *App {
	return a.Handle(http.MethodPatch, path, fn, opts...)
}
func (a *App) Delete(path string, fn any, opts ...RouteOption) *App {
	return a.Handle(http.MethodDelete, path, fn, opts...)
}

// Static serves the files of fsys under prefix.
func (a *App) Static(prefix string, fsys fs.FS) *App {
	a.mutable("Static")
	prefix = "/" + strings.Trim(prefix, "/")
	a.statics = append(a.statics, func() {
		a.router.Handle(http.MethodGet, joinPath(prefix, "/{path...}"),
			http.StripPrefix(strings.TrimSuffix(prefix, "/"), http.FileServerFS(fsys)))
	})
	return a
}

// Build resolves the app: every route's parameters, results and view, and the
// dependency graph. It reports every problem at once, before anything is
// served; Handler, Start and Run call it.
func (a *App) Build() error {
	a.once.Do(func() { a.handler, a.err = a.build() })
	return a.err
}

func (a *App) take(pkg string) bool { return a.only == nil || a.only[pkg] }

func (a *App) build() (http.Handler, error) {
	prefixes := map[reflect.Type]string{}
	typeAttrs := map[reflect.Type][]any{}
	for _, t := range a.reg.services {
		if a.take(t.Reflect.PkgPath()) {
			a.c.provide(t.New, "service "+t.Name)
		}
	}
	for _, c := range a.reg.controllers {
		if a.take(c.typ.Reflect.PkgPath()) {
			a.c.provide(c.typ.New, "controller "+c.typ.Name)
			prefixes[reflect.PointerTo(c.typ.Reflect)] = c.prefix
			typeAttrs[reflect.PointerTo(c.typ.Reflect)] = flattenAttrs(c.typ.Attrs)
		}
	}
	var specs []*routeSpec
	for _, r := range a.reg.routes {
		d := r.decl
		if !a.take(d.Pkg) {
			continue
		}
		s := &routeSpec{method: r.method, path: r.path, where: fmt.Sprintf("%s at %s:%d", d.Name, d.File, d.Line),
			fn: reflect.ValueOf(d.Func), recv: d.Recv, params: d.Params, mw: a.reg.uses[declKey(d)]}
		if d.Recv != nil {
			t := d.Recv
			if t.Kind() != reflect.Pointer {
				t = reflect.PointerTo(t)
			}
			s.path = joinPath(prefixes[t], r.path)
			s.attrs = append(s.attrs, typeAttrs[t]...)
		}
		s.attrs = append(s.attrs, flattenAttrs(d.Attrs)...)
		if v, ok := a.reg.views[declKey(d)]; ok {
			s.view = &v
		}
		specs = append(specs, s)
	}
	for _, ls := range a.reg.lives {
		if a.take(ls.pkg) {
			specs = append(specs, ls.route())
		}
	}
	specs = append(specs, a.specs...)
	errs := slices.Clone(a.c.errs)
	seen := map[string]string{}
	for _, s := range specs {
		key := s.method + " " + s.path
		if prev, dup := seen[key]; dup {
			errs = append(errs, fmt.Errorf("web: %s is declared twice: by %s and by %s", key, prev, s.where))
			continue
		}
		seen[key] = s.where
		h, err := a.compile(s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for i := len(s.mw) - 1; i >= 0; i-- {
			h = a.layer(s.mw[i], h)
		}
		h, err = a.wrapRoute(s, h)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		a.router.Handle(s.method, s.path, h)
	}
	for _, p := range a.eager {
		if err := a.c.check(p.out, p.source); err != nil {
			errs = append(errs, err)
		}
	}
	for _, f := range a.checks {
		if err := f(a); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, takeEnvErrors()...) // the routes' middleware may read Env
	if err := a.checkRequired(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	for _, f := range a.statics {
		f()
	}
	a.router.Handle(http.MethodGet, liveScript, http.HandlerFunc(serveLiveJS))
	a.router.Handle(http.MethodGet, liveSocket, http.HandlerFunc(a.serveLive))
	a.routeCSS()
	if a.tb != nil {
		a.routeToolbar()
		a.tb.install()
	}
	if a.relay != nil {
		a.relay.Subscribe(a.hub.deliver)
	}
	var h http.Handler = a.router
	if a.csrf != nil {
		h = a.csrfProtect(h)
	}
	for i := len(a.mw) - 1; i >= 0; i-- {
		h = a.layer(a.mw[i], h)
	}
	if a.tb != nil {
		h = a.tb.wrap(h)
	}
	return h, nil
}

// wrapRoute wraps a compiled route in the middleware WrapRoutes functions
// give it, after checking every RouteAttr it carries has its function.
func (a *App) wrapRoute(s *routeSpec, h http.Handler) (http.Handler, error) {
	for _, x := range s.attrs {
		ra, ok := x.(RouteAttr)
		if !ok {
			continue
		}
		name := ra.RouteWrapper()
		if !slices.ContainsFunc(a.wraps, func(w routeWrap) bool { return w.name == name }) {
			return nil, fmt.Errorf("web: %s %s (%s): its attribute %T needs %s, which the app doesn't have", s.method, s.path, s.where, x, name)
		}
	}
	info := RouteInfo{Method: s.method, Path: s.path, Where: s.where, Attrs: slices.Clip(s.attrs), Live: s.live != nil, HTML: s.html || s.live != nil}
	for i := len(a.wraps) - 1; i >= 0; i-- {
		if mw := a.wraps[i].f(info); mw != nil {
			h = a.layer(mw, h)
		}
	}
	return h, nil
}

// layer wraps h in middleware; in development, in a span of the toolbar's
// timeline too.
func (a *App) layer(mw Middleware, h http.Handler) http.Handler {
	if a.tb != nil {
		return a.tb.timed(mw, h)
	}
	return mw(h)
}

// Handler is the app as an http.Handler, to serve or mount anywhere. It
// panics when Build fails.
func (a *App) Handler() http.Handler {
	if err := a.Build(); err != nil {
		panic(err)
	}
	return a.handler
}

// Resolve returns the app's T, building it and its dependencies.
func Resolve[T any](a *App) (T, error) {
	var zero T
	if err := a.Build(); err != nil {
		return zero, err
	}
	v, err := a.c.get(typeOf[T]())
	if err != nil {
		return zero, err
	}
	return v.Interface().(T), nil
}

// Start builds every dependency the routes need and every Eager one, then
// runs the start sequence: each supplied or built value implementing Starter
// and each OnStart hook, in dependency order (supplied values and hooks as
// added, before the values built from constructors).
func (a *App) Start(ctx context.Context) error {
	if err := a.Build(); err != nil {
		return err
	}
	for _, t := range a.needs {
		if _, err := a.c.get(t); err != nil {
			return err
		}
	}
	for _, p := range a.eager {
		if _, err := a.c.get(p.out); err != nil {
			return err
		}
	}
	if err := errors.Join(takeEnvErrors()...); err != nil {
		return err
	}
	return errors.Join(a.c.start(ctx), errors.Join(takeEnvErrors()...))
}

// Stop runs the stop sequence: each value implementing Stopper and each
// OnStop hook, in the reverse order of the start sequence.
func (a *App) Stop(ctx context.Context) error {
	a.hub.closeAll()
	a.css.close()
	if a.tb != nil {
		a.tb.streams.close()
	}
	return a.c.stop(ctx)
}

// Run serves the app on addr (":" + $PORT, else ":8080", when empty) until
// SIGINT or SIGTERM, then shuts down gracefully and stops the services.
func (a *App) Run(addr string) error {
	if addr == "" {
		addr = ":8080"
		if p := os.Getenv("PORT"); p != "" {
			addr = ":" + p
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := a.Start(ctx); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: a.handler, ReadHeaderTimeout: 10 * time.Second}
	srv.RegisterOnShutdown(a.hub.closeAll)
	log.Printf("web: listening on http://%s", ln.Addr())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err = <-served:
	case <-ctx.Done():
		sctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		err = srv.Shutdown(sctx)
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	sctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	return errors.Join(err, a.Stop(sctx))
}

func funcName(v reflect.Value) string {
	if f := runtime.FuncForPC(v.Pointer()); f != nil {
		file, line := f.FileLine(f.Entry())
		return fmt.Sprintf("%s at %s:%d", f.Name(), shortFile(file), line)
	}
	return v.Type().String()
}

func shortFile(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
