package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

const xssPayload = "javascript:void(window.pwned=document.domain)"

// xssPage is a live page linking (data-vk-link) to an app's redirector.
type xssPage struct{ ui.Live }

func (p *xssPage) Boom() error {
	return errors.New(`pq: duplicate key value violates unique constraint "users_email_key" (SELECT password_hash FROM users WHERE email='a@b.c')`)
}

func (p *xssPage) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("a", []ui.Attr{{Name: "id", Value: "go"}, {Name: "data-vk-link", Value: true}, {Name: "href", Value: "/out?to=" + xssPayload}}, ui.Text("external link")),
		ui.El("button", []ui.Attr{{Name: "id", Value: "boom"}, {Name: "onClick", Value: ui.On(p.Boom)}}, ui.Text("boom")),
	)
}

func xssApp() *App {
	a := New(Only("none"))
	// a JS command carrying a URL a visitor gave (their "website")
	a.Get("/nav", func() ui.Node {
		return ui.El("html", nil, ui.El("body", nil,
			ui.El("button", []ui.Attr{{Name: "id", Value: "b"}, {Name: "onClick", Value: ui.On(ui.JS.Navigate(xssPayload))}}, ui.Text("open")),
			ui.El("button", []ui.Attr{{Name: "id", Value: "attr"}, {Name: "onClick", Value: ui.On(ui.JS.SetAttr(ui.Sel("#a"), "href", xssPayload))}}, ui.Text("set")),
			ui.El("a", []ui.Attr{{Name: "id", Value: "a"}, {Name: "href", Value: "/"}}, ui.Text("a"))))
	})
	// an app's open redirect ("you are leaving the site")
	a.Get("/out", func(r *http.Request) Redirect { return Redirect(r.URL.Query().Get("to")) })
	a.Live("/live", &xssPage{})
	return a
}

func TestBrowserNavigateJavascriptURL(t *testing.T) {
	srv := httptest.NewServer(xssApp().Handler())
	defer srv.Close()
	p := chrome.Launch(t).Visit(srv.URL + "/nav")
	p.Click("#b")
	p.Click("#attr")
	time.Sleep(300 * time.Millisecond)
	var got any
	p.Eval("[window.pwned === undefined, document.querySelector('#a').getAttribute('href')]", &got)
	if g, ok := got.([]any); !ok || g[0] != true || g[1] != "about:invalid#TemplFailedSanitizationURL" {
		t.Fatalf("JS.Navigate / SetAttr with a javascript: URL: %v", got)
	}
}

func TestBrowserLiveLinkRedirectJavascriptURL(t *testing.T) {
	srv := httptest.NewServer(xssApp().Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/live")
	p.Click("#go")
	time.Sleep(800 * time.Millisecond)
	var got any
	p.Eval("window.pwned === undefined", &got)
	if got != true {
		t.Fatalf("a data-vk-link to an open redirect ran javascript: (window.pwned = %v)", got)
	}
}

func TestBrowserHandlerErrorHidden(t *testing.T) {
	srv := httptest.NewServer(xssApp().Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/live")
	p.Eval(`window.__err = null; document.addEventListener("vk:error", e => window.__err = e.detail.message)`, nil)
	p.Click("#boom")
	p.Wait(`window.__err !== null`)
	var got any
	p.Eval("window.__err", &got)
	if got != "Internal Server Error" {
		t.Fatalf("vk:error in the browser: %v", got)
	}
}
