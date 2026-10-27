package web

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/forms"
	"github.com/vuka-lang/ui/v2/live"
)

type joinForm struct {
	ui.FormState
	Name  string `label:"Full name" validate:"required"`
	Email string `validate:"required,email"`
	Agree bool   `validate:"required"`
}

type joinPage struct {
	ui.Live
	Form joinForm
}

var joinSaves atomic.Int32

func (j *joinPage) Save(f joinForm) error {
	joinSaves.Add(1)
	return Redirect("/welcome?name=" + url.QueryEscape(f.Name))
}

func (j *joinPage) Render() ui.Node {
	return ui.El("main", nil, ui.Form{For: ui.For(&j.Form), OnSubmit: j.Save})
}

func joinApp() *App {
	a := New(Only("none"))
	a.Live("/join", func() *joinPage { return &joinPage{} })
	return a
}

var hiddenField = regexp.MustCompile(`<input type="hidden" name="(_vk_\w+)" value="([^"]*)">`)

// formOf is the hidden fields of the page's form, and the cookie its GET set.
func formOf(t *testing.T, h http.Handler, path string) (url.Values, string) {
	t.Helper()
	code, body, hdr := do(t, h, "GET", path, "")
	if code != 200 {
		t.Fatalf("GET %s: %d", path, code)
	}
	v := url.Values{}
	for _, m := range hiddenField.FindAllStringSubmatch(body, -1) {
		v.Set(m[1], html.UnescapeString(m[2]))
	}
	cookie := ""
	for _, c := range (&http.Response{Header: hdr}).Cookies() {
		cookie = c.Name + "=" + c.Value
	}
	return v, cookie
}

func post(t *testing.T, h http.Handler, path string, v url.Values, header ...string) (int, string, http.Header) {
	t.Helper()
	return do(t, h, "POST", path, v.Encode(), append([]string{"Content-Type", "application/x-www-form-urlencoded", "Accept", "text/html", "Sec-Fetch-Dest", "document", "Sec-Fetch-Site", "same-origin"}, header...)...)
}

// TestFormPlainPost: without a connection a live page's form posts to the
// page: 422 with the errors, 303 once its handler ran; a repeated post of a
// render runs it once.
func TestFormPlainPost(t *testing.T) {
	joinSaves.Store(0)
	h := joinApp().Handler()
	v, cookie := formOf(t, h, "/join")
	if v.Get("_vk_form") != "join-form" || v.Get("_vk_idem") == "" || v.Get("_vk_fields") == "" || v.Get("_vk_csrf") == "" {
		t.Fatalf("the form's hidden fields: %v", v)
	}
	v.Set("email", "pa@")
	code, body, _ := post(t, h, "/join", v, "Cookie", cookie)
	if code != 422 || !strings.Contains(body, "<title>Error: joinPage</title>") || !strings.Contains(body, "There is a problem") ||
		!strings.Contains(body, `<a href="#join-form-name">Enter full name</a>`) || !strings.Contains(body, `value="pa@"`) {
		t.Fatalf("a failed post: %d\n%s", code, body)
	}
	v.Set("name", "Pa")
	v.Set("email", "pa@example.com")
	v.Set("agree", "true")
	code, _, hdr := post(t, h, "/join", v, "Cookie", cookie)
	if code != 303 || hdr.Get("Location") != "/welcome?name=Pa" {
		t.Fatalf("a successful post: %d %v", code, hdr)
	}
	code, _, hdr = post(t, h, "/join", v, "Cookie", cookie)
	if code != 303 || hdr.Get("Location") != "/welcome?name=Pa" || joinSaves.Load() != 1 {
		t.Errorf("a repeated post: %d %v, %d saves", code, hdr, joinSaves.Load())
	}
	// a post naming no form is what it was: not allowed
	if code, _, _ := post(t, h, "/join", url.Values{"x": {"1"}}); code != 405 {
		t.Errorf("a post without a form: %d", code)
	}
	if code, _, _ := post(t, h, "/join", url.Values{"_vk_form": {"other"}}); code != 400 {
		t.Errorf("a post of a form the page doesn't have: %d", code)
	}
}

// TestFormCSRF: a post saying nothing of where it comes from, carrying the
// app's cookie, needs the form's token.
func TestFormCSRF(t *testing.T) {
	h := joinApp().Handler()
	v, cookie := formOf(t, h, "/join")
	v.Set("email", "pa@")
	tok := v.Get("_vk_csrf")
	v.Del("_vk_csrf")
	plain := []string{"Content-Type", "application/x-www-form-urlencoded", "Cookie", cookie}
	if code, _, _ := do(t, h, "POST", "/join", v.Encode(), plain...); code != 403 {
		t.Errorf("no token: %d", code)
	}
	v.Set("_vk_csrf", tok)
	if code, _, _ := do(t, h, "POST", "/join", v.Encode(), plain...); code != 422 {
		t.Errorf("the form's token: %d", code)
	}
	if code, _, _ := do(t, h, "POST", "/join", v.Encode(), "Content-Type", "application/x-www-form-urlencoded", "Origin", "https://evil.example", "Cookie", cookie); code != 403 {
		t.Errorf("a cross-origin post: %d", code)
	}
	b := New(Only("none"), CSRFBypass("POST /hook"))
	b.Post("/hook", func() string { return "ok" })
	if code, _, _ := do(t, b.Handler(), "POST", "/hook", "", "Origin", "https://pay.example", "Sec-Fetch-Site", "cross-site"); code != 200 {
		t.Errorf("a bypassed route: %d", code)
	}
}

// TestFormLive: over the connection the same form's submit is an event: a
// failure renders the errors and summary, a success moves the page.
func TestFormLive(t *testing.T) {
	joinSaves.Store(0)
	srv := httptest.NewServer(joinApp().Handler())
	defer srv.Close()
	c, page := joined(t, srv, "/join")
	values := func(page string, fields map[string]string) map[string][]string {
		v := map[string][]string{}
		for _, m := range hiddenField.FindAllStringSubmatch(page, -1) {
			v[m[1]] = []string{html.UnescapeString(m[2])}
		}
		for k, x := range fields {
			v[k] = []string{x}
		}
		return v
	}
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, page, "focusout", 0), "event": "focusout",
		"form": values(page, map[string]string{"email": "pa@", "_vk_used": "email"})})
	m := must(t, c)
	if m.Error != "" || len(m.Updates) == 0 || !strings.Contains(m.Updates[0].HTML, "Enter an email address in the correct format") || strings.Contains(m.Updates[0].HTML, "Enter full name") {
		t.Fatalf("leaving a field: %+v", m)
	}
	page = m.Updates[0].HTML
	send(t, c, map[string]any{"type": "event", "ref": 3, "target": handlerOf(t, page, "submit", 0), "event": "submit",
		"form": values(page, map[string]string{"email": "pa@"})})
	if m = must(t, c); m.Error != "" || !strings.Contains(m.Updates[0].HTML, "There is a problem") {
		t.Fatalf("a failed submit: %+v", m)
	}
	page = m.Updates[0].HTML
	send(t, c, map[string]any{"type": "event", "ref": 4, "target": handlerOf(t, page, "submit", 0), "event": "submit",
		"form": values(page, map[string]string{"name": "Pa", "email": "pa@example.com", "agree": "true"})})
	m = must(t, c)
	js, _ := json.Marshal(m.JS)
	if m.Error != "" || !strings.Contains(string(js), `"navigate"`) || !strings.Contains(string(js), `/welcome?name=Pa`) || joinSaves.Load() != 1 {
		t.Fatalf("a successful submit moves the page: %+v %s", m, js)
	}
}

// TestRedirectFromEvent: any live handler returning a Redirect moves the
// page there.
func TestRedirectFromEvent(t *testing.T) {
	s := live.NewSession(context.Background(), func(context.Context) ui.Node {
		return ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(func() error { return Redirect("/next") })}})
	})
	if _, err := s.Render(); err != nil {
		t.Fatal(err)
	}
	p, err := s.Event(ui.Event{Target: "c0:0", Type: "click"})
	js, _ := json.Marshal(p.JS)
	if err != nil || p.Error != "" || !strings.Contains(string(js), `/next`) {
		t.Errorf("%v %+v %s", err, p, js)
	}
}

type placeForm struct {
	ui.FormState
	Country string `validate:"required"`
	City    string `validate:"required" depends:"Country"`
}

func (f *placeForm) CountryChoices() []ui.Option {
	return []ui.Option{{Value: "tz", Label: "Tanzania"}, {Value: "ke", Label: "Kenya"}}
}

func (f *placeForm) CityChoices() []ui.Option {
	if f.Country == "ke" {
		return []ui.Option{{Value: "nbo", Label: "Nairobi"}}
	}
	return []ui.Option{{Value: "dar", Label: "Dar es Salaam"}}
}

type placePage struct {
	ui.Live
	Form placeForm
}

func (p *placePage) Save(placeForm) error { return Redirect("/done") }

func (p *placePage) Render() ui.Node { return ui.Form{For: ui.For(&p.Form), OnSubmit: p.Save} }

// TestFormIntentPost: a field's Update button, the no-script way to have
// its dependants' choices follow it, answers 200 with the form, saving
// nothing.
func TestFormIntentPost(t *testing.T) {
	a := New(Only("none"))
	a.Live("/place", func() *placePage { return &placePage{} })
	h := a.Handler()
	v, cookie := formOf(t, h, "/place")
	v.Set("country", "ke")
	v.Set("city", "dar")
	v.Set(ui.FormIntentField, "update")
	code, body, _ := post(t, h, "/place", v, "Cookie", cookie)
	if code != 200 || strings.Contains(body, "Error: ") || strings.Contains(body, "There is a problem") || !strings.Contains(body, `value="nbo">Nairobi`) {
		t.Fatalf("an Update: %d\n%s", code, body)
	}
	v.Del(ui.FormIntentField)
	if code, body, _ := post(t, h, "/place", v, "Cookie", cookie); code != 422 || !strings.Contains(body, "Select city from the list") {
		t.Fatalf("a City not among the choices: %d\n%s", code, body)
	}
	v.Set("city", "nbo")
	if code, _, hdr := post(t, h, "/place", v, "Cookie", cookie); code != 303 || hdr.Get("Location") != "/done" {
		t.Fatalf("a submit: %d %v", code, hdr)
	}
}

// TestFormLocale: a page's forms speak the language of the request's
// Accept-Language, from the app's translations.
func TestFormLocale(t *testing.T) {
	ui.Configure(ui.Config{Messages: forms.Translations{"sw": forms.Messages{"Full name": "Jina kamili", "validate.required": "Weka {label}", "form.problem": "Kuna tatizo"}}})
	defer ui.Configure(ui.Config{})
	h := joinApp().Handler()
	_, body, _ := do(t, h, "GET", "/join", "", "Accept-Language", "sw-TZ, en;q=0.5")
	if !strings.Contains(body, ">Jina kamili<") {
		t.Errorf("the label isn't translated:\n%s", body)
	}
	v, cookie := formOf(t, h, "/join")
	code, body, _ := post(t, h, "/join", v, "Cookie", cookie, "Accept-Language", "sw")
	if code != 422 || !strings.Contains(body, `<a href="#join-form-name">Weka jina kamili</a>`) || !strings.Contains(body, "Kuna tatizo") {
		t.Errorf("a failed post's messages: %d\n%s", code, body)
	}
	if _, body, _ := do(t, h, "GET", "/join", "", "Accept-Language", "fr"); !strings.Contains(body, ">Full name<") {
		t.Errorf("a language without translations isn't English:\n%s", body)
	}
}
