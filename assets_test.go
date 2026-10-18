package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/web/internal/chrome"
	"github.com/vuka-lang/web/internal/testenv"
)

// The island runtime — function, state and live props — in node over a
// small DOM.
func TestLiveJSIslands(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		testenv.Needs(t, "no node")
	}
	out, err := exec.Command(node, "testdata/livejs/islands.js", "live.js", "loader.js").CombinedOutput()
	if err != nil {
		t.Fatalf("%s", out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// The loader with live.js, in node over a small DOM: hooks on load, join,
// patch and navigation, ctx, and JS calls reaching a component's methods.
func TestLoaderJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		testenv.Needs(t, "no node")
	}
	out, err := exec.Command(node, "testdata/livejs/loader.js", "live.js", "loader.js").CombinedOutput()
	if err != nil {
		t.Fatalf("%s", out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// The development toolbar's build state, in node over a small DOM: an
// island build failing shows on the handle with Vite's output, and clears.
func TestToolbarJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		testenv.Needs(t, "no node")
	}
	out, err := exec.Command(node, "testdata/livejs/toolbar.js", "live.js", "toolbar.js").CombinedOutput()
	if err != nil {
		t.Fatalf("%s", out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// vuka run's word that the assets' last compile failed reaches the loader
// (an island not built says why) and the toolbar's build state; an app
// with no scripts yet loads the loader while it stands.
func TestAssetsFailure(t *testing.T) {
	withToolbar(t)
	dir := devAssets(t)
	t.Setenv(DevDirEnv, t.TempDir())
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"scopes":{"k1":{"name":"x","styles":["a.css"]}}}`), 0o644)
	h := scopedApp().Handler()
	loader := func() string {
		page := tbDo(t, h, "GET", "/plain").body
		m := regexp.MustCompile(`<script src="(/_vuka/c/loader\.js\?v=[^"]+)" defer></script>`).FindStringSubmatch(page)
		if m == nil {
			return ""
		}
		return tbDo(t, h, "GET", m[1]).body
	}
	state := func() buildState {
		var st buildState
		json.Unmarshal([]byte(tbDo(t, h, "GET", "/_vuka/toolbar/build").body), &st)
		return st
	}
	if js := loader(); js != "" {
		t.Errorf("no scripts, no failure: a loader")
	}
	if st := state(); st.Assets != nil {
		t.Errorf("no failure: %+v", st.Assets)
	}
	time.Sleep(110 * time.Millisecond)
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"failed":true,"islands":true,"output":"vite build: exit status 1\n"}`), 0o644)
	time.Sleep(110 * time.Millisecond)
	if js := loader(); !strings.Contains(js, `"d":true,"x":true`) {
		t.Errorf("the loader doesn't know the island build failed: %.300s", js)
	}
	if st := state(); st.Assets == nil || !st.Assets.Islands || st.Assets.Output != "vite build: exit status 1\n" {
		t.Errorf("the toolbar's state: %+v", st.Assets)
	}
	os.Remove(filepath.Join(dir, "state.json"))
	time.Sleep(110 * time.Millisecond)
	if js := loader(); js != "" {
		t.Errorf("the failure gone: still a loader")
	}
	if st := state(); st.Assets != nil {
		t.Errorf("the failure gone: %+v", st.Assets)
	}
}

// scopedCounter and scopedLabel are components as the vuka command
// compiles them with web.Style and web.Script: their elements carry their
// scopes, statics of their frames.
type scopedCounter struct {
	ui.Live
	n int
}

func (c *scopedCounter) Inc()    { c.n++ }
func (c *scopedCounter) Secret() { c.n = 100 }
func (c *scopedCounter) Render() ui.Node {
	s := func(more ...ui.Attr) []ui.Attr { return append(more, ui.Attr{Name: "data-vk-s", Value: "k1"}) }
	return ui.F(0x1c1, "Es(Es(h)Eds(t)Eds(t)c)", ui.El("div", s(),
		ui.El("output", s(ui.Attr{Name: "id", Value: "count"}), ui.Child(c.n)),
		ui.El("button", s(ui.Attr{Name: "id", Value: "inc"}, ui.Attr{Name: "onClick", Value: ui.On(c.Inc)}), ui.Text("+")),
		ui.El("button", s(ui.Attr{Name: "id", Value: "bump"}, ui.Attr{Name: "onClick", Value: ui.On(ui.JS.Call("bump", 2))}), ui.Text("bump")),
		ui.Component("label", nil, &scopedLabel{})))
}

type scopedLabel struct{ ui.Live }

func (l *scopedLabel) Render() ui.Node {
	return ui.F(0x1c2, "Es(t)", ui.El("output", []ui.Attr{{Name: "id", Value: "label"}, {Name: "data-vk-s", Value: "k2"}}, ui.Text("label")))
}

// devAssets writes what vuka run keeps for an app with these components:
// the manifest and the module (VUKA_ASSETS) and the stylesheet
// (VUKA_TAILWIND_CSS).
func devAssets(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "c"), 0o755)
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", `{"scopes":{"k1":{"name":"web.scopedCounter","styles":["counter.css"],"scripts":["counter.ts"],"module":"k1.0a1b2c3d.js","methods":["bump"],"css":64,`+
		`"type":"github.com/vuka-lang/web.scopedCounter","pushes":["Inc"]},`+
		`"k2":{"name":"web.scopedLabel","styles":["label.css"],"css":48}}}`)
	write("c/k1.0a1b2c3d.js", `export default [{
	mounted(el, ctx) { (window.hooks ||= []).push("mounted " + el.dataset.vkId); },
	updated(el, ctx) { window.hooks.push("updated " + el.dataset.vkId); },
	bump(el, ctx, by) { window.hooks.push("bump " + by + " " + ctx.target.id + " " + ctx.event.type + " " + el.dataset.vkId); },
	async press(el, ctx, name) {
		try { await ctx.push(name); window.hooks.push("pushed " + name); } catch (e) { window.hooks.push("refused " + e.message); }
	},
}];
`)
	write("app.css", `output[data-vk-s="k1"]{color:rgb(200, 0, 0)} output[data-vk-s="k2"]{color:rgb(0, 0, 200)}`)
	t.Setenv(AssetsEnv, dir)
	t.Setenv(CSSEnv, filepath.Join(dir, "app.css"))
	return dir
}

func scopedApp() *App {
	return New(Only("none")).Live("/", &scopedCounter{}, Style("counter.css"), Script("counter.ts"),
		Layout(func(p ui.Node) ui.Node {
			return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("s"))), ui.El("body", nil, p))
		}, Style("layout.css"), CSS("main{}"), JS("export default {}"))).
		Get("/plain", func() ui.Node { return ui.El("html", nil, ui.El("head", nil), ui.El("body", nil, ui.Text("plain"))) })
}

// A page of an app with component scripts loads the loader, which names
// each scope's module and methods; modules are served from the directory
// vuka run keeps; a new manifest is a new loader.
func TestAssetsServe(t *testing.T) {
	dir := devAssets(t)
	srv := httptest.NewServer(scopedApp().Handler())
	defer srv.Close()
	get := func(path string) (int, string, http.Header) {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b), res.Header
	}
	_, page, _ := get("/")
	tag := regexp.MustCompile(`<script src="(/_vuka/c/loader\.js\?v=[^"]+)" defer></script>`).FindStringSubmatch(page)
	if tag == nil || strings.Index(page, tag[0]) > strings.Index(page, "</head>") {
		t.Fatalf("the page doesn't load the loader in its head: %s", page)
	}
	if _, plain, _ := get("/plain"); !strings.Contains(plain, tag[1]) {
		t.Errorf("every page of the app loads it (a live navigation may bring a component): %s", plain)
	}
	_, js, _ := get(tag[1])
	if !strings.Contains(js, `{"m":{"k1":"/_vuka/c/k1.0a1b2c3d.js"},"f":{"k1":["bump"]},"e":"/_vuka/c/events","d":true}`) {
		t.Errorf("the loader's manifest: %s", js)
	}
	if code, mod, _ := get("/_vuka/c/k1.0a1b2c3d.js"); code != 200 || !strings.Contains(mod, "bump(el") {
		t.Errorf("module: %d %s", code, mod)
	}
	for _, p := range []string{"/_vuka/c/nope.js", "/_vuka/c/..%2Fmanifest.json", "/_vuka/c/manifest.json"} {
		if code, _, _ := get(p); code != 404 {
			t.Errorf("%s: %d", p, code)
		}
	}
	time.Sleep(150 * time.Millisecond)
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"scopes":{"k1":{"name":"x","module":"k1.ffff0000.js"}}}`), 0o644)
	time.Sleep(150 * time.Millisecond)
	if _, page, _ := get("/"); strings.Contains(page, tag[1]) {
		t.Errorf("a new manifest kept the loader's tag")
	}
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"scopes":{"k1":{"name":"x","styles":["a.css"]}}}`), 0o644)
	time.Sleep(150 * time.Millisecond)
	if _, page, _ := get("/"); strings.Contains(page, "loader.js") {
		t.Errorf("an app without scripts loads the loader: %s", page)
	}
}

// A by-name push reaches only the methods the manifest lists for the
// component (what its scripts push), under either protocol; any other is an
// unknown handler. Handlers the page renders work as ever.
func TestAssetsPushGate(t *testing.T) {
	devAssets(t)
	a := scopedApp()
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	defer a.Stop(context.Background())
	for _, v := range []int{1, 2} {
		_, tok := pageToken(t, srv, "/")
		c, err := dial(t, srv, tok, nil)
		if err != nil {
			t.Fatal(err)
		}
		send(t, c, map[string]any{"type": "join", "ref": 1, "v": v})
		recv2(t, c)
		for i, target := range []string{"c1:Inc", "c1:Secret", "c1:Render", "c2:Render", "c1:0"} {
			send(t, c, map[string]any{"type": "event", "ref": 2 + i, "target": target, "event": "push"})
			m, raw := recv2(t, c)
			ok := target == "c1:Inc" || target == "c1:0"
			if ok && m.Type != "patch" || !ok && (m.Type != "error" || m.Error != `live: no handler "`+target+`"`) {
				t.Errorf("v%d %s: %s", v, target, raw)
			}
		}
		c.CloseNow()
	}
}

// What vuka build registers: the manifest and the modules, cached for good.
func TestAssetsRegistered(t *testing.T) {
	RegisterAssets([]byte(`{"scopes":{"k1":{"name":"c","module":"k1.12345678.js"}},"global":{"name":"app","module":"app.87654321.js","methods":["hi"]}}`))
	RegisterScript("k1.12345678.js", []byte("export default [];"))
	t.Cleanup(func() { RegisterAssets(nil); delete(scriptReg, "k1.12345678.js") })
	srv := httptest.NewServer(scopedApp().Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/_vuka/c/k1.12345678.js")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(b) != "export default [];" || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("module: %d %v %s", res.StatusCode, res.Header, b)
	}
	res, _ = http.Get(srv.URL + "/_vuka/c/loader.js")
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), `"g":"/_vuka/c/app.87654321.js"`) || !strings.Contains(string(b), `"f":{"":["hi"]}`) || strings.Contains(string(b), `"e":`) {
		t.Errorf("loader: %s", b)
	}
}

func TestIslandAssets(t *testing.T) {
	RegisterAssets([]byte(`{"islands":{"Counter":{"name":"Counter","module":"counter.abcd1234.js","framework":"vanilla"}}}`))
	RegisterIsland("counter.abcd1234.js", []byte("export function mount(){}"))
	t.Cleanup(func() { RegisterAssets(nil); delete(islandReg, "counter.abcd1234.js") })
	srv := httptest.NewServer(scopedApp().Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/_vuka/i/counter.abcd1234.js")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(b) != "export function mount(){}" || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("island chunk: %d %v %s", res.StatusCode, res.Header, b)
	}

	// The loader learns the island's module URL (adapter empty → vanilla).
	res, _ = http.Get(srv.URL + "/_vuka/c/loader.js")
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), `"i":{"Counter":["/_vuka/i/counter.abcd1234.js",""]}`) {
		t.Errorf("loader islands map missing: %s", b)
	}

	// An app with only islands still loads the loader.
	if res, _ := http.Get(srv.URL + "/_vuka/i/missing.js"); res.StatusCode != 404 {
		t.Errorf("unknown island should 404, got %d", res.StatusCode)
	}
}

// The toolbar's Assets panel lists the page's scoped components.
func TestAssetsToolbar(t *testing.T) {
	devAssets(t)
	toolbarInTests = true
	t.Cleanup(func() { toolbarInTests = false })
	a := New(Only("none"), Dev()).Live("/", &scopedCounter{})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	e := a.tb.get(res.Header.Get("X-Vuka-Toolbar"))
	if e == nil {
		t.Fatal("no record")
	}
	var b bytes.Buffer
	if err := renderPanels(&b, e.snapshot()); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{`data-panel="Assets"`, "web.scopedCounter", "web.scopedLabel", "<code>counter.ts</code>", "/_vuka/c/k1.0a1b2c3d.js"} {
		if !strings.Contains(html, want) {
			t.Errorf("the panel lacks %s", want)
		}
	}
}

// In Chrome: two components styling output each have their own color, and
// a component's button reaches its script's bump method with ui.JS.Call;
// a patch runs its updated hook.
func TestBrowserAssets(t *testing.T) {
	devAssets(t)
	a := scopedApp()
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	defer a.Stop(context.Background())
	p := chrome.Launch(t).Visit(srv.URL + "/")
	p.Wait(`String(window.hooks) === "mounted c1"`)
	var colors []string
	p.Eval(`["#count", "#label"].map((s) => getComputedStyle(document.querySelector(s)).color)`, &colors)
	if len(colors) != 2 || colors[0] != "rgb(200, 0, 0)" || colors[1] != "rgb(0, 0, 200)" {
		t.Errorf("colors: %v", colors)
	}
	p.Wait(`document.documentElement.dataset.vkState === "connected"`)
	p.Click("#bump")
	p.Wait(`String(window.hooks) === "mounted c1,bump 2 bump click c1"`)
	p.Click("#inc")
	p.WaitText("#count", "1")
	p.Wait(`String(window.hooks) === "mounted c1,bump 2 bump click c1,updated c1"`)

	// The scripts push Inc, which the manifest lists; not Secret.
	p.Eval(`(async () => { const inst = window.vukaComponents.instances()[0]; await inst.hooks[0].press(inst.el, inst.ctx, "Inc"); await inst.hooks[0].press(inst.el, inst.ctx, "Secret"); })()`, nil)
	p.WaitText("#count", "2")
	p.Wait(`String(window.hooks).endsWith("updated c1,pushed Inc,refused live: no handler \"c1:Secret\"")`)
}
