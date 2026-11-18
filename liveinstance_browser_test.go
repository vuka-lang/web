package web

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// instancePage renders one of two components of the same markup shape, at
// the same place, each root with its own instance id; the frame stays, only
// the root's identity and the scope's props differ.
type instancePage struct {
	ui.Live
	second bool
	n      int
	bare   bool // the roots carry no instance id: the scope alone tells them apart
}

func (p *instancePage) Toggle() { p.second = !p.second }
func (p *instancePage) Bump()   { p.n++ }

func (p *instancePage) Render() ui.Node {
	id, scope := "i1", "k1"
	if p.second {
		id, scope = "i2", "k2"
	}
	attrs := []ui.Attr{{Name: "class", Value: "comp"}, {Name: "data-vk-s", Value: scope}}
	if !p.bare {
		attrs = append(attrs, ui.Attr{Name: "data-vk-id", Value: id})
	}
	root := ui.El("div", attrs,
		ui.El("div", []ui.Attr{{Name: "class", Value: "pop"}}, ui.Child(p.n)))
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "id", Value: "toggle"}, {Name: "onClick", Value: ui.On(p.Toggle)}}, ui.Text("toggle")),
		ui.El("button", []ui.Attr{{Name: "id", Value: "bump"}, {Name: "onClick", Value: ui.On(p.Bump)}}, ui.Text("bump")),
		root)
}

func instanceBrowser(t *testing.T, page *instancePage) *chrome.Page {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "c"), 0o755)
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", `{"scopes":{"k1":{"name":"web.instA","scripts":["a.ts"],"module":"k1.0a1b2c3d.js","type":"github.com/vuka-lang/web/v2.instancePage"},"k2":{"name":"web.instB","scripts":["b.ts"],"module":"k2.0a1b2c3d.js","type":"github.com/vuka-lang/web/v2.instancePage"}}}`)
	write("c/k1.0a1b2c3d.js", `export default [{
	mounted(el) { (window.hooks ||= []).push("mounted A"); el.querySelector(".pop").style.width = "123px"; el.style.left = "5px"; },
	destroyed() { window.hooks.push("destroyed A"); },
}];
`)
	write("c/k2.0a1b2c3d.js", `export default [{
	mounted(el) { (window.hooks ||= []).push("mounted B"); },
	destroyed() { window.hooks.push("destroyed B"); },
}];
`)
	t.Setenv(AssetsEnv, dir)
	app := New(Only("none"))
	app.Live("/", page, Script("a.ts"), Script("b.ts"))
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Open(srv.URL + "/")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected" && String(window.hooks) === "mounted A"`)
	return p
}

// TestBrowserInstanceChangeDropsOwnedState: what a component's script wrote
// on the server's elements belongs to its instance; the element another
// component's markup takes over at that place doesn't inherit it.
func TestBrowserInstanceChangeDropsOwnedState(t *testing.T) {
	p := instanceBrowser(t, &instancePage{})
	p.Click("#toggle")
	p.Wait(`String(window.hooks) === "mounted A,destroyed A,mounted B"`)
	var got string
	p.Eval(`(() => { const e = document.querySelector(".comp"); return (e.getAttribute("style") || "") + "|" + (e.querySelector(".pop").getAttribute("style") || ""); })()`, &got)
	if got != "|" {
		t.Fatalf("the new instance inherited the old one's attributes: %q", got)
	}
	p.Click("#toggle")
	p.Wait(`String(window.hooks) === "mounted A,destroyed A,mounted B,destroyed B,mounted A"`)
	p.Eval(`(() => { const e = document.querySelector(".comp"); return e.style.left + "|" + e.querySelector(".pop").style.width; })()`, &got)
	if got != "5px|123px" {
		t.Fatalf("the remounted script's own writes: %q", got)
	}
}

// TestBrowserSameInstanceKeepsOwnedState: a patch that leaves the instance
// as it is keeps what its script wrote.
func TestBrowserSameInstanceKeepsOwnedState(t *testing.T) {
	p := instanceBrowser(t, &instancePage{})
	p.Eval(`document.querySelector(".pop").style.height = "7px"; window.__first = document.querySelector(".comp")`, nil)
	p.Click("#bump")
	p.WaitText(".pop", "1")
	var ok bool
	p.Eval(`document.querySelector(".comp") === window.__first && document.querySelector(".pop").style.height === "7px" && document.querySelector(".pop").style.width === "123px" && String(window.hooks) === "mounted A"`, &ok)
	if !ok {
		t.Fatal("a patch of the same instance lost the script's attributes or remounted it")
	}
}

// TestBrowserScopeChangeDropsOwnedState: a component's root that carries no
// instance id (it sits inside a live component) is told apart by its scope;
// the markup of another scope taking its place doesn't inherit what the old
// script wrote on it.
func TestBrowserScopeChangeDropsOwnedState(t *testing.T) {
	p := instanceBrowser(t, &instancePage{bare: true})
	p.Click("#toggle")
	p.Wait(`String(window.hooks) === "mounted A,destroyed A,mounted B"`)
	var got string
	p.Eval(`(() => { const e = document.querySelector(".comp"); return (e.getAttribute("style") || "") + "|" + (e.querySelector(".pop").getAttribute("style") || ""); })()`, &got)
	if got != "|" {
		t.Fatalf("the new component inherited the old one's attributes: %q", got)
	}
}
