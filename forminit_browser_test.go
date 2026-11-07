package web

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

type lamp struct {
	Name  string
	Price int
}

// lampForm is a form type whose Init gives a new lamp its price.
type lampForm struct {
	ui.FormFor[lamp]
}

var lampInits atomic.Int32

func (f *lampForm) Init(ctx context.Context) error {
	lampInits.Add(1)
	if f.Value.Price == 0 {
		f.Value.Price = 25
	}
	return nil
}

// lampPage holds its form in a modal the browser opens.
type lampPage struct {
	ui.Live
	Form lampForm
}

func (p *lampPage) Add(lamp) error { return nil }

func (p *lampPage) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "id", Value: "open"}, {Name: "onClick", Value: ui.OpenModal("add")}}, ui.Text("Add")),
		&ui.Modal{Ref: "add", Children: ui.Form{For: ui.For(&p.Form), OnSubmit: p.Add, Bound: []string{"Name", "Price"}, Children: ui.Fragment(
			ui.Field{For: ui.For(&p.Form.Value.Name, &p.Form)},
			ui.Field{For: ui.For(&p.Form.Value.Price, &p.Form)},
		)}})
}

// A real browser: a form type's form in a closed modal runs Init only once
// the modal opens, its fields then showing what Init set.
func TestBrowserFormInitWhenShown(t *testing.T) {
	app := New(Only("none"))
	app.Live("/lamps", func() *lampPage { return &lampPage{} })
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)

	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/lamps")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	var marked bool
	p.Eval(`document.querySelector("form").hasAttribute("data-vk-init") && document.querySelector("form").getAttribute("aria-busy") === "true"`, &marked)
	if !marked || lampInits.Load() != 0 {
		t.Fatalf("a closed modal's form: marked %v, %d Init runs", marked, lampInits.Load())
	}
	p.Click("#open")
	p.Wait(`document.querySelector("#lamp-price") && document.querySelector("#lamp-price").value === "25" && !document.querySelector("form").hasAttribute("data-vk-init")`)
	if n := lampInits.Load(); n != 1 {
		t.Errorf("Init ran %d times", n)
	}
}
