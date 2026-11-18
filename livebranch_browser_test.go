package web

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// branchPage swaps one {if} branch for another; both render the root of the
// same component script (scope k1) at the same place, with other props.
type branchPage struct {
	ui.Live
	second bool
	n      int
	keyed  bool // each branch in a <div key>, as the same element with another key
	rekey  bool // one frame, in a <div key> whose key follows second
	multi  bool // branches of three elements each, the same tags
	marked bool // hand-written markup, each branch's root saying which it is (data-vk-br)
}

func (p *branchPage) Toggle() { p.second = !p.second }
func (p *branchPage) Bump()   { p.n++ }

func (p *branchPage) Render() ui.Node {
	root := func(props string, kids ...ui.Node) ui.Node {
		return ui.El("div", []ui.Attr{{Name: "class", Value: "combo"}, {Name: "data-vk-p", Value: props}, {Name: "data-vk-s", Value: "k1"}}, kids...)
	}
	var branch ui.Node
	switch {
	case p.marked:
		br, props := "one", `{"n":1}`
		if p.second {
			br, props = "two", `{"n":2}`
		}
		branch = ui.El("div", []ui.Attr{{Name: "class", Value: "combo"}, {Name: "data-vk-br", Value: br}, {Name: "data-vk-p", Value: props}, {Name: "data-vk-s", Value: "k1"}}, ui.Text(br))
	case p.multi:
		fp, shape, class := uint64(0x3b6), "Es(t)Ess()Es(t)", "r1"
		if p.second {
			fp, class = 0x3b7, "r2"
		}
		branch = ui.F(fp, shape,
			ui.El("div", []ui.Attr{{Name: "class", Value: class}}, ui.Text("x")),
			ui.El("input", []ui.Attr{{Name: "id", Value: "field"}, {Name: "type", Value: "text"}}),
			ui.El("p", []ui.Attr{{Name: "title", Value: "a>b"}}, ui.Text("p")))
	case p.rekey:
		props, key, text := `{"n":1}`, "first", "one"
		if p.second {
			props, key, text = `{"n":2}`, "second", "two"
		}
		branch = ui.F(0x3b5, "Ed(Esds(h))", ui.El("div", []ui.Attr{{Name: "key", Value: key}}, root(props, ui.Child(text))))
	case p.keyed && p.second:
		branch = ui.F(0x3b4, "Ed(Esds(t))", ui.El("div", []ui.Attr{{Name: "key", Value: "second"}}, root(`{"n":2}`, ui.Text("two"))))
	case p.keyed:
		branch = ui.F(0x3b3, "Ed(Esds(t))", ui.El("div", []ui.Attr{{Name: "key", Value: "first"}}, root(`{"n":1}`, ui.Text("one"))))
	case p.second:
		branch = ui.F(0x3b2, "Esds(t)", root(`{"n":2}`, ui.Text("two")))
	default:
		branch = ui.F(0x3b1, "Esds(t)", root(`{"n":1}`, ui.Text("one")))
	}
	return ui.F(0x3b0, "E(Esd(t)Esd(t)Es(h)b)",
		ui.El("main", nil,
			ui.El("button", []ui.Attr{{Name: "id", Value: "toggle"}, {Name: "onClick", Value: ui.On(p.Toggle)}}, ui.Text("toggle")),
			ui.El("button", []ui.Attr{{Name: "id", Value: "bump"}, {Name: "onClick", Value: ui.On(p.Bump)}}, ui.Text("bump")),
			ui.El("output", []ui.Attr{{Name: "id", Value: "n"}}, ui.Child(p.n)),
			ui.Nodes(func(add func(ui.Node)) { add(branch) })))
}

func branchAssets(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "c"), 0o755)
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", `{"scopes":{"k1":{"name":"web.branchPage","scripts":["combo.ts"],"module":"k1.0a1b2c3d.js","type":"github.com/vuka-lang/web/v2.branchPage"}}}`)
	write("c/k1.0a1b2c3d.js", `export default [{
	mounted(el) { (window.hooks ||= []).push("mounted " + JSON.parse(el.getAttribute("data-vk-p")).n); el.style.width = "123px"; el.dataset.script = "1"; },
	destroyed(el) { window.hooks.push("destroyed " + JSON.parse(el.getAttribute("data-vk-p")).n); },
}];
`)
	t.Setenv(AssetsEnv, dir)
}

func branchBrowser(t *testing.T, page *branchPage) *chrome.Page {
	t.Helper()
	branchAssets(t)
	app := New(Only("none"))
	app.Live("/", page, Script("combo.ts"))
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Open(srv.URL + "/")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected" && (!document.querySelector(".combo") || String(window.hooks) === "mounted 1")`)
	return p
}

// TestBrowserBranchSwitchRemounts: a patch that changes which {if} branch
// renders replaces its elements and remounts the component script, though the
// two branches' roots share a tag, a scope and a place; what the script wrote
// in the browser doesn't carry over.
func TestBrowserBranchSwitchRemounts(t *testing.T) {
	p := branchBrowser(t, &branchPage{})
	p.Eval(`window.__first = document.querySelector(".combo")`, nil)
	p.Click("#toggle")
	p.Wait(`String(window.hooks) === "mounted 1,destroyed 1,mounted 2"`)
	var same bool
	p.Eval(`document.querySelector(".combo") === window.__first`, &same)
	if same {
		t.Fatal("the second branch reuses the first branch's element")
	}
	p.Eval(`document.querySelector(".combo").style.width = "999px"; document.querySelector(".combo").dataset.stale = "1"`, nil)
	p.Click("#toggle")
	p.Wait(`String(window.hooks) === "mounted 1,destroyed 1,mounted 2,destroyed 2,mounted 1"`)
	var got string
	p.Eval(`(() => { const e = document.querySelector(".combo"); return e.style.width + "|" + (e.dataset.stale || "") + "|" + e.textContent; })()`, &got)
	if got != "123px||one" {
		t.Errorf("the first branch again: %q", got)
	}
}

// TestBrowserBranchSameMorphsInPlace: a patch that leaves the branch as it
// is morphs its elements in place, and the script isn't remounted.
func TestBrowserBranchSameMorphsInPlace(t *testing.T) {
	p := branchBrowser(t, &branchPage{})
	p.Eval(`window.__first = document.querySelector(".combo")`, nil)
	p.Click("#bump")
	p.WaitText("#n", "1")
	var same bool
	p.Eval(`document.querySelector(".combo") === window.__first && document.querySelector(".combo").style.width === "123px" && String(window.hooks) === "mounted 1"`, &same)
	if !same {
		t.Fatal("a patch that kept the branch replaced its element or remounted its script")
	}
}

// TestBrowserBranchKeyedSwitch: a key outside a loop is an element's identity
// too: the same element with another key is another element.
func TestBrowserBranchKeyedSwitch(t *testing.T) {
	p := branchBrowser(t, &branchPage{keyed: true})
	p.Click("#toggle")
	p.Wait(`String(window.hooks) === "mounted 1,destroyed 1,mounted 2"`)
	p.Click("#toggle")
	p.Wait(`String(window.hooks) === "mounted 1,destroyed 1,mounted 2,destroyed 2,mounted 1"`)
}

// TestBrowserKeyChangeRemounts: the frame stays and its key changes: the
// element is another one, and the script remounts.
func TestBrowserKeyChangeRemounts(t *testing.T) {
	p := branchBrowser(t, &branchPage{rekey: true})
	p.Click("#toggle")
	p.Wait(`String(window.hooks) === "mounted 1,destroyed 1,mounted 2"`)
	p.Click("#toggle")
	p.Wait(`String(window.hooks) === "mounted 1,destroyed 1,mounted 2,destroyed 2,mounted 1"`)
}

// TestBrowserBranchSwitchReplacesEveryRoot: a branch of several elements is
// replaced whole, though the other branch has the same tags in the same
// order; a patch within one branch keeps its elements, what was typed in
// them and their focus.
func TestBrowserBranchSwitchReplacesEveryRoot(t *testing.T) {
	p := branchBrowser(t, &branchPage{multi: true})
	p.Eval(`(() => { window.__old = Array.from(document.querySelector("main").children).slice(3); const f = document.querySelector("#field"); f.value = "typed"; f.focus(); })()`, nil)
	p.Eval(`document.querySelector("#bump").click()`, nil)
	p.WaitText("#n", "1")
	var kept bool
	p.Eval(`(() => { const now = Array.from(document.querySelector("main").children).slice(3); return now.length === 3 && now.every((e, i) => e === window.__old[i]) &&
		document.activeElement === now[1] && now[1].value === "typed"; })()`, &kept)
	if !kept {
		t.Fatal("a patch within the branch replaced its elements, or lost the typed value or focus")
	}
	p.Click("#toggle")
	p.Wait(`document.querySelector("#field").value === ""`)
	var fresh bool
	p.Eval(`(() => { const now = Array.from(document.querySelector("main").children).slice(3); return now.length === 3 && now.every((e, i) => e !== window.__old[i]); })()`, &fresh)
	if !fresh {
		t.Fatal("a root of the new branch is an element of the old one")
	}
}

// TestBrowserBranchMarkedByServer: markup that names its branch itself
// (data-vk-br, as a server rendering no tree does) is replaced when the name
// changes and morphed in place while it doesn't.
func TestBrowserBranchMarkedByServer(t *testing.T) {
	p := branchBrowser(t, &branchPage{marked: true})
	p.Eval(`window.__first = document.querySelector(".combo")`, nil)
	p.Eval(`document.querySelector("#bump").click()`, nil)
	p.WaitText("#n", "1")
	var same bool
	p.Eval(`document.querySelector(".combo") === window.__first && String(window.hooks) === "mounted 1"`, &same)
	if !same {
		t.Fatal("a patch that kept the branch replaced its element")
	}
	p.Click("#toggle")
	p.Wait(`String(window.hooks) === "mounted 1,destroyed 1,mounted 2"`)
	p.Eval(`document.querySelector(".combo") !== window.__first`, &same)
	if !same {
		t.Fatal("the other branch took the first one's element")
	}
}
