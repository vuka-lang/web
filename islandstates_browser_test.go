package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// earlyIsland is a live page with an island taking client states as props:
// n a state, f a field of a struct state, m a state the island writes as it
// mounts.
type earlyIsland struct{ ui.Live }

func (c *earlyIsland) Render() ui.Node {
	return ui.Safe(`<main><vk-island data-vk-island="Early" data-vk-client="load" data-vk-props="{` +
		`&#34;n&#34;:{&#34;$vk&#34;:&#34;state&#34;,&#34;s&#34;:&#34;t:n&#34;,&#34;i&#34;:3},` +
		`&#34;f&#34;:{&#34;$vk&#34;:&#34;state&#34;,&#34;s&#34;:&#34;t:o&#34;,&#34;i&#34;:{&#34;a&#34;:4},&#34;p&#34;:[&#34;a&#34;]},` +
		`&#34;m&#34;:{&#34;$vk&#34;:&#34;state&#34;,&#34;s&#34;:&#34;t:m&#34;,&#34;i&#34;:0}}"></vk-island></main>`)
}

// An island whose module loads before the live runtime runs reads its state
// props' initial values, follows the states once the runtime starts, and a
// state it wrote before reaches the runtime (vuka-lang/vuka#244).
func TestBrowserIslandStatesBeforeLive(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "i"), 0o755)
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"islands":{"Early":{"name":"Early","module":"early.js"}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "i", "early.js"), []byte(`export function mount(el, props, ctx) {
	window.__early = !(window.vuka && window.vuka.states);
	const show = (p) => { el.textContent = "n " + p.n + " f " + p.f + " m " + p.m; };
	ctx.set("m", 9);
	show({ ...props, m: 9 });
	return { update: show };
}
`), 0o644)
	t.Setenv(AssetsEnv, dir)

	app := New(Only("none"))
	app.Live("/early", &earlyIsland{})
	h := app.Handler()
	// live.js comes late: the island's module loads and mounts first.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/live.js") {
			time.Sleep(500 * time.Millisecond)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/early")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	var early bool
	p.Eval(`window.__early === true`, &early)
	if !early {
		t.Fatalf("the island didn't mount before the live runtime ran")
	}
	p.WaitText("vk-island", "n 3 f 4 m 9")
	var m float64
	p.Eval(`window.vuka.states.read({s: "t:m", i: 0})`, &m)
	if m != 9 {
		t.Errorf("state m = %v, want 9: what the island set before the runtime ran", m)
	}
	p.Eval(`window.vuka.states.write({s: "t:n", i: 3}, 7), window.vuka.states.write({s: "t:o", i: {a: 4}, p: ["a"]}, 8), 0`, nil)
	p.WaitText("vk-island", "n 7 f 8 m 9")
}
