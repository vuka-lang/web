package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// plainBoard renders what priceBoard does, per page, for the browser tests to
// compare the shared render with.
type plainBoard struct {
	ui.Live
	seq int
}

func (b *plainBoard) Update(n int) { b.seq = n }
func (b *plainBoard) Render() ui.Node {
	rows := make([]ui.Node, 3)
	for i := range rows {
		rows[i] = ui.Key(i, &boardRow{N: i, Seq: b.seq})
	}
	if b.seq >= 100 {
		rows = append(rows, ui.Key("wide", &wideRow{Seq: b.seq}))
	}
	return ui.El("ul", []ui.Attr{{Name: "id", Value: "plain"}}, rows...)
}

func (b *plainBoard) LiveSchema(l *ui.LiveSchema) { l.Mount(b.mount) }
func (b *plainBoard) mount(_ context.Context) error {
	b.Subscribe("plain")
	return nil
}

type mixedPage struct {
	ui.Live
	n int
}

func (p *mixedPage) Bump() { p.n++ }
func (p *mixedPage) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "id", Value: "bump"}, {Name: "onClick", Value: ui.On(p.Bump)}}, ui.Text("bump")),
		ui.El("p", []ui.Attr{{Name: "id", Value: "n"}}, ui.Text(fmt.Sprint("n=", p.n))),
		ui.Component("board", nil, &priceBoard{}),
		ui.Component("plain", nil, &plainBoard{}))
}

// Several real pages show one shared fragment next to per-page parts: after
// each broadcast every page's fragment is the markup a per-page render of the
// same state makes, and a page's own event touches only that page.
func TestBrowserSharedFragmentMatchesPerPageRender(t *testing.T) {
	b := chrome.Launch(t)
	app := New(Only("none"))
	app.Live("/mixed", &mixedPage{})
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	pages := []*chrome.Page{b.Open(srv.URL + "/mixed"), b.Open(srv.URL + "/mixed"), b.Open(srv.URL + "/mixed")}
	for _, p := range pages {
		p.WaitText("#n", "n=0")
		p.WaitText("#board", "0:0-1:0-2:0-")
	}
	for _, seq := range []int{7, 8, 150, 151, 9} {
		if err := bcast(app, seq); err != nil {
			t.Fatal(err)
		}
		if err := app.Broadcast(context.Background(), "plain", seq); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("0:%d-1:%d-2:%d-", seq, seq, seq)
		if seq >= 100 {
			want += fmt.Sprintf("wide%d", seq)
		}
		for _, p := range pages {
			p.WaitText("#board", want)
			p.WaitText("#plain", want)
			var shared, plain string
			p.Eval(`document.querySelector("#board").innerHTML.replace(/ ?data-vk-[a-z]+="[^"]*"/g, "")`, &shared)
			p.Eval(`document.querySelector("#plain").innerHTML.replace(/ ?data-vk-[a-z]+="[^"]*"/g, "")`, &plain)
			if shared != plain {
				t.Errorf("seq %d: the shared fragment differs from the per-page render:\n%s\n%s", seq, shared, plain)
			}
		}
	}
	pages[0].Click("#bump")
	pages[0].WaitText("#n", "n=1")
	for _, p := range pages[1:] {
		if got := p.Text("#n"); got != "n=0" {
			t.Errorf("another page's counter changed: %q", got)
		}
	}
	// a page that joins late holds the current fragment
	late := b.Open(srv.URL + "/mixed")
	late.WaitText("#board", "0:9-1:9-2:9-")
	bcast(app, 10)
	for _, p := range append(pages, late) {
		p.WaitText("#board", "0:10-1:10-2:10-")
	}
}
