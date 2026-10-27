package web

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2"
)

// The development toolbar. A development app (Dev, VUKA_ENV=development,
// or run by vuka run, which names its session directory in VUKA_DEV_DIR)
// records each request — its route and handler, the statements it ran
// (NoteStatement), its middleware, decorators, handler and render as spans,
// what it logged through slog, and its live page's socket messages — in a
// ring of the latest requests, and every full page it serves gets a
// script drawing a handle and a drawer of panels for the request, in a
// shadow DOM of its own. Anything else has no toolbar: nothing recorded,
// nothing served, nothing installed.

const (
	toolbarJS    = livePrefix + "toolbar.js"
	toolbarData  = livePrefix + "toolbar/r/{id}"
	toolbarBuild = livePrefix + "toolbar/build"
	// ToolbarHeader names the toolbar record of a development app's
	// response; the toolbar lists a page's fetch and XHR calls by it.
	ToolbarHeader = "X-Vuka-Toolbar"
	// DevDirEnv names the session directory vuka run passes the app: the
	// toolbar is on, and with -watch the directory's build.json says how
	// the next build is going.
	DevDirEnv = "VUKA_DEV_DIR"

	toolbarKeep    = 100  // requests kept
	toolbarLive    = 200  // socket messages kept per page
	toolbarMaxRows = 1000 // statements, spans, log records and notes kept per request
)

//go:embed toolbar.js
var toolbarScript []byte

//go:embed toolbar.css
var toolbarCSS string

//go:embed toolbar-logo.svg
var toolbarLogo []byte

// toolbarSource is the script served, the stylesheet of its shadow DOM in it
// (the Vuka logo in that as a data URL).
var toolbarSource, toolbarTag = func() ([]byte, string) {
	logo := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(toolbarLogo)
	css, _ := json.Marshal(strings.Replace(toolbarCSS, "__LOGO__", logo, 1))
	src := bytes.Replace(toolbarScript, []byte(`"__CSS__"`), css, 1)
	return src, cssTag(src)
}()

// toolbarInTests turns the toolbar on in this package's tests. Elsewhere a
// test binary has none, so tests see the pages the app serves, unless the
// test names a session directory (VUKA_DEV_DIR) as vuka run does.
var toolbarInTests bool

// NoToolbar turns the development toolbar off.
func NoToolbar() Option { return OptionFunc(func(a *App) { a.noToolbar = true }) }

// Toolbar reports whether the app shows the development toolbar: for a
// library feeding it (NoteStatement) only then.
func (a *App) Toolbar() bool { return a.tb != nil }

func (a *App) newToolbar() *toolbar {
	dir := os.Getenv(DevDirEnv)
	if a.noToolbar || !a.dev && dir == "" || testing.Testing() && dir == "" && !toolbarInTests {
		return nil
	}
	return &toolbar{byID: map[string]*tbEntry{}, dir: dir, streams: newStreams()}
}

// toolbar is an app's ring of request records.
type toolbar struct {
	mu   sync.Mutex
	ring [toolbarKeep]*tbEntry
	next int
	byID map[string]*tbEntry
	dir  string

	crons []*tbEntry // the latest cron runs (toolbar_crons.go)

	streams *streams // pages hearing of vuka run -watch's build state
}

// tbEntry is a request's record, or a live page's socket message's.
type tbEntry struct {
	mu sync.Mutex
	tbRecord
	depth int        // spans open
	wire  *wireStats // the response's compression; nil when the app has none
}

type tbRecord struct {
	ID      string
	Method  string // a socket message's type: join, event, navigate, patch, broadcast
	URI     string // a socket message's subject: the handler, URL or topic
	Route   string // "GET /pets/{id}"
	Handler string // "main.ShowPet at pages.vuka:24"
	Status  int
	Start   time.Time
	Dur     time.Duration
	Done    bool
	Earlier bool // a page rendered by an earlier run of the app: its socket's messages only
	Req     http.Header
	Resp    http.Header
	Params  []tbParam
	Queries []Statement
	Spans   []tbSpan
	Logs    []tbLog
	Notes   []tbNote
	Assets  []tbAsset     // the page's scoped components and the app-wide assets
	Facts   []RequestNote // NoteRequest's rows
	// GQLFields and GQLBatches are a GraphQL request's field trace.
	GQLFields  []GraphQLField
	GQLBatches []GraphQLBatch
	Live       []*tbEntry
	Ref        int    // a socket message's ref
	Bytes      int    // its reply's size
	Err        string // its reply's error
	Dropped    int    // rows over toolbarMaxRows
	Seq        int    // a socket message's number, for its frames' views
	Frames     []tbFrame
	// FramesDropped says a socket message's frames were dropped, the
	// page's record keeping the latest messages' (wirePageMax).
	FramesDropped bool
	// Now is the record of the page a live connection opened from this
	// page shows now, after navigating over the socket; "" when it hasn't.
	Now string
	// Encoding is the response's (br, gzip; "" when sent as written), Body
	// the bytes written and Wire those sent.
	Encoding   string
	Body, Wire int64
}

type tbParam struct{ Name, From, Value string }

type tbSpan struct {
	Name, Kind string
	At, Dur    time.Duration
	Depth      int
	Open       bool
}

type tbLog struct {
	At    time.Duration
	Level slog.Level
	Msg   string
	Attrs string
}

type tbNote struct {
	Panel string
	Value any
}

// Statement is a statement for the toolbar's SQL panel: what a database layer
// passes NoteStatement. orm's ormweb reports the ORM's statements by itself.
type Statement struct {
	SQL      string
	Args     []any
	DB       string // the database's name
	Start    time.Time
	Duration time.Duration
	Rows     int64 // rows affected; -1 for a query
	Err      error
	// Refused is a statement the database layer refused before sending it;
	// Err says why.
	Refused bool
	// Origin and Field are what ran it, as StatementOrigin marked its
	// context: a GraphQL field's path and name; NoteStatement fills them.
	Origin, Field string
}

// NoteStatement records a statement the request of ctx (or its live page's
// socket message) ran, for the toolbar's SQL panel. It does nothing for a
// context that no development toolbar watches.
func NoteStatement(ctx context.Context, q Statement) {
	if e := entryOf(ctx); e != nil {
		if o := originOf(ctx); o.field != "" && q.Field == "" {
			q.Origin, q.Field = o.path, o.field
		}
		e.mu.Lock()
		if e.room() {
			e.Queries = append(e.Queries, q)
		}
		e.mu.Unlock()
	}
}

// Note records v for the toolbar's panel named panel, drawn by the
// function AddPanel registered for it, else as a list of the values. It
// does nothing for a context that no development toolbar watches, so it
// can stay in production code.
func Note(ctx context.Context, panel string, v any) {
	if e := entryOf(ctx); e != nil {
		e.mu.Lock()
		if e.room() {
			e.Notes = append(e.Notes, tbNote{panel, v})
		}
		e.mu.Unlock()
	}
}

var (
	panelsMu sync.Mutex
	panels   []customPanel
)

type customPanel struct {
	name   string
	render func(notes []any) ui.Node
}

// AddPanel adds a panel to the development toolbar: render draws it for a
// request from the values the request noted for it (Note), in order. A
// panel added again under the same name replaces it.
func AddPanel(name string, render func(notes []any) ui.Node) {
	panelsMu.Lock()
	defer panelsMu.Unlock()
	if i := slices.IndexFunc(panels, func(p customPanel) bool { return p.name == name }); i >= 0 {
		panels[i].render = render
		return
	}
	panels = append(panels, customPanel{name, render})
}

func (e *tbEntry) room() bool {
	if len(e.Queries)+len(e.Spans)+len(e.Logs)+len(e.Notes)+len(e.GQLFields)+len(e.GQLBatches) < toolbarMaxRows {
		return true
	}
	e.Dropped++
	return false
}

// span opens a span of the entry; the function returned closes it.
func (e *tbEntry) span(kind, name string) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.room() {
		return func() {}
	}
	i := len(e.Spans)
	e.Spans = append(e.Spans, tbSpan{Name: name, Kind: kind, At: time.Since(e.Start), Depth: e.depth, Open: true})
	e.depth++
	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		s := &e.Spans[i]
		s.Dur, s.Open = time.Since(e.Start)-s.At, false
		e.depth--
	}
}

func (e *tbEntry) snapshot() tbRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.tbRecord
	r.Params, r.Queries, r.Spans = slices.Clone(r.Params), slices.Clone(r.Queries), slices.Clone(r.Spans)
	r.Logs, r.Notes, r.Live = slices.Clone(r.Logs), slices.Clone(r.Notes), slices.Clone(r.Live)
	r.Facts = slices.Clone(r.Facts)
	r.GQLFields, r.GQLBatches = slices.Clone(r.GQLFields), slices.Clone(r.GQLBatches)
	r.Frames = slices.Clone(r.Frames)
	r.Encoding, r.Body, r.Wire = e.wire.get()
	if !r.Done {
		r.Dur = time.Since(r.Start)
	}
	return r
}

// A request's context carries its sink: the entry being recorded. A live
// connection's sink moves from message to message.
type tbSink struct {
	tb   *toolbar
	page *tbEntry // a live connection's page: the one on screen
	cur  atomic.Pointer[tbEntry]

	origin *tbEntry            // the page the connection was opened by
	shown  map[string]*tbEntry // the connection's pages, by URI
}

type tbKey struct{}

func entryOf(ctx context.Context) *tbEntry {
	if s := sinkOf(ctx); s != nil {
		return s.cur.Load()
	}
	return nil
}

// sinkOf is ctx's sink, else the one of the request or socket message the
// goroutine serves: decorators of functions without a context, and slog
// calls without one, still reach the request.
func sinkOf(ctx context.Context) *tbSink {
	if ctx != nil {
		if s, ok := ctx.Value(tbKey{}).(*tbSink); ok {
			return s
		}
	}
	if tbBound.Load() == 0 {
		return nil
	}
	s, _ := tbGoroutines.Load(goid())
	sink, _ := s.(*tbSink)
	return sink
}

var (
	tbGoroutines sync.Map // goroutine id → *tbSink, while it serves a request or a socket message
	tbBound      atomic.Int64
)

func bindGoroutine(s *tbSink) (unbind func()) {
	id := goid()
	prev, had := tbGoroutines.Swap(id, s)
	tbBound.Add(1)
	return func() {
		if had { // a cron run by hand while serving a request
			tbGoroutines.Store(id, prev)
		} else {
			tbGoroutines.Delete(id)
		}
		tbBound.Add(-1)
	}
}

// goid is the goroutine's id, from the first line of its stack: for the
// toolbar, in development, and for a live connection (hooks.go).
func goid() uint64 {
	var b [64]byte
	s := b[:runtime.Stack(b[:], false)]
	s = s[len("goroutine "):]
	id, _ := strconv.ParseUint(string(s[:bytes.IndexByte(s, ' ')]), 10, 64)
	return id
}

// parentGoid is the id of the goroutine that started the calling one, from
// the last line of its stack ("created by f in goroutine 7"); 0 when there is
// none (the main goroutine) or the stack doesn't say.
func parentGoid() uint64 {
	buf := make([]byte, 8<<10)
	for {
		n := runtime.Stack(buf, false)
		if n < len(buf) || len(buf) >= 1<<20 {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	i := bytes.LastIndex(buf, []byte("created by "))
	if i < 0 {
		return 0
	}
	line := buf[i:]
	if j := bytes.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	k := bytes.LastIndex(line, []byte(" in goroutine "))
	if k < 0 {
		return 0
	}
	id, _ := strconv.ParseUint(string(bytes.TrimSpace(line[k+len(" in goroutine "):])), 10, 64)
	return id
}

func newToolbarID() string {
	b := make([]byte, 9)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func validToolbarID(id string) bool {
	return len(id) > 0 && len(id) <= 32 && strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") == ""
}

func (tb *toolbar) add(e *tbEntry) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if old := tb.ring[tb.next]; old != nil {
		delete(tb.byID, old.ID)
	}
	tb.ring[tb.next] = e
	tb.next = (tb.next + 1) % toolbarKeep
	tb.byID[e.ID] = e
}

func (tb *toolbar) get(id string) *tbEntry {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.byID[id]
}

// wrap records each request the app serves, but its own /_vuka/ routes and
// the runs of a live page's route for its socket, which record into the
// socket's message.
func (tb *toolbar) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, livePrefix) || r.Context().Value(tbKey{}) != nil {
			next.ServeHTTP(w, r)
			return
		}
		e := &tbEntry{tbRecord: tbRecord{ID: newToolbarID(), Method: r.Method, URI: r.URL.RequestURI(), Start: time.Now(), Req: hideSecrets(r.Header.Clone())}}
		e.wire, _ = r.Context().Value(wireKey{}).(*wireStats)
		tb.add(e)
		s := &tbSink{tb: tb}
		s.cur.Store(e)
		w.Header().Set(ToolbarHeader, e.ID)
		tw := &tbWriter{ResponseWriter: w}
		unbind := bindGoroutine(s)
		defer func() {
			unbind()
			e.mu.Lock()
			e.Status, e.Dur, e.Done, e.Resp = cmp.Or(tw.status, http.StatusOK), time.Since(e.Start), true, hideSecrets(w.Header().Clone())
			e.mu.Unlock()
		}()
		next.ServeHTTP(tw, r.WithContext(context.WithValue(r.Context(), tbKey{}, s)))
	})
}

func hideSecrets(h http.Header) http.Header {
	for _, k := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie"} {
		if vs := h[k]; vs != nil {
			h[k] = []string{"(hidden)"}
		}
	}
	return h
}

// tbWriter notes the response's status.
type tbWriter struct {
	http.ResponseWriter
	status int
}

func (w *tbWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *tbWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *tbWriter) Flush()                      { http.NewResponseController(w.ResponseWriter).Flush() }
func (w *tbWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *tbWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.status = http.StatusSwitchingProtocols
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// spanOf opens a span in ctx's entry: middleware, a handler, a render.
func spanOf(ctx context.Context, kind, name string) func() {
	if e := entryOf(ctx); e != nil {
		return e.span(kind, name)
	}
	return noop
}

// timed wraps a layer of middleware in a span named after it.
func (tb *toolbar) timed(mw Middleware, h http.Handler) http.Handler {
	name := shortName(funcName(reflect.ValueOf(mw)))
	inner := mw(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer spanOf(r.Context(), "middleware", name)()
		inner.ServeHTTP(w, r)
	})
}

// noteRoute records the route serving ctx's request.
func noteRoute(ctx context.Context, route, handler string) {
	if e := entryOf(ctx); e != nil {
		e.mu.Lock()
		e.Route, e.Handler = route, handler
		e.mu.Unlock()
	}
}

// noteParams records the arguments the request's handler was called with.
func noteParams(ctx context.Context, params []tbParam) {
	if e := entryOf(ctx); e != nil {
		e.mu.Lock()
		e.Params = params
		e.mu.Unlock()
	}
}

// paramSource is where a handler's parameter comes from, for the toolbar:
// "" for the request's own values (the context, the request, the writer).
func (a *App) paramSource(p vuka.Param, pathNames []string) tbParam {
	t, from := p.Type, ""
	switch {
	case t == requestType || t == writerType || t == contextType:
	case a.c.has(t):
		from = "dependency"
	case scalar(t) && slices.Contains(pathNames, p.Name):
		from = "path"
	case scalar(t):
		from = "query"
	default:
		from = "body"
	}
	return tbParam{Name: cmp.Or(p.Name, "_"), From: from}
}

// showParams is the arguments of a handler's call as the toolbar shows them.
func showParams(shown []tbParam, args []reflect.Value) []tbParam {
	var out []tbParam
	for i, p := range shown {
		if p.From == "" || i >= len(args) {
			continue
		}
		if p.From == "dependency" {
			p.Value = args[i].Type().String()
		} else {
			p.Value = showValue(args[i])
			if len(p.Value) > 500 {
				p.Value = p.Value[:500] + "…"
			}
		}
		out = append(out, p)
	}
	return out
}

// showValue is an argument as the toolbar shows it: a struct's fields
// named like a password, token or secret hidden.
func showValue(v reflect.Value) string {
	s := v
	for s.Kind() == reflect.Pointer && !s.IsNil() {
		s = s.Elem()
	}
	if s.Kind() != reflect.Struct {
		return fmt.Sprintf("%+v", v.Interface())
	}
	if _, ok := v.Interface().(fmt.Stringer); ok {
		return fmt.Sprintf("%+v", v.Interface())
	}
	var b strings.Builder
	b.WriteByte('{')
	for _, f := range reflect.VisibleFields(s.Type()) {
		if f.Anonymous {
			continue
		}
		if b.Len() > 1 {
			b.WriteByte(' ')
		}
		b.WriteString(f.Name + ":")
		switch {
		case secretName.MatchString(f.Name) || secretName.MatchString(string(f.Tag)):
			b.WriteString("(hidden)")
		case !f.IsExported():
			b.WriteString("…")
		default:
			if fv, err := s.FieldByIndexErr(f.Index); err == nil {
				fmt.Fprintf(&b, "%+v", fv.Interface())
			}
		}
	}
	b.WriteByte('}')
	return b.String()
}

func noop() {}

// span opens a span of the request of ctx when the app has a toolbar.
func (a *App) span(ctx context.Context, kind, name string) func() {
	if a.tb == nil {
		return noop
	}
	return spanOf(ctx, kind, name)
}

// shortName is a function's name without its package path, position and
// closure suffixes: "main.timed", "ormweb.UsingFrom", "(*Counter).Inc".
func shortName(s string) string {
	s, _, _ = strings.Cut(s, " at ")
	s = strings.TrimSuffix(s, "-fm")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		rest := s[i+1:]
		if dot := strings.IndexByte(rest, '.'); dot > 0 && isMajor(rest[:dot]) {
			rest = s[strings.LastIndexByte(s[:i], '/')+1:i] + rest[dot:]
		}
		s = rest
	}
	for {
		i := strings.LastIndex(s, ".func")
		if i < 0 || strings.Trim(s[i+5:], "0123456789.") != "" {
			break
		}
		s = s[:i]
	}
	return s
}

// handlerName names an event handler, a method value of a component:
// "Counter.Inc".
func handlerName(fn any) string {
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func || v.IsNil() {
		return ""
	}
	f := runtime.FuncForPC(v.Pointer())
	if f == nil {
		return ""
	}
	s := shortName(f.Name())
	if _, rest, ok := strings.Cut(s, "."); ok {
		s = rest
	}
	return strings.NewReplacer("(*", "", ")", "").Replace(s)
}

var decoratorNames sync.Map // pc → "@timed"

// observeCall times the steps of decorated calls made for a request: each
// decorator and the function, from vuka.ObserveCalls.
func observeCall(c *vuka.Call, d vuka.Decorator) func() {
	s := sinkOf(c.Context())
	if s == nil {
		return nil
	}
	e := s.cur.Load()
	if e == nil {
		return nil
	}
	if d == nil {
		return e.span("func", strings.TrimPrefix(c.Name, "main."))
	}
	pc := reflect.ValueOf(d).Pointer()
	name, ok := decoratorNames.Load(pc)
	if !ok {
		n := "@decorator"
		if f := runtime.FuncForPC(pc); f != nil {
			n = shortName(f.Name())
			if _, rest, ok := strings.Cut(n, "."); ok {
				n = rest
			}
			n = "@" + n
		}
		name, _ = decoratorNames.LoadOrStore(pc, n)
	}
	return e.span("decorator", name.(string))
}

var installOnce sync.Once

// install hooks the toolbar into the program, once: decorated calls and
// slog's default logger.
func (tb *toolbar) install() {
	installOnce.Do(func() { vuka.ObserveCalls(observeCall) })
	if _, ours := slog.Default().Handler().(*tbLogHandler); ours {
		return
	}
	next := slog.Default().Handler()
	builtin := fmt.Sprintf("%T", next) == "*slog.defaultHandler"
	w, flags, prefix := log.Writer(), log.Flags(), log.Prefix()
	slog.SetDefault(slog.New(&tbLogHandler{next: next}))
	if builtin {
		// SetDefault sent the log package's output to slog; slog's built-in
		// handler writes through the log package: put it back as it was.
		log.SetOutput(w)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	}
}

// tbLogHandler records what slog's default logger writes for a request,
// then passes it on.
type tbLogHandler struct {
	next  slog.Handler
	attrs string
	group string
}

func (h *tbLogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l) || entryOf(ctx) != nil
}

func (h *tbLogHandler) Handle(ctx context.Context, r slog.Record) error {
	if e := entryOf(ctx); e != nil {
		var b strings.Builder
		b.WriteString(h.attrs)
		r.Attrs(func(a slog.Attr) bool {
			writeAttr(&b, h.group, a)
			return true
		})
		e.mu.Lock()
		if e.room() {
			e.Logs = append(e.Logs, tbLog{At: r.Time.Sub(e.Start), Level: r.Level, Msg: r.Message, Attrs: strings.TrimSpace(b.String())})
		}
		e.mu.Unlock()
	}
	if h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

func writeAttr(b *strings.Builder, group string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		g := a.Key
		if group != "" {
			g = group + "." + g
		}
		for _, x := range a.Value.Group() {
			writeAttr(b, g, x)
		}
		return
	}
	key := a.Key
	if group != "" {
		key = group + "." + key
	}
	fmt.Fprintf(b, " %s=%s", key, strconv.Quote(a.Value.String()))
}

func (h *tbLogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range as {
		writeAttr(&b, h.group, a)
	}
	return &tbLogHandler{next: h.next.WithAttrs(as), attrs: b.String(), group: h.group}
}

func (h *tbLogHandler) WithGroup(name string) slog.Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &tbLogHandler{next: h.next.WithGroup(name), attrs: h.attrs, group: g}
}

// tag is the script a full page gets for the toolbar of its request.
func (tb *toolbar) tag(r *http.Request) string {
	e := entryOf(r.Context())
	if e == nil || r.Context().Value(joinKey{}) != nil || navigation(r) {
		return ""
	}
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "", "document", "iframe", "frame":
	default:
		return "" // a fetch of a page, not one the browser shows
	}
	t := `<script src="` + toolbarJS + `?v=` + toolbarTag + `" data-vk-toolbar="` + e.ID + `"`
	if tb.dir != "" {
		t += ` data-vk-build`
	}
	return t + ` defer></script>`
}

// routeToolbar serves the script, the records and the build's state.
func (a *App) routeToolbar() {
	tb := a.tb
	a.router.Handle(http.MethodGet, toolbarJS, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveTagged(w, r, "text/javascript; charset=utf-8", toolbarTag, toolbarSource)
	}))
	a.router.Handle(http.MethodGet, toolbarData, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		e := tb.get(a.router.Param(r, "id"))
		if e == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "the toolbar keeps the latest " + strconv.Itoa(toolbarKeep) + " requests; this one is gone"})
			return
		}
		rec := e.snapshot()
		if q := r.URL.Query(); q.Has("wire") {
			seq, _ := strconv.Atoi(q.Get("wire"))
			i := slices.IndexFunc(rec.Live, func(m *tbEntry) bool { return m.Seq == seq })
			if i < 0 {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "the toolbar keeps the latest " + strconv.Itoa(toolbarLive) + " messages of a page; this one is gone"})
				return
			}
			var b bytes.Buffer
			if err := renderWire(&b, rec.Live[i].snapshot()); err != nil {
				fmt.Fprintf(&b, "<p class=errtext>%s</p>", template.HTMLEscapeString(err.Error()))
			}
			writeJSON(w, http.StatusOK, map[string]any{"html": b.String()})
			return
		}
		out := summary(rec)
		if r.URL.Query().Has("panels") {
			var b bytes.Buffer
			if err := renderPanels(&b, rec, append(a.cronPanel(), a.graphiqlPanel()...)...); err != nil {
				fmt.Fprintf(&b, "<p class=err>%s</p>", err)
			}
			out["html"] = b.String()
		}
		writeJSON(w, http.StatusOK, out)
	}))
	a.router.Handle(http.MethodGet, toolbarBuild, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			// The page's stream: the state when it connects, then each change.
			tb.streams.serve(w, r, func() (string, bool) {
				st := readBuildState(tb.dir, a.assets)
				st.Elapsed = 0 // the page counts from Since
				b, _ := json.Marshal(st)
				return string(b), true
			})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, readBuildState(tb.dir, a.assets))
	}))
	a.routeCrons()
}

// buildState is how vuka run -watch's next build is going, from its
// session directory: "building" since a time, "failed" with what the
// build said, "ok"; "" outside vuka run -watch.
type buildState struct {
	State   string    `json:"state"`
	Since   time.Time `json:"since,omitzero"`
	At      time.Time `json:"at,omitzero"`
	Took    float64   `json:"took,omitempty"`
	Output  string    `json:"output,omitempty"`
	Elapsed float64   `json:"elapsed,omitempty"` // seconds building, when read
	// Assets is the last compile of the styles, scripts and islands when it
	// failed: the last good ones keep serving.
	Assets *assetsFailure `json:"assets,omitempty"`
}

func readBuildState(dir string, as *assetSet) buildState {
	var st buildState
	if dir == "" {
		return st
	}
	st.Assets = as.failure()
	b, err := os.ReadFile(filepath.Join(dir, "build.json"))
	if err != nil || json.Unmarshal(b, &st) != nil {
		return buildState{Assets: st.Assets}
	}
	if st.State == "building" && !st.Since.IsZero() {
		st.Elapsed = time.Since(st.Since).Seconds()
	}
	return st
}

// liveSink is the sink of a live connection opened by the page whose
// record is id: its messages are recorded under the page — or under the page
// it navigated to since, on a reconnect. A page an earlier run of the app
// rendered (vuka run -watch swapped the build) gets a record of its own
// under the same id.
func (tb *toolbar) liveSink(id, uri string) *tbSink {
	if !validToolbarID(id) {
		return nil
	}
	e := tb.get(id)
	if e == nil {
		e = &tbEntry{tbRecord: tbRecord{ID: id, Method: http.MethodGet, URI: uri, Start: time.Now(), Done: true, Earlier: true}}
		tb.add(e)
	}
	s := &tbSink{tb: tb, page: e, origin: e, shown: map[string]*tbEntry{}}
	e.mu.Lock()
	now, at := e.Now, e.URI
	e.mu.Unlock()
	s.shown[at] = e
	if p := tb.get(now); p != nil && now != "" {
		s.page = p
		p.mu.Lock()
		s.shown[p.URI] = p
		p.mu.Unlock()
	}
	return s
}

var tbSeq atomic.Int64

// message starts recording a socket message under the page; client is its
// frame (nil for a broadcast).
func (s *tbSink) message(typ, subject string, ref int, client []byte) *tbEntry {
	m := &tbEntry{tbRecord: tbRecord{Method: typ, URI: subject, Ref: ref, Start: time.Now(), Seq: int(tbSeq.Add(1))}}
	if client != nil {
		m.Frames = []tbFrame{newFrame("client", client)}
	}
	s.page.addLive(m)
	s.cur.Store(m)
	return m
}

func (p *tbEntry) addLive(m *tbEntry) {
	p.mu.Lock()
	if len(p.Live) >= toolbarLive {
		p.Live = slices.Delete(p.Live, 0, 1)
	}
	p.Live = append(p.Live, m)
	p.mu.Unlock()
}

// done ends a socket message: frames are the server frames it sent.
func (s *tbSink) done(m *tbEntry, frames []tbFrame) {
	m.mu.Lock()
	m.Dur, m.Done = time.Since(m.Start), true
	m.Frames = append(m.Frames, frames...)
	if n := len(frames); n > 0 {
		last := frames[n-1]
		m.Bytes = last.Size
		if r := last.Report; r != nil && r.Type == "error" {
			m.Err = cmp.Or(r.Error, "error")
		} else if r != nil {
			m.Err = r.Error
		}
		if m.Method == "broadcast" && slices.IndexFunc(frames, func(f tbFrame) bool { return f.Report != nil && len(f.Report.Insts) > 0 }) < 0 &&
			slices.IndexFunc(frames, func(f tbFrame) bool { return f.Report != nil && len(f.Report.JS) > 0 }) >= 0 {
			m.Method = "push" // JS commands only
		}
	}
	m.mu.Unlock()
	s.cur.Store(nil)
	s.page.trimFrames()
}

// navigated follows the page to uri, where the socket message m moved it:
// a patch updates the page's URI and parameters; another page gets a
// record of its own — the one it had when the connection showed it before
// and the record is still kept — holding m's server work, and m.
func (s *tbSink) navigated(m *tbEntry, uri string, patch bool, h http.Header) {
	r := m.snapshot()
	if patch {
		p := s.page
		p.mu.Lock()
		p.URI = uri
		if r.Route != "" {
			p.Route, p.Handler, p.Params = r.Route, r.Handler, r.Params
		}
		p.mu.Unlock()
		s.shown[uri] = p
		return
	}
	old := s.page
	p := s.shown[uri]
	if p == nil || s.tb.get(p.ID) != p {
		p = &tbEntry{tbRecord: tbRecord{ID: newToolbarID()}}
		s.tb.add(p)
		s.shown[uri] = p
	}
	req := hideSecrets(h.Clone())
	for _, k := range upgradeHeaders {
		req.Del(k)
	}
	req.Set("X-Vuka-Live", "navigate")
	p.mu.Lock()
	p.Method, p.URI, p.Route, p.Handler, p.Params = http.MethodGet, uri, r.Route, r.Handler, r.Params
	p.Queries, p.Spans, p.Logs, p.Notes, p.Dropped = r.Queries, r.Spans, r.Logs, r.Notes, r.Dropped
	p.Status, p.Start, p.Dur, p.Done, p.Earlier, p.Req, p.Resp = http.StatusOK, r.Start, r.Dur, true, false, req, nil
	p.mu.Unlock()
	if old != p {
		old.mu.Lock()
		if i := slices.Index(old.Live, m); i >= 0 {
			old.Live = slices.Delete(old.Live, i, i+1)
		}
		old.mu.Unlock()
		p.addLive(m)
	}
	s.page = p
	s.origin.mu.Lock()
	s.origin.Now = p.ID
	s.origin.mu.Unlock()
}
