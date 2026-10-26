package viewtest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/forms"
	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/viewtest"
)

type signup struct {
	ui.FormState
	Name  string `label:"Full name" validate:"required"`
	Email string `validate:"required,email"`
}

func (f *signup) CleanEmail(ctx context.Context) error {
	if !forms.Live(ctx) && f.Email == "taken@example.com" {
		return forms.Errorf("signup.email.taken")
	}
	return nil
}

type joinPage struct {
	ui.Live
	Form signup
}

func (j *joinPage) Save(f signup) error { return web.Redirect("/welcome") }

func (j *joinPage) Render() ui.Node { return ui.Form{For: ui.For(&j.Form), OnSubmit: j.Save} }

func formApp() *web.App {
	app := web.New(web.Only("none"), web.NoDotEnv())
	app.Live("/join", func() *joinPage { return &joinPage{} })
	return app
}

func TestFormLive(t *testing.T) {
	p := viewtest.Mount[*joinPage](t, formApp(), "/join")
	p.Fill(&p.Page.Form.Email, "pa@")
	p.Expect(&p.Page.Form.Email).NoError() // typing: nothing shows yet
	p.Blur(&p.Page.Form.Email)
	p.Expect(&p.Page.Form.Email).Error("validate.email")
	p.Expect("name").NoError() // empty and required: only on submit
	p.Fill(&p.Page.Form.Email, "taken@example.com").Submit()
	p.Expect(&p.Page.Form.Name).Error("Enter full name")
	p.Expect(&p.Page.Form.Email).Error("signup.email.taken")
	p.Fill("name", "Pa").Fill(&p.Page.Form.Email, "pa@example.com").Submit()
	p.ExpectRedirect("/welcome")
}

func TestFormPlain(t *testing.T) {
	p := viewtest.Mount[*joinPage](t, formApp(), "/join", viewtest.Plain())
	p.Fill(&p.Page.Form.Email, "pa@").Submit()
	if p.Status() != 422 || !strings.Contains(p.HTML(), "There is a problem") {
		t.Fatalf("%d\n%s", p.Status(), p.HTML())
	}
	p.Expect(&p.Page.Form.Email).Error("validate.email")
	p.Fill("name", "Pa").Fill(&p.Page.Form.Email, "pa@example.com").Submit()
	p.ExpectRedirect("/welcome")
}

// TestFillUnrendered: filling a field the form doesn't render fails.
func TestFillUnrendered(t *testing.T) {
	var ft fakeT
	func() {
		defer func() { recover() }()
		p := viewtest.Mount[*joinPage](&ft, formApp(), "/join")
		p.Fill("phone", "1")
	}()
	if !strings.Contains(ft.msg, `renders no field "phone"`) {
		t.Errorf("got %q", ft.msg)
	}
}

type fakeT struct {
	testing.TB
	msg     string
	cleanup []func()
}

func (f *fakeT) Helper()                   {}
func (f *fakeT) Cleanup(fn func())         { f.cleanup = append(f.cleanup, fn) }
func (f *fakeT) Fatalf(s string, a ...any) { f.msg = fmtS(s, a...); panic("fatal") }
func (f *fakeT) Errorf(s string, a ...any) { f.msg = fmtS(s, a...) }

func fmtS(s string, a ...any) string { return fmt.Sprintf(s, a...) }
