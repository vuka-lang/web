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
	type prop struct {
		index []int
		name  string
		typ   reflect.Type
		dep   bool
	}
	var props []prop
	pathNames := pathParams(s.path)
	for _, f := range reflect.VisibleFields(ls.typ) {
		switch {
		case !f.IsExported() || f.Anonymous:
		case a.c.has(f.Type):
			if err := a.c.check(f.Type, s.where); err != nil {
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
		c := reflect.New(ls.typ)
		if ls.template.IsValid() {
			c.Elem().Set(ls.template)
		}
		for _, p := range props {
			f := c.Elem().FieldByIndex(p.index)
			if p.dep {
				v, err := a.c.get(p.typ)
				if err != nil {
					a.writeError(w, r, err, true)
					return
				}
				f.Set(v)
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
					a.writeError(w, r, BadRequest(fmt.Sprintf("%s: %v", p.name, err)), true)
					return
				}
			}
		}
		a.page(rq, layout(ui.Component(site, nil, c.Interface().(ui.Stateful))))
	}), nil
}

func matchName(names []string, name string) string {
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return n
		}
	}
	return ""
}

// page answers with a rendered page. A GET page rendering stateful
// components (or event handlers) is live: the runtime script and a join token
// go before </body>. A page with only JS commands (data-vk-js-*) that push
// to no handler gets the script without a token: it runs them and never
// connects. Under a join the page is the session's instead.
func (a *App) page(rq *request, n ui.Node) {
	w, r := rq.w, rq.r
	if j, ok := r.Context().Value(joinKey{}).(*join); ok {
		j.page, j.ctx = n, rq.ctx
		return
	}
	var html []byte
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		s := live.NewSession(rq.ctx, func(context.Context) ui.Node { return n })
		out, err := s.Render()
		s.Close()
		if err != nil {
			a.writeError(w, r, err, true)
			return
		}
		html = []byte(out)
		if bytes.Contains(html, []byte(" data-vk-")) {
			live := bytes.Contains(html, []byte(" data-vk-id=")) || bytes.Contains(html, []byte(" data-vk-on-")) ||
				bytes.Contains(html, []byte("[&#34;push&#34;,{&#34;h&#34;:")) // a JS command's push to a handler
			html = injectScript(html, a.scriptTag(r, live))
		}
	} else {
		var b bytes.Buffer
		if n != nil {
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
	return `<script src="` + liveScript + `?v=` + liveJSTag + `" data-vk-token="` + a.signToken(r.URL.RequestURI()) + `" defer></script>`
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

func randomKey() []byte {
	k := make([]byte, 32)
	rand.Read(k)
	return k
}
