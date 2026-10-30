package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

type signinForm struct {
	ui.FormState
	Email    string `validate:"required,email"`
	Password string `input:"current-password" validate:"required"`
	Remember bool   `label:"Keep me signed in"`
}

var signinSaves atomic.Int32

// signinPage is a sign-in page that isn't live: its form is checked and
// submitted over HTTP, and its Save sets a cookie, as signing in does.
type signinPage struct {
	Form   signinForm
	Placed bool
}

func (p *signinPage) Save(ctx context.Context, f signinForm) error {
	signinSaves.Add(1)
	if f.Email != "ann@shop.test" || f.Password != "open sesame" {
		return p.Form.Fail("Wrong email or password")
	}
	http.SetCookie(ResponseWriter(ctx), &http.Cookie{Name: "who", Value: "ann", Path: "/", HttpOnly: true})
	return Redirect("/account")
}

func (p *signinPage) Render() ui.Node {
	kids := []ui.Node{ui.Field{For: ui.For(&p.Form.Email, &p.Form)}}
	if p.Placed {
		kids = append(kids, ui.Summary{})
	}
	kids = append(kids, ui.Field{For: ui.For(&p.Form.Password, &p.Form)}, ui.Field{For: ui.For(&p.Form.Remember, &p.Form)},
		ui.Submit{Children: ui.Text("Sign in")})
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Sign in"))),
		ui.El("body", nil, ui.StyleTag(), ui.El("main", nil, ui.Form{For: ui.For(&p.Form), OnSubmit: p.Save, Children: ui.Fragment(kids...)})))
}

func signinApp() *App {
	a := New(Only("none"))
	a.Get("/signin", func() ui.Node { return (&signinPage{}).Render() })
	a.Get("/signin/placed", func() ui.Node { return (&signinPage{Placed: true}).Render() })
	a.Get("/account", func(r *http.Request) ui.Node {
		who := "nobody"
		if c, err := r.Cookie("who"); err == nil {
			who = c.Value
		}
		return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Account"))), ui.El("body", nil, ui.El("p", []ui.Attr{{Name: "id", Value: "who"}}, ui.Text(who))))
	})
	return a
}

// TestFormHTTPNotLive: a page whose only live part is a struct form doesn't
// connect; its form checks and submits over HTTP.
func TestFormHTTPNotLive(t *testing.T) {
	srv := httptest.NewServer(signinApp().Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/signin")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	page := string(b)
	if strings.Contains(page, "data-vk-token") || strings.Contains(page, "data-vk-on-") || !strings.Contains(page, "data-vk-http") || !strings.Contains(page, "/_vuka/live.js") {
		t.Fatalf("the sign-in page: %s", page)
	}
	// a check runs no Save, and answers the errors as JSON
	signinSaves.Store(0)
	post := func(vals url.Values, mode string) (*http.Response, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/signin", strings.NewReader(vals.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if mode != "" {
			req.Header.Set(FormHeader, mode)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, string(b)
	}
	vals := url.Values{"_vk_form": {"signin-form"}, "_vk_used": {"email"}, "_vk_check": {"1"}, "email": {"ann"}, "password": {"x"}}
	res, body := post(vals, "check")
	if res.StatusCode != 200 || !strings.Contains(body, `"path":"email"`) || strings.Contains(body, `"path":"password"`) || strings.Contains(body, `"summary":{`) || signinSaves.Load() != 0 {
		t.Errorf("a check: %d %s (%d saves)", res.StatusCode, body, signinSaves.Load())
	}
	vals.Del("_vk_check")
	vals.Set("email", "ann@shop.test")
	res, body = post(vals, "submit")
	if res.StatusCode != 422 || !strings.Contains(body, `"Wrong email or password"`) || strings.Count(body, "Wrong email") != 1 {
		t.Errorf("a failed submit: %d %s", res.StatusCode, body)
	}
	vals.Set("password", "open sesame")
	res, body = post(vals, "submit")
	if res.StatusCode != 200 || !strings.Contains(body, `"location":"/account"`) || !hasCookie(res, "who", "ann") {
		t.Errorf("a submit signing in: %d %s %q", res.StatusCode, body, res.Header.Values("Set-Cookie"))
	}
	// without script: a plain post, 303 or the page with its errors
	vals.Set("password", "nope")
	res, body = post(vals, "")
	if res.StatusCode != 422 || !strings.Contains(body, "Wrong email or password") || !strings.Contains(body, "<title>Error: Sign in") {
		t.Errorf("a plain post failing: %d %.400s", res.StatusCode, body)
	}
	vals.Set("password", "open sesame")
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("POST", srv.URL+"/signin", strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if res, err := noFollow.Do(req); err != nil || res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/account" || !hasCookie(res, "who", "ann") {
		t.Errorf("a plain post signing in: %v %v", res, err)
	}
	// a POST naming no form: refused before the page's handler runs
	res, _ = post(url.Values{"x": {"1"}}, "")
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("a POST naming no form: %d", res.StatusCode)
	}
}

func hasCookie(res *http.Response, name, value string) bool {
	for _, c := range res.Cookies() {
		if c.Name == name && c.Value == value {
			return true
		}
	}
	return false
}

// TestBrowserFormHTTP: a sign-in form on a page that isn't live shows its
// errors as the visitor leaves a field and fixes it, never touching what
// was typed or where focus is; a failed submit shows the summary; one that
// succeeds signs in and goes where Save sent the visitor.
func TestBrowserFormHTTP(t *testing.T) {
	srv := httptest.NewServer(signinApp().Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	for _, path := range []string{"/signin", "/signin/placed"} {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			signinSaves.Store(0)
			p := b.Visit(srv.URL + path)
			var got any
			p.Eval(`[document.documentElement.dataset.vkState || "", !!window.vuka]`, &got)
			if g, _ := got.([]any); len(g) != 2 || g[0] == "connected" || g[1] != true {
				t.Fatalf("the page connected, or has no runtime: %v", got)
			}
			p.Eval(`window.__email = document.querySelector('input[name="email"]')`, nil)
			p.Type(`input[name="email"]`, "ann")
			p.Press("Tab") // leaving the field: its check
			p.Wait(`document.querySelector("#signin-form-email-error") !== null`)
			p.Eval(`[document.querySelector("#signin-form-email-error").textContent, document.querySelector('input[name="email"]').getAttribute("aria-invalid"),
				document.querySelector('input[name="email"]').getAttribute("aria-describedby"), document.activeElement.name,
				document.querySelector('input[name="email"]') === window.__email, document.querySelector('input[name="email"]').value,
				document.querySelector("#signin-form-password-error") === null, document.querySelector(".vk-summary") === null]`, &got)
			g, _ := got.([]any)
			if len(g) != 8 || !strings.Contains(g[0].(string), "email address") || g[1] != "true" || g[2] != "signin-form-email-error" || g[3] != "password" || g[4] != true || g[5] != "ann" || g[6] != true || g[7] != true {
				t.Fatalf("after leaving the email field: %v", got)
			}
			// typing in it fixes it: the error goes, focus and the field stay
			p.Eval(`document.querySelector('input[name="email"]').focus()`, nil)
			p.Keys("@shop.test")
			p.Wait(`document.querySelector("#signin-form-email-error") === null`)
			p.Eval(`[document.activeElement.name, document.activeElement === window.__email, document.activeElement.value, document.querySelector('input[name="email"]').hasAttribute("aria-invalid"),
				document.querySelector('input[name="email"]').closest(".vk-field").classList.contains("vk-field--invalid")]`, &got)
			if g, _ := got.([]any); len(g) != 5 || g[0] != "email" || g[1] != true || g[2] != "ann@shop.test" || g[3] != false || g[4] != false {
				t.Errorf("after fixing the email: %v", got)
			}
			if n := signinSaves.Load(); n != 0 {
				t.Errorf("the checks ran Save %d times", n)
			}
			// a failed submit: the summary, the title, the values kept
			p.Type(`input[name="password"]`, "wrong")
			p.Click(".vk-btn--submit")
			p.Wait(`document.querySelector(".vk-summary") !== null`)
			p.Eval(`[document.title, document.activeElement.className, document.querySelector(".vk-summary").textContent,
				[...document.querySelectorAll("form input:not([type=hidden])")].map((i) => i.type === "checkbox" ? i.checked : i.value), location.pathname,
				document.querySelectorAll(".vk-summary").length, document.querySelectorAll('[id^="signin-form-summary-"]').length]`, &got)
			g, _ = got.([]any)
			if len(g) != 7 || g[0] != "Error: Sign in" || g[1] != "vk-summary" || !strings.Contains(g[2].(string), "Wrong email or password") || g[4] != path || g[5] != float64(1) || g[6] != float64(strings.Count(path, "placed")) { // the form's top place, left when the page placed its own
				t.Errorf("after a failed submit: %v", got)
			}
			if v, _ := g[3].([]any); len(v) != 3 || v[0] != "ann@shop.test" || v[1] != "wrong" || v[2] != false {
				t.Errorf("the values after a failed submit: %v", g[3])
			}
			if strings.Count(g[2].(string), "Wrong email") != 1 {
				t.Errorf("the summary: %v", g[2])
			}
			if path == "/signin/placed" {
				p.Eval(`document.querySelector(".vk-summary").previousElementSibling.querySelector('input[name="email"]') !== null`, &got)
				if got != true {
					t.Errorf("the summary isn't where the page placed it")
				}
			}
			// the right password: signed in, at /account
			p.Eval(`const pw = document.querySelector('input[name="password"]'); pw.value = "open sesame"`, nil)
			p.Click(".vk-btn--submit")
			p.Wait(`location.pathname === "/account"`)
			p.WaitText("#who", "ann")
			if n := signinSaves.Load(); n != 2 {
				t.Errorf("%d saves, want 2", n)
			}
			p.Eval(`document.cookie = "who=; Max-Age=0; path=/"`, nil)
		})
	}
}
