package web

import (
	"net/http"
	"strings"
)

// Router matches requests to routes. Patterns use the canonical syntax:
// "/pets/{id}" binds a segment, "/files/{path...}" the rest of the path, and a
// pattern matches its path exactly. Param returns a bound value for a request
// the router dispatched.
type Router interface {
	http.Handler
	Handle(method, pattern string, h http.Handler)
	Param(r *http.Request, name string) string
}

// ServeMux is the default Router: net/http's ServeMux with Go 1.22 patterns.
type ServeMux struct{ mux *http.ServeMux }

func NewServeMux() *ServeMux { return &ServeMux{http.NewServeMux()} }

func (m *ServeMux) Handle(method, pattern string, h http.Handler) {
	if strings.HasSuffix(pattern, "/") {
		pattern += "{$}"
	}
	m.mux.Handle(method+" "+pattern, h)
}

func (m *ServeMux) Param(r *http.Request, name string) string   { return r.PathValue(name) }
func (m *ServeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

// pathParams lists the names bound by a pattern's {name} and {name...} segments.
func pathParams(pattern string) []string {
	var names []string
	for _, seg := range strings.Split(pattern, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			if n := strings.TrimSuffix(seg[1:len(seg)-1], "..."); n != "$" {
				names = append(names, n)
			}
		}
	}
	return names
}

func joinPath(prefix, path string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	switch {
	case prefix == "":
		return path
	case path == "/" || path == "":
		return prefix
	case !strings.HasPrefix(path, "/"):
		return prefix + "/" + path
	}
	return prefix + path
}
