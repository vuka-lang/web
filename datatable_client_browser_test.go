package web

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// clientTable is the markup ui.DataTable renders with Client: every row,
// the controls the runtime drives.
func clientTable() string {
	var rows strings.Builder
	for i, name := range []string{"Dune", "Emma", "Ulysses", "Persuasion", "Beloved"} {
		fmt.Fprintf(&rows, `<tr data-vk-table-row data-vk-text="%s"><td>%s</td></tr>`, strings.ToLower(name)+map[bool]string{true: " austen"}[i == 1 || i == 3], name)
	}
	return `<div id="records" class="vk-datatable" data-vk-table="client">
<input type="search" id="q" data-vk-table-search>
<div class="vk-table-wrap"><table><tbody>` + rows.String() + `</tbody></table></div>
<div id="nomatch" data-vk-table-empty="match" hidden><button type="button" id="clear" data-vk-table-clear>Clear search</button></div>
<p id="count"><span data-vk-table-from>1</span>-<span data-vk-table-to>2</span> of <span data-vk-table-total>5</span>, page <span data-vk-table-page>1</span> of <span data-vk-table-pages>3</span></p>
<select id="size" data-vk-table-size><option value="2" selected>2</option><option value="5">5</option></select>
<button type="button" id="prev" data-vk-table-go="prev" disabled>prev</button><button type="button" id="next" data-vk-table-go="next">next</button><button type="button" id="last" data-vk-table-go="last">last</button>
</div>`
}

type clientTablePage struct {
	ui.Live
	N int
}

func (p *clientTablePage) Bump() { p.N++ }
func (p *clientTablePage) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{attr("id", "bump"), attr("type", "button"), attr("onClick", ui.On(p.Bump))}, ui.Textf("renders %d", p.N)),
		ui.RawHTML(clientTable()))
}

// TestBrowserClientTable: the runtime pages and searches a client table's
// rows, and keeps its page through a live render.
func TestBrowserClientTable(t *testing.T) {
	a := New(Only("none"))
	a.Live("/books", func() *clientTablePage { return &clientTablePage{} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/books")
	shown := `[...document.querySelectorAll("[data-vk-table-row]")].filter(r => !r.hidden).map(r => r.textContent).join(",")`
	expect := func(rows, count string) {
		t.Helper()
		p.Wait(shown + ` === ` + fmt.Sprintf("%q", rows) + ` && document.querySelector("#count").textContent === ` + fmt.Sprintf("%q", count))
	}
	expect("Dune,Emma", "1-2 of 5, page 1 of 3")
	p.Click("#next")
	expect("Ulysses,Persuasion", "3-4 of 5, page 2 of 3")
	p.Click("#last")
	expect("Beloved", "5-5 of 5, page 3 of 3")
	p.Wait(`document.querySelector("#next").disabled && !document.querySelector("#prev").disabled`)

	// A live render keeps the table's page.
	p.Click("#bump")
	p.WaitText("#bump", "renders 1")
	expect("Beloved", "5-5 of 5, page 3 of 3")

	// Searching starts again on the first page.
	p.Type("#q", "austen")
	expect("Emma,Persuasion", "1-2 of 2, page 1 of 1")
	p.Type("#q", "zz")
	expect("", "0-0 of 0, page 1 of 1")
	p.Wait(`!document.querySelector("#nomatch").hidden && document.querySelector(".vk-table-wrap").hidden`)
	p.Click("#clear")
	expect("Dune,Emma", "1-2 of 5, page 1 of 3")
	p.Wait(`document.querySelector("#nomatch").hidden && !document.querySelector(".vk-table-wrap").hidden`)

	// The page size.
	p.Eval(`(s => { s.value = "5"; s.dispatchEvent(new Event("change", { bubbles: true })) })(document.querySelector("#size"))`, nil)
	expect("Dune,Emma,Ulysses,Persuasion,Beloved", "1-5 of 5, page 1 of 1")
}
