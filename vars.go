package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/vuka/v2/diag"
)

// Context processors give every template of the app variables it reads by
// name — the signed-in user, the request, flash messages — without each
// page passing them down. A processor is a plain function returning a
// struct; each exported field of the struct is a variable, named in lower
// camel case (User → user, CSRFToken → csrfToken):
//
//	type Vars struct{ Me *User }
//
//	func Context(ctx context.Context, s *Service) (Vars, error) {
//		id := auth.Current(ctx)
//		if id == nil {
//			return Vars{}, nil
//		}
//		u, err := s.ByID(ctx, id.ID)
//		return Vars{Me: u}, err
//	}
//
// The app lists every processor in main, the built-in ones too; nothing
// runs that isn't listed:
//
//	web.New(
//		web.ContextProcessors(
//			web.RequestContext,
//			auth.Context,
//			web.MessagesContext,
//			web.CSRFContext,
//			ui.PageContext,
//			users.Context,
//		),
//	)
//
// Templates read the variables by name, which the vuka command compiles to
// Var; Go code calls Var itself: web.Var[*users.User](ctx, "me").
//
// A processor runs lazily: only when something reads one of its variables,
// and at most once per request or live event, every read sharing its
// result (concurrent reads wait for the one call). What it reads of the
// request is recorded as its inputs — the URL (Request, RequestContext),
// the signed-in identity and its permission version (package auth), the
// variables of other processors (Var), anything a library records with
// RecordInput — and on a live page it runs again only when one of them
// changed; one that recorded none runs again for each page the connection
// moves to. A live session records each read against the component whose
// render made it, and when a variable's value changes renders again only
// the components that read it: a layout that stays across navigations and
// shows request.Path updates that spot and keeps the rest.

// ContextProcessors lists the app's context processors: functions
// func(ctx context.Context, deps…) (S, error) with S a struct, whose
// parameters after ctx the app fills as it fills a handler's (what
// app.Service and Provide register, ParamFrom types, the request); or one
// wrapped by Auth, Browser or Personal. Build fails when one has another
// shape, a parameter nothing provides, or a variable another processor
// gives too (each error names both). It may be given more than once; the
// lists add up.
func ContextProcessors(processors ...any) Option {
	return OptionFunc(func(a *App) {
		a.mutable("ContextProcessors")
		a.procList = append(a.procList, processors...)
	})
}

// Processor is a context processor wrapped by Auth, Browser or Personal,
// for ContextProcessors' list.
type Processor struct {
	fn       any
	gates    []any
	browser  bool
	personal bool
	gated    bool
}

func wrap(p any) Processor {
	if w, ok := p.(Processor); ok {
		return w
	}
	return Processor{fn: p}
}

// Auth makes reads of p's variables pass gates first, as a route's: given
// auth.Required, a read by an anonymous visitor sends them down the
// sign-in path — a page answers with the redirect to the login page (or
// 401), a live page loads again — instead of the processor running. A gate
// is an attribute value, a vuka.Bundle, or a function returning one (pass
// auth.Required itself, or auth.Requires("x")); a library guarding reads
// (GuardVars, which auth.Module installs) decides it, and Build fails when
// the app has none.
func Auth(p any, gates ...any) Processor {
	w := wrap(p)
	w.gated = true
	w.gates = append(w.gates, gates...)
	return w
}

// Browser lets the browser read p's variables: islands' components and the
// page's scripts get them with ctx.value("name"). No other processor's
// variable reaches the browser. Build refuses a Browser processor that
// reads the signed-in user (one of package auth, or taking one of its
// types) unless it is wrapped in Personal too, and a value whose type is
// NotForBrowser (auth.Perms) is never sent.
func Browser(p any) Processor {
	w := wrap(p)
	w.browser = true
	return w
}

// Personal says p's variables are the visitor's own, which Browser may
// send to their browser though p reads the signed-in identity:
// web.Browser(web.Personal(p)). It means nothing outside Browser, which
// Build reports.
func Personal(p any) Processor {
	w := wrap(p)
	w.personal = true
	return w
}

// NotForBrowser is a type whose values Browser never sends to the browser:
// auth.Perms, a list of what the visitor may do, is one.
type NotForBrowser interface{ NotForBrowser() }

// GuardVars has f decide each read of a variable of a processor wrapped in
// Auth, given the read's context and the gates Auth was given (each
// vuka.Bundle flattened into its attributes): an error refuses the read.
// A redirect (Redirect) or a *Error answers a page with it; on a live page
// the connection closes, and the page loads again. Package auth installs
// one.
func GuardVars(f func(ctx context.Context, gates []any) error) Option {
	return OptionFunc(func(a *App) {
		a.mutable("GuardVars")
		a.varGuards = append(a.varGuards, f)
	})
}

// PersonalPackage says that what package path (an import path) provides
// reads who the visitor is: a Browser processor of that package, or taking
// one of its types as a parameter, must be Personal too. Package auth
// declares itself.
func PersonalPackage(path string) Option {
	return OptionFunc(func(a *App) { a.personalPkgs = append(a.personalPkgs, path) })
}

// varSet is an app's compiled processors.
type varSet struct {
	procs  []*varProc
	byName map[string]*varField
	byType map[reflect.Type]*varProc
	guards []func(context.Context, []any) error
	set    *App
}

type varProc struct {
	fn       reflect.Value
	name     string // "users.Context"
	where    string // "users.Context at users.go:12"
	typ      reflect.Type
	deps     []func(context.Context) (reflect.Value, error)
	fields   []*varField
	gates    []any
	browser  bool
	personal bool
}

type varField struct {
	name  string
	index int
	typ   reflect.Type
	proc  *varProc
}

var errorType = typeOf[error]()

// compileVars checks and compiles the processors ContextProcessors listed.
func (a *App) compileVars() []error {
	if len(a.procList) == 0 {
		return nil
	}
	set := &varSet{byName: map[string]*varField{}, byType: map[reflect.Type]*varProc{}, guards: a.varGuards, set: a}
	var errs []error
	for i, x := range a.procList {
		w := wrap(x)
		p, err := a.compileProc(w, i)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if prev, dup := set.byType[p.typ]; dup {
			errs = append(errs, diag.New("E0408", "context processors %s and %s both return %s", prev.name, p.name, p.typ).
				At(whereSpan(p.where)).WithRelated(withLabel(whereSpan(prev.where), "listed here first")).
				WithHelp("list each processor once; give each its own result type"))
			continue
		}
		clash := false
		for _, f := range p.fields {
			if prev, dup := set.byName[f.name]; dup {
				errs = append(errs, diag.New("E0408", "the template variable %q is given by %s and by %s", f.name, prev.proc.name, p.name).
					At(whereSpan(p.where)).WithRelated(withLabel(whereSpan(prev.proc.where), "given here first")).
					WithHelp("rename one of the fields: a variable comes from one processor"))
				clash = true
			}
		}
		if clash {
			continue
		}
		for _, f := range p.fields {
			set.byName[f.name] = f
		}
		set.byType[p.typ] = p
		set.procs = append(set.procs, p)
	}
	if len(errs) > 0 {
		return errs
	}
	a.vars = set
	for _, p := range set.procs {
		if p.browser {
			ui.BrowserVars = browserVars
			break
		}
	}
	return nil
}

func (a *App) compileProc(w Processor, i int) (*varProc, error) {
	v := reflect.ValueOf(w.fn)
	what := fmt.Sprintf("ContextProcessors' %s processor", ordinal(i+1))
	if !v.IsValid() || v.Kind() != reflect.Func {
		return nil, diag.New("E0410", "%s, %T, isn't a function", what, w.fn).
			WithHelp("list functions func(ctx context.Context, deps…) (S, error), S a struct")
	}
	t := v.Type()
	p := &varProc{fn: v, typ: nil, gates: flattenGates(w.gates), browser: w.browser, personal: w.personal}
	file, line := "", 0
	if f := runtime.FuncForPC(v.Pointer()); f != nil {
		p.name = shortName(f.Name())
		file, line = f.FileLine(f.Entry())
		p.where = fmt.Sprintf("%s at %s:%d", p.name, file, line)
	} else {
		p.name, p.where = t.String(), t.String()
	}
	at := func(d *diag.Diagnostic) *diag.Diagnostic {
		if file != "" {
			return d.At(diag.Span{File: file, Line: line})
		}
		return d
	}
	shape := "a context processor is func(ctx context.Context, deps…) (S, error), S a struct whose exported fields are the variables"
	if t.IsVariadic() || t.NumIn() == 0 || t.In(0) != contextType {
		return nil, at(diag.New("E0410", "the context processor %s doesn't take a context.Context first", p.name).WithHelp("%s", shape))
	}
	if t.NumOut() != 2 || t.Out(1) != errorType {
		return nil, at(diag.New("E0410", "the context processor %s doesn't return (S, error)", p.name).WithHelp("%s", shape))
	}
	p.typ = t.Out(0)
	if p.typ.Kind() != reflect.Struct {
		return nil, at(diag.New("E0410", "the context processor %s returns %s, not a struct", p.name, p.typ).WithHelp("%s", shape))
	}
	switch {
	case w.gated && len(p.gates) == 0:
		return nil, at(diag.New("E0410", "web.Auth(%s) has no gate", p.name).WithHelp("give it one: web.Auth(%s, auth.Required)", p.name))
	case w.gated && len(a.varGuards) == 0:
		return nil, at(diag.New("E0404", "web.Auth(%s): the app has nothing guarding variables", p.name).WithHelp("add the module whose gates these are (auth.Module)"))
	case w.personal && !w.browser:
		return nil, at(diag.New("E0410", "web.Personal(%s) outside web.Browser", p.name).WithHelp("web.Personal lets web.Browser send a visitor's own values: web.Browser(web.Personal(%s)); without web.Browser, drop it", p.name))
	}
	if w.browser && !w.personal {
		if pkg := a.personalIn(v, t); pkg != "" {
			return nil, at(diag.New("E0410", "web.Browser(%s) would send what %s knows of the visitor to the browser", p.name, pkg).
				WithHelp("send a visitor's own values only when meant to: web.Browser(web.Personal(%s)); or keep the processor off the browser", p.name))
		}
	}
	var errs []error
	chk := checking{injector{a}, p.where, &errs}
	for k := 1; k < t.NumIn(); k++ {
		in := t.In(k)
		f := chk.Inject(in)
		if f == nil {
			return nil, at(diag.New("E0407", "the context processor %s's %s parameter, %s, is provided by nothing", p.name, ordinal(k+1), in).WithHelp(provideHelp))
		}
		p.deps = append(p.deps, f)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	for k := 0; k < p.typ.NumField(); k++ {
		sf := p.typ.Field(k)
		if !sf.IsExported() || sf.Anonymous {
			continue
		}
		p.fields = append(p.fields, &varField{name: lowerCamel(sf.Name), index: k, typ: sf.Type, proc: p})
	}
	if len(p.fields) == 0 {
		return nil, at(diag.New("E0410", "the context processor %s's %s has no exported field: it gives no variable", p.name, p.typ).WithHelp("%s", shape))
	}
	return p, nil
}

// personalIn is the personal package (PersonalPackage) the processor fn of
// type t belongs to or takes a type of, "" for none.
func (a *App) personalIn(fn reflect.Value, t reflect.Type) string {
	if len(a.personalPkgs) == 0 {
		return ""
	}
	if f := runtime.FuncForPC(fn.Pointer()); f != nil {
		name := f.Name()
		for _, pkg := range a.personalPkgs {
			if strings.HasPrefix(name, pkg+".") {
				return pkg
			}
		}
	}
	for k := 1; k < t.NumIn(); k++ {
		in := t.In(k)
		for in.Kind() == reflect.Pointer || in.Kind() == reflect.Slice {
			in = in.Elem()
		}
		if slices.Contains(a.personalPkgs, in.PkgPath()) {
			return in.PkgPath()
		}
	}
	return ""
}

func flattenGates(gates []any) []any {
	var out []any
	for _, g := range gates {
		switch g := g.(type) {
		case func() vuka.Bundle:
			out = append(out, g().Attrs()...)
		case vuka.Bundle:
			out = append(out, g.Attrs()...)
		case nil:
		default:
			out = append(out, g)
		}
	}
	return out
}

// lowerCamel is a field's name as a template variable: User → user,
// CSRFToken → csrfToken, ID → id.
func lowerCamel(s string) string {
	rs := []rune(s)
	n := 0
	for n < len(rs) && unicode.IsUpper(rs[n]) {
		n++
	}
	switch {
	case n == 0:
		return s
	case n > 1 && n < len(rs) && unicode.IsLower(rs[n]):
		n-- // the capital starting the next word stays
	}
	for i := 0; i < n; i++ {
		rs[i] = unicode.ToLower(rs[i])
	}
	return string(rs)
}

// varStore is a request's processor results: what a page's renders, and a
// live page's events, read. On a live connection the results live in the
// connection's varHistory, so a result whose inputs didn't change carries
// over from one page to the next.
type varStore struct {
	set  *varSet
	hist *varHistory // the live connection's; nil outside one
	id   uint64      // this request's, unique: the input of processors that record none
	rq   *request

	mu      sync.Mutex
	own     map[*varProc]*varEntry // outside a live connection
	refused error                  // a read Auth's gates refused
	msgs    []Message              // Flash's, for a render of this very request
	shown   []Message              // what MessagesContext gave this request's render
}

// varHistory is a live connection's processor results and messages.
type varHistory struct {
	mu      sync.Mutex
	entries map[*varProc]*varEntry
	msgs    []Message
	msgVer  uint64
	shown   bool // the messages were read since the last event
}

func newVarHistory() *varHistory { return &varHistory{entries: map[*varProc]*varEntry{}} }

// varEntry is a processor's result: its value, error and version, and the
// inputs it read, which say whether it still holds.
type varEntry struct {
	mu     sync.Mutex
	done   bool
	val    reflect.Value
	err    error
	ver    uint64
	inputs []varInput
	// personal: an input read who the visitor is (RecordInput)
	personal bool
	stale    bool // RefreshVars: run again on the next read
}

type varInput struct {
	key string
	val string
	now func(context.Context) string
}

var (
	varVersions atomic.Uint64 // versions of results, unique across stores
	requestIDs  atomic.Uint64
)

type histKey struct{}

func storeOf(ctx context.Context) *varStore {
	st, _ := ui.VarSourceOf(ctx).(*varStore)
	return st
}

// withVars gives a request's context its store of processor results, when
// the app has processors.
func (a *App) withVars(rq *request) {
	if a.vars == nil {
		return
	}
	hist, _ := rq.ctx.Value(histKey{}).(*varHistory)
	st := &varStore{set: a.vars, hist: hist, id: requestIDs.Add(1), rq: rq}
	rq.ctx = ui.WithVarSource(rq.ctx, st)
}

func (st *varStore) entry(p *varProc) *varEntry {
	if st.hist != nil {
		st.hist.mu.Lock()
		defer st.hist.mu.Unlock()
		e := st.hist.entries[p]
		if e == nil {
			e = &varEntry{}
			st.hist.entries[p] = e
		}
		return e
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.own == nil {
		st.own = map[*varProc]*varEntry{}
	}
	e := st.own[p]
	if e == nil {
		e = &varEntry{}
		st.own[p] = e
	}
	return e
}

// procRun marks a processor's run in its context: the inputs it records,
// and the processors running around it.
type procRun struct {
	p      *varProc
	up     *procRun
	mu     sync.Mutex
	inputs []varInput
	person bool
}

type runKey struct{}

// procRuns counts the processors running now: none, and runOf costs a load.
var procRuns atomic.Int64

func runOf(ctx context.Context) *procRun {
	if ctx == nil || procRuns.Load() == 0 {
		return nil
	}
	r, _ := ctx.Value(runKey{}).(*procRun)
	return r
}

// Input is something a context processor's result depends on, which
// RecordInput records: a live page runs the processor again when Now
// returns another value than it did when the processor ran.
type Input struct {
	// Key names it, once per processor run: "auth", "request".
	Key string
	// Now is its current value for ctx — fast, and recording nothing.
	Now func(ctx context.Context) string
	// Personal says it is who the visitor is: a Browser processor reading
	// it must be Personal.
	Personal bool
}

// RecordInput records in, when ctx is a context processor's: what makes
// its result. A library whose values processors read records its own —
// package auth records the identity and its permission version when
// auth.Current is called with a processor's context. Elsewhere it does
// nothing.
func RecordInput(ctx context.Context, in Input) {
	r := runOf(ctx)
	if r == nil || in.Now == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.person = r.person || in.Personal
	for _, x := range r.inputs {
		if x.key == in.Key {
			return
		}
	}
	r.inputs = append(r.inputs, varInput{in.Key, in.Now(ctx), in.Now})
}

// valid reports whether the entry's result still holds for ctx: every input
// it recorded has the value it had.
func (e *varEntry) valid(ctx context.Context) bool {
	if e.stale {
		return false
	}
	for _, in := range e.inputs {
		if in.now(ctx) != in.val {
			return false
		}
	}
	return true
}

// value is p's result for ctx and its version: the entry's while its
// inputs hold, else p run again.
func (st *varStore) value(ctx context.Context, p *varProc) (reflect.Value, uint64, *varEntry, error) {
	for r := runOf(ctx); r != nil; r = r.up {
		if r.p == p {
			return reflect.Value{}, 0, nil, fmt.Errorf("web: the context processor %s reads its own variables (through the processors it reads)", p.name)
		}
	}
	e := st.entry(p)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done && e.valid(ctx) {
		return e.val, e.ver, e, e.err
	}
	run := &procRun{p: p, up: runOf(ctx)}
	pctx := context.WithValue(ui.WithLiveHost(ctx, nil), runKey{}, run)
	procRuns.Add(1)
	val, err := p.call(pctx)
	procRuns.Add(-1)
	inputs := run.inputs
	if len(inputs) == 0 {
		// It read nothing tracked: its result is this request's.
		id := strconv.FormatUint(st.id, 10)
		inputs = []varInput{{key: "request id", val: id, now: requestID}}
	}
	if !e.done || err != nil || e.err != nil || !reflect.DeepEqual(e.val.Interface(), val.Interface()) {
		e.ver = varVersions.Add(1)
	}
	e.val, e.err, e.inputs, e.personal, e.done, e.stale = val, err, inputs, run.person, true, false
	return e.val, e.ver, e, e.err
}

func requestID(ctx context.Context) string {
	if st := storeOf(ctx); st != nil {
		return strconv.FormatUint(st.id, 10)
	}
	return ""
}

// call runs the processor with ctx, its dependencies filled.
func (p *varProc) call(ctx context.Context) (v reflect.Value, err error) {
	args := make([]reflect.Value, 1, 1+len(p.deps))
	args[0] = reflect.ValueOf(&ctx).Elem()
	for _, d := range p.deps {
		a, err := d(ctx)
		if err != nil {
			return reflect.Zero(p.typ), fmt.Errorf("web: the context processor %s: %w", p.name, err)
		}
		args = append(args, a)
	}
	defer func() {
		if r := recover(); r != nil {
			v, err = reflect.Zero(p.typ), fmt.Errorf("web: the context processor %s panicked: %v", p.name, r)
		}
	}()
	out := p.fn.Call(args)
	if e := out[1].Interface(); e != nil {
		return out[0], e.(error)
	}
	return out[0], nil
}

// VarVersion is the current version of the variable name (ui.VarSource),
// for a live session deciding whether a component that read it renders
// again.
func (st *varStore) VarVersion(ctx context.Context, name string) uint64 {
	if _, ok := overrideOf(ctx, name); ok {
		return 0
	}
	f := st.set.byName[name]
	if f == nil {
		return 0
	}
	_, ver, _, _ := st.value(ctx, f.proc)
	return ver
}

func (st *varStore) refuse(err error) {
	st.mu.Lock()
	if st.refused == nil {
		st.refused = err
	}
	st.mu.Unlock()
}

// takeRefused is the error of a read Auth's gates refused, once.
func (st *varStore) takeRefused() error {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	err := st.refused
	st.refused = nil
	return err
}

// refusedVars is the error of a read the gates of ctx's request refused.
func refusedVars(ctx context.Context) error { return storeOf(ctx).takeRefused() }

// Var is the template variable name, as a context processor gives it: what
// a template's bare name compiles to, and what Go code calls for one. Its
// processor runs on the first read of the request (or live event) and
// every later read shares the result; on a live page the read is recorded
// against the component rendering, which renders again when the value
// changes. Var returns the zero T when the read fails — no such variable, a
// T that isn't the variable's type, a processor's error, a gate refusing
// (Auth) — and VarErr says why.
func Var[T any](ctx context.Context, name string) T {
	v, err := VarErr[T](ctx, name)
	if err != nil && !errors.Is(err, errVarRefused) {
		logVarError(ctx, name, err)
	}
	return v
}

// VarErr is Var with its error.
func VarErr[T any](ctx context.Context, name string) (T, error) {
	var zero T
	want := typeOf[T]()
	if ov, ok := overrideOf(ctx, name); ok {
		ui.ReadContext(ctx)
		return varAs[T](name, ov, want)
	}
	st := storeOf(ctx)
	if st == nil {
		return zero, fmt.Errorf("web: no template variable %q here: ctx isn't a request's or a live page's of an app with web.ContextProcessors", name)
	}
	f := st.set.byName[name]
	if f == nil {
		return zero, unknownVar(st.set, name)
	}
	if !typeFits(f.typ, want) {
		return zero, fmt.Errorf("web: the template variable %q is a %s (from %s), read as a %s", name, f.typ, f.proc.name, want)
	}
	if len(f.proc.gates) > 0 {
		for _, g := range st.set.guards {
			if err := g(ctx, f.proc.gates); err != nil {
				st.refuse(err)
				return zero, fmt.Errorf("%w: %w", errVarRefused, err)
			}
		}
	}
	val, ver, _, err := st.value(ctx, f.proc)
	RecordInput(ctx, Input{Key: "var " + name, Now: func(ctx context.Context) string {
		if st := storeOf(ctx); st != nil {
			return strconv.FormatUint(st.VarVersion(ctx, name), 10)
		}
		return ""
	}})
	ui.ReadVar(ctx, name, ver)
	if err != nil {
		return zero, err
	}
	return varAs[T](name, val.Field(f.index), want)
}

var errVarRefused = errors.New("web: the read of a template variable was refused")

func typeFits(have, want reflect.Type) bool {
	return have == want || want.Kind() == reflect.Interface && have.Implements(want)
}

func varAs[T any](name string, v reflect.Value, want reflect.Type) (T, error) {
	var zero T
	if !typeFits(v.Type(), want) {
		return zero, fmt.Errorf("web: the template variable %q is a %s, read as a %s", name, v.Type(), want)
	}
	if !v.IsValid() {
		return zero, nil
	}
	x, _ := v.Interface().(T)
	return x, nil
}

func unknownVar(set *varSet, name string) error {
	names := make([]string, 0, len(set.byName))
	for n := range set.byName {
		names = append(names, n)
	}
	slices.Sort(names)
	if near := closest(name, names); near != "" {
		return fmt.Errorf("web: no template variable %q; did you mean %q?", name, near)
	}
	return fmt.Errorf("web: no template variable %q: no processor in web.ContextProcessors gives one (%s)", name, strings.Join(names, ", "))
}

// closest is the name within two edits of name, "" for none.
func closest(name string, names []string) string {
	best, bestD := "", 3
	for _, n := range names {
		if d := editDistance(strings.ToLower(name), strings.ToLower(n)); d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, utf8.RuneCountInString(b)+1)
	cur := make([]int, len(prev))
	for j := range prev {
		prev[j] = j
	}
	for _, ra := range a {
		cur[0] = prev[0] + 1
		j := 1
		for _, rb := range b {
			cost := 1
			if ra == rb {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			j++
		}
		prev, cur = cur, prev
	}
	return prev[len(prev)-1]
}

var varLogged sync.Map // name + error text → struct{}: each logged once

func logVarError(ctx context.Context, name string, err error) {
	if _, dup := varLogged.LoadOrStore(name+"\x00"+err.Error(), struct{}{}); dup {
		return
	}
	log.Printf("web: reading the template variable %q: %v", name, err)
}

// overrides are WithVars' values, nearest first.
type override struct {
	v  reflect.Value
	up *override
}

type overrideKey struct{}

// WithVars is ctx in which the variables of s — a struct, the result type
// of a context processor — are s's fields: below ctx, every read of them
// returns s's values, and the processor returning that type doesn't run.
// A part of a page shows other values this way, and a test gives its own
// (viewtest.With, viewtest.Context). s may be a pointer to the struct; it
// panics for anything else.
func WithVars(ctx context.Context, s any) context.Context {
	v := reflect.ValueOf(s)
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		panic(fmt.Sprintf("web: WithVars(%T): not a struct", s))
	}
	up, _ := ctx.Value(overrideKey{}).(*override)
	return context.WithValue(ctx, overrideKey{}, &override{v, up})
}

var fieldNames sync.Map // reflect.Type → map[string]int: a struct's variables

func varFields(t reflect.Type) map[string]int {
	if m, ok := fieldNames.Load(t); ok {
		return m.(map[string]int)
	}
	m := map[string]int{}
	for i := 0; i < t.NumField(); i++ {
		if f := t.Field(i); f.IsExported() && !f.Anonymous {
			m[lowerCamel(f.Name)] = i
		}
	}
	fieldNames.Store(t, m)
	return m
}

// overrideOf is the value WithVars gives the variable name in ctx.
func overrideOf(ctx context.Context, name string) (reflect.Value, bool) {
	if ctx == nil {
		return reflect.Value{}, false
	}
	for o, _ := ctx.Value(overrideKey{}).(*override); o != nil; o = o.up {
		if i, ok := varFields(o.v.Type())[name]; ok {
			return o.v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// browserVars is ui.BrowserVars: the variables of the processors listed as
// Browser, by name, each value read as Var reads it — but those of type
// NotForBrowser, and those of a processor not Personal that read who the
// visitor is.
func browserVars(ctx context.Context) any {
	st := storeOf(ctx)
	if st == nil {
		return nil
	}
	out := map[string]any{}
	for _, p := range st.set.procs {
		if !p.browser {
			continue
		}
		if len(p.gates) > 0 && slices.ContainsFunc(st.set.guards, func(g func(context.Context, []any) error) bool { return g(ctx, p.gates) != nil }) {
			continue
		}
		val, ver, e, err := st.value(ctx, p)
		if err != nil {
			logVarError(ctx, p.name, err)
			continue
		}
		if e.personal && !p.personal {
			logVarError(ctx, p.name, fmt.Errorf("web.Browser(%s) read who the visitor is; it sends nothing to the browser unless it is web.Browser(web.Personal(%[1]s))", p.name))
			continue
		}
		for _, f := range p.fields {
			if f.typ.Implements(notForBrowser) || reflect.PointerTo(f.typ).Implements(notForBrowser) {
				continue
			}
			ui.ReadVar(ctx, f.name, ver)
			out[f.name] = val.Field(f.index).Interface()
		}
	}
	return out
}

var notForBrowser = typeOf[NotForBrowser]()
