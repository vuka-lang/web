package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2"
)

type signup struct {
	Email string `form:"email"`
	Name  string `form:"name"`
}

// signupPage is the form, with what was typed and the error.
func signupPage(in signup, problem string) ui.Node {
	var msg ui.Node
	if problem != "" {
		msg = ui.El("p", []ui.Attr{{Name: "role", Value: "alert"}}, ui.Text(problem))
	}
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Sign up"))), ui.El("body", nil,
		ui.El("form", []ui.Attr{{Name: "method", Value: "post"}},
			msg,
			ui.El("input", []ui.Attr{{Name: "name", Value: "email"}, {Name: "value", Value: in.Email}}),
			ui.El("input", []ui.Attr{{Name: "name", Value: "name"}, {Name: "value", Value: in.Name}}),
			ui.Safe(`<button data-vk-js-click="[]">Sign up</button>`))))
}

func formsApp() *App {
	a := New(Only("none"))
	a.Get("/signup", func() ui.Node { return signupPage(signup{}, "") })
	a.Post("/signup", func(in signup) ui.Node {
		if !strings.Contains(in.Email, "@") {
			return signupPage(in, "An email address has an @.")
		}
		return Redirect("/welcome")
	})
	a.Post("/counter", func() ui.Node {
		return ui.El("html", nil, ui.El("head", nil), ui.El("body", nil, ui.Component("c#1", nil, &counter{Label: "n="})))
	})
	a.Post("/view", func(in signup) Page { return Page{Title: in.Name} },
		View(vuka.FileOf("forms:p.html", "<html><head></head><body>{{.Title}}</body></html>")))
	return a
}

func TestFormPostFailsWithWholePage(t *testing.T) {
	withCSS(t, ".x{}")
	h := formsApp().Handler()
	form := url.Values{"email": {"ada.example.com"}, "name": {"Ada <L>"}}.Encode()
	post := func(path string, header ...string) (int, string) {
		code, body, _ := do(t, h, "POST", path, form, append([]string{"Content-Type", "application/x-www-form-urlencoded"}, header...)...)
		return code, body
	}
	navigation := []string{"Sec-Fetch-Dest", "document", "Sec-Fetch-Mode", "navigate", "Accept", "text/html,application/xhtml+xml"}

	code, body := post("/signup", navigation...)
	if code != 200 || !strings.Contains(body, "An email address has an @.") ||
		!strings.Contains(body, `value="ada.example.com"`) || !strings.Contains(body, `value="Ada &lt;L&gt;"`) {
		t.Fatalf("the failed post: %d %s", code, body)
	}
	if m := linkRE.FindAllString(body, -1); len(m) != 1 || strings.Index(body, m[0]) > strings.Index(body, "</head>") {
		t.Errorf("the failed post's page doesn't link the stylesheet in its head: %s", body)
	}
	if !strings.Contains(body, `<script src="/_vuka/live.js?v=`) || strings.Contains(body, "data-vk-token") {
		t.Errorf("the page's JS commands have no script (or it has a join token): %s", body)
	}

	// The same page, as GET serves it but for the values.
	_, get, _ := do(t, h, "GET", "/signup", "")
	if strings.Count(get, "<link") != strings.Count(body, "<link") || strings.Count(get, "<script") != strings.Count(body, "<script") {
		t.Errorf("the post's page differs from the GET's in its head or scripts:\n%s\n%s", body, get)
	}

	// Without Sec-Fetch-Dest (an older browser), a request asking for HTML.
	if _, body := post("/signup", "Accept", "text/html"); !linkRE.MatchString(body) {
		t.Errorf("a form post from a browser without Sec-Fetch-Dest: %s", body)
	}
	if _, body := post("/signup", "Sec-Fetch-Dest", "iframe"); !linkRE.MatchString(body) {
		t.Errorf("a form post in a frame: %s", body)
	}

	// Scripts' requests get the fragment the handler rendered.
	for _, hdr := range [][]string{
		nil,
		{"Sec-Fetch-Dest", "empty", "Accept", "text/html"},
		{"HX-Request", "true", "Accept", "text/html"},
	} {
		_, body := post("/signup", hdr...)
		if !strings.Contains(body, "An email address has an @.") || strings.Contains(body, "app.css") || strings.Contains(body, "<script") {
			t.Errorf("a script's post %v got a whole page: %s", hdr, body)
		}
	}

	// A page with stateful components, posted to: rendered, not live.
	_, body = post("/counter", navigation...)
	if !strings.Contains(body, "n=0") || strings.Contains(body, "data-vk-token") || !strings.Contains(body, "/_vuka/live.js") {
		t.Errorf("a posted page with a component: %s", body)
	}

	// An html/template view.
	if _, body := post("/view", navigation...); !strings.Contains(body, "Ada &lt;L&gt;") || !linkRE.MatchString(body) {
		t.Errorf("a posted view: %s", body)
	}
	if _, body := post("/view"); linkRE.MatchString(body) {
		t.Errorf("a script's posted view got the stylesheet: %s", body)
	}

	// Success still redirects.
	code, _, hdr := do(t, h, "POST", "/signup", url.Values{"email": {"ada@example.com"}}.Encode(),
		append([]string{"Content-Type", "application/x-www-form-urlencoded"}, navigation...)...)
	if code != 303 || hdr.Get("Location") != "/welcome" {
		t.Errorf("a good post: %d %q", code, hdr.Get("Location"))
	}
}
