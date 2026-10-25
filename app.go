package web

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/vuka/v2/diag"
	"github.com/vuka-lang/web/v2/cron"
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
	recheck map[reflect.Type]bool // island pages whose check waits for their constructor

	wraps      []routeWrap
	binders    map[reflect.Type]func(*http.Request) (reflect.Value, error)
	liveChecks []func(context.Context) error
	liveGuards []func(context.Context, LiveHandler) error
	csrf       *http.CrossOriginProtection

	envFiles []string // nil: the defaults
	env      *envLoad
	required []string

	liveLayout  func(ui.Node) ui.Node
	liveKey     []byte
	liveOrigins []string
	liveDeflate LiveCompressionMode
	liveTTL     time.Duration
	liveMax     int // the newest live protocol version spoken
	relay       Relay
	hub         *hub
	adoptWindow time.Duration                  // LiveAdopt: 0 or less mounts fresh on every join
	liveIDs     []func(context.Context) string // LiveIdentity
	parked      parkStore                      // live sessions between a render and its join

	cronSpecs []*cronSpec
	cronClock cron.Clock
	noCrons   bool
	sched     *scheduler // nil: no crons
	scheduled bool       // Start added the scheduler to the lifecycle

	devTools bool      // Dev: development, with Tailwind and Vite, where the process runs in it
	tools    *devTools // nil: none to run

	tb        *toolbar // nil: no development toolbar
	noToolbar bool

	noCSS     bool
	cssSource string      // web.Tailwind's path, for the vuka command
	css       *stylesheet // nil: pages link no stylesheet
	assets    *assetSet   // nil: no compiled styles or scripts

	frontends  []*frontend
	exactPaths bool // no trailing-slash redirect

	headers       *SecurityHeaders // nil: the defaults
	noHeaders     bool
	proxies       []netip.Prefix // TrustedProxies
	maxBody       int64          // MaxRequestBody; 0: the default
	trackRequests bool
	cronCtx       []func(context.Context) context.Context

	procList     []any // ContextProcessors'
	varGuards    []func(context.Context, []any) error
	personalPkgs []string
	vars         *varSet // nil: no context processors
	joinMessages msgStash

	once    sync.Once
	handler http.Handler
	err     error
}

// New makes an app of every route, service and controller declared so far
// (each app takes its own snapshot, with its own singletons). It first reads
// the env files into the process environment (see LoadEnv), so constructors
// and Start methods see their values through os.Getenv.
func New(opts ...Option) *App {
	a := &App{c: newContainer(), reg: snapshot(), liveTTL: 12 * time.Hour, liveMax: 2, hub: newHub(), adoptWindow: defaultLiveAdoptWindow}
	for _, o := range opts {
		o.apply(a)
	}
	if a.env = loadEnv(a.envFiles); a.env.err != nil {
		a.c.errs = append(a.c.errs, a.env.err)
	}
	// VUKA_ENV=development, VUKA_DEV=1, or VUKA_DEV_DIR under `vuka run`; never an env file's (loadEnv skips VUKA_ keys)
	a.dev = IsDev() || a.devTools && testBinary
	if a.liveKey == nil {
		a.liveKey = devLiveKey(a.dev)
	}
	if a.router == nil {
		a.router = NewServeMux()
	}
	if a.devTools && devActive() {
		a.tools = a.newDevTools()
	}
	a.css = a.appCSS()
	a.assets = a.appAssets()
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

// RouteOption configures a registration (App.Get, Registry.Live, …): one
// of web's (Template, Use, Layout, Wrap, Attrs), another package's
// (an auth gate, ui.Island) or any value carrying typed attributes, which
// become the route's, as attributes written on a decorated handler are.
type RouteOption interface{ Attrs() []any }

// routeOpt is an option applied to the route's spec when the app is built.
type routeOpt func(*routeSpec)

func (o routeOpt) Attrs() []any { return []any{o} }

// View renders the route's result with a view, as Template does.
func View(f vuka.File, name ...string) RouteOption { return Template(f, name...) }

// Wrap adds middleware to the route, as Use does.
func Wrap(mw ...Middleware) RouteOption {
	return routeOpt(func(s *routeSpec) { s.mw = append(s.mw, mw...) })
}

// Handle registers fn for method and path, as Routes.Handle does for every
// app.
func (a *App) Handle(method, path string, fn any, opts ...RouteOption) *App {
	a.mutable("Handle")
	a.reg.handle(method, path, fn, opts, true)
	return a
}

// Controller makes the struct ctor builds a service whose route methods are
// served under prefix, as Routes.Controller does for every app.
func (a *App) Controller(prefix string, ctor any, opts ...RouteOption) *App {
	a.mutable("Controller")
	a.reg.controller(prefix, ctor, opts, true)
	return a
}

// Service registers a service: ctor, func(deps…) *T or func(deps…) (*T,
// error) with T a struct, builds the app's one T, when something first
// needs it, its parameters filled from the container. Hooks, handlers,
// constructors, layouts' and guards' Load and middleware constructors then
// take *T as a parameter. It is how an app declares its services.
//
//	app := web.New()
//	app.Service(NewTodos) // func NewTodos(db *DB) *Todos
func (a *App) Service(ctor any) *App {
	a.mutable("Service")
	t, err := ctorOf(reflect.TypeOf(ctor))
	if err != nil {
		a.c.errs = append(a.c.errs, diag.New("E0418", "Service(%T): %v", ctor, err).WithHelp("pass the service's constructor: func(deps…) *Service"))
		return a
	}
	a.c.provide(ctor, "service "+t.String())
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

// Static serves the files of fsys under prefix: a directory as its
// index.html, never as a listing, and no dotfile or dot directory but
// .well-known.
func (a *App) Static(prefix string, fsys fs.FS) *App {
	a.mutable("Static")
	prefix = "/" + strings.Trim(prefix, "/")
	a.statics = append(a.statics, func() {
		a.router.Handle(http.MethodGet, joinPath(prefix, "/{path...}"),
			http.StripPrefix(strings.TrimSuffix(prefix, "/"), staticFS(fsys)))
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
	for _, c := range a.reg.controllers {
		if c.own || a.take(c.typ.Reflect.PkgPath()) {
			a.c.provide(c.typ.New, "controller "+c.typ.Name)
			prefixes[reflect.PointerTo(c.typ.Reflect)] = c.prefix
			attrs := flattenAttrs(c.typ.Attrs)
			if c.registered {
				attrs = append(flattenAttrs(vuka.TypeAttrs(c.typ.Reflect)), attrs...)
			}
			typeAttrs[reflect.PointerTo(c.typ.Reflect)] = attrs
		}
	}
	specs := []*routeSpec(nil)
	errs := slices.Clone(a.c.errs)
	errs = append(errs, a.reg.errs...)
	errs = append(errs, a.decoratedPages()...)
	for _, r := range a.reg.routes {
		d := r.decl
		if (r.opts == nil || !r.opts.own) && !a.take(d.Pkg) {
			continue
		}
		if t := pageCtor(d); t != nil && ui.IsIsland(t) {
			ps, err := a.pageSpecs(typePage{r.method, r.path, r.opts.pageType(t, d.Func), reflect.ValueOf(d.Func)})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			// A folder's layouts, guards and middleware (pages/route.go's
			// options) apply to the page its island backs too.
			for _, s := range ps {
				r.opts.applyTo(s)
			}
			specs = append(specs, ps...)
			continue
		}
		s := &routeSpec{method: r.method, path: r.path, where: fmt.Sprintf("%s at %s:%d", d.Name, d.File, d.Line),
			fn: reflect.ValueOf(d.Func), recv: d.Recv, params: slices.Clone(d.Params), mw: a.reg.uses[declKey(d)]}
		if d.Recv != nil {
			t := d.Recv
			if t.Kind() != reflect.Pointer {
				t = reflect.PointerTo(t)
			}
			s.path = joinPath(prefixes[t], r.path)
			s.attrs = append(s.attrs, typeAttrs[t]...)
		}
		s.attrs = append(s.attrs, flattenAttrs(d.Attrs)...)
		if r.opts != nil {
			ref, _ := vuka.FuncOf(d.Func)
			s.attrs = append(s.attrs, flattenAttrs(ref.Attrs)...)
		}
		if v, ok := a.reg.views[declKey(d)]; ok {
			s.view = &v
		}
		r.opts.applyTo(s)
		if t := s.attrType(); t != nil {
			s.attrs = append(flattenAttrs(vuka.TypeAttrs(t)), s.attrs...)
		}
		specs = append(specs, s)
	}
	for _, ls := range a.reg.lives {
		if ls.own || a.take(ls.pkg) {
			specs = append(specs, ls.route())
		}
	}
	for _, p := range a.reg.pages {
		if !a.take(p.typ.Reflect.PkgPath()) {
			continue
		}
		ps, err := a.pageSpecs(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		specs = append(specs, ps...)
	}
	specs = append(specs, a.specs...)
	rec := &recordingRouter{Router: a.router}
	a.router = rec
	defer func() { a.router = rec.Router }()
	seen := map[string]string{}
	for _, s := range specs {
		key := s.method + " " + s.path
		if prev, dup := seen[key]; dup {
			errs = append(errs, diag.New("E0401", "%s is declared twice: by %s and by %s", key, whereName(prev), whereName(s.where)).
				At(whereSpan(s.where)).WithRelated(withLabel(whereSpan(prev), "declared here first")).WithHelp("give one of them another path or method"))
			continue
		}
		seen[key] = s.where
		outer, mwErr := a.middlewareOf(s)
		if mwErr != nil {
			errs = append(errs, mwErr)
		}
		h, err := a.compile(s)
		if err != nil {
			errs = append(errs, err)
		}
		if err != nil || mwErr != nil {
			continue
		}
		for i := len(s.mw) - 1; i >= 0; i-- {
			h = a.layer(s.mw[i], h)
		}
		for i := len(outer) - 1; i >= 0; i-- {
			h = a.layer(outer[i], h)
		}
		h, err = a.wrapRoute(s, h)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		a.router.Handle(s.method, s.path, h)
	}
	if len(a.frontends) > 0 {
		gets := map[string]string{} // GET patterns, wildcards unnamed: what the router can't tell apart
		for _, s := range specs {
			if s.method == http.MethodGet {
				gets[wildcards.ReplaceAllString(s.path, "{}")] = s.method + " " + s.path + " (" + s.where + ")"
			}
		}
		for _, f := range a.frontends {
			if err := f.prepare(a); err != nil {
				errs = append(errs, err)
				continue
			}
			for _, p := range f.routes() {
				key := wildcards.ReplaceAllString(p, "{}")
				if prev, dup := gets[key]; dup {
					errs = append(errs, diag.New("E0402", "Frontend(%q) serves GET %s, as %s does", f.root, p, prev).
						WithHelp("move the handler, or the frontend's page, to another path"))
				}
				gets[key] = fmt.Sprintf("Frontend(%q)", f.root)
			}
		}
	}
	for _, p := range a.eager {
		if err := a.c.check(p.out, p.source); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, a.compileCrons()...)
	errs = append(errs, a.compileVars()...)
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
	if a.assets == nil {
		a.assets = distAssets(a.frontends)
	}
	a.routeAssets()
	if a.tb != nil {
		a.routeToolbar()
		a.tb.install()
	}
	served := rec.patterns
	for _, f := range a.frontends {
		for _, p := range f.routes() {
			rec.Router.Handle(http.MethodGet, p, f) // a fallback: the trailing-slash redirect goes past it
		}
	}
	if a.relay != nil {
		a.relay.Subscribe(a.hub.deliver)
	}
	var h http.Handler = rec.Router
	if !a.exactPaths {
		var targets [][2]string
		for _, s := range specs {
			targets = append(targets, [2]string{s.method, s.path})
		}
		h = newSlashes(h, served, targets)
	}
	if a.csrf != nil {
		h = a.csrfProtect(h)
	}
	for i := len(a.mw) - 1; i >= 0; i-- {
		h = a.layer(a.mw[i], h)
	}
	if a.tb != nil {
		h = a.tb.wrap(h)
	}
	return a.recoverHTTP(a.edge(h)), nil
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
			return nil, failf("E0404", s.method+" "+s.path+" ("+whereName(s.where)+")", s.where, "its attribute %T needs %s, which the app doesn't have", x, name).
				WithHelp("provide it, or add the module that does (web/auth for its gates)")
		}
	}
	if a.trackRequests {
		inner := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer enterRoute(r, r.Context())()
			inner.ServeHTTP(w, r)
		})
	}
	info := RouteInfo{Method: s.method, Path: s.path, Where: s.where, Attrs: slices.Clip(s.attrs), Live: s.live != nil, HTML: s.html || s.live != nil, RootLayout: rootLayout(s)}
	if s.live != nil {
		info.LiveType = s.live.typ
	}
	if info.RootLayout != nil {
		h = withRoot(info.RootLayout, h)
	}
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

// Start builds every dependency the routes and crons need and every Eager
// one, then runs the start sequence: each supplied or built value
// implementing Starter and each OnStart hook, in dependency order (supplied
// values and hooks as added, before the values built from constructors),
// and last the crons' scheduler, which Stop stops first.
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
	if a.sched != nil && !a.noCrons && !a.scheduled {
		// After every value the crons need was built: started last, the
		// scheduler stops first.
		a.scheduled = true
		a.c.hook(hook{name: "crons", start: a.sched.start, stop: a.sched.stop})
	}
	if err := errors.Join(a.c.start(ctx), errors.Join(takeEnvErrors()...)); err != nil {
		return err
	}
	if a.tools != nil {
		a.tools.start(a.frontends)
	}
	return nil
}

// Stop runs the stop sequence: each value implementing Stopper and each
// OnStop hook, in the reverse order of the start sequence.
func (a *App) Stop(ctx context.Context) error {
	if a.tools != nil {
		a.tools.stop()
	}
	a.hub.closeAll()
	a.parked.closeAll()
	a.css.close()
	a.assets.close()
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
	srv.RegisterOnShutdown(a.parked.closeAll)
	log.Printf("web: listening on %s", browseURL(ln.Addr()))
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

// browseURL is the address a browser opens for a listener: localhost for
// one on every interface (":8080" listens on [::] or 0.0.0.0).
func browseURL(a net.Addr) string {
	host, port, err := net.SplitHostPort(a.String())
	if err != nil {
		return "http://" + a.String()
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
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
