package web

import (
	"net/http"
	"net/url"
	"strings"
)

// ExactPaths turns off the trailing-slash redirect: a path differing from a
// route only by a trailing slash is then a 404 (or whatever else answers
// it, a frontend's index.html).
func ExactPaths() Option { return OptionFunc(func(a *App) { a.exactPaths = true }) }

// slashes redirects a request whose path matches no route but would with its
// trailing slash added or removed: GET /pets/ goes to /pets when only
// /pets is declared. Only the app's routes and live pages are targets, and
// a path any registered pattern answers (a route, a static file tree,
// /_vuka/) is never redirected; a frontend's catch-all doesn't count, so
// the app's own route wins over its index.html, as for the exact path.
type slashes struct {
	next    http.Handler
	served  map[string][]pattern // by method: everything the router answers but frontends
	targets map[string][]pattern // by method: the routes a redirect may go to
}

// pattern is a route pattern split into segments; rest is true when it ends
// in {name...}, which matches the rest of the path, empty included.
type pattern struct {
	segs []string // anySeg for a {name} wildcard
	rest bool
}

// anySeg is a {name} segment in a pattern's segs: no path segment is it.
const anySeg = "\x01"

func parsePattern(p string) pattern {
	var pt pattern
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range segs {
		switch {
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "...}") && i == len(segs)-1:
			pt.rest = true
			return pt
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
			s = anySeg
		}
		pt.segs = append(pt.segs, s)
	}
	return pt
}

// match reports whether the pattern answers a path's segments. A literal
// segment matches itself, a {name} one any non-empty segment; the last
// segment of "/pets/" is "", which only the empty last segment of a
// pattern "/pets/" (or a {name...}) matches.
func (p pattern) match(segs []string) bool {
	if p.rest { // "/files/{path...}" answers "/files/" and below, not "/files"
		if len(segs) <= len(p.segs) {
			return false
		}
	} else if len(segs) != len(p.segs) {
		return false
	}
	for i, s := range p.segs {
		if s == anySeg && segs[i] == "" || s != anySeg && segs[i] != s {
			return false
		}
	}
	return true
}

func splitPath(p string) []string { return strings.Split(strings.TrimPrefix(p, "/"), "/") }

func matchAny(ps []pattern, segs []string) bool {
	for _, p := range ps {
		if p.match(segs) {
			return true
		}
	}
	return false
}

// newSlashes returns next behind the trailing-slash redirect, given every
// pattern the router serves (served, frontends excluded) and the routes
// (targets).
func newSlashes(next http.Handler, served, targets [][2]string) http.Handler {
	s := &slashes{next: next, served: map[string][]pattern{}, targets: map[string][]pattern{}}
	for _, mp := range served {
		s.served[mp[0]] = append(s.served[mp[0]], parsePattern(mp[1]))
	}
	for _, mp := range targets {
		if p := parsePattern(mp[1]); !p.rest && mp[1] != "/" {
			s.targets[mp[0]] = append(s.targets[mp[0]], p)
		}
	}
	if len(s.targets) == 0 {
		return next
	}
	return s
}

// methods are the patterns' methods a request's method is answered by: a
// GET route answers HEAD too.
func methods(m string) []string {
	if m == http.MethodHead {
		return []string{http.MethodHead, http.MethodGet}
	}
	return []string{m}
}

func (s *slashes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/" || p == "" || strings.HasPrefix(p, livePrefix) {
		s.next.ServeHTTP(w, r)
		return
	}
	alt := p + "/"
	if strings.HasSuffix(p, "/") {
		alt = strings.TrimRight(p, "/")
		if alt == "" || alt+"/" != p { // "//" and the like are the router's to clean
			s.next.ServeHTTP(w, r)
			return
		}
	}
	segs, altSegs := splitPath(p), splitPath(alt)
	ms := methods(r.Method)
	for _, m := range ms {
		if matchAny(s.served[m], segs) {
			s.next.ServeHTTP(w, r)
			return
		}
	}
	for _, m := range ms {
		if matchAny(s.targets[m], altSegs) {
			slashRedirect(w, r)
			return
		}
	}
	s.next.ServeHTTP(w, r)
}

// slashRedirect answers 308 to the path with its trailing slash changed,
// the query kept. The Location is relative to the request's path, so an
// app mounted under a prefix (http.StripPrefix) redirects within it.
func slashRedirect(w http.ResponseWriter, r *http.Request) {
	esc := r.URL.EscapedPath()
	var loc string
	if strings.HasSuffix(esc, "/") {
		// /a/b/ → ../b: up from the empty last segment, back into b.
		trimmed := strings.TrimSuffix(esc, "/")
		loc = "../" + trimmed[strings.LastIndexByte(trimmed, '/')+1:]
	} else {
		// /a/b → ./b/ ("./" keeps a segment with a colon from reading as a scheme).
		loc = "./" + esc[strings.LastIndexByte(esc, '/')+1:] + "/"
	}
	if r.URL.RawQuery != "" {
		loc += "?" + r.URL.RawQuery
	}
	if u, err := url.Parse(loc); err != nil || u.Scheme != "" || u.Host != "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusPermanentRedirect)
}

// recordingRouter is the app's router while Build registers routes: it
// notes each method and pattern, for the trailing-slash redirect.
type recordingRouter struct {
	Router
	patterns [][2]string
}

func (r *recordingRouter) Handle(method, pattern string, h http.Handler) {
	r.patterns = append(r.patterns, [2]string{method, pattern})
	r.Router.Handle(method, pattern, h)
}
