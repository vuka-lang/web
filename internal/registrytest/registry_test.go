package registrytest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/web"
)

type role string

func (role) RouteWrapper() string { return "roles" }

type Pet struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type Store struct{ pets map[int]Pet }

func newStore() *Store { return &Store{pets: map[int]Pet{1: {1, "Rex"}, 2: {2, "Tom"}}} }

func (s *Store) Find(id int) vuka.Result[Pet] {
	if p, ok := s.pets[id]; ok {
		return vuka.Ok(p)
	}
	return vuka.Err[Pet](web.NotFound)
}

func get(t *testing.T, h http.Handler, target string) (int, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), rec.Header()
}

func expect(t *testing.T, h http.Handler, target string, code int, want string) {
	t.Helper()
	if got, b, _ := get(t, h, target); got != code || !strings.Contains(b, want) {
		t.Errorf("GET %s: %d %q; want %d containing %q", target, got, b, code, want)
	}
}

func regShow(id int, s *Store) vuka.Result[Pet] { return s.Find(id) }

type regAdmin struct{ store *Store }

func newRegAdmin(store *Store) *regAdmin { return &regAdmin{store} }

func (a *regAdmin) Show(id int) vuka.Result[Pet] { return a.store.Find(id) }

type regCounter struct {
	ui.Live
	Start int
	store *Store
}

func newRegCounter(store *Store) *regCounter {
	return &regCounter{Start: len(store.pets) * 10, store: store}
}

func (c *regCounter) Render() ui.Node { return ui.Text(fmt.Sprint("start ", c.Start)) }

type regGreeter struct{ greeting string }

func newRegGreeter() *regGreeter { return &regGreeter{"hello"} }

func regGreet(g *regGreeter, name string) string { return g.greeting + " " + name }

func regGated() string { return "gated" }

func init() {
	vuka.RegisterFunc(regGated, role("admin"))
	vuka.RegisterParams(regGreet, "g", "name") // as the compiler records a handler's names
	web.Routes.Controller("/reg/admin", newRegAdmin)
	web.Routes.Get("/{id}", (*regAdmin).Show)
	web.Routes.Get("/reg/pets/{id}", regShow)
	web.Routes.Get("/reg/greet", regGreet, web.Use(regHeader))
	web.Routes.Get("/reg/gated", regGated)
	web.Routes.Live("/reg/counter", newRegCounter)
}

func regHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Reg", "1")
		next.ServeHTTP(w, r)
	})
}

// regApps are an app of what web.Routes holds and one registering the same with
// its own methods: they serve alike.
func regApps() map[string]*web.App {
	gate := web.WrapRoutes("roles", func(info web.RouteInfo) web.Middleware {
		for _, a := range info.Attrs {
			if a == role("admin") {
				return func(http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
				}
			}
		}
		return nil
	})
	own := web.New(web.Only("none"), web.Provide(newStore), gate).
		Service(newRegGreeter).
		Controller("/reg/admin", newRegAdmin).
		Get("/{id}", (*regAdmin).Show).
		Get("/reg/pets/{id}", regShow).
		Get("/reg/greet", regGreet, web.Use(regHeader)).
		Get("/reg/gated", regGated).
		Live("/reg/counter", newRegCounter)
	return map[string]*web.App{
		"Routes": web.New(web.Only("github.com/vuka-lang/web/internal/registrytest"), web.Provide(newStore), gate).Service(newRegGreeter),
		"App":    own,
	}
}

func TestRegistrations(t *testing.T) {
	for name, a := range regApps() {
		t.Run(name, func(t *testing.T) {
			h := a.Handler()
			expect(t, h, "/reg/pets/2", 200, `"name":"Tom"`)
			expect(t, h, "/reg/admin/1", 200, `"name":"Rex"`)
			expect(t, h, "/reg/counter", 200, "start 20")
			expect(t, h, "/reg/gated", 403, "")
			code, body, hdr := get(t, h, "/reg/greet?name=ada")
			if code != 200 || !strings.Contains(body, "hello ada") || hdr.Get("X-Reg") != "1" {
				t.Errorf("GET /reg/greet: %d %q %v", code, body, hdr)
			}
		})
	}
}
