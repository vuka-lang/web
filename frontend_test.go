package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

//go:embed all:testdata/frontend/dist
var fixtureDist embed.FS

const fixtureRoot = "testdata/frontend/dist"

func spaApp(t *testing.T, opts ...Option) *App {
	t.Helper()
	a := New(append([]Option{Only("none")}, opts...)...)
	a.Get("/api/hello", func() map[string]string { return map[string]string{"message": "hello from the API"} })
	a.Get("/about/team", func() ui.Node { return ui.El("p", nil, ui.Text("a server page")) })
	return a
}

func TestFrontendFallbackAndPrecedence(t *testing.T) {
	h := spaApp(t, Frontend(fixtureDist, fixtureRoot)).Handler()
	for _, c := range []struct {
		path string
		code int
		want string
	}{
		{"/", 200, "<title>Fixture SPA</title>"},
		{"/about", 200, "<title>Fixture SPA</title>"},         // a client-side route
		{"/pets/7/edit", 200, "<title>Fixture SPA</title>"},   // deep
		{"/index.html", 200, "<title>Fixture SPA</title>"},    // the page itself
		{"/api/hello", 200, `"message":"hello from the API"`}, // the app's route wins
		{"/about/team", 200, "a server page"},
		{"/assets/index-Dk3Qm9xA.js", 200, "client-side routes"},
		{"/robots.txt", 200, "User-agent"},
		{"/assets/missing-AAAAAAAA.js", 404, ""}, // a file is a file or nothing
		{"/favicon.ico", 404, ""},
		{"/.vite/manifest.json", 404, ""}, // never served
		{"/_vuka/nothing", 404, ""},
		{"/_vuka/live.js", 200, ""}, // web's own still served
	} {
		code, body, _ := do(t, h, "GET", c.path, "")
		if code != c.code || !strings.Contains(body, c.want) {
			t.Errorf("GET %s: %d %.80q; want %d containing %q", c.path, code, body, c.code, c.want)
		}
	}
	code, _, hdr := do(t, h, "GET", "/assets/index-Dk3Qm9xA.js", "")
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/javascript") {
		t.Errorf("the script's type: %q", hdr.Get("Content-Type"))
	}
	if code, body, _ := do(t, h, "HEAD", "/about", ""); code != 200 || body != "" {
		t.Errorf("HEAD /about: %d %q", code, body)
	}
}

func TestFrontendClash(t *testing.T) {
	a := spaApp(t, Frontend(fixtureDist, fixtureRoot))
	a.Get("/{rest...}", func() string { return "mine" })
	if err := a.Build(); err == nil || !strings.Contains(err.Error(), `error[E0402]: Frontend("testdata/frontend/dist") serves GET /{path...}, as GET /{rest...} (`) {
		t.Fatalf("Build: %v", err)
	}
	a = spaApp(t, Frontend(fixtureDist, fixtureRoot), Frontend(fixtureDist, fixtureRoot))
	if err := a.Build(); err == nil || !strings.Contains(err.Error(), `serves GET /{path...}, as Frontend("testdata/frontend/dist") does`) {
		t.Fatalf("Build of two frontends at /: %v", err)
	}
}

func TestFrontendAt(t *testing.T) {
	h := spaApp(t, Frontend(fixtureDist, fixtureRoot, FrontendAt("/admin/"))).Handler()
	for _, c := range []struct {
		path string
		code int
		want string
	}{
		{"/admin", 200, "Fixture SPA"},
		{"/admin/", 200, "Fixture SPA"},
		{"/admin/users/3", 200, "Fixture SPA"},
		{"/admin/robots.txt", 200, "User-agent"},
		{"/admin/assets/index-Bw8nT4cE.css", 200, "#app"},
		{"/administrator", 404, ""},
		{"/", 404, ""},
		{"/robots.txt", 404, ""},
		{"/api/hello", 200, "hello"},
	} {
		code, body, _ := do(t, h, "GET", c.path, "")
		if code != c.code || !strings.Contains(body, c.want) {
			t.Errorf("GET %s: %d %.80q; want %d containing %q", c.path, code, body, c.code, c.want)
		}
	}
}

func TestFrontendCaching(t *testing.T) {
	h := spaApp(t, Frontend(fixtureDist, fixtureRoot)).Handler()
	immutable := "public, max-age=31536000, immutable"
	for path, want := range map[string]string{
		"/assets/index-Dk3Qm9xA.js":  immutable,  // the manifest's entry
		"/assets/index-Bw8nT4cE.css": immutable,  // its css
		"/assets/logo-Q7mZx2Lp.svg":  immutable,  // its assets
		"/assets/stray-Hh4kL0pQ.js":  "no-cache", // hashed-looking, but not output the manifest lists
		"/robots.txt":                "no-cache",
		"/":                          "no-cache",
		"/deep/link":                 "no-cache",
	} {
		code, _, hdr := do(t, h, "GET", path, "")
		if code != 200 || hdr.Get("Cache-Control") != want || hdr.Get("ETag") == "" {
			t.Errorf("GET %s: %d Cache-Control %q ETag %q; want %q", path, code, hdr.Get("Cache-Control"), hdr.Get("ETag"), want)
			continue
		}
		code, body, _ := do(t, h, "GET", path, "", "If-None-Match", hdr.Get("ETag"))
		if code != http.StatusNotModified || body != "" {
			t.Errorf("GET %s with its ETag: %d %q; want 304", path, code, body)
		}
	}
	_, _, a := do(t, h, "GET", "/one", "")
	_, _, b := do(t, h, "GET", "/two", "")
	if a.Get("ETag") != b.Get("ETag") {
		t.Errorf("index.html's ETag differs by route: %q %q", a.Get("ETag"), b.Get("ETag"))
	}
}

// devDir makes a frontend's dist directory in a temporary working
// directory, as development reads it from disk.
func devDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	for name, content := range files {
		p := filepath.Join(dir, "dist", filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(dir, "dist"), 0o755)
	return filepath.Join(dir, "dist")
}

func TestFrontendNotBuilt(t *testing.T) {
	empty := fstest.MapFS{"dist/.gitkeep": {}}
	for name, fsys := range map[string]fstest.MapFS{"empty": empty, "missing": {}} {
		err := spaApp(t, Frontend(fsys, "dist")).Build()
		if err == nil || !strings.Contains(err.Error(), "error[E0601]: Frontend(\"dist\"): the frontend is empty or missing\n  = help: build it first") {
			t.Errorf("%s: Build: %v", name, err)
		}
	}
	noIndex := fstest.MapFS{"dist/app.js": {Data: []byte("x")}}
	if err := spaApp(t, Frontend(noIndex, "dist")).Build(); err == nil || !strings.Contains(err.Error(), "has no index.html") {
		t.Errorf("no index.html: Build: %v", err)
	}

	// In development: a page saying how to build, the API still served.
	devDir(t, map[string]string{".gitkeep": ""})
	h := spaApp(t, Dev(), Frontend(empty, "dist")).Handler()
	expect(t, h, "GET", "/somewhere", "", 200, "The frontend isn't built yet")
	expect(t, h, "GET", "/api/hello", "", 200, "hello")
	expect(t, h, "GET", "/app.js", "", 404, "")
}

// TestFrontendIslands: the islands vuka build-islands wrote into a
// frontend's build are served from it, cached for good, and pages load
// them; a build of islands alone answers other paths 404.
func TestFrontendIslands(t *testing.T) {
	islands := fstest.MapFS{
		"dist/.vuka/manifest.json":       {Data: []byte(`{"islands":{"Chart":{"name":"Chart","module":"chart.0a1b2c3d.js","framework":"vanilla"}}}`)},
		"dist/.vuka/i/chart.0a1b2c3d.js": {Data: []byte("export function mount() {}")},
	}
	a := spaApp(t, Frontend(islands, "dist"))
	a.Get("/page", func() ui.Node { return ui.El("html", nil, ui.El("head", nil), ui.El("body", nil)) })
	h := a.Handler()
	code, body, hdr := do(t, h, "GET", "/_vuka/i/chart.0a1b2c3d.js", "")
	if code != 200 || body != "export function mount() {}" || !strings.Contains(hdr.Get("Cache-Control"), "immutable") {
		t.Errorf("the island's chunk: %d %q %q", code, body, hdr.Get("Cache-Control"))
	}
	expect(t, h, "GET", "/page", "", 200, `<script src="/_vuka/c/loader.js?v=`)
	expect(t, h, "GET", "/_vuka/c/loader.js", "", 200, `"i":{"Chart":["/_vuka/i/chart.0a1b2c3d.js",""]}`)
	expect(t, h, "GET", "/somewhere", "", 404, "")
	expect(t, h, "GET", "/.vuka/manifest.json", "", 404, "")

	islands["dist/index.html"] = &fstest.MapFile{Data: []byte("<html><head><title>SPA</title></head></html>")}
	h = spaApp(t, Frontend(islands, "dist")).Handler()
	expect(t, h, "GET", "/somewhere", "", 200, "<title>SPA</title>")
	expect(t, h, "GET", "/_vuka/i/chart.0a1b2c3d.js", "", 200, "mount")
}

// The index.html vuka new --islands wrote doesn't answer unknown paths, in
// production or in development; a real page still does.
func TestFrontendStubIndex(t *testing.T) {
	stub := fstest.MapFS{"dist/index.html": {Data: []byte(islandsStub + "\n")}}
	expect(t, spaApp(t, Frontend(stub, "dist")).Handler(), "GET", "/nope", "", 404, "")
	expect(t, spaApp(t, Dev(), Frontend(stub, "dist")).Handler(), "GET", "/nope", "", 404, "")

	page := fstest.MapFS{"dist/index.html": {Data: []byte("<p>hi</p>")}}
	expect(t, spaApp(t, Frontend(page, "dist")).Handler(), "GET", "/nope", "", 200, "hi")
}

func TestFrontendDevReadsDisk(t *testing.T) {
	dist := devDir(t, map[string]string{"index.html": "<html><head><title>v1</title></head><body></body></html>", "a.js": "1"})
	h := spaApp(t, Dev(), Frontend(fstest.MapFS{"dist/index.html": {Data: []byte("embedded")}}, "dist")).Handler()
	expect(t, h, "GET", "/x", "", 200, "<title>v1</title>")
	os.WriteFile(filepath.Join(dist, "index.html"), []byte("<title>v2</title>"), 0o644)
	expect(t, h, "GET", "/x", "", 200, "<title>v2</title>") // rebuilt: no restart needed
	_, _, hdr := do(t, h, "GET", "/x", "")
	if hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("a development page's Cache-Control: %q", hdr.Get("Cache-Control"))
	}
	_, _, hdr = do(t, h, "GET", "/a.js", "")
	if hdr.Get("Cache-Control") != "no-cache" {
		t.Errorf("a development file's Cache-Control: %q", hdr.Get("Cache-Control"))
	}
}

// fakeVite is a dev server answering with a page as Vite transforms it.
func fakeVite(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><html><head><script type="module">import { injectIntoGlobalHook } from "/@react-refresh";
injectIntoGlobalHook(window);</script>
<script type="module" src="/@vite/client"></script>
<link rel="icon" href="/favicon.svg"><link rel="preconnect" href="https://fonts.example.com">
<title>from vite</title></head><body><a href="/about">about</a><div id="root"></div><script type="module" src="/src/main.tsx?t=1"></script></body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeHot(t *testing.T, dist, origin string, pid int) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"origin": origin, "base": "/", "pid": pid})
	os.MkdirAll(filepath.Join(dist, ".vite"), 0o755)
	if err := os.WriteFile(filepath.Join(dist, ".vite", "vuka-hot.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFrontendViteDevServer(t *testing.T) {
	vite := fakeVite(t)
	dist := devDir(t, map[string]string{"index.html": "<title>the build</title>", "logo.svg": "<svg/>"})
	h := spaApp(t, Dev(), Frontend(fixtureDist, "dist")).Handler()

	// missing: the build on disk
	expect(t, h, "GET", "/x", "", 200, "<title>the build</title>")

	// live: the dev server's page, its modules loaded from it
	writeHot(t, dist, vite.URL, os.Getpid())
	_, body, _ := do(t, h, "GET", "/deep/link", "")
	for _, want := range []string{
		`from "` + vite.URL + `/@react-refresh"`,
		`<script type="module" src="` + vite.URL + `/@vite/client">`,
		`<script type="module" src="` + vite.URL + `/src/main.tsx?t=1">`,
		`<link rel="icon" href="` + vite.URL + `/favicon.svg">`,
		`href="https://fonts.example.com"`,
		`<a href="/about">`, // links stay on the app
		"<title>from vite</title>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the dev page lacks %s:\n%s", want, body)
		}
	}
	code, _, hdr := do(t, h, "GET", "/logo.svg", "")
	if code != http.StatusTemporaryRedirect || hdr.Get("Location") != vite.URL+"/logo.svg" {
		t.Errorf("a file with the dev server up: %d %q", code, hdr.Get("Location"))
	}
	expect(t, h, "GET", "/.vite/vuka-hot.json", "", 404, "")

	// stale: its process is gone
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Skip(err)
	}
	writeHot(t, dist, vite.URL, c.Process.Pid)
	expect(t, h, "GET", "/x", "", 200, "<title>the build</title>")
	expect(t, h, "GET", "/logo.svg", "", 200, "<svg/>")

	// outside development the hot file means nothing
	writeHot(t, dist, vite.URL, os.Getpid())
	p := spaApp(t, Frontend(fixtureDist, fixtureRoot)).Handler()
	expect(t, p, "GET", "/x", "", 200, "Fixture SPA")
}

func TestDevPage(t *testing.T) {
	in := `<script type=module src=/@vite/client></script><script type="module">import "/src/a.js"; await import('/src/b.js'); import x from "//cdn/x.js"</script><script>fetch("/api")</script><img src="/i.png"><link href='/s.css' rel=stylesheet>`
	want := `<script type=module src=http://v:5/@vite/client></script><script type="module">import "http://v:5/src/a.js"; await import('http://v:5/src/b.js'); import x from "//cdn/x.js"</script><script>fetch("/api")</script><img src="/i.png"><link href='http://v:5/s.css' rel=stylesheet>`
	if got := string(devPage([]byte(in), "http://v:5")); got != want {
		t.Errorf("devPage:\n got %s\nwant %s", got, want)
	}
}

func TestFrontendEnv(t *testing.T) {
	t.Setenv("PUBLIC_API", "https://api.example.com/</script>")
	t.Setenv("SECRET_KEY", "hunter2")
	h := spaApp(t, Frontend(fixtureDist, fixtureRoot, FrontendEnv("PUBLIC_API", "UNSET_ONE"))).Handler()
	_, body, _ := do(t, h, "GET", "/", "")
	want := `<head><script>globalThis.__VUKA_ENV__=Object.freeze({"PUBLIC_API":"https://api.example.com/\u003c/script\u003e"})</script>`
	if !strings.Contains(body, want) || strings.Contains(body, "hunter2") || strings.Contains(body, "UNSET_ONE") {
		t.Errorf("the page:\n%s", body)
	}
	// none named, none passed
	_, body, _ = do(t, spaApp(t, Frontend(fixtureDist, fixtureRoot)).Handler(), "GET", "/", "")
	if strings.Contains(body, "__VUKA_ENV__") {
		t.Errorf("a page without FrontendEnv: %s", body)
	}
	if err := spaApp(t, Frontend(fixtureDist, fixtureRoot, FrontendEnv("A=B"))).Build(); err == nil {
		t.Error("FrontendEnv(\"A=B\") built")
	}
}

func TestFrontendToolbar(t *testing.T) {
	withToolbar(t)
	devDir(t, map[string]string{"index.html": "<html><head><title>t</title></head><body><div id=app></div></body></html>"})
	h := spaApp(t, Dev(), Frontend(fixtureDist, "dist")).Handler()
	_, body, hdr := do(t, h, "GET", "/x", "")
	if !strings.Contains(body, `<script src="/_vuka/toolbar.js?v=`) || !strings.Contains(body, `data-vk-toolbar="`+hdr.Get(ToolbarHeader)+`"`) {
		t.Errorf("the dev page has no toolbar:\n%s", body)
	}
	if strings.Contains(body, cssPath) {
		t.Errorf("the frontend's page got the app's stylesheet:\n%s", body)
	}
	_, body, _ = do(t, h, "GET", "/x", "", "Sec-Fetch-Dest", "empty")
	if strings.Contains(body, "toolbar.js") {
		t.Errorf("a fetch of the page got the toolbar")
	}
}

func TestFrontendChrome(t *testing.T) {
	b := chrome.Launch(t)
	t.Setenv("PUBLIC_NAME", "pets")
	srv := httptest.NewServer(spaApp(t, Frontend(fixtureDist, fixtureRoot, FrontendEnv("PUBLIC_NAME"))).Handler())
	defer srv.Close()

	p := b.Load(srv.URL + "/")
	p.WaitText("#api", "hello from the API")
	var env string
	p.Eval(`window.__env.PUBLIC_NAME`, &env)
	if env != "pets" {
		t.Errorf("the page's env: %q", env)
	}
	p.Click(`a[href="/about"]`)
	p.Wait(`location.pathname === "/about" && document.querySelector("#app").dataset.route === "/about"`)
	p.WaitText("main h1", "About")
	p.Click(`a[href="/pets/7"]`)
	p.WaitText("main h1", "Pet 7")
	var renders string
	p.Eval(`document.querySelector("#app").dataset.renders`, &renders)
	if renders != "3" {
		t.Errorf("client-side navigation reloaded the page: %s renders", renders)
	}
	p.Eval(`history.back()`, nil)
	p.WaitText("main h1", "About")

	// a reload on a deep link
	d := b.Load(srv.URL + "/pets/42")
	d.WaitText("main h1", "Pet 42")
	var font string
	d.Eval(`getComputedStyle(document.querySelector("#app")).fontFamily`, &font)
	if !strings.Contains(font, "sans-serif") {
		t.Errorf("the stylesheet didn't load: %q", font)
	}
}

func TestFrontendToolbarChrome(t *testing.T) {
	withToolbar(t)
	b := chrome.Launch(t)
	files := map[string]string{}
	for _, f := range []string{"index.html", "assets/index-Dk3Qm9xA.js", "assets/index-Bw8nT4cE.css"} {
		data, _ := fixtureDist.ReadFile(fixtureRoot + "/" + f)
		files[f] = string(data)
	}
	devDir(t, files) // the build, on disk
	srv := httptest.NewServer(spaApp(t, Dev(), Frontend(fixtureDist, "dist")).Handler())
	defer srv.Close()
	p := b.Load(srv.URL + "/about")
	p.Viewport(1400, 800, false)
	tb := `document.querySelector("vuka-toolbar").shadowRoot`
	p.Wait(tb + `.querySelector(".handle .num")`)
	p.WaitText("main h1", "About")
	p.Click(`a[href="/"]`)
	p.WaitText("#api", "hello from the API")
	p.Wait(tb + `.querySelector(".handle .calls")`) // the API call, listed
	var calls string
	p.Eval(tb+`.querySelector(".handle .calls").textContent`, &calls)
	if calls != "+1" {
		t.Errorf("the toolbar's calls: %q", calls)
	}
}
