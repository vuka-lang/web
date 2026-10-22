package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
)

type vAddress struct {
	City string `json:"city" form:"town" validate:"required,min=2"`
}

type vSignup struct {
	ID      int        `form:"id" validate:"min=1"`
	Email   string     `json:"email" form:"mail" validate:"required,email"`
	Plan    string     `json:"plan" validate:"omitempty,oneof=free pro"`
	Address vAddress   `json:"address"`
	Items   []vAddress `json:"items" validate:"max=2"`
}

func TestBindValidates(t *testing.T) {
	p := pkgOf(t)
	route(Post("/signup/{id}"), decl(p, "signup", func(in vSignup) string { return in.Email }, "in"))
	route(Get("/find/{id}"), decl(p, "find", func(q *vSignup) string { return q.Email }, "q"))
	h := New(Only(p)).Handler()
	jsonCT := []string{"Content-Type", "application/json"}
	formCT := []string{"Content-Type", "application/x-www-form-urlencoded"}

	expect(t, h, "POST", "/signup/3", `{"email":"ada@example.com","address":{"city":"Dar"}}`, 200, `"ada@example.com"`, jsonCT...)
	expect(t, h, "POST", "/signup/3", `{"plan":"gold","address":{"city":"D"},"items":[{"city":"Arusha"},{},{"city":"Moshi"}]}`, 422,
		`{"error":"invalid: address.city: at least 2 characters; email: required; items: at most 2 items; plan: one of free, pro",`+
			`"fields":{"address.city":"at least 2 characters","email":"required","items":"at most 2 items","plan":"one of free, pro"},"status":422}`,
		jsonCT...)
	expect(t, h, "POST", "/signup/3", `{"email":"ada@example.com","address":{"city":"Dar"},"items":[{"city":"Arusha"},{}]}`, 422,
		`"fields":{"items.1.city":"required"}`, jsonCT...)
	expect(t, h, "POST", "/signup/3", `mail=nope`, 422, `"fields":{"address.town":"required","mail":"an email address"}`, formCT...)
	expect(t, h, "POST", "/signup/0", `{"email":"ada@example.com","address":{"city":"Dar"}}`, 422, `"fields":{"ID":"at least 1"}`, jsonCT...)
	expect(t, h, "POST", "/signup/0", `mail=ada@example.com`, 422, `"fields":{"address.town":"required","id":"at least 1"}`, formCT...)
	mp := "--b\r\nContent-Disposition: form-data; name=\"mail\"\r\n\r\nnope\r\n--b\r\nContent-Disposition: form-data; name=\"town\"\r\n\r\nDar\r\n--b--\r\n"
	expect(t, h, "POST", "/signup/3", mp, 422, `"fields":{"address.town":"required","mail":"an email address"}`, "Content-Type", "multipart/form-data; boundary=b")
	expect(t, h, "GET", "/find/2?mail=x", "", 422, `"fields":{"address.town":"required","mail":"an email address"}`)
	expect(t, h, "GET", "/find/0", "", 422, `"id":"at least 1"`)
}

func TestBindInvalidIsWebInvalid(t *testing.T) {
	p := pkgOf(t)
	route(Post("/signup"), decl(p, "signup", func(in vSignup) ui.Node { return ui.Text(in.Email) }, "in"))
	var got Invalid
	var status int
	h := New(Only(p), OnError(func(w http.ResponseWriter, r *http.Request, e *Error, html bool) {
		status = e.Status
		errors.As(e, &got)
		w.WriteHeader(e.Status)
	})).Handler()
	do(t, h, "POST", "/signup", `mail=nope`, "Content-Type", "application/x-www-form-urlencoded")
	if status != 422 || got["mail"] != "an email address" || got["address.town"] != "required" {
		t.Errorf("status %d, fields %v", status, got)
	}

	p = pkgOf(t) + "/html"
	route(Post("/signup"), decl(p, "signup", func(in vSignup) ui.Node { return ui.Text(in.Email) }, "in"))
	code, body, _ := do(t, New(Only(p)).Handler(), "POST", "/signup", `mail=nope`, "Content-Type", "application/x-www-form-urlencoded")
	if code != 422 || !strings.Contains(body, "<h1>Unprocessable Entity</h1>") || !strings.Contains(body, "mail: an email address") {
		t.Errorf("html page: %d %s", code, body)
	}
}

type vBadRule struct {
	Name  string `validate:"requird"`
	Inner struct {
		N int `validate:"min=x"`
	}
}

type vBadNested struct {
	Items []struct {
		N int `validate:"min=x"`
	}
}

func TestBindBadRuleFailsBuild(t *testing.T) {
	p := pkgOf(t)
	route(Post("/bad"), decl(p, "bad", func(in vBadRule) error { return nil }, "in"))
	err := New(Only(p)).Build()
	if err == nil || !strings.Contains(err.Error(), "POST /bad") || !strings.Contains(err.Error(), `unknown rule "requird"`) {
		t.Errorf("bad rule: %v", err)
	}
	p = pkgOf(t) + "/nested"
	route(Post("/bad"), decl(p, "bad", func(in *vBadNested) error { return nil }, "in"))
	if err := New(Only(p)).Build(); err == nil || !strings.Contains(err.Error(), "min takes a number") {
		t.Errorf("nested bad rule: %v", err)
	}
}
