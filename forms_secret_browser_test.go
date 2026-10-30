package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/forms"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

type signupForm struct {
	ui.FormState
	Email    string `validate:"required,email"`
	Password string `input:"new-password" validate:"required"`
	Confirm  string `input:"new-password" label:"Confirm password" validate:"required"`
}

func (f *signupForm) Clean() error {
	if f.Password != f.Confirm {
		return forms.Errorf("Passwords don't match").At("confirm")
	}
	return nil
}

// accountPage is a signup page with a script of its own, so its root carries
// its props (data-vk-p) as a template with a component script does. Placed
// puts a <ui.Summary/> between the fields; else the form puts its own at
// its top.
type accountPage struct {
	ui.Live
	Title  string
	Placed bool `json:"-"`
	Form   signupForm
}

func (p *accountPage) Save(signupForm) error { return nil }

func (p *accountPage) Render() ui.Node {
	fields := []ui.Node{ui.Field{For: ui.For(&p.Form.Email, &p.Form)}}
	if p.Placed {
		fields = append(fields, ui.Summary{})
	}
	fields = append(fields,
		ui.Field{For: ui.For(&p.Form.Password, &p.Form)},
		ui.Field{For: ui.For(&p.Form.Confirm, &p.Form)},
		ui.Submit{Children: ui.Text("Create account")})
	return ui.El("main", []ui.Attr{{Name: "data-vk-p", Value: ui.Props(p)}}, ui.StyleTag(),
		ui.Form{For: ui.For(&p.Form), OnSubmit: p.Save, Children: ui.Fragment(fields...)})
}

func signupApp() *App {
	a := New(Only("none"))
	a.Live("/join", func() *accountPage { return &accountPage{Title: "Join"} })
	a.Live("/join/placed", func() *accountPage { return &accountPage{Title: "Join", Placed: true} })
	return a
}

// recordWire keeps the text of every WebSocket message the page receives.
const recordWire = `(() => {
	const W = window.WebSocket, got = window.__got = [];
	window.WebSocket = class extends W {
		constructor(...a) { super(...a); this.addEventListener("message", (e) => { if (typeof e.data === "string") got.push(e.data); }); }
	};
})()`

// TestBrowserFormSecretsAndSummary: a failed signup keeps every typed value
// in its own field, the summary at the form's top or placed by hand
// (vuka#230), and no password typed reaches the page's props, its HTML or
// a patch (vuka#229).
func TestBrowserFormSecretsAndSummary(t *testing.T) {
	srv := httptest.NewServer(signupApp().Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	for _, path := range []string{"/join", "/join/placed"} {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			p := b.Start(srv.URL+path, `document.documentElement.dataset.vkState === "connected"`, recordWire)
			p.Type(`input[name="email"]`, "ann@shop.test")
			p.Type(`input[name="password"]`, "hunter2-first")
			p.Type(`input[name="confirm"]`, "hunter2-second")
			p.Click(".vk-btn--submit")
			p.Wait(`document.querySelector(".vk-summary") !== null && !document.querySelector("form").hasAttribute("aria-busy")`)
			var got any
			p.Eval(`[...document.querySelectorAll("form input:not([type=hidden])")].map((i) => i.name + "=" + i.value)`, &got)
			g, _ := got.([]any)
			if len(g) != 3 || g[0] != "email=ann@shop.test" || g[1] != "password=hunter2-first" || g[2] != "confirm=hunter2-second" {
				t.Errorf("after a failed submit the fields hold %v", got)
			}
			var html, wire, props any
			p.Eval(`document.documentElement.outerHTML`, &html)
			p.Eval(`window.__got.join("\n")`, &wire)
			p.Eval(`document.querySelector("main").getAttribute("data-vk-p")`, &props)
			if props != `{"Title":"Join"}` {
				t.Errorf("the page's props: %v", props)
			}
			for what, s := range map[string]any{"the page's HTML": html, "a live message": wire} {
				if s, _ := s.(string); strings.Contains(s, "hunter2") {
					t.Errorf("a typed password is in %s", what)
				}
			}
			if s, _ := wire.(string); !strings.Contains(s, "match") {
				t.Errorf("no patch with the summary was recorded: %.300s", s)
			}
		})
	}
}

// TestFormPropsLeaveSecretsOut: a page's props, as its first HTTP render
// writes them, hold no form.
func TestFormPropsLeaveSecretsOut(t *testing.T) {
	srv := httptest.NewServer(signupApp().Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/join")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `data-vk-p="{&#34;Title&#34;:&#34;Join&#34;}"`) && !strings.Contains(string(body), `data-vk-p='{"Title":"Join"}'`) {
		t.Errorf("the page's props: %s", body)
	}
}
