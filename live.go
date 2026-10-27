package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/vuka/v2/diag"
)

// Live serves a stateful component as a page at path, a GET route:
//
//	@web.Live("/rooms/{room}")
//	type Chat struct {
//		vuka.Live
//		Room  string     // a prop bound from the path segment, else ?room=
//		Store *ChatStore // a prop of a provided type, injected
//		log   []string   // state
//	}
//
// Its exported fields of provided types are injected; the others that hold
// text bind from the path segment or query parameter of their name. The page
// is the component inside the app's layout (LiveLayout), or the one Layout
// names, or else a bare HTML document. Use adds middleware.
//
// On an @ui.Island struct embedding ui.Live it is a live page backed by
// that component (see islandpage.go); the island's ui.Layout wins over
// Layout.
func Live(path string, opts ...LiveOption) func(*vuka.Type) {
	return func(t *vuka.Type) {
		ls := &liveSpec{path: path, typ: t.Reflect, name: t.Name, pkg: t.Reflect.PkgPath(), attrs: flattenAttrs(t.Attrs)}
		for _, o := range opts {
			o.liveOption(ls)
		}
		record(func(r *registry) { r.lives = append(r.lives, ls) })
	}
}

// LiveOption configures a page declared with Live; it is a RouteOption too.
type LiveOption interface {
	RouteOption
	liveOption(*liveSpec)
}

type liveOptionFunc func(*liveSpec)

func (f liveOptionFunc) liveOption(s *liveSpec) { f(s) }
func (f liveOptionFunc) Attrs() []any           { return []any{f} }

func (u Uses) liveOption(s *liveSpec) { s.mw = append(s.mw, u.middleware()...) }

// Layout wraps a Live page's component: the page is layout(component).
// Styles and scripts (Style, Script, CSS, JS) given with it belong to the
// layout's own markup.
func Layout(layout func(page ui.Node) ui.Node, assets ...Asset) LiveOption {
	return liveOptionFunc(func(s *liveSpec) { s.layout = layout })
}

type liveSpec struct {
	path, name, pkg string
	own             bool         // registered on an App: Only doesn't apply
	registered      bool         // by Registry or App: its type's attributes are read at Build
	typ             reflect.Type // the struct
	template        reflect.Value
	ctor            reflect.Value // its constructor, when registered with one
	layout          func(ui.Node) ui.Node
	mw              []Middleware
	mwOf            []useOf    // UseOf's middleware, resolved when the app is built
	attrs           []any      // the type's typed attributes
	island          bool       // a page backed by its island (@web.Live or @web.Get on an @ui.Island type)
	routeOpts       []routeOpt // Attrs, Wrap, Params given with it
}

// route is the page's GET route.
func (ls *liveSpec) route() *routeSpec {
	s := &routeSpec{method: http.MethodGet, path: ls.path, where: "live page " + ls.name, live: ls, mw: slices.Clone(ls.mw), mwOf: slices.Clone(ls.mwOf), attrs: slices.Clone(ls.attrs)}
	if ls.registered {
		s.attrs = append(flattenAttrs(vuka.TypeAttrs(ls.typ)), s.attrs...)
	}
	for _, o := range ls.routeOpts {
		o(s)
	}
	return s
}

// Live serves a stateful component as a page at path, as Routes.Live does
// for every app: page is its constructor (NewBoard), or a pointer to the
// component whose fields are the defaults.
func (a *App) Live(path string, page any, opts ...RouteOption) *App {
	a.mutable("Live")
	a.reg.live(path, page, opts, true)
	return a
}

// LiveLayout is the layout of every Live page without its own. Styles and
// scripts given with it belong to the layout's own markup, as Layout's do.
func LiveLayout(layout func(page ui.Node) ui.Node, assets ...Asset) Option {
	return OptionFunc(func(a *App) { a.liveLayout = layout })
}

// LiveKey is the key signing live pages' join tokens. Without it each app
// draws a random one, so a page rendered by one process can't join another:
// set it when several replicas serve the app.
func LiveKey(key []byte) Option { return OptionFunc(func(a *App) { a.liveKey = key }) }

// LiveOrigins lets pages of other origins open live connections: host
// patterns as path.Match takes them ("app.example.com", "*.example.com").
// By default only the app's own origin may.
func LiveOrigins(patterns ...string) Option {
	return OptionFunc(func(a *App) { a.liveOrigins = append(a.liveOrigins, patterns...) })
}

// LiveProtocol caps the live protocol version the app speaks: 2 (render
// trees: a page gets each component's statics once, then only the dynamics
// that changed) by default, or 1 (each changed component's HTML). A page asks
// for the newest it knows and gets at most this; v2 also needs a Vuka live
// session that speaks it, else pages get v1.
func LiveProtocol(v int) Option { return OptionFunc(func(a *App) { a.liveMax = max(1, v) }) }

// LiveTokenTTL is how long a rendered live page may (re)join; 12 hours by
// default. A page older than that reloads when it reconnects.
func LiveTokenTTL(d time.Duration) Option { return OptionFunc(func(a *App) { a.liveTTL = d }) }

const (
	livePrefix = "/_vuka/"
	liveSocket = livePrefix + "live"
	liveScript = livePrefix + "live.js"
)

//go:embed live.js
var liveJS []byte

var liveJSTag = func() string {
	sum := sha256.Sum256(liveJS)
	return base64.RawURLEncoding.EncodeToString(sum[:6])
}()

var statefulType = typeOf[ui.Stateful]()

// embedsLive reports whether struct t embeds ui.Live.
func embedsLive(t reflect.Type) bool {
	f, ok := t.FieldByName("Live")
	return ok && f.Anonymous && f.Type == typeOf[ui.Live]()
}

func (a *App) compileLive(s *routeSpec) (http.Handler, error) {
	ls := s.live
	what := "GET " + s.path + " (" + whereName(s.where) + ")"
	fail := func(format string, args ...any) *diag.Diagnostic {
		return failf("E0330", what, s.where, format, args...)
	}
	island := ls.island || ls.typ.Kind() == reflect.Struct && ui.IsIsland(ls.typ)
	if island && !ls.island {
		if !reflect.PointerTo(ls.typ).Implements(statefulType) {
			if embedsLive(ls.typ) {
				return nil, fail("%s is a page backed by its island on a live route, and *%[1]s has no Render() ui.Node method", ls.name).WithHelp("add func (p *%s) Render() ui.Node { return ui.IslandPage(p) }", ls.typ.Name())
			}
			return nil, fail("%s is a page backed by its island on a live route (web.Live, @web.Live), and it doesn't embed ui.Live", ls.name).WithHelp("embed ui.Live, or route it with Get (a stateless page)")
		}
		if err := a.checkIsland(ls.typ, ls.ctor); err != nil {
			return nil, failure("E0320", what, s.where, err)
		}
		c := *ls
		c.island = true
		ls = &c
	}
	if ls.typ.Kind() != reflect.Struct || !reflect.PointerTo(ls.typ).Implements(statefulType) {
		if ls.typ.Kind() == reflect.Struct && embedsLive(ls.typ) {
			return nil, failf("E0329", what, s.where, "%s is not a stateful component: *%[1]s has no Render() ui.Node method", ls.name).WithHelp("add func (p *%s) Render() ui.Node", ls.typ.Name())
		}
		return nil, failf("E0329", what, s.where, "%s is not a stateful component", ls.name).WithHelp("embed ui.Live in a struct with a Render() ui.Node method")
	}
	if err := a.checkCtor(ls.ctor, s.where); err != nil {
		return nil, err
	}
	if err := a.checkParams(ls.typ, what, s.where); err != nil {
		return nil, err
	}
	pathNames := pathParams(s.path)
	props, err := a.liveProps(ls.typ, s.where)
	switch {
	case ls.island:
		props, err = a.islandBinds(ls.typ, s.path, s.where)
	case s.page:
		if props, err = a.pageProps(ls.typ, s.path, s.where); err != nil {
			err = failure("E0328", what, s.where, err)
		}
	}
	if err == nil && s.page {
		err = a.checkPathProps(ls.typ, props, pathNames)
	}
	if err != nil {
		return nil, err
	}
	var layouts []*layoutSpec
	if len(s.layouts) > 0 {
		if layouts, err = a.compileLayouts(s, what); err != nil {
			return nil, err
		}
	}
	bind := a.bindProps
	if s.page {
		bind = a.bindPage
	}
	params, err := patchParams(ls.typ)
	if err != nil {
		return nil, failure("E0334", what, s.where, err)
	}
	var paramProps []prop
	if params != nil {
		if paramProps, err = a.liveProps(params.typ, s.where); err != nil {
			return nil, err
		}
	}
	layout := ls.layout
	if layout == nil {
		layout = a.liveLayout
	}
	if layout == nil {
		layout = defaultLayout(ls.typ.Name())
	}
	var islandLayout ui.LayoutFunc
	if ls.island {
		islandLayout = a.islandLayout(ls.typ, ls.layout)
	}
	site := "web.Live " + s.path
	var urlFields []string
	for _, p := range props {
		if !p.dep {
			urlFields = append(urlFields, ls.typ.FieldByIndex(p.index).Name)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rq := a.newRequest(w, r, s)
		if a.tb != nil {
			noteRoute(rq.ctx, "GET "+s.path, s.where)
		}
		end := a.span(rq.ctx, "handler", s.where)
		c, err := a.newComponent(ls.typ, ls.template, ls.ctor)
		if err == nil {
			err = bind(c.Elem(), props, pathNames, r)
		}
		if err != nil {
			end()
			a.writeError(w, r, err, true)
			return
		}
		comp := c.Interface().(ui.Stateful)
		if j, ok := r.Context().Value(joinKey{}).(*join); ok && params != nil {
			pv := reflect.New(params.typ)
			if err := a.bindProps(pv.Elem(), paramProps, pathNames, r); err != nil {
				end()
				a.writeError(w, r, err, true)
				return
			}
			if !params.ptr {
				pv = pv.Elem()
			}
			j.updates = map[ui.Stateful]any{comp: pv.Interface()}
		}
		tag := &ui.ComponentNode{Site: site, Props: comp, Routed: true, URL: urlFields}
		var page ui.Node
		if framed(layouts) {
			if islandLayout != nil {
				rq.ctx = crumbed(ui.WithPageTitle(rq.ctx, pageTitle(c)), c)
			} else {
				rq.ctx = titled(rq.ctx, c)
			}
		}
		loaded, err := a.loadLayouts(rq, layouts, pathNames)
		if err != nil {
			end()
			a.writeError(w, r, err, true)
			return
		}
		if framed(layouts) {
			page = a.nestLayouts(rq, layouts, loaded, tag)
		} else if islandLayout != nil {
			rq.ctx = crumbed(ui.WithPageTitle(rq.ctx, pageTitle(c)), c)
			page = islandLayout(rq.ctx, tag)
		} else {
			if s.page {
				rq.ctx = titled(rq.ctx, c)
			}
			page = layout(tag)
		}
		end()
		a.page(rq, page)
	}), nil
}

// prop is a field of a Live page's component, or of its patch params, that
// binds per request: a dependency injected, or text from the path segment or
// query parameter of its name.
type prop struct {
	index      []int
	name       string
	typ        reflect.Type
	dep        bool
	unexported bool                      // a page-backed island's server-only field
	query      bool                      // only from the query parameter (url:"name")
	path       bool                      // only from the path segment
	check      func(reflect.Value) error // a path parameter's validate: tag (PageRoute)
}

func (a *App) liveProps(t reflect.Type, where string) ([]prop, error) {
	var props []prop
	for _, f := range reflect.VisibleFields(t) {
		switch {
		case !f.IsExported() || f.Anonymous:
		case a.c.has(f.Type):
			if err := a.c.check(f.Type, where); err != nil {
				return nil, err
			}
			a.needs = append(a.needs, f.Type)
			props = append(props, prop{index: f.Index, name: f.Name, typ: f.Type, dep: true})
		case scalar(f.Type):
			name := f.Name
			if n, _, _ := strings.Cut(f.Tag.Get("form"), ","); n != "" && n != "-" {
				name = n
			}
			if u := ui.FieldOptionsOf(t, f).URL; u != "" {
				// @ui.URL("q"): the query parameter it names, which an
				// event assigning the field writes
				props = append(props, prop{index: f.Index, name: u, typ: f.Type, query: true})
				continue
			}
			props = append(props, prop{index: f.Index, name: name, typ: f.Type})
		}
	}
	return props, nil
}

// bindProps sets v's props for the request r.
func (a *App) bindProps(v reflect.Value, props []prop, pathNames []string, r *http.Request) error {
	for _, p := range props {
		f := field(v, p.index)
		if p.dep {
			d, err := a.c.get(p.typ)
			if err != nil {
				return err
			}
			f.Set(d)
			continue
		}
		var vals []string
		if n := matchName(pathNames, p.name); n != "" && !p.query {
			vals = []string{a.router.Param(r, n)}
		} else if !p.path {
			for k, vs := range r.URL.Query() {
				if strings.EqualFold(k, p.name) {
					vals = vs
				}
			}
		}
		if len(vals) > 0 {
			if err := setText(f, vals); err != nil {
				return BadRequest(fmt.Sprintf("%s: %v", p.name, err))
			}
		}
	}
	return nil
}

// URLParams, embedded in a struct, makes it the parameters a Live page's
// Update method takes when the page is patched — moved to another URL of
// its route, the component staying mounted:
//
//	type ShelfParams struct {
//		web.URLParams
//		Page int // ?page=
//	}
//
//	func (s *Shelf) Update(p ShelfParams) { s.load(p.Page) }
//
// Its fields bind from the new URL as the component's props do (path
// segment, else query parameter, of their name); the component's props are
// bound again too. Update runs before the patch renders. Mount, not Update,
// sees the first URL.
//
// Only a page with such an Update stays mounted across a patch, whatever
// the browser asks: any other mounts again, Mount seeing the new URL. So
// Update must make every check Mount makes of what the URL names — that
// the visitor may see order {id}, say. An error it returns fails the
// patch: nothing of the page stays mounted, and the browser loads the URL,
// the route answering as for any request.
//
// An Update taking URLParams itself keeps the page mounted taking nothing
// of the URL: for a page whose components keep their state in it (@ui.URL
// fields, Query), which fill from the new URL as they render.
type URLParams struct{}

var urlParamsType = typeOf[URLParams]()

// params is the type a Live page's Update takes on a patch, a struct
// embedding URLParams (ptr: its pointer).
type params struct {
	typ reflect.Type
	ptr bool
}

// patchParams finds the Update overload of t taking a URLParams struct.
func patchParams(t reflect.Type) (*params, error) {
	var found *params
	pt := reflect.PointerTo(t)
	for i := 0; i < pt.NumMethod(); i++ {
		m := pt.Method(i)
		if !ui.IsUpdate(m) {
			continue
		}
		in := m.Type.In(m.Type.NumIn() - 1)
		p := &params{typ: in}
		if in.Kind() == reflect.Pointer {
			p.typ, p.ptr = in.Elem(), true
		}
		if p.typ.Kind() != reflect.Struct {
			continue
		}
		if f, ok := p.typ.FieldByName("URLParams"); p.typ != urlParamsType && (!ok || !f.Anonymous || f.Type != urlParamsType) {
			continue
		}
		if found != nil {
			return nil, diag.New("E0334", "%s has two Update methods taking web.URLParams structs, %s and %s", t.Name(), found.typ, p.typ).WithHelp("keep one Update taking a struct that embeds web.URLParams")
		}
		found = p
	}
	return found, nil
}

func matchName(names []string, name string) string {
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return n
		}
	}
	return ""
}

// page answers with a rendered page. A full GET page links the app's
// stylesheet (linkCSS). A GET page rendering stateful
// components (or event handlers) is live: the runtime script and a join token
// go before </body>. A page with only JS commands (data-vk-js-*) that push
// to no handler gets the script without a token: it runs them and never
// connects; so does one with links (data-vk-link), which the runtime
// follows in place (nav.go). Under a join or a live navigation the page is
// the session's instead; a navigation over HTTP (NavigateHeader) gets the
// page with what the browser needs to patch it in (navPage).
//
// Another method's page is a fragment, as rendered, unless the browser
// shows it as the document (wholePage: a form posted without script, which
// re-renders the page with its errors): then it is a GET page that isn't
// live — a join would rebuild it with GET — so its script has no token.
func (a *App) page(rq *request, n ui.Node) {
	w, r := rq.w, rq.r
	if rq.route != nil && rq.route.formPost {
		a.formPost(rq, n)
		return
	}
	if j, ok := r.Context().Value(joinKey{}).(*join); ok {
		j.page, j.ctx = n, rq.ctx
		return
	}
	buf := pageBufs.Get().(*bytes.Buffer)
	defer putPageBuf(buf)
	if !wholePage(r) {
		if n != nil {
			defer a.span(rq.ctx, "render", "page")()
			if err := render(rq.ctx, n, buf); err != nil {
				a.writeError(w, r, err, true)
				return
			}
		}
		if err := refusedVars(rq.ctx); err != nil {
			a.WriteError(w, r, err, true)
			return
		}
		writeHTML(w, buf.Bytes())
		return
	}
	// The page renders straight to HTML (ui.RenderPage) unless it meets
	// something live: then a session renders it again, to mount its
	// components and number its handlers.
	end := a.span(rq.ctx, "render", "page")
	info, err := ui.RenderPage(rq.ctx, n, buf)
	html := buf.Bytes()
	var s *live.Session
	if err == nil && (info.Live || info.Opaque && pageLive(html)) {
		s = live.NewSession(rq.ctx, func(context.Context) ui.Node { return n })
		var out string
		if out, err = s.Render(); err == nil {
			buf.Reset()
			buf.WriteString(out)
			html = buf.Bytes()
		}
	}
	end()
	if err == nil {
		if rerr := refusedVars(rq.ctx); rerr != nil {
			if s != nil {
				s.Close()
			}
			a.WriteError(w, r, rerr, true)
			return
		}
	}
	if err != nil {
		if s != nil {
			s.Close()
		}
		a.writeError(w, r, err, true)
		return
	}
	live := s != nil && pageLive(html)
	var token string
	if live && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		token = a.signToken(r.URL.RequestURI())
	}
	nav := navigation(r)
	if nav && !a.navPage(w, r, html, token) {
		if s != nil {
			s.Close()
		}
		return
	}
	if token != "" {
		// the messages this page showed show again when its socket joins
		a.joinMessages.put(token, shownMessages(rq.ctx))
	}
	if token != "" && r.Method == http.MethodGet && a.adoptWindow > 0 {
		// keep the mounted session for the page's join to adopt,
		// so Mount runs once per view (livepark.go)
		window := a.adoptWindow
		if prefetching(r) {
			window = max(window, prefetchWindow) // the page may open any time the browser keeps it
		}
		a.parked.park(token, &parkedPage{s: s, uri: r.URL.RequestURI(), who: a.liveWho(rq.ctx)}, window)
	} else if s != nil {
		s.Close()
	}
	var script string
	if live || (s != nil || info.Opaque) && (bytes.Contains(html, []byte(" data-vk-js-")) || bytes.Contains(html, []byte(" data-vk-b"))) || s == nil && !info.Opaque && info.JS ||
		bytes.Contains(html, []byte(" data-vk-link")) ||
		bytes.Contains(html, []byte(" data-vk-check=")) { // a form's browser checks (ui.Config.BrowserChecks)
		script = a.scriptTag(r, token)
	}
	html = withTitle(html, ui.PageTitle(rq.ctx))
	head := a.headTags(r, html)
	if a.tb != nil && a.assets != nil {
		if e := entryOf(rq.ctx); e != nil {
			e.mu.Lock()
			e.Assets = a.assets.pageAssets(html)
			e.mu.Unlock()
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	if prefetching(r) && w.Header().Get("Cache-Control") == "" {
		// the browser keeps a prefetched page as long as the runtime does
		w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(int(prefetchWindow/time.Second)))
	}
	if nav && token == "" && etagged(w, r, html, []byte(head), []byte(script)) {
		return
	}
	writePage(w, html, head, script)
}

// prefetchWindow is how long the runtime keeps a page it prefetched for a
// link, and so its session parked for the join to adopt.
const prefetchWindow = 30 * time.Second

// prefetching reports whether r is the browser fetching a page ahead of a
// navigation (Sec-Purpose: prefetch), what the runtime asks for a link
// hovered, focused or in view: the page renders as for a visit, its live
// session parked until the page opens, or dropped.
func prefetching(r *http.Request) bool {
	p := r.Header.Get("Sec-Purpose")
	if p == "" {
		p = r.Header.Get("Purpose")
	}
	return strings.HasPrefix(p, "prefetch")
}

// pageLive reports whether a page's HTML is live: it holds stateful
// components, event handlers or a JS command's push to a handler.
func pageLive(html []byte) bool {
	return bytes.Contains(html, []byte(" data-vk-id=")) || bytes.Contains(html, []byte(" data-vk-on-")) ||
		bytes.Contains(html, []byte("[&#34;push&#34;,{&#34;h&#34;:")) // a JS command's push to a handler
}

// writePage writes html with script (the runtime's tag, if any) before its
// last </body>, or at its end, and head (linkCSS's tags) where linkCSS
// puts them, after the script when both go before one </body>.
func writePage(w http.ResponseWriter, html []byte, head, script string) {
	if head == "" && script == "" {
		w.Write(html)
		return
	}
	si, hi := len(html), -1
	if script != "" {
		if i := indexTag(html, "</body>", true); i >= 0 {
			si = i
		}
	}
	if head != "" {
		hi = headAt(html)
	}
	if hi < 0 {
		head = ""
		hi = si
	}
	first, second, fi, li := script, head, si, hi
	if hi < si {
		first, second, fi, li = head, script, hi, si
	}
	w.Write(html[:fi])
	io.WriteString(w, first)
	w.Write(html[fi:li])
	io.WriteString(w, second)
	w.Write(html[li:])
}

func writeHTML(w http.ResponseWriter, html []byte) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.Write(html)
}

// pageBufs are the buffers pages render into.
var pageBufs = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func putPageBuf(b *bytes.Buffer) {
	if b.Cap() > 1<<20 {
		return
	}
	b.Reset()
	pageBufs.Put(b)
}

// wholePage reports whether r's page is a whole document, with the app's
// stylesheet, scripts and toolbar: a GET or HEAD, or a browser loading the
// response as its document — a form posted without script, which
// Sec-Fetch-Dest says ("document", or "iframe" for a form in a frame); from
// a browser that doesn't send it, a request asking for HTML that isn't
// htmx's. A fetch, htmx or the like gets the fragment the handler rendered.
func wholePage(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" {
		return d == "document" || d == "iframe"
	}
	return r.Header.Get("HX-Request") == "" && acceptsHTML(r)
}

// scriptTag is the page's runtime script tag, with token (a live page's
// join token) when the page may connect.
func (a *App) scriptTag(r *http.Request, token string) string {
	var root string
	if t, ok := r.Context().Value(rootKey{}).(reflect.Type); ok {
		root = ` data-vk-root="` + rootID(t) + `"` // the page's root layout: another one is a full page load
	}
	build := ` data-vk-build-id="` + a.buildID() + `"` // a navigation names it (nav.go)
	if a.dev {
		build += ` data-vk-dev` // the runtime says in the console why a navigation loads in full
	}
	if token == "" {
		return `<script src="` + liveScript + `?v=` + liveJSTag + `"` + root + build + ` defer></script>`
	}
	var tb string
	if a.tb != nil {
		if e := entryOf(r.Context()); e != nil {
			tb = ` data-vk-toolbar="` + e.ID + `"` // the socket's messages join the page's record
		}
	}
	return `<script src="` + liveScript + `?v=` + liveJSTag + `" data-vk-token="` + token + `"` + root + tb + build + ` defer></script>`
}

// A join token names the page a live connection rebuilds: its URI, when the
// token expires, and a nonce telling two renders of one URI apart (the key
// a render's session is parked under, livepark.go), signed with the app's
// key.
//
//	base64url(expiry as 8 bytes, big-endian ‖ URI ‖ 0x00 ‖ nonce) "." base64url(HMAC-SHA256)
//
// A token without the nonce (an earlier release's) still verifies.
func (a *App) signToken(uri string) string {
	msg := binary.BigEndian.AppendUint64(nil, uint64(time.Now().Add(a.liveTTL).Unix()))
	msg = append(msg, uri...)
	msg = append(msg, 0)
	var nonce [8]byte
	rand.Read(nonce[:])
	msg = append(msg, nonce[:]...)
	mac := hmac.New(sha256.New, a.liveKey)
	mac.Write(msg)
	return base64.RawURLEncoding.EncodeToString(msg) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

var (
	errBadToken     = errors.New("live: invalid page token")
	errExpiredToken = errors.New("live: the page token expired")
)

func (a *App) verifyToken(tok string) (string, error) {
	m, s, ok := strings.Cut(tok, ".")
	msg, err1 := base64.RawURLEncoding.DecodeString(m)
	sig, err2 := base64.RawURLEncoding.DecodeString(s)
	if !ok || err1 != nil || err2 != nil || len(msg) < 9 {
		return "", errBadToken
	}
	mac := hmac.New(sha256.New, a.liveKey)
	mac.Write(msg)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", errBadToken
	}
	if time.Now().Unix() > int64(binary.BigEndian.Uint64(msg)) {
		return "", errExpiredToken
	}
	rest := msg[8:]
	if i := bytes.IndexByte(rest, 0); i >= 0 {
		rest = rest[:i] // the nonce follows; the URI holds no NUL
	}
	uri := string(rest)
	if !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") {
		return "", errBadToken
	}
	return uri, nil
}

// devLiveKeyEnv names the key vuka run -watch draws for its session, so a
// live page rendered by one build of a development app rejoins the next
// instead of reloading. Only a development app (Dev, VUKA_ENV=development)
// reads it.
const devLiveKeyEnv = "VUKA_DEV_LIVE_KEY"

// devLiveKey is the key of an app without LiveKey: the session's under vuka
// run -watch in development, else a random one.
func devLiveKey(dev bool) []byte {
	if k := os.Getenv(devLiveKeyEnv); dev && len(k) >= 32 {
		return []byte(k)
	}
	return randomKey()
}

func randomKey() []byte {
	k := make([]byte, 32)
	rand.Read(k)
	return k
}
