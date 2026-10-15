package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"strings"
	"time"
	"unsafe"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
)

// Pages backed by a component: @web.Get on an @ui.Island struct.
//
//	@web.Get("/applicants")
//	@ui.Island("components/Applicants.vue")
//	type Applicants struct {
//		ui.Live                  // optional: a live page; without it, stateless
//		Q    string `url:"q"`    // a prop, kept in the address's ?q=
//		Rows []Applicant         // a prop
//		db   *DB                 // the server's own: injected, never sent
//	}
//
//	func (a *Applicants) Mount(ctx context.Context) error          { … }
//	func (a *Applicants) Search(ctx context.Context, q string) error { … }
//
// The page renders the component, mounted on load, with the exported fields
// as its props. The component calls the exported methods (useLive<T>() in
// it). A live page (ui.Live embedded) is a live view whose markup is the
// island: a method runs as a live event on the server's instance, and the
// fields it changes reach the component as new props, with no remount. A
// stateless page keeps nothing: a call posts the method, its arguments and
// the props the page was rendered with — signed, so they can't be forged —
// to the page's URL; the server builds the struct again from them, injects
// its dependencies, runs Mount and the method, and answers with the new
// props. Calls are refused cross-origin and pass the type's gates.

// typePage is a type a route decorator names: a page backed by its island.
type typePage struct {
	method, path string
	typ          *vuka.Type
}

// islandPage is a stateless page's: its type and its fields' bindings.
type islandPage struct {
	typ   reflect.Type
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
		return nil, fmt.Errorf("web: %s %s (%s): a page backed by a component is a GET: @web.Get", p.method, p.path, where)
	}
	if t.Kind() != reflect.Struct || !ui.IsIsland(t) {
		return nil, fmt.Errorf("web: GET %s (%s): @web.Get on a type serves its component: add @ui.Island(\"path/to/Component.vue\")", p.path, where)
	}
	attrs := flattenAttrs(p.typ.Attrs)
	if reflect.PointerTo(t).Implements(statefulType) {
		ls := &liveSpec{path: p.path, typ: t, name: p.typ.Name, pkg: t.PkgPath(), attrs: attrs, island: true}
		return []*routeSpec{ls.route()}, nil
	}
	ip := &islandPage{typ: t, name: p.typ.Name, path: p.path}
	return []*routeSpec{
		{method: http.MethodGet, path: p.path, where: where, attrs: attrs, island: ip, html: true},
		{method: http.MethodPost, path: p.path, where: where + " (its calls)", attrs: attrs, island: ip},
	}, nil
}

// islandBinds are the bindings of a page-backed island's fields: its
// unexported fields of provided types, injected; its exported fields
// tagged url:"name", from that query parameter; its exported fields naming
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
			if q, _, _ := strings.Cut(f.Tag.Get("url"), ","); q != "" && q != "-" {
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

// field is v's field at index, settable even when unexported.
func field(v reflect.Value, index []int) reflect.Value {
	f := v.FieldByIndex(index)
	if !f.CanSet() && f.CanAddr() {
		f = reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
	}
	return f
}

// mountPage runs x's Mount — Mount(), Mount(ctx), either returning an error.
func mountPage(ctx context.Context, x reflect.Value) error {
	m := x.MethodByName("Mount")
	if !m.IsValid() {
		return nil
	}
	var in []reflect.Value
	if m.Type().NumIn() == 1 && m.Type().In(0) == reflect.TypeFor[context.Context]() {
		in = append(in, reflect.ValueOf(ctx))
	} else if m.Type().NumIn() > 0 {
		return fmt.Errorf("web: %s's Mount takes a context.Context or nothing", x.Type().Elem())
	}
	out := m.Call(in)
	if len(out) == 1 && out[0].Type() == errType && !out[0].IsNil() {
		return out[0].Interface().(error)
	}
	return nil
}

// compileIslandPage is a stateless page's GET: the struct built for the
// request, mounted, and rendered as its island with its signed props.
func (a *App) compileIslandPage(s *routeSpec) (http.Handler, error) {
	ip := s.island
	binds, err := a.islandBinds(ip.typ, s.path, s.where)
	if err != nil {
		return nil, err
	}
	ip.binds = binds
	pathNames := pathParams(s.path)
	layout := a.liveLayout
	if layout == nil {
		layout = defaultLayout(ip.typ.Name())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rq := a.newRequest(w, r)
		end := a.span(rq.ctx, "handler", s.where)
		x := reflect.New(ip.typ)
		if err := a.bindProps(x.Elem(), binds, pathNames, r); err != nil {
			end()
			a.writeError(w, r, err, true)
			return
		}
		if err := mountPage(rq.ctx, x); err != nil {
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
		a.page(rq, layout(ui.IslandPage(x.Interface(), ui.CallAt(r.URL.Path, a.signProps(r.URL.Path, props)))))
	}), nil
}

// pageCall is what a stateless page's component posts to call a method.
type pageCall struct {
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args"`
	Props  string          `json:"props"`
	Sig    string          `json:"sig"`
}

var (
	errPageSig     = &Error{Status: http.StatusForbidden, Message: "the page's props aren't the server's"}
	errPageExpired = &Error{Status: http.StatusForbidden, Message: "the page expired: reload it"}
)

// compileIslandCall is a stateless page's calls: the struct built again
// from the signed props, its dependencies injected, Mount and the method
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
		if err := a.verifyProps(r.URL.Path, call.Props, call.Sig); err != nil {
			a.writeError(w, r, err, false)
			return
		}
		m, ok := methods[call.Method]
		if !ok {
			a.writeError(w, r, BadRequest(fmt.Sprintf("%s has no method %q", ip.name, call.Method)), false)
			return
		}
		end := a.span(rq.ctx, "handler", s.where+"."+m.Method.Name)
		x := reflect.New(ip.typ)
		raw, err := ui.UntagProps([]byte(call.Props))
		if err == nil {
			err = json.Unmarshal(raw, x.Interface())
		}
		if err == nil {
			err = a.bindProps(x.Elem(), append(slicesClone(paths), deps...), pathNames, r)
		}
		if err == nil {
			err = mountPage(rq.ctx, x)
		}
		if err == nil {
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
				log.Printf("web: %d POST %s %s: %v", e.Status, r.URL.Path, m.Name, err)
			}
			writeJSON(w, e.Status, map[string]any{"error": e})
			return
		}
		props, err := ui.PropsJSON(rq.ctx, x.Elem().Interface())
		if err != nil {
			a.writeError(w, r, err, false)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"props": string(props), "sig": a.signProps(r.URL.Path, props)})
	})
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.writeError(w, r, errCrossOrigin, false)
	}))
	return cop.Handler(h), nil
}

func slicesClone(p []prop) []prop { return append([]prop(nil), p...) }

// A stateless page's props are signed with the app's key (LiveKey), bound
// to the page's path and an expiry (LiveTokenTTL):
//
//	base64url(expiry, 8 bytes big-endian) "." base64url(HMAC-SHA256("page" 0 path 0 expiry props))
func (a *App) signProps(path string, props []byte) string {
	exp := binary.BigEndian.AppendUint64(nil, uint64(time.Now().Add(a.liveTTL).Unix()))
	return base64.RawURLEncoding.EncodeToString(exp) + "." + base64.RawURLEncoding.EncodeToString(a.propsMAC(path, exp, props))
}

func (a *App) propsMAC(path string, exp, props []byte) []byte {
	mac := hmac.New(sha256.New, a.liveKey)
	io.WriteString(mac, "page\x00"+path+"\x00")
	mac.Write(exp)
	mac.Write([]byte{0})
	mac.Write(props)
	return mac.Sum(nil)
}

func (a *App) verifyProps(path, props, sig string) error {
	e, m, ok := strings.Cut(sig, ".")
	exp, err1 := base64.RawURLEncoding.DecodeString(e)
	mac, err2 := base64.RawURLEncoding.DecodeString(m)
	if !ok || err1 != nil || err2 != nil || len(exp) != 8 {
		return errPageSig
	}
	if !hmac.Equal(mac, a.propsMAC(path, exp, []byte(props))) {
		return errPageSig
	}
	if time.Now().Unix() > int64(binary.BigEndian.Uint64(exp)) {
		return errPageExpired
	}
	return nil
}

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
	ps, err := a.pageSpecs(typePage{http.MethodGet, path, &vuka.Type{Name: t.String(), Reflect: t, Attrs: attrs}})
	if err != nil {
		a.c.errs = append(a.c.errs, err)
		return a
	}
	a.specs = append(a.specs, ps...)
	return a
}
