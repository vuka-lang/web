package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/forms"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// renameRow is the form of a task's row.
type renameRow struct {
	ui.FormState
	Name string `validate:"required,max=20"`
}

// CleanName takes its time for a name starting "slow" — a check that
// looks something up — and refuses "slow-bad".
func (f *renameRow) CleanName(ctx context.Context) error {
	if strings.HasPrefix(f.Name, "slow") {
		time.Sleep(400 * time.Millisecond)
	}
	if f.Name == "slow-bad" {
		return forms.Errorf("Not that one")
	}
	return nil
}

// rowsPage is a stream of tasks, each row holding a form renaming it,
// written as the compiler writes a .vuka.html file's.
type rowsPage struct {
	ui.Live
	tasks ui.Stream[feedItem]
}

func (p *rowsPage) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }

func (p *rowsPage) mount() error {
	p.tasks.Reset(feedItem{1, "one"}, feedItem{2, "two"})
	return nil
}

func (p *rowsPage) Rename(ctx context.Context, id int, f renameRow) error {
	if f.Name == "taken" {
		return f.Fail("That name is taken")
	}
	p.tasks.Insert(feedItem{id, f.Name})
	return nil
}

func (p *rowsPage) Render() ui.Node {
	var lis []ui.Node
	for _, it := range p.tasks.Rows() {
		lis = append(lis, ui.HTML(`<li key={id}><span class="name">{text}</span>{form}</li>`,
			it.ID, it.Text, ui.Form{For: &renameRow{Name: it.Text}, OnSubmit: ui.JS.Push(p.Rename, it.ID)}))
	}
	rows := p.tasks.Block(lis)
	return ui.HTML(`<main><h1>Tasks</h1><ul id="tasks" {attrs...}>{rows}</ul></main>`, rows.Attrs(), rows)
}

// TestBrowserStreamRowForm: a struct form in a stream row submits as a
// push of its owner's method; failing its checks, its errors are placed on
// that row's form from the reply's report — the row isn't rendered again,
// so what was typed stays, and so does focus in another row being typed
// in meanwhile. A success inserts the row again, which morphs in place
// keeping what was typed since the submit, and clears the form's errors.
func TestBrowserStreamRowForm(t *testing.T) {
	a := New(Only("none"))
	a.Live("/tasks", func() *rowsPage { return &rowsPage{} })
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Start(srv.URL+"/tasks", `document.documentElement.dataset.vkState === "connected"`)
	idle := ` && !document.querySelector("[aria-busy]")`
	val := func(sel string) string {
		var s string
		if err := p.Eval(`document.querySelector(`+fmt.Sprintf("%q", sel)+`).value`, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	focused := func() string {
		var s string
		p.Eval(`document.activeElement && document.activeElement.id`, &s)
		return s
	}
	p.Eval(`document.querySelectorAll("#tasks > li").forEach((l) => l.__mark = l.dataset.vkRow)`, nil)
	marked := func() {
		t.Helper()
		var ok bool
		p.Eval(`[...document.querySelectorAll("#tasks > li")].every((l) => l.__mark === l.dataset.vkRow)`, &ok)
		if !ok {
			t.Fatal("a row was replaced rather than kept")
		}
	}

	// a field's error, placed on row 1's form; row 2's has none
	p.Eval(`document.querySelector("#rename-row-1-name").value = ""`, nil)
	p.Click(`#rename-row-1 button[type="submit"]`)
	p.Wait(`document.querySelector("#rename-row-1-name-error") !== null` + idle)
	var st struct {
		Invalid, Summary, Other bool
	}
	p.Eval(`({invalid: document.querySelector("#rename-row-1-name").getAttribute("aria-invalid") === "true",
		summary: !!document.querySelector("#rename-row-1-summary"),
		other: !!document.querySelector("#rename-row-2 .vk-field__error, #rename-row-2-summary")})`, &st)
	if !st.Invalid || !st.Summary || st.Other {
		t.Fatalf("after row 1's failed submit: %+v", st)
	}
	if f := focused(); f != "rename-row-1-summary" {
		t.Fatalf("focus is on %q, not row 1's summary", f)
	}
	marked()

	// typing in row 2 while row 1's slow check runs: its report leaves row
	// 2's field, its text and its focus alone
	p.Eval(`document.querySelector("#rename-row-1-name").value = "slow-bad"`, nil)
	p.Type("#rename-row-2-name", "ab")
	p.Eval(`document.querySelector("#rename-row-1").requestSubmit()`, nil)
	p.Keys("cd")
	p.Wait(`(document.querySelector("#rename-row-1-name-error") || {}).textContent.includes("Not that one")` + idle)
	if v, f := val("#rename-row-2-name"), focused(); v != "twoabcd" || f != "rename-row-2-name" {
		t.Fatalf("row 2's field holds %q with focus on %q", v, f)
	}
	if v := val("#rename-row-1-name"); v != "slow-bad" {
		t.Fatalf("row 1's field holds %q", v)
	}
	marked()

	// a success typed on after the submit: the row comes back renamed, in
	// place, the field keeping what was typed since and the errors gone
	p.Eval(`(() => { const f = document.querySelector("#rename-row-1-name"); f.value = "slow-ok"; f.focus(); })()`, nil)
	p.Eval(`document.querySelector("#rename-row-1").requestSubmit()`, nil)
	p.Keys("!")
	p.WaitText(`li[data-vk-row="1"] .name`, "slow-ok")
	p.Wait(`!document.querySelector("#rename-row-1-name-error") && !document.querySelector("#rename-row-1-summary")` + idle)
	if v := val("#rename-row-1-name"); v != "slow-ok!" {
		t.Fatalf("row 1's field holds %q, not what was typed since its submit", v)
	}
	var invalid any
	p.Eval(`document.querySelector("#rename-row-1-name").getAttribute("aria-invalid")`, &invalid)
	if invalid != nil {
		t.Fatalf("row 1's field is still aria-invalid=%v", invalid)
	}
	marked()

	// the method's own error: in row 2's summary
	p.Eval(`document.querySelector("#rename-row-2-name").value = "taken"`, nil)
	p.Click(`#rename-row-2 button[type="submit"]`)
	p.Wait(`(document.querySelector("#rename-row-2-summary") || {}).textContent.includes("That name is taken")` + idle)
	p.Eval(`document.querySelector("#rename-row-2-name").value = "zwei"`, nil)
	p.Click(`#rename-row-2 button[type="submit"]`)
	p.WaitText(`li[data-vk-row="2"] .name`, "zwei")
	p.Wait(`!document.querySelector("#rename-row-2-summary")` + idle)
	if v := val("#rename-row-2-name"); v != "zwei" {
		t.Fatalf("row 2's field holds %q", v)
	}
	marked()
}

// TestBrowserStreamLimitTrim: a limit whose end is fixed trims it wherever
// a batch's rows went in — a newest-first feed keeps its newest rows at the
// top whether new ones are prepended or older ones inserted at the bottom,
// and a log keeps its last rows whichever end a batch went in at.
func TestBrowserStreamLimitTrim(t *testing.T) {
	a := New(Only("none"))
	a.Live("/feed", func() *feedPage {
		return &feedPage{Log: &keyedLog{}, Max: 3, Trim: "end", Start: []feedItem{{1, "one"}, {2, "two"}, {3, "three"}, {4, "four"}}}
	})
	a.Live("/log", func() *feedPage {
		return &feedPage{Log: &keyedLog{}, Max: 3, Trim: "start", Start: []feedItem{{1, "one"}, {2, "two"}, {3, "three"}, {4, "four"}}}
	})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	b := chrome.Launch(t)
	for _, c := range []struct {
		path  string
		steps [][2]string // a button, and the rows after it
	}{
		{"/feed", [][2]string{{"", "1 2 3"}, {"#top", "101 1 2"}, {"#add", "101 1 2"}, {`#feed > li[data-vk-row="2"] .del`, "101 1"}, {"#add", "101 1 103"}, {"#top", "104 101 1"}}},
		{"/log", [][2]string{{"", "2 3 4"}, {"#add", "3 4 101"}, {"#top", "3 4 101"}, {`#feed > li[data-vk-row="3"] .del`, "4 101"}, {"#top", "103 4 101"}, {"#add", "4 101 104"}}},
	} {
		t.Run(strings.TrimPrefix(c.path, "/"), func(t *testing.T) {
			p := b.Start(srv.URL+c.path, `document.documentElement.dataset.vkState === "connected"`)
			for _, s := range c.steps {
				if s[0] != "" {
					p.Click(s[0])
				}
				p.Wait(`[...document.querySelectorAll("#feed > li")].map((l) => l.dataset.vkRow).join(" ") === ` + fmt.Sprintf("%q", s[1]) + ` && !document.querySelector("[aria-busy]")`)
			}
		})
	}
}

// joinAttrs has the page join without the stream extension, as a runtime
// predating it does: the server sends containers' attributes as they are.
const joinAttrs = `(() => {
	const W = window.WebSocket, got = window.__got = [];
	window.WebSocket = class extends W {
		constructor(...a) { super(...a); this.addEventListener("message", (e) => got.push(e.data)); }
		send(d) { try { const m = JSON.parse(d); if (m.type === "join") { delete m.ext; d = JSON.stringify(m); } } catch (_) {} super.send(d); }
	};
})()`

// TestBrowserStreamCompactWire: a container's attributes sent compact
// ({"st":…}) build the very container the attribute form does, step by
// step, in fewer bytes.
func TestBrowserStreamCompactWire(t *testing.T) {
	a := New(Only("none"))
	a.Live("/feed", func() *feedPage {
		return &feedPage{Log: &keyedLog{}, Max: 6, Start: []feedItem{{1, "<one> & \"1\""}, {2, "two"}, {3, "three"}}}
	})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	b := chrome.Launch(t)
	compact := b.Start(srv.URL+"/feed", `document.documentElement.dataset.vkState === "connected"`, recordWire)
	attrs := b.Start(srv.URL+"/feed", `document.documentElement.dataset.vkState === "connected"`, joinAttrs)
	gen := regexp.MustCompile(`data-vk-batch="\w+\.`)
	feed := func(p *chrome.Page) string {
		var s string
		p.Wait(`!document.querySelector("[aria-busy]")`)
		if err := p.Eval(`document.querySelector("#feed").outerHTML`, &s); err != nil {
			t.Fatal(err)
		}
		return gen.ReplaceAllString(s, `data-vk-batch="g.`)
	}
	for _, sel := range []string{"", "#add", "#top", "#at", `#feed > li[data-vk-row="2"] .edit`, `#feed > li[data-vk-row="1"] .move`, "#rename", `#feed > li[data-vk-row="3"] .del`, "#reload", "#atend", "#clear", "#add"} {
		for _, p := range []*chrome.Page{compact, attrs} {
			if sel != "" {
				p.Click(sel)
			}
		}
		compact.Wait(`!document.querySelector("[aria-busy]")`)
		attrs.Wait(`!document.querySelector("[aria-busy]")`)
		// the batch the last reply carried is the same in both
		var nc, na string
		for nc == "" || nc != na {
			time.Sleep(20 * time.Millisecond)
			compact.Eval(`document.querySelector("#feed").dataset.vkBatch.split(".")[1]`, &nc)
			attrs.Eval(`document.querySelector("#feed").dataset.vkBatch.split(".")[1]`, &na)
		}
		if c, at := feed(compact), feed(attrs); c != at {
			t.Fatalf("after %q the compact form built\n%s\nand the attributes\n%s", sel, c, at)
		}
	}
	var cw, aw string
	compact.Eval(`window.__got.join("\n")`, &cw)
	attrs.Eval(`window.__got.join("\n")`, &aw)
	if !strings.Contains(cw, `{"st":{"n":"items"`) || strings.Contains(cw, "data-vk-ops") {
		t.Fatalf("the compact page's wire: %.600s", cw)
	}
	if strings.Contains(aw, `{"st":`) || !strings.Contains(aw, "data-vk-ops") {
		t.Fatalf("the attribute page's wire: %.600s", aw)
	}
	if len(cw) >= len(aw) {
		t.Fatalf("the compact form took %d bytes, the attributes %d", len(cw), len(aw))
	}
}

// dropOrRepeat lets a test drop the page's next socket message carrying a
// stream batch (window.__vkDrop), or deliver it twice (window.__vkTwice).
const dropOrRepeat = `(() => {
	const W = window.WebSocket;
	window.WebSocket = class extends W {
		set onmessage(fn) {
			super.onmessage = (e) => {
				const batch = typeof e.data === "string" && e.data.includes('{"st":');
				if (batch && window.__vkDrop) { window.__vkDrop = false; return; }
				fn(e);
				if (batch && window.__vkTwice) { window.__vkTwice = false; fn(e); }
			};
		}
	};
})()`

// TestBrowserStreamToolbar: the development toolbar's Live panel shows,
// red under the message that carried it, a batch the browser ignored (a
// batch it had applied already, sent again) and one it found out of step
// (a reply lost), as the runtime reports them.
func TestBrowserStreamToolbar(t *testing.T) {
	withToolbar(t)
	a := New(Only("none"), Dev())
	a.Live("/feed", func() *feedPage {
		return &feedPage{Log: &keyedLog{}, Max: 5, Start: []feedItem{{1, "one"}, {2, "two"}}}
	})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Start(srv.URL+"/feed", `document.documentElement.dataset.vkState === "connected"`, dropOrRepeat)
	tb := `document.querySelector("vuka-toolbar").shadowRoot`
	rows := func(ks string) {
		t.Helper()
		p.Wait(`[...document.querySelectorAll("#feed > li")].map((l) => l.dataset.vkRow).join(" ") === ` + fmt.Sprintf("%q", ks) + ` && !document.querySelector("[aria-busy]")`)
	}

	p.Eval(`window.__vkTwice = true`, nil) // delivered twice: the second ignored
	p.Click("#add")
	rows("1 2 101")
	p.Eval(`window.__vkTwice = true`, nil)
	p.Click("#top")
	rows("102 1 2 101")
	p.Eval(`window.__vkDrop = true`, nil) // lost: the next is out of step, and the page joins again
	p.Click("#add")
	p.Click("#add")
	rows("1 2")

	p.Wait(tb + `.querySelector(".handle")`)
	p.ClickElement(tb + `.querySelector(".handle")`)
	p.Wait(tb + `.querySelector('.tab[data-tab="Live"]')`)
	p.ClickElement(tb + `.querySelector('.tab[data-tab="Live"]')`)
	p.Wait(tb + `.querySelectorAll('.panel[data-panel="Live"] [data-stream-problem]').length === 3`)
	var lines []struct {
		Text, Msg string
		Red       bool
	}
	p.Eval(`[...`+tb+`.querySelectorAll('.panel[data-panel="Live"] [data-stream-problem]')].map((d) => ({
		text: d.textContent, msg: d.closest("details").querySelector("summary code").textContent, red: d.closest("details").classList.contains("err")}))`, &lines)
	if len(lines) != 3 {
		t.Fatalf("lines %+v", lines)
	}
	var problems []string
	for _, l := range lines {
		if !l.Red {
			t.Errorf("%q isn't under a red message", l.Text)
		}
		problems = append(problems, l.Text)
	}
	got := strings.Join(problems, "\n")
	for _, want := range []string{`c1 items \w+\.5: out of step, the browser applied \w+\.3`, `c1 items \w+\.3: ignored, the browser applied \w+\.3`, `c1 items \w+\.2: ignored, the browser applied \w+\.2`} {
		if !regexp.MustCompile(want).MatchString(got) {
			t.Errorf("no line %s", want)
		}
	}
	for _, l := range lines {
		if !strings.Contains(l.Msg, "feedPage.") {
			t.Errorf("%q is under %q, not the event whose reply carried it", l.Text, l.Msg)
		}
	}
	if t.Failed() {
		t.Fatalf("the Live panel's red lines:\n%s\n%+v", got, lines)
	}
}
