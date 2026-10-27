package web

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

type noteForm struct {
	ui.FormState
	Title string `validate:"required,max=20"`
	Code  string `input:"numeric" validate:"required,len=4"`
}

// notePage is a live page editing a note: its form leaves with a
// question, a slow Save, a Reset, and a "Saved" note after a submit.
type notePage struct {
	ui.Live
	Form noteForm
}

var noteSaves atomic.Int32

func (p *notePage) LiveSchema(l *ui.LiveSchema) { l.Mount(p.load) }

func (p *notePage) load(context.Context) error {
	p.Form.Load(noteForm{Title: "First", Code: "1234"})
	return nil
}

func (p *notePage) Save(noteForm) error {
	time.Sleep(400 * time.Millisecond)
	noteSaves.Add(1)
	return nil
}

func (p *notePage) Reset() error { p.Form.Reset(); return nil }

func (p *notePage) Render() ui.Node {
	saved := ui.Node(nil)
	if p.Form.Saved() {
		saved = ui.El("p", []ui.Attr{{Name: "id", Value: "saved"}}, ui.Text("Saved"))
	}
	return ui.El("main", nil, ui.StyleTag(),
		ui.Form{For: ui.For(&p.Form), OnSubmit: p.Save, ConfirmLeave: true},
		ui.El("button", []ui.Attr{{Name: "id", Value: "reset"}, {Name: "onClick", Value: ui.On(p.Reset)}}, ui.Text("Reset")),
		ui.El("a", []ui.Attr{{Name: "id", Value: "away"}, {Name: "data-vk-link", Value: true}, {Name: "href", Value: "/away"}}, ui.Text("away")),
		saved)
}

func noteApp() *App {
	a := New(Only("none"))
	a.Live("/note", func() *notePage { return &notePage{} })
	a.Get("/away", func() ui.Node { return ui.El("p", nil, ui.Text("away")) })
	a.Get("/plain", func() ui.Node {
		f := &noteForm{}
		return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Note"))),
			ui.El("body", nil, ui.Form{For: ui.For(f), Action: "/note"})) // the note page's form, posted to it
	})
	return a
}

// TestBrowserFormBusy: a second click on the submit button while the
// first submit is on its way reaches the button — no CSS keeps it from
// it — and the runtime drops the submit it would send.
func TestBrowserFormBusy(t *testing.T) {
	noteSaves.Store(0)
	srv := httptest.NewServer(noteApp().Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/note")
	p.Eval(`window.__clicks = 0; document.querySelector(".vk-btn--submit").addEventListener("click", () => window.__clicks++)`, nil)
	p.Click(".vk-btn--submit")
	p.Wait(`document.querySelector("form").getAttribute("aria-busy") === "true"`)
	var pe any
	p.Eval(`getComputedStyle(document.querySelector(".vk-btn--submit")).pointerEvents`, &pe)
	if pe == "none" {
		t.Errorf("a busy form's submit button takes no clicks (pointer-events: none)")
	}
	p.Click(".vk-btn--submit")
	p.Wait(`!document.querySelector("form").hasAttribute("aria-busy")`)
	time.Sleep(600 * time.Millisecond)
	var clicks any
	p.Eval(`window.__clicks`, &clicks)
	if clicks != float64(2) || noteSaves.Load() != 1 {
		t.Errorf("%v clicks reached the button, %d saves: the second click is the button's, its submit dropped", clicks, noteSaves.Load())
	}
	p.Wait(`document.querySelector("#saved") !== null`)
	p.Wait(`document.querySelector("#saved") === null`) // Saved is over after 2s
}

// TestBrowserFormLeaveAndReset: a form with changes asks before a link
// leaves it; Reset puts its loaded values back, into the focused field too.
func TestBrowserFormLeaveAndReset(t *testing.T) {
	srv := httptest.NewServer(noteApp().Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/note")
	p.Eval(`window.__asked = 0; window.confirm = () => (window.__asked++, false)`, nil)
	p.Type(`input[name="title"]`, " draft")
	p.Click("#away")
	time.Sleep(300 * time.Millisecond)
	var got any
	p.Eval(`[window.__asked, location.pathname]`, &got)
	if g, ok := got.([]any); !ok || g[0] != float64(1) || g[1] != "/note" {
		t.Fatalf("a form with changes, the visitor staying: %v", got)
	}
	p.Wait(`document.querySelector("form").hasAttribute("data-vk-dirty")`)
	p.Eval(`const t = document.querySelector('input[name="title"]'); t.focus(); document.querySelector("#reset").click()`, nil)
	p.Wait(`document.querySelector('input[name="title"]').value === "First"`)
	p.Eval(`[document.activeElement.name, document.querySelector("form").hasAttribute("data-vk-dirty")]`, &got)
	if g, ok := got.([]any); !ok || g[0] != "title" || g[1] != false {
		t.Errorf("after Reset: %v", got)
	}
	// no changes left: the link goes without asking
	p.Click("#away")
	p.Wait(`location.pathname === "/away"`)
	p.Eval(`window.__asked`, &got)
	if got != float64(1) {
		t.Errorf("a form without changes asked: %v", got)
	}
}

// TestBrowserFormChecks: with Config.BrowserChecks, a submit sent without
// a connection breaking a rule shows the server's messages and summary at
// once, and goes once fixed.
func TestBrowserFormChecks(t *testing.T) {
	ui.Configure(ui.Config{BrowserChecks: true})
	defer ui.Configure(ui.Config{})
	srv := httptest.NewServer(noteApp().Handler())
	defer srv.Close()
	p := chrome.Launch(t).Visit(srv.URL + "/plain")
	p.Type(`input[name="code"]`, "12")
	p.Click(".vk-btn--submit")
	p.Wait(`document.querySelector(".vk-summary") !== null`)
	var got any
	p.Eval(`[location.pathname, document.title.startsWith("Error: "), document.activeElement.className,
		Array.from(document.querySelectorAll(".vk-summary a")).map(a => a.textContent + " " + a.getAttribute("href")),
		document.querySelector("#note-form-code").getAttribute("aria-invalid"),
		document.querySelector("#note-form-code").getAttribute("aria-describedby"),
		document.querySelector("#note-form-title-error").textContent]`, &got)
	g, _ := got.([]any)
	if len(g) != 7 || g[0] != "/plain" || g[1] != true || g[2] != "vk-summary" || g[4] != "true" || g[5] != "note-form-code-error" || g[6] != "Error: Enter title" {
		t.Fatalf("the checks' errors: %v", got)
	}
	if links, _ := g[3].([]any); len(links) != 2 || links[0] != "Enter title #note-form-title" || links[1] != "Code must be 4 characters #note-form-code" {
		t.Errorf("the summary: %v", g[3])
	}
	// letters in a digits field: the server's to judge, the submit goes
	p.Eval(`document.querySelector('input[name="code"]').value = "12ab"; document.querySelector('input[name="title"]').value = "A note"; document.querySelector('input[name="title"]').dispatchEvent(new Event("input", {bubbles: true}))`, nil)
	p.Wait(`document.querySelector("#note-form-title-error") === null`)
	p.Click(".vk-btn--submit")
	p.Wait(`document.querySelector(".vk-summary") !== null && !document.querySelector(".vk-summary").hasAttribute("data-vk-client")`)
}
