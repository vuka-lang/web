package auth_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/auth"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

type signInForm struct {
	ui.FormState
	Email    string `validate:"required,email"`
	Password string `input:"current-password" validate:"required"`
}

// signInPage is a sign-in page that isn't live: its form submits over
// HTTP, so its Save signs in (vuka#231, vuka#232).
type signInPage struct{ Form signInForm }

func (p *signInPage) Save(ctx context.Context, f signInForm) error {
	if _, err := auth.Login(ctx, f.Email, f.Password); err != nil {
		if errors.Is(err, auth.ErrInvalidLogin) {
			return p.Form.Fail("Wrong email or password")
		}
		return err
	}
	return web.Redirect("/me")
}

func (p *signInPage) Render() ui.Node {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Sign in"))),
		ui.El("body", nil, ui.Form{For: ui.For(&p.Form), OnSubmit: p.Save, SubmitLabel: "Sign in"}))
}

func TestBrowserFormSignsIn(t *testing.T) {
	app := newApp(t, users(), nil)
	app.Get("/signin", func() ui.Node { return (&signInPage{}).Render() }, web.Attrs(auth.Public()))
	app.Get("/me", func(me auth.Identity) ui.Node {
		return ui.El("html", nil, ui.El("body", nil, ui.El("p", []ui.Attr{{Name: "id", Value: "me"}}, ui.Text(me.ID))))
	})
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Visit(srv.URL + "/signin")
	p.Type(`input[name="email"]`, "bob@example.com")
	p.Type(`input[name="password"]`, "wrong")
	p.Click(".vk-btn--submit")
	p.Wait(`document.querySelector(".vk-summary") !== null`)
	if got := p.Text(".vk-summary li"); got != "Wrong email or password" {
		t.Errorf("a wrong password: %q", got)
	}
	p.Eval(`document.querySelector('input[name="password"]').value = "hunter22"`, nil)
	p.Click(".vk-btn--submit")
	p.Wait(`location.pathname === "/me"`)
	p.WaitText("#me", "2")
}
