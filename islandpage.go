package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	htmlpkg "html"
	"io"
	"log"
	"net/http"
	"reflect"
	"strings"
	"time"
	"unsafe"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/vuka/diag"
)

// Pages backed by a component: @web.Live (a live page, embedding ui.Live)
// or @web.Get (a stateless page) on an @ui.Island struct.
//
//	@web.Live("/applicants")
//	@ui.Island("components/Applicants.vue", ui.Layout(layouts.Page("Applicants")))
//	type Applicants struct {
//		ui.Live                  // a live page; @web.Get without it is stateless
//		Q    string `url:"q" bind:"q"` // a prop, kept in the address's ?q=; the component may set it
//		Rows []Applicant         // a prop
//		db   *DB                 // the server's own: injected, never sent
//	}
//
//	@ui.Mount
//	func (a *Applicants) mount(ctx context.Context) error           { … }
//
//	func (a *Applicants) Search(ctx context.Context, q string) error { … }
//
// The page renders the component, mounted on load, with the exported fields
// as its props. The component calls the exported methods (useLive<T>() in
// it), and sets the fields tagged bind: (live.q.set(v)), after which the
// server runs its @ui.OnChange hook. A live page (ui.Live embedded) is a live view whose markup is the
// island: a method runs as a live event on the server's instance, and the
// fields it changes reach the component as new props, with no remount. A
// stateless page keeps nothing: a call posts the method and its arguments
// (or the fields set) and
// the props the page was rendered with — signed for the page and the
// visitor, so they can't be forged or replayed by someone else — to the
// page's URL; the server builds the struct again from them, injects
// its dependencies, runs its mount hook and the method, and answers with the new
// props. Calls are refused cross-origin and pass the type's gates.

// typePage is a type a route decorator names: a page backed by its island.
type typePage struct {
	method, path string
	typ          *vuka.Type
	ctor         reflect.Value // its constructor, when registered with one
}

// islandPage is a stateless page's: its type and its fields' bindings.
type islandPage struct {
	typ   reflect.Type
	ctor  reflect.Value
	name  string
	path  string
	binds []prop
}

// pageSpecs are a typed page's routes: a live page's, or a stateless page's
// GET and its calls' POST.
func (a *App) pageSpecs(p typePage) ([]*routeSpec, error) {
	t := p.typ.Reflect
	where := "page " + p.typ.Name
	if p.method != http.MethodGet {
		return nil, diag.New("E0331", "%s %s (%s): a page backed by a component is a GET", p.method, p.path, where).WithHelp("route the type with @web.Get or @web.Live")
	}
	if t.Kind() != reflect.Struct || !ui.IsIsland(t) {
		return nil, diag.New("E0332", "GET %s (%s): @web.Get on a type serves its component, and %s has no @ui.Island", p.path, where, p.typ.Name).WithHelp("add @ui.Island (or @ui.Island(\"path/to/Component.vue\")) to the type")
	}
	if err := a.checkIsland(t, p.ctor); err != nil {
		return nil, failure("E0320", "GET "+p.path+" ("+where+")", "", err)
	}
	attrs := flattenAttrs(p.typ.Attrs)
	if reflect.PointerTo(t).Implements(statefulType) {
		ls := &liveSpec{path: p.path, typ: t, name: p.typ.Name, pkg: t.PkgPath(), attrs: attrs, island: true, ctor: p.ctor}
		return []*routeSpec{ls.route()}, nil
	}
	ip := &islandPage{typ: t, ctor: p.ctor, name: p.typ.Name, path: p.path}
	return []*routeSpec{
		{method: http.MethodGet, path: p.path, where: where, attrs: attrs, island: ip, html: true},
		{method: http.MethodPost, path: p.path, where: where + " (its calls)", attrs: attrs, island: ip},
	}, nil
}

// islandBinds are the bindings of a page-backed island's fields: its
// unexported fields of provided types, injected; its exported fields its
// LiveSchema gives a URL (ui.URL("name")), or tagged url:"name", from that
// query parameter; its exported fields naming
// a path segment, from the path.
func (a *App) islandBinds(t reflect.Type, path, where string) ([]prop, error) {
	var props []prop
	names := pathParams(path)
	for _, f := range reflect.VisibleFields(t) {
		if f.Anonymous {
			continue
		}
		switch {
		case !f.IsExported():
			if !a.c.has(f.Type) {
				continue
			}
			if err := a.c.check(f.Type, where); err != nil {
				return nil, err
			}
			a.needs = append(a.needs, f.Type)
			props = append(props, prop{index: f.Index, name: f.Name, typ: f.Type, dep: true, unexported: true})
		case !scalar(f.Type):
		default:
			q, _, _ := strings.Cut(f.Tag.Get("url"), ",")
			if u := ui.FieldOptionsOf(t, f).URL; u != "" {
				q = u
			}
			if q != "" && q != "-" {
				props = append(props, prop{index: f.Index, name: q, typ: f.Type, query: true})
				continue
			}
			jn, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if n := matchName(names, f.Name); n != "" || jn != "" && matchName(names, jn) != "" {
				name := f.Name
				if n == "" {
					name = jn
				}
				props = append(props, prop{index: f.Index, name: name, typ: f.Type, path: true})
			}
		}
	}
	return props, nil
}

// field is v's field at index, settable even when unexported; a nil
// embedded struct pointer on the way is allocated.
func field(v reflect.Value, index []int) reflect.Value {
	f := v
	for i, x := range index {
		if i > 0 && f.Kind() == reflect.Pointer {
			if f.IsNil() {
				settable(f).Set(reflect.New(f.Type().Elem()))
			}
			f = f.Elem()
		}
		f = settable(f.Field(x))
	}
	return f
}

func settable(f reflect.Value) reflect.Value {
	if !f.CanSet() && f.CanAddr() {
		return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
	}
	return f
}

// compileIslandPage is a stateless page's GET: the struct built for the
// request, mounted, and rendered as its island with its signed props.
func (a *App) compileIslandPage(s *routeSpec) (http.Handler, error) {
	ip := s.island
	binds, err := a.islandBinds(ip.typ, s.path, s.where)
	if err == nil {
		err = a.checkCtor(ip.ctor, s.where)
	}
	if err != nil {
		return nil, err
	}
	ip.binds = binds
	pathNames := pathParams(s.path)
	layout := a.islandLayout(ip.typ, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rq := a.newRequest(w, r)
		end := a.span(rq.ctx, "handler", s.where)
		x, err := a.newComponent(ip.typ, reflect.Value{}, ip.ctor)
		if err == nil {
			err = a.bindProps(x.Elem(), binds, pathNames, r)
		}
		if err != nil {
			end()
			a.writeError(w, r, err, true)
			return
		}
		if err := ui.MountPage(rq.ctx, x.Interface()); err != nil {
			end()
			a.writeError(w, r, err, true)
			return
		}
		props, err := ui.PropsJSON(rq.ctx, x.Elem().Interface())
		if err != nil {
			end()
			a.writeError(w, r, err, true)
			return
		}
		end()
		rq.ctx = ui.WithPageTitle(rq.ctx, pageTitle(x))
		a.page(rq, layout(rq.ctx, ui.IslandPage(x.Interface(), ui.CallAt(r.URL.Path, a.signProps(rq.ctx, ip, r.URL.Path, props)))))
	}), nil
}

// pageCall is what a stateless page's component posts to call a method.
type pageCall struct {
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args"`
	Set    json.RawMessage `json:"set"`  // the bound fields the component set
	Load   bool            `json:"load"` // its deferred fields, asked for once it mounts
	Props  string          `json:"props"`
	Sig    string          `json:"sig"`
}

var (
	errPageSig     = &Error{Status: http.StatusForbidden, Message: "the page's props aren't the server's"}
	errPageExpired = &Error{Status: http.StatusForbidden, Message: "the page expired: reload it"}
)

// compileIslandCall is a stateless page's calls: the struct built again
// from the signed props, its dependencies injected, its mount hook and the method
// run, and the props it leaves sent back, signed.
func (a *App) compileIslandCall(s *routeSpec) (http.Handler, error) {
	ip := s.island
	binds, err := a.islandBinds(ip.typ, s.path, s.where)
	if err != nil {
		return nil, err
	}
	methods := map[string]ui.IslandMethod{}
	for _, m := range ui.IslandMethods(ip.typ) {
		methods[m.Name] = m
	}
	pathNames := pathParams(s.path)
	var deps, paths []prop
	for _, b := range binds {
		switch {
		case b.dep:
			deps = append(deps, b)
		case b.path:
			paths = append(paths, b)
		}
	}
	cop := http.NewCrossOriginProtection()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rq := a.newRequest(w, r)
		var call pageCall
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&call); err != nil {
			a.writeError(w, r, BadRequest("invalid call: "+err.Error()), false)
			return
		}
		if err := a.verifyProps(rq.ctx, ip, r.URL.Path, call.Props, call.Sig); err != nil {
			a.writeError(w, r, err, false)
			return
		}
		m, ok := methods[call.Method]
		set := len(call.Set) > 0 && string(call.Set) != "null"
		if !ok && (call.Method != "" || !set && !call.Load) {
			a.writeError(w, r, BadRequest(fmt.Sprintf("%s has no method %q", ip.name, call.Method)), false)
			return
		}
		what := m.Method.Name
		switch {
		case call.Load:
			what = "load"
		case !ok:
			what = "set"
		}
		end := a.span(rq.ctx, "handler", s.where+"."+what)
		x, err := a.newComponent(ip.typ, reflect.Value{}, ip.ctor)
		loads := ui.LoadsFrom(ip.typ, []byte(call.Props))
		var raw []byte
		if err == nil {
			raw, err = ui.UntagProps([]byte(call.Props))
		}
		if err == nil {
			err = json.Unmarshal(raw, x.Interface())
		}
		if err == nil {
			err = a.bindProps(x.Elem(), append(slicesClone(paths), deps...), pathNames, r)
		}
		if err == nil {
			err = ui.MountPage(rq.ctx, x.Interface())
		}
		if err == nil && call.Load {
			loads.Load(rq.ctx, x, func(err error) string { return a.loadErrorText(r, ip, err) })
		}
		if err == nil && set {
			err = ui.SetBound(rq.ctx, x, call.Set)
		}
		if err == nil && ok {
			err = m.Call(rq.ctx, x, call.Args)
		}
		end()
		var to Redirect
		switch {
		case errors.As(err, &to):
			writeJSON(w, http.StatusOK, map[string]any{"redirect": string(to)})
			return
		case err != nil:
			e := ui.ErrorOf(err)
			if e.Status == 0 {
				e.Status = http.StatusInternalServerError
				if a.dev && e.Message == "error" {
					e.Message = err.Error()
				}
			}
			if e.Status >= 500 {
				log.Printf("web: %d POST %s %s: %v", e.Status, r.URL.Path, what, err)
			}
			writeJSON(w, e.Status, map[string]any{"error": e})
			return
		}
		props, err := ui.PropsJSON(rq.ctx, x.Elem().Interface(), ui.WithLoads(loads))
		if err != nil {
			a.writeError(w, r, err, false)
			return
		}
		res := map[string]any{"props": string(props), "sig": a.signProps(rq.ctx, ip, r.URL.Path, props)}
		if call.Load {
			res["defer"] = loads.JSON(ip.typ)
		}
		writeJSON(w, http.StatusOK, res)
	})
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.writeError(w, r, errCrossOrigin, false)
	}))
	return cop.Handler(h), nil
}

// loadErrorText is a stateless page's failed loader's error as its
// component reads it (live.stats.error): a public error's message, in
// development any error's, else "error", the error logged.
func (a *App) loadErrorText(r *http.Request, ip *islandPage, err error) string {
	if e := ui.ErrorOf(err); e.Message != "error" {
		return e.Message
	}
	if a.dev {
		return err.Error()
	}
	log.Printf("web: POST %s load (%s): %v", r.URL.Path, ip.name, err)
	return "error"
}

func slicesClone(p []prop) []prop { return append([]prop(nil), p...) }

// A stateless page's props are signed with the app's key (LiveKey), bound
// to the page's type and path, the visitor (LiveIdentity: their user and
// sign-in, or no one) and an expiry (LiveTokenTTL):
//
//	base64url(expiry, 8 bytes big-endian) "." base64url(HMAC-SHA256("page" type path visitor expiry props))
//
// each of type, path and visitor length-prefixed. Props replayed by another
// visitor, after a sign-out or to another page don't verify.
func (a *App) signProps(ctx context.Context, ip *islandPage, path string, props []byte) string {
	exp := binary.BigEndian.AppendUint64(nil, uint64(time.Now().Add(a.liveTTL).Unix()))
	return base64.RawURLEncoding.EncodeToString(exp) + "." + base64.RawURLEncoding.EncodeToString(a.propsMAC(ctx, ip, path, exp, props))
}

func (a *App) propsMAC(ctx context.Context, ip *islandPage, path string, exp, props []byte) []byte {
	mac := hmac.New(sha256.New, a.liveKey)
	io.WriteString(mac, "page\x00")
	for _, s := range []string{ip.typ.PkgPath() + "." + ip.typ.Name(), path, a.liveWho(ctx)} {
		mac.Write(binary.AppendUvarint(nil, uint64(len(s))))
		io.WriteString(mac, s)
	}
	mac.Write(exp)
	mac.Write(props)
	return mac.Sum(nil)
}

func (a *App) verifyProps(ctx context.Context, ip *islandPage, path, props, sig string) error {
	e, m, ok := strings.Cut(sig, ".")
	exp, err1 := base64.RawURLEncoding.DecodeString(e)
	mac, err2 := base64.RawURLEncoding.DecodeString(m)
	if !ok || err1 != nil || err2 != nil || len(exp) != 8 {
		return errPageSig
	}
	if !hmac.Equal(mac, a.propsMAC(ctx, ip, path, exp, []byte(props))) {
		return errPageSig
	}
	if time.Now().Unix() > int64(binary.BigEndian.Uint64(exp)) {
		return errPageExpired
	}
	return nil
}

// islandLayout is the layout of a page backed by t's island: the
// island's own (@ui.Island(ui.Layout(…))), else the route's (web.Layout),
// else the app's (web.LiveLayout), else a bare document titled
// ui.PageTitle.
func (a *App) islandLayout(t reflect.Type, route func(ui.Node) ui.Node) ui.LayoutFunc {
	if l := ui.IslandLayout(t); l != nil {
		return l
	}
	l := route
	if l == nil {
		l = a.liveLayout
	}
	if l != nil {
		return func(_ context.Context, page ui.Node) ui.Node { return l(page) }
	}
	return func(ctx context.Context, page ui.Node) ui.Node { return defaultLayout(ui.PageTitle(ctx))(page) }
}

// pageTitle is the title of the page x (a pointer to an island's struct)
// backs: its Title(), else its type's name.
func pageTitle(x reflect.Value) string {
	if t, ok := x.Interface().(interface{ Title() string }); ok {
		return t.Title()
	}
	return x.Type().Elem().Name()
}

// withTitle gives a document whose head has no <title> one.
func withTitle(html []byte, title string) []byte {
	if title == "" {
		return html
	}
	head := indexFold(html, "<head")
	if head < 0 || head+5 >= len(html) || html[head+5] != '>' && html[head+5] != ' ' {
		return html
	}
	open := bytes.IndexByte(html[head:], '>')
	if open < 0 {
		return html
	}
	open += head + 1
	end := indexFold(html[open:], "</head")
	if end < 0 || indexFold(html[open:open+end], "<title") >= 0 {
		return html
	}
	t := "<title>" + htmlpkg.EscapeString(title) + "</title>"
	out := make([]byte, 0, len(html)+len(t))
	out = append(out, html[:open]...)
	out = append(out, t...)
	return append(out, html[open:]...)
}

func indexFold(b []byte, s string) int { return bytes.Index(b, []byte(s)) }

func defaultLayout(title string) func(ui.Node) ui.Node {
	return func(page ui.Node) ui.Node {
		return ui.El("html", nil,
			ui.El("head", nil, ui.El("meta", []ui.Attr{{Name: "charset", Value: "utf-8"}}),
				ui.El("meta", []ui.Attr{{Name: "name", Value: "viewport"}, {Name: "content", Value: "width=device-width, initial-scale=1"}}),
				ui.El("title", nil, ui.Text(title))),
			ui.El("body", nil, page))
	}
}

func init() {
	ui.IsRedirect = func(err error) bool {
		var to Redirect
		return errors.As(err, &to)
	}
}

// Page serves a page backed by component's island at path, as @web.Get on
// its type does: component is a pointer to the @ui.Island struct (only its
// type counts). attrs are the type's attributes as a decorator would give
// them (auth gates…).
func (a *App) Page(path string, component any, attrs ...any) *App {
	a.mutable("Page")
	t := reflect.TypeOf(component)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	ps, err := a.pageSpecs(typePage{http.MethodGet, path, &vuka.Type{Name: t.String(), Reflect: t, Attrs: attrs}, reflect.Value{}})
	if err != nil {
		a.c.errs = append(a.c.errs, err)
		return a
	}
	a.specs = append(a.specs, ps...)
	return a
}
