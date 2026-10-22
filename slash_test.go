package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/vuka-lang/ui"
)

type slashAdmin struct{}

func (*slashAdmin) Index() string { return "admin index" }

func slashApp(opts ...Option) *App {
	a := New(append([]Option{Only("none")}, opts...)...)
	a.Get("/ajira", func() string { return "ajira" })
	a.Get("/pets/{id}", func(id string) string { return "pet " + id })
	a.Post("/pets", func() string { return "added" })
	a.Get("/docs/", func() string { return "docs" })
	a.Get("/both", func() string { return "both" })
	a.Get("/both/", func() string { return "both/" })
	a.Get("/files/{path...}", func(path string) string { return "file " + path })
	a.Get("/files", func() string { return "files" })
	a.Static("/static", fstest.MapFS{"a.txt": {Data: []byte("a")}, "index.html": {Data: []byte("static index")}})
	a.Get("/static", func() string { return "static route" })
	a.Get("/a:b", func() string { return "colon" })
	return a
}

func TestTrailingSlashRedirect(t *testing.T) {
	h := slashApp().Handler()
	for _, c := range []struct {
		method, path string
		code         int
		loc, body    string
	}{
		{"GET", "/ajira/", 308, "../ajira", ""},
		{"HEAD", "/ajira/", 308, "../ajira", ""},
		{"GET", "/ajira/?next=%2Fx&a=1", 308, "../ajira?next=%2Fx&a=1", ""},
		{"GET", "/pets/7/", 308, "../7", ""},
		{"POST", "/pets/", 308, "../pets", ""}, // the method is kept: 308
		{"GET", "/docs", 308, "./docs/", ""},
		{"GET", "/a:b/", 308, "../a:b", ""},
		{"GET", "/ajira", 200, "", "ajira"},
		{"GET", "/both", 200, "", "both"}, // both forms declared: each its own
		{"GET", "/both/", 200, "", "both/"},
		{"GET", "/files/", 200, "", "file "},         // {path...} answers /files/ itself
		{"GET", "/static/", 200, "", "static index"}, // so does a static tree
		{"GET", "/_vuka/live.js/", 404, "", ""},
		{"DELETE", "/ajira/", 404, "", ""}, // no DELETE /ajira to go to
		{"GET", "/nothing/", 404, "", ""},
		{"GET", "/ajira//", 307, "/ajira/", ""}, // the router cleans the path first
	} {
		code, body, hdr := do(t, h, c.method, c.path, "")
		if code != c.code || hdr.Get("Location") != c.loc || !strings.Contains(body, c.body) {
			t.Errorf("%s %s: %d %q %.60q; want %d %q %q", c.method, c.path, code, hdr.Get("Location"), body, c.code, c.loc, c.body)
		}
		if code == 308 {
			base, _ := url.Parse("http://x" + c.path)
			to, _ := url.Parse(c.loc)
			if got := base.ResolveReference(to); strings.TrimSuffix(got.Path, "/") != strings.TrimSuffix(base.Path, "/") {
				t.Errorf("%s %s: Location resolves to %s", c.method, c.path, got)
			}
		}
	}
	// A {path...} pattern doesn't answer its prefix without the slash.
	tree := New(Only("none"))
	tree.Get("/tree/", func() string { return "tree" })
	tree.Static("/tree/files", fstest.MapFS{"a.txt": {Data: []byte("a")}})
	th := tree.Handler()
	if code, _, hdr := do(t, th, "GET", "/tree", ""); code != 308 || hdr.Get("Location") != "./tree/" {
		t.Errorf("GET /tree: %d %q", code, hdr.Get("Location"))
	}
	if code, body, _ := do(t, th, "GET", "/tree/files/a.txt", ""); code != 200 || body != "a" {
		t.Errorf("GET /tree/files/a.txt: %d %q", code, body)
	}
	if code, _, _ := do(t, slashApp(ExactPaths()).Handler(), "GET", "/ajira/", ""); code != 404 {
		t.Errorf("ExactPaths: GET /ajira/ answered %d", code)
	}
}

func TestTrailingSlashUnderPrefix(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/app/", http.StripPrefix("/app", slashApp().Handler()))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL + "/app/ajira/?q=1")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || res.Request.URL.Path != "/app/ajira" || res.Request.URL.RawQuery != "q=1" {
		t.Errorf("under /app: %d %s", res.StatusCode, res.Request.URL)
	}
}

func TestTrailingSlashFrontend(t *testing.T) {
	a := spaApp(t, Frontend(fixtureDist, fixtureRoot))
	a.Get("/settings", func() ui.Node { return ui.El("p", nil, ui.Text("server settings")) })
	h := a.Handler()
	for _, c := range []struct {
		path string
		code int
		loc  string
		want string
	}{
		{"/settings/", 308, "../settings", ""},             // the app's route, not the SPA's index.html
		{"/about/team/", 308, "../team", ""},               //
		{"/about/", 200, "", "<title>Fixture SPA</title>"}, // a client-side route stays the SPA's
		{"/robots.txt", 200, "", "User-agent"},
		{"/assets/index-Dk3Qm9xA.js", 200, "", "client-side routes"},
	} {
		code, body, hdr := do(t, h, "GET", c.path, "")
		if code != c.code || hdr.Get("Location") != c.loc || !strings.Contains(body, c.want) {
			t.Errorf("GET %s: %d %q %.60q; want %d %q %q", c.path, code, hdr.Get("Location"), body, c.code, c.loc, c.want)
		}
	}

	b := New(Only("none"), Frontend(fixtureDist, fixtureRoot, FrontendAt("/admin")))
	b.Get("/admin/users", func() string { return "users" })
	hb := b.Handler()
	if code, _, hdr := do(t, hb, "GET", "/admin/users/", ""); code != 308 || hdr.Get("Location") != "../users" {
		t.Errorf("GET /admin/users/ under a prefixed frontend: %d %q", code, hdr.Get("Location"))
	}
	if code, body, _ := do(t, hb, "GET", "/admin/", ""); code != 200 || !strings.Contains(body, "Fixture SPA") {
		t.Errorf("GET /admin/: %d %.60q", code, body)
	}
}

func TestTrailingSlashController(t *testing.T) {
	a := New(Only("none"), Provide(&slashAdmin{}))
	a.Get("/admin", (*slashAdmin).Index)
	h := a.Handler()
	if code, _, hdr := do(t, h, "GET", "/admin/", ""); code != 308 || hdr.Get("Location") != "../admin" {
		t.Errorf("GET /admin/: %d %q", code, hdr.Get("Location"))
	}
}
