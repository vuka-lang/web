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
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/ui/live"
	"github.com/vuka-lang/vuka"
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
func Live(path string, opts ...LiveOption) func(*vuka.Type) {
	return func(t *vuka.Type) {
		ls := &liveSpec{path: path, typ: t.Reflect, name: t.Name, pkg: t.Reflect.PkgPath()}
		for _, o := range opts {
			o.liveOption(ls)
		}
		record(func(r *registry) { r.lives = append(r.lives, ls) })
	}
}

// LiveOption configures a page declared with Live.
type LiveOption interface{ liveOption(*liveSpec) }

type liveOptionFunc func(*liveSpec)

func (f liveOptionFunc) liveOption(s *liveSpec) { f(s) }

func (u Uses) liveOption(s *liveSpec) {
	var mw []Middleware
	u(&vuka.Decl{Func: &mw})
	s.mw = append(s.mw, mw...)
}

// Layout wraps a Live page's component: the page is layout(component).
func Layout(layout func(page ui.Node) ui.Node) LiveOption {
	return liveOptionFunc(func(s *liveSpec) { s.layout = layout })
}

type liveSpec struct {
	path, name, pkg string
	typ             reflect.Type // the struct
	template        reflect.Value
	layout          func(ui.Node) ui.Node
	mw              []Middleware
}

// Live serves component (a pointer to a stateful component, whose fields are
// the defaults) as a page at path, as the Live decorator does.
func (a *App) Live(path string, component ui.Stateful, opts ...LiveOption) *App {
	a.mutable("Live")
	v := reflect.ValueOf(component)
	ls := &liveSpec{path: path, typ: v.Type().Elem(), name: v.Type().Elem().String(), template: v.Elem()}
	for _, o := range opts {
		o.liveOption(ls)
	}
	a.specs = append(a.specs, &routeSpec{method: http.MethodGet, path: path, where: "live page " + ls.name, live: ls, mw: ls.mw})
	return a
}

// LiveLayout is the layout of every Live page without its own.
func LiveLayout(layout func(page ui.Node) ui.Node) Option {
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

func (a *App) compileLive(s *routeSpec) (http.Handler, error) {
	ls := s.live
	fail := func(format string, args ...any) error {
		return fmt.Errorf("web: GET %s (%s): %s", s.path, s.where, fmt.Sprintf(format, args...))
	}
	if ls.typ.Kind() != reflect.Struct || !reflect.PointerTo(ls.typ).Implements(statefulType) {
		return nil, fail("%s is not a stateful component: a struct embedding vuka.Live with a Render() vuka.Node method", ls.name)
	}
	pathNames := pathParams(s.path)
	props, err := a.liveProps(ls.typ, s.where)
	if err != nil {
		return nil, err
	}
	params, err := patchParams(ls.typ)
	if err != nil {
		return nil, fail("%v", err)
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
		title := ls.typ.Name()
		layout = func(page ui.Node) ui.Node {
			return ui.El("html", nil,
				ui.El("head", nil, ui.El("meta", []ui.Attr{{Name: "charset", Value: "utf-8"}}),
					ui.El("meta", []ui.Attr{{Name: "name", Value: "viewport"}, {Name: "content", Value: "width=device-width, initial-scale=1"}}),
					ui.El("title", nil, ui.Text(title))),
				ui.El("body", nil, page))
		}
	}
	site := "web.Live " + s.path
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rq := a.newRequest(w, r)
		if a.tb != nil {
			noteRoute(rq.ctx, "GET "+s.path, s.where)
		}
		end := a.span(rq.ctx, "handler", s.where)
		c := reflect.New(ls.typ)
		if ls.template.IsValid() {
			c.Elem().Set(ls.template)
		}
		if err := a.bindProps(c.Elem(), props, pathNames, r); err != nil {
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
		page := layout(ui.Component(site, nil, comp))
		end()
		a.page(rq, page)
	}), nil
}

// prop is a field of a Live page's component, or of its patch params, that
// binds per request: a dependency injected, or text from the path segment or
// query parameter of its name.
type prop struct {
	index []int
	name  string
	typ   reflect.Type
	dep   bool
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
			props = append(props, prop{f.Index, f.Name, f.Type, true})
		case scalar(f.Type):
			name := f.Name
			if n, _, _ := strings.Cut(f.Tag.Get("form"), ","); n != "" && n != "-" {
				name = n
			}
			props = append(props, prop{f.Index, name, f.Type, false})
		}
	}
	return props, nil
}

// bindProps sets v's props for the request r.
func (a *App) bindProps(v reflect.Value, props []prop, pathNames []string, r *http.Request) error {
	for _, p := range props {
		f := v.FieldByIndex(p.index)
		if p.dep {
			d, err := a.c.get(p.typ)
			if err != nil {
				return err
			}
			f.Set(d)
			continue
		}
		var vals []string
		if n := matchName(pathNames, p.name); n != "" {
			vals = []string{a.router.Param(r, n)}
		} else {
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
// bound again too. Update runs before the patch renders, and an error it
// returns is the patch's, shown as a handler's. Mount, not Update, sees the
// first URL.
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
		if m.Name != "Update" && !strings.HasPrefix(m.Name, "Update__") {
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
		if f, ok := p.typ.FieldByName("URLParams"); !ok || !f.Anonymous || f.Type != urlParamsType {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%s has two Update methods taking web.URLParams structs, %s and %s", t.Name(), found.typ, p.typ)
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
// connects; links (data-vk-link) alone need no script. Under a join or a
// navigation the page is the session's instead.
func (a *App) page(rq *request, n ui.Node) {
	w, r := rq.w, rq.r
	if j, ok := r.Context().Value(joinKey{}).(*join); ok {
		j.page, j.ctx = n, rq.ctx
		return
	}
	var html []byte
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		s := live.NewSession(rq.ctx, func(context.Context) ui.Node { return n })
		end := a.span(rq.ctx, "render", "page")
		out, err := s.Render()
		end()
		s.Close()
		if err != nil {
			a.writeError(w, r, err, true)
			return
		}
		html = []byte(out)
		live := bytes.Contains(html, []byte(" data-vk-id=")) || bytes.Contains(html, []byte(" data-vk-on-")) ||
			bytes.Contains(html, []byte("[&#34;push&#34;,{&#34;h&#34;:")) // a JS command's push to a handler
		if live || bytes.Contains(html, []byte(" data-vk-js-")) {
			html = injectScript(html, a.scriptTag(r, live))
		}
		html = a.linkCSS(r, html)
	} else {
		var b bytes.Buffer
		if n != nil {
			defer a.span(rq.ctx, "render", "page")()
			if err := n.Render(rq.ctx, &b); err != nil {
				a.writeError(w, r, err, true)
				return
			}
		}
		html = b.Bytes()
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.Write(html)
}

func (a *App) scriptTag(r *http.Request, live bool) string {
	if !live {
		return `<script src="` + liveScript + `?v=` + liveJSTag + `" defer></script>`
	}
	var tb string
	if a.tb != nil {
		if e := entryOf(r.Context()); e != nil {
			tb = ` data-vk-toolbar="` + e.ID + `"` // the socket's messages join the page's record
		}
	}
	return `<script src="` + liveScript + `?v=` + liveJSTag + `" data-vk-token="` + a.signToken(r.URL.RequestURI()) + `"` + tb + ` defer></script>`
}

func injectScript(html []byte, tag string) []byte {
	i := bytes.LastIndex(bytes.ToLower(html), []byte("</body>"))
	if i < 0 {
		return append(html, tag...)
	}
	return append(html[:i:i], append([]byte(tag), html[i:]...)...)
}

// A join token names the page a live connection rebuilds: its URI, and when
// the token expires, signed with the app's key.
//
//	base64url(expiry as 8 bytes, big-endian ‖ URI) "." base64url(HMAC-SHA256)
func (a *App) signToken(uri string) string {
	msg := binary.BigEndian.AppendUint64(nil, uint64(time.Now().Add(a.liveTTL).Unix()))
	msg = append(msg, uri...)
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
	uri := string(msg[8:])
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
