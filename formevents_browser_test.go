package web

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

type eventsForm struct {
	ui.FormState
	Title string `validate:"required"`
}

// eventsPage is a form with every lifecycle event: a submit's success or
// error, a reset, and a field's own change after the form's.
type eventsPage struct {
	ui.Live
	Form    eventsForm
	Changed string
}

func (p *eventsPage) LiveSchema(l *ui.LiveSchema) { l.Mount(p.load) }

func (p *eventsPage) load(context.Context) error {
	p.Form.Load(eventsForm{Title: "First"})
	return nil
}

func (p *eventsPage) Save(f eventsForm) error {
	if f.Title == "fail" {
		return p.Form.Fail("That title is taken")
	}
	return nil
}

func (p *eventsPage) TitleChanged() error {
	p.Changed = p.Form.Title
	return nil
}

func (p *eventsPage) Render() ui.Node {
	return ui.El("main", nil,
		ui.FormElement{For: ui.For(&p.Form), OnSubmit: p.Save,
			OnSuccess: ui.JS.SetAttr(ui.Sel("#status"), "data-state", "saved"),
			OnError:   ui.JS.SetAttr(ui.Sel("#status"), "data-state", "failed"),
			OnReset:   ui.JS.SetAttr(ui.Sel("#status"), "data-state", "reset"),
			Children: ui.Fragment(
				ui.El("input", append(ui.Model(ui.ModelSpec{Tag: "input", Own: "onChange"}, &p.Form.Title, &p.Form), ui.Attr{Name: "onChange", Value: ui.On(p.TitleChanged)})),
				ui.El("button", []ui.Attr{{Name: "id", Value: "save"}}, ui.Text("Save")),
				ui.El("button", []ui.Attr{{Name: "id", Value: "reset"}, {Name: "type", Value: "reset"}}, ui.Text("Reset")))},
		ui.El("p", []ui.Attr{{Name: "id", Value: "status"}}),
		ui.El("p", []ui.Attr{{Name: "id", Value: "changed"}}, ui.Text(p.Changed)))
}

// TestBrowserFormEvents: a form's onSuccess and onError commands run
// after its submit's reply; a reset is the server's, the loaded values
// back, then onReset; a field's own onChange sees the value the form took.
func TestBrowserFormEvents(t *testing.T) {
	a := New(Only("none"))
	a.Live("/events", func() *eventsPage { return &eventsPage{} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/events")
	title := `input[name="title"]`
	p.Eval(`document.querySelector('input[name="title"]').value = ""`, nil)
	p.Type(title, "fail")
	p.Eval(`document.querySelector('input[name="title"]').dispatchEvent(new Event("change", { bubbles: true }))`, nil)
	p.Wait(`document.querySelector("#changed").textContent === "fail"`)
	p.Click("#save")
	p.Wait(`document.querySelector("#status").getAttribute("data-state") === "failed"`)
	p.Eval(`document.querySelector('input[name="title"]').value = ""`, nil)
	p.Type(title, "Second")
	p.Eval(`document.querySelector("#save").click()`, nil) // the summary going may move the button under the mouse
	p.Wait(`document.querySelector("#status").getAttribute("data-state") === "saved"`)
	p.Type(title, " draft")
	p.Click("#reset")
	p.Wait(`document.querySelector("#status").getAttribute("data-state") === "reset"`)
	var v any
	p.Eval(`document.querySelector('input[name="title"]').value`, &v)
	if v != "Second" {
		t.Errorf("after a reset the title is %v, want the saved Second", v)
	}
}
