package web

import (
	"net/http/httptest"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// footerPage is the note's form with its Save outside it: a modal
// footer's button naming the form (form="note-form").
type footerPage struct{ notePage }

func (p *footerPage) Render() ui.Node {
	return ui.El("main", nil, ui.StyleTag(),
		ui.Form{For: ui.For(&p.Form), OnSubmit: p.Save, Children: ui.Fragment(
			ui.Field{For: ui.For(&p.Form.Title, &p.Form)})},
		ui.El("button", []ui.Attr{{Name: "id", Value: "outside"}, {Name: "type", Value: "submit"}, {Name: "form", Value: ui.ModelID(&p.Form)}}, ui.Text("Save")))
}

// TestBrowserFormSubmitterBusy: while a submit is on its way, the button
// that sent it — here outside the form, naming it — is busy, and the
// form's controls are aria-disabled but still posted; the reply puts
// them back.
func TestBrowserFormSubmitterBusy(t *testing.T) {
	noteSaves.Store(0)
	a := New(Only("none"))
	a.Live("/footer", func() *footerPage { return &footerPage{} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/footer")
	p.Click("#outside")
	p.Wait(`document.querySelector("#outside").getAttribute("aria-busy") === "true"`)
	var got any
	p.Eval(`[document.querySelector("#outside").hasAttribute("data-vk-busy"), document.querySelector('input[name="title"]').getAttribute("aria-disabled"), document.querySelector('input[name="title"]').disabled]`, &got)
	if g, ok := got.([]any); !ok || g[0] != true || g[1] != "true" || g[2] != false {
		t.Errorf("a submit in flight: [submitter busy, field aria-disabled, field disabled] = %v", got)
	}
	p.Wait(`!document.querySelector("#outside").hasAttribute("aria-busy") && !document.querySelector('input[name="title"]').hasAttribute("aria-disabled")`)
	if noteSaves.Load() != 1 {
		t.Errorf("%d saves, want 1", noteSaves.Load())
	}
}
