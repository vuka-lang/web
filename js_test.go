package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/web/internal/chrome"
)

// The runtime's JS commands, in node over a small DOM.
func TestLiveJSCommands(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	out, err := exec.Command(node, "testdata/livejs/js.js", "live.js").CombinedOutput()
	if err != nil {
		t.Fatalf("%s", out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

type op = []any

func attrs(kv ...any) []ui.Attr {
	var as []ui.Attr
	for i := 0; i+1 < len(kv); i += 2 {
		as = append(as, ui.Attr{Name: kv[i].(string), Value: kv[i+1]})
	}
	return as
}

// A page with JS commands and no live parts: a dropdown, a modal and a copy
// button.
func jsStaticPage() ui.Node {
	cancel, _ := ui.EncodeJS(context.Background(), ui.JS.Hide(ui.This).PopFocus())
	return ui.El("main", nil,
		ui.El("button", attrs("id", "menu-btn", "type", "button", "aria-expanded", "false",
			"onClick", ui.On(ui.JS.ToggleClass("#menu", "open").ToggleAttr(ui.This, "aria-expanded", "true", "false"))), ui.Text("Menu")),
		ui.El("ul", attrs("id", "menu", "class", "menu"), ui.El("li", nil, ui.Text("One"))),
		ui.El("button", attrs("id", "open", "type", "button", "onClick", ui.On(ui.JS.PushFocus().
			Show("#modal", ui.Transition("fade", "o-0", "o-100"), ui.Time(300*time.Millisecond)).
			FocusFirst("#modal"))), ui.Text("Open")),
		ui.El("div", attrs("id", "modal", "hidden", true, "data-cancel", string(cancel),
			"onKeyDown", ui.On(ui.JS.Key("Escape").Exec(ui.This, "data-cancel"))),
			ui.El("input", attrs("id", "name")),
			ui.El("button", attrs("id", "close", "type", "button", "onClick", ui.On(ui.JS.Exec(ui.Closest("#modal"), "data-cancel"))), ui.Text("×"))),
		ui.El("input", attrs("id", "code", "value", "go run .", "readonly", true)),
		ui.El("button", attrs("id", "copy", "type", "button", "onClick", ui.On(ui.JS.Copy("#code"))), ui.Text("Copy")),
	)
}

// A live component whose elements run commands: a dropdown whose items
// push their value, deletes that ask first, and a handler that pushes
// commands back.
type jsDemo struct {
	ui.Live
	items  []string
	picked string
	tone   string
	n      int
	saved  int
}

func (d *jsDemo) Mount()          { d.items, d.tone = []string{"a", "b", "c"}, "calm" }
func (d *jsDemo) Pick(e ui.Event) { d.picked = e.Value }
func (d *jsDemo) Delete(e ui.Event) {
	d.items = slices.DeleteFunc(d.items, func(s string) bool { return s == e.Value })
}
func (d *jsDemo) Inc()  { d.n++ }
func (d *jsDemo) Loud() { d.tone = "loud" }
func (d *jsDemo) Save() {
	d.saved++
	d.PushJS(ui.JS.On(ui.Ref("note")).AddClass("flash").SetAttr("data-n", fmt.Sprint(d.saved)))
}

func (d *jsDemo) Render() ui.Node {
	var menu, items []ui.Node
	for _, it := range d.items {
		menu = append(menu, ui.El("li", nil, ui.El("button", attrs("type", "button", "value", it,
			"onClick", ui.On(ui.JS.AddClass(ui.Ref("menu"), "hidden").Push(d.Pick))), ui.Text(it))))
		items = append(items, ui.El("li", attrs("id", "item-"+it), ui.Text(it), ui.El("button", attrs("class", "del", "type", "button", "value", it,
			"onClick", ui.On(ui.JS.Confirm("Delete "+it+"?").Push(d.Delete))), ui.Text("delete"))))
	}
	return ui.El("div", attrs("class", "demo"),
		ui.El("button", attrs("id", "pick", "type", "button", "onClick", ui.On(ui.JS.RemoveClass(ui.Ref("menu"), "hidden"))), ui.Text("Pick")),
		ui.El("ul", attrs("ref", "menu", "class", "menu hidden"), menu...),
		ui.El("output", attrs("id", "picked"), ui.Text(d.picked)),
		ui.El("ul", attrs("id", "items"), items...),
		ui.El("p", attrs("id", "tone", "class", "tone "+d.tone), ui.Text(d.n)),
		ui.El("button", attrs("id", "inc", "type", "button", "onClick", ui.On(ui.JS.AddClass("#tone", "marked").Push(d.Inc))), ui.Text("+")),
		ui.El("button", attrs("id", "loud", "type", "button", "onClick", ui.On(d.Loud)), ui.Text("loud")),
		ui.El("p", attrs("ref", "note", "class", "note"), ui.Text(fmt.Sprint("saved ", d.saved))),
		ui.El("button", attrs("id", "save", "type", "button", "onClick", ui.On(d.Save)), ui.Text("save")),
	)
}

func jsApp(opts ...Option) *App {
	app := New(append([]Option{Only("none")}, opts...)...)
	app.Get("/static", jsStaticPage)
	app.Live("/live", &jsDemo{})
	return app
}

func TestJSStaticPageHasNoToken(t *testing.T) {
	srv := httptest.NewServer(jsApp().Handler())
	defer srv.Close()
	body, tok := pageToken(t, srv, "/static")
	if tok != "" || !strings.Contains(body, `<script src="/_vuka/live.js?v=`) {
		t.Fatalf("a page with only JS commands: %s", body)
	}
	if _, tok := pageToken(t, srv, "/live"); tok == "" {
		t.Fatal("a live page without its token")
	}
}

func TestBrowserJSStatic(t *testing.T) {
	srv := httptest.NewServer(jsApp().Handler())
	defer srv.Close()
	p := chrome.Launch(t).Visit(srv.URL + "/static")
	p.Click("#menu-btn")
	p.Wait(`document.querySelector("#menu").className === "menu open" && document.querySelector("#menu-btn").getAttribute("aria-expanded") === "true"`)
	p.Click("#menu-btn")
	p.Wait(`document.querySelector("#menu").className === "menu" && document.querySelector("#menu-btn").getAttribute("aria-expanded") === "false"`)

	// The modal: shown with a transition, its first field focused; Escape
	// hides it and gives the focus back.
	p.Click("#open")
	p.Wait(`(() => { const m = document.querySelector("#modal"); return !m.hidden && m.classList.contains("fade") && document.activeElement.id === "name"; })()`)
	p.Wait(`document.querySelector("#modal").className === "" && getComputedStyle(document.querySelector("#modal")).display === "block"`)
	p.Press("Escape")
	p.Wait(`getComputedStyle(document.querySelector("#modal")).display === "none" && document.activeElement.id === "open"`)
	p.Click("#open")
	p.Wait(`document.activeElement.id === "name"`)
	p.Click("#close")
	p.Wait(`getComputedStyle(document.querySelector("#modal")).display === "none" && document.activeElement.id === "open"`)

	p.Eval(`navigator.clipboard.writeText = (t) => { window.copied = t; return Promise.resolve(); }`, nil)
	p.Click("#copy")
	p.Wait(`window.copied === "go run ." && document.querySelector("#copy").hasAttribute("data-copied")`)

	p.Wait(`window.__wire.sockets.length === 0 && !document.documentElement.dataset.vkState`)
}

func TestBrowserJSLive(t *testing.T) {
	b := chrome.Launch(t)
	for _, v := range []int{1, 2} {
		t.Run(fmt.Sprint("v", v), func(t *testing.T) {
			srv := httptest.NewServer(jsApp(LiveProtocol(v)).Handler())
			defer srv.Close()
			p := b.Open(srv.URL + "/live")
			p.Wait(fmt.Sprintf(`document.documentElement.dataset.vkProtocol === "%d"`, v))

			// A dropdown: opened in the browser, an item closes it and
			// pushes its value to the server.
			p.Click("#pick")
			p.Wait(`document.querySelector("[data-vk-ref=menu]").className === "menu"`)
			p.Click(`[data-vk-ref=menu] button[value=b]`)
			p.WaitText("#picked", "b")
			p.Wait(`document.querySelector("[data-vk-ref=menu]").className === "menu hidden"`)

			// Reconciliation: what a command changed stays across patches
			// that render the attribute as before; the server's change wins.
			p.Click("#pick")
			p.Click("#inc") // the chain, then the server's handler
			p.WaitText("#tone", "1")
			p.Wait(`document.querySelector("#tone").className === "tone calm marked" && document.querySelector("[data-vk-ref=menu]").className === "menu"`)
			p.Click("#loud")
			p.Wait(`document.querySelector("#tone").className === "tone loud"`)

			// Confirm, then push: declined, nothing goes.
			p.Eval(`window.confirm = () => false`, nil)
			p.Click("#item-b .del")
			p.Click("#inc")
			p.WaitText("#tone", "2")
			p.Wait(`document.querySelector("#item-b")`)
			p.Eval(`window.confirm = (m) => (window.asked = m, true)`, nil)
			p.Click("#item-b .del")
			p.Wait(`!document.querySelector("#item-b") && window.asked === "Delete b?"`)

			// Commands a handler pushes run once its reply is applied, and
			// what they change stays through later patches.
			p.Click("#save")
			p.Wait(`(() => { const n = document.querySelector("[data-vk-ref=note]"); return n.textContent === "saved 1" && n.className === "note flash" && n.dataset.n === "1"; })()`)
			p.Click("#inc")
			p.WaitText("#tone", "3")
			p.Wait(`document.querySelector("[data-vk-ref=note]").className === "note flash"`)
		})
	}
}

// Commands a server pushes with a render or a patch, from a stand-in
// server: they run once the message is applied, on the instance's root, and
// what they change stays until the server renders the attribute otherwise.
func TestBrowserJSPushed(t *testing.T) {
	b := chrome.Launch(t)
	for _, v := range []int{1, 2} {
		t.Run(fmt.Sprint("v", v), func(t *testing.T) {
			srv := httptest.NewServer(pushingServer(t, v))
			defer srv.Close()
			p := b.Open(srv.URL + "/")
			p.Wait(fmt.Sprintf(`document.documentElement.dataset.vkProtocol === "%d" && document.querySelector("#top").className === "joined"`, v))
			p.Click("#go")
			p.Wait(`(() => { const m = document.querySelector("[data-vk-ref=msg]"); return m.textContent === "1" && m.className === "msg flash" && m.dataset.n === "1"; })()`)
			p.Click("#go")
			p.Wait(`(() => { const m = document.querySelector("[data-vk-ref=msg]"); return m.textContent === "2" && m.className === "msg flash" && m.dataset.n === "2"; })()`)
			p.Click("#go") // the server renders another class
			p.Wait(`(() => { const m = document.querySelector("[data-vk-ref=msg]"); return m.textContent === "3" && m.className === "msg done"; })()`)
		})
	}
}

func pushingServer(t *testing.T, v int) http.Handler {
	pageHTML := func(n int) (string, string) {
		cls := "msg"
		if n >= 3 {
			cls = "msg done"
		}
		return cls, fmt.Sprint(n)
	}
	statics := []string{`<div data-vk-id="c1"><p data-vk-ref="msg" class="`, `">`, `</p><button id="go" type="button" data-vk-on-click="c1:0">go</button></div><p id="top"></p>`}
	body := func(n int) string {
		cls, text := pageHTML(n)
		return statics[0] + cls + statics[1] + text + statics[2]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/_vuka/live.js", serveLiveJS)
	mux.HandleFunc("/_vuka/live", func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := context.Background()
		n := 0
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m struct {
				Type string `json:"type"`
				Ref  int    `json:"ref"`
			}
			json.Unmarshal(data, &m)
			var reply map[string]any
			pushed := func(id string, ops ...op) []any { return []any{map[string]any{"id": id, "ops": ops}} }
			switch m.Type {
			case "join":
				reply = map[string]any{"type": "render", "ref": m.Ref, "js": pushed("c0", op{"add_class", map[string]any{"to": map[string]any{"sel": "#top"}, "names": []string{"joined"}}})}
				if v == 2 {
					cls, text := pageHTML(n)
					reply["v"], reply["trees"] = 2, []any{map[string]any{"id": "c0", "full": true, "tree": map[string]any{"t": "p", "s": statics, "d": []string{cls, text}}}}
				} else {
					reply["html"] = body(n)
				}
			case "event":
				n++
				ops := []op{op{"set_attr", map[string]any{"to": map[string]any{"ref": "msg"}, "name": "data-n", "value": fmt.Sprint(n)}}}
				if n == 1 {
					ops = append(ops, op{"add_class", map[string]any{"to": map[string]any{"ref": "msg"}, "names": []string{"flash"}}})
				}
				reply = map[string]any{"type": "patch", "ref": m.Ref, "js": pushed("c1", ops...)}
				if v == 2 {
					cls, text := pageHTML(n)
					reply["trees"] = []any{map[string]any{"id": "c0", "tree": map[string]any{"u": map[string]any{"0": cls, "1": text}}}}
				} else {
					reply["updates"] = []any{map[string]any{"id": "c1", "html": strings.SplitAfter(body(n), "</div>")[0]}}
				}
			default:
				continue
			}
			b, _ := json.Marshal(reply)
			if ws.Write(ctx, websocket.MessageText, b) != nil {
				return
			}
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html><html><head><title>pushed</title></head><body>`+body(0)+`<script src="/_vuka/live.js" data-vk-token="x" defer></script></body></html>`)
	})
	return mux
}
