package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/vuka-lang/vuka/v2"
)

// In-place navigation for pages that aren't live: a click on a ui.Link
// (data-vk-link) on a page the runtime runs without a connection is
// fetched with the header NavigateHeader, its value the build the page
// was rendered by (the runtime's script tag says, data-vk-build-id). The
// route runs as for a page load — guards, every layout's Load, the page —
// and answers with the page's HTML, which the browser patches into
// <body> in place; a few response headers tell it what it needs to:
//
//	Vk-Page    the page's root layout (data-vk-root), "-" for none: a
//	           page under another one is another document, a page load
//	Vk-Live    a live page's join token: the runtime joins it
//	Vk-Reload  why the browser must load the URL instead: the build
//	           changed ("build"), the route asks for it ("route", FullLoad),
//	           the page's body has scripts of its own ("scripts")
//	Vk-Reload-Detail  in development, why in words, for the browser's
//	           console and the toolbar (the terminal says it too, once)
//
// A response without Vk-Page (a redirect elsewhere, a file, JSON, a page
// the app wrote itself) is the browser's to load too. Nothing in the
// request is trusted for auth or rendering: the header only picks the
// response's format. A GET without it is answered as ever.
const (
	NavigateHeader = "Vk-Navigate"
	navPageHeader  = "Vk-Page"
	navLiveHeader  = "Vk-Live"
	navReload      = "Vk-Reload"
	navReloadWhy   = "Vk-Reload-Detail"
)

// navigation reports whether r is the runtime fetching a page to patch in
// place: a GET carrying NavigateHeader, not a live connection's run of a
// route.
func navigation(r *http.Request) bool {
	if r.Method != http.MethodGet || r.Header.Get(NavigateHeader) == "" {
		return false
	}
	_, joining := r.Context().Value(joinKey{}).(*join)
	return !joining
}

// buildID names what a page's document holds of the app: the runtime, the
// component loader (its manifest) and the stylesheet. A page rendered by
// another build can't be patched into this one's document: the runtime's
// script tag carries it (data-vk-build-id), and a navigation naming
// another answers Vk-Reload: build.
func (a *App) buildID() string {
	id := liveJSTag
	if a.assets != nil {
		_, _, tag := a.assets.current()
		id += "." + tag
	}
	if a.css != nil {
		_, tag, _ := a.css.current()
		id += "." + tag
	}
	sum := sha256.Sum256([]byte(id))
	return base64.RawURLEncoding.EncodeToString(sum[:9])
}

// navigations answers the navigations of another build with Vk-Reload,
// before the route runs, and marks the others' responses: they vary on
// NavigateHeader and are private to the visitor.
func (a *App) navigations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !navigation(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get(NavigateHeader) != a.buildID() {
			a.reload(w, r, "build", "")
			return
		}
		varyNav(w.Header())
		next.ServeHTTP(&navWriter{ResponseWriter: w}, r)
	})
}

// reload answers a navigation with Vk-Reload: the browser loads the URL.
// what is the cause's subject: the route (route), the script tag
// (scripts). In development the response says why in words
// (Vk-Reload-Detail) and the app logs it, once per cause.
func (a *App) reload(w http.ResponseWriter, r *http.Request, why, what string) {
	h := w.Header()
	h.Set(navReload, why)
	if a.dev {
		detail := reloadDetail(why, what)
		h.Set(navReloadWhy, detail)
		a.warnFullLoad(r, why, what, detail)
	}
	h.Set("Cache-Control", "no-store")
	varyNav(h)
	w.WriteHeader(http.StatusNoContent)
}

// reloadDetail says why a navigation loads in full, for people: in the
// terminal, the browser's console and the toolbar. ASCII, for a header.
func reloadDetail(why, what string) string {
	switch why {
	case "build":
		return "the app was rebuilt since the page loaded (expected after a rebuild)"
	case "route":
		return "its route " + what + " is web.FullLoad: its pages always load as a document"
	case "scripts":
		return "its body runs a script of its own: " + what + "; move it into <head>, make it a component script, or mark the link reload"
	}
	return why
}

// scriptText is a script tag as a detail names it: whitespace collapsed,
// ASCII, at most 160 bytes.
func scriptText(tag []byte) string {
	const max = 160
	var b strings.Builder
	space := false
	for _, c := range tag {
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			space = true
			continue
		case c < 0x20 || c >= 0x7f:
			c = '?'
		}
		if space && b.Len() > 0 && c != '>' {
			b.WriteByte(' ')
		}
		space = false
		b.WriteByte(c)
	}
	s := b.String()
	if len(s) > max {
		s = s[:max-4] + "...>"
	}
	return s
}

// fullLoadsSeen is what a development app has said of navigations that load in
// full: each cause once.
type fullLoadsSeen struct {
	mu   sync.Mutex
	seen map[string]bool
}

// warnFullLoad logs a navigation that loads in full, once per cause (why
// and what: the route, the script tag), and notes it on the request's
// toolbar record. A rebuild's is expected, logged at debug.
func (a *App) warnFullLoad(r *http.Request, why, what, detail string) {
	NoteRequest(r.Context(), RequestNote{Section: "Navigation", Name: "full load: " + why, Value: detail, Refused: true})
	key := why + "\x00" + what
	a.fullLoadsSeen.mu.Lock()
	seen := a.fullLoadsSeen.seen[key]
	if !seen && len(a.fullLoadsSeen.seen) < 256 {
		if a.fullLoadsSeen.seen == nil {
			a.fullLoadsSeen.seen = map[string]bool{}
		}
		a.fullLoadsSeen.seen[key] = true
	}
	a.fullLoadsSeen.mu.Unlock()
	if seen {
		return
	}
	level := slog.LevelWarn
	switch why {
	case "build":
		level = slog.LevelDebug
	case "route":
		level = slog.LevelInfo
	}
	slog.Log(r.Context(), level, "web: navigation to "+r.URL.Path+" loads in full: "+detail)
}

// varyNav adds NavigateHeader to h's Vary, once.
func varyNav(h http.Header) {
	if !headerHasToken(h, "Vary", NavigateHeader) {
		h.Add("Vary", NavigateHeader)
	}
}

// navWriter makes a navigation's response private unless the route said
// otherwise.
type navWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *navWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		if h := w.Header(); h.Get("Cache-Control") == "" {
			h.Set("Cache-Control", "private")
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *navWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *navWriter) Flush()                      { http.NewResponseController(w.ResponseWriter).Flush() }
func (w *navWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// FullLoad has a route's page always load as a document: a link to it on a
// page that isn't live is followed by the browser, not fetched and patched
// in place. For a page whose body runs its own scripts as it loads, or
// that must start from a fresh document. On a page type:
//
//	vuka.RegisterType[Report](web.FullLoad())
//
// and in a .vuka file @web.FullLoad above the type. A link marked
// data-vk-reload does the same for one link.
func FullLoad() vuka.Bundle { return vuka.Compose(fullLoad{}) }

type fullLoad struct{}

// fullLoads wraps a FullLoad route: its navigations answer Vk-Reload
// before it runs.
func (a *App) fullLoads(s *routeSpec, h http.Handler) http.Handler {
	if !slices.ContainsFunc(s.attrs, func(x any) bool { _, ok := x.(fullLoad); return ok }) {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if navigation(r) {
			a.reload(w, r, "route", s.method+" "+s.path)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// navPage is what a.page tells a navigation's response: the page's root
// layout, its join token when live; ok false when the browser must load
// it instead (its body runs scripts of its own: it has answered).
func (a *App) navPage(w http.ResponseWriter, r *http.Request, html []byte, token string) bool {
	if tag := plainScripts(html); tag != nil {
		a.reload(w, r, "scripts", scriptText(tag))
		return false
	}
	h := w.Header()
	root := "-"
	if t, ok := r.Context().Value(rootKey{}).(reflect.Type); ok {
		root = rootID(t)
	}
	h.Set(navPageHeader, root)
	if token != "" {
		h.Set(navLiveHeader, token)
		h.Set("Cache-Control", "private, no-store") // its token joins once
	}
	return true
}

// navError marks an HTML error page answering a navigation: a page under
// no root layout, which the browser patches in place when the page it
// leaves has none either.
func navError(w http.ResponseWriter, r *http.Request) {
	if navigation(r) {
		w.Header().Set(navPageHeader, "-")
	}
}

// etagged answers a navigation whose page is the one the browser holds
// (If-None-Match) with 304; parts are the response's body.
func etagged(w http.ResponseWriter, r *http.Request, parts ...[]byte) bool {
	sum := sha256.New()
	for _, p := range parts {
		sum.Write(p)
	}
	tag := `"` + base64.RawURLEncoding.EncodeToString(sum.Sum(nil)[:12]) + `"`
	w.Header().Set("ETag", tag)
	for _, m := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if strings.TrimPrefix(strings.TrimSpace(m), "W/") == tag {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}

// plainScripts is the first script of a page's body's own, its tag from
// "<script" to its ">"; nil when it has none: a <script> that would run as
// the document loads, which a patch of the body doesn't run. Data blocks
// (JSON, templates), modules the head loads and the runtime's own tags
// (added after this check) don't count; scripts in the head are merged
// into the document's head, run once added.
func plainScripts(html []byte) []byte {
	body := html
	if i := indexTag(html, "<body", false); i >= 0 {
		body = html[i:]
	} else if i := indexTag(html, "</head>", false); i >= 0 {
		body = html[i:]
	}
	for {
		i := indexTag(body, "<script", false)
		if i < 0 {
			return nil
		}
		tag := body[i:]
		body = body[i+len("<script"):]
		if len(body) > 0 && !isSpaceOrEnd(body[0]) {
			continue // <scripts…>, another tag
		}
		end := bytes.IndexByte(body, '>')
		if end < 0 {
			return tag
		}
		if runs(body[:end]) {
			return tag[:len("<script")+end+1]
		}
		body = body[end:]
	}
}

func isSpaceOrEnd(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '>' || c == '/'
}

// runs reports whether a script tag's attributes make it a script the
// browser runs: no type, a JavaScript type or module.
func runs(attrs []byte) bool {
	t, ok := attrValue(attrs, "type")
	if !ok {
		return true
	}
	t = strings.ToLower(strings.TrimSpace(t))
	switch t {
	case "", "module", "text/javascript", "application/javascript", "application/ecmascript", "text/ecmascript":
		return true
	}
	return false
}

// attrValue is the value of attribute name in a tag's attributes, as the
// renderer writes them (name="value").
func attrValue(attrs []byte, name string) (string, bool) {
	s := string(attrs)
	low := strings.ToLower(s)
	for i := 0; ; {
		j := strings.Index(low[i:], name)
		if j < 0 {
			return "", false
		}
		j += i
		i = j + len(name)
		if j > 0 && !isSpaceOrEnd(low[j-1]) {
			continue
		}
		rest := strings.TrimLeft(s[i:], " \t\n\r\f")
		if !strings.HasPrefix(rest, "=") {
			if rest == "" || isSpaceOrEnd(rest[0]) {
				return "", true
			}
			continue
		}
		rest = strings.TrimLeft(rest[1:], " \t\n\r\f")
		if rest == "" {
			return "", true
		}
		if q := rest[0]; q == '"' || q == '\'' {
			if k := strings.IndexByte(rest[1:], q); k >= 0 {
				return rest[1 : 1+k], true
			}
			return rest[1:], true
		}
		if k := strings.IndexAny(rest, " \t\n\r\f>"); k >= 0 {
			return rest[:k], true
		}
		return rest, true
	}
}
