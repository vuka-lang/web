package web

import (
	"fmt"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// manyRows is a live page of keyed rows that a button replaces with n new
// ones: every row of a fill is a key the page doesn't have yet.
type manyRows struct {
	ui.Live
	rows []int
	next int
}

func (p *manyRows) fill(n int) {
	p.rows = p.rows[:0]
	for range n {
		p.next++
		p.rows = append(p.rows, p.next)
	}
}

func (p *manyRows) Fill500() { p.fill(500) }
func (p *manyRows) Fill2k() { p.fill(2000) }

func (p *manyRows) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "id", Value: "fill500"}, {Name: "onClick", Value: ui.On(p.Fill500)}}, ui.Text("500")),
		ui.El("button", []ui.Attr{{Name: "id", Value: "fill2k"}, {Name: "onClick", Value: ui.On(p.Fill2k)}}, ui.Text("2k")),
		ui.El("table", nil, ui.El("tbody", nil, ui.Each(p.rows, func(id int) ui.Node {
			return ui.El("tr", []ui.Attr{{Name: "key", Value: id}},
				ui.El("td", nil, ui.Text(strconv.Itoa(id))),
				ui.El("td", nil, ui.El("a", []ui.Attr{{Name: "class", Value: "lbl"}}, ui.Text("row "+strconv.Itoa(id)))),
				ui.El("td", nil, ui.El("a", []ui.Attr{{Name: "class", Value: "remove"}}, ui.Text("x"))))
		}))))
}

// TestBrowserKeyedInsertScales: a patch putting n new keyed rows in a
// container costs the browser about n, not n² — each new row's key is looked
// up in an index of the container's children, never by a scan of them.
func TestBrowserKeyedInsertScales(t *testing.T) {
	a := New(Only("none"))
	a.Live("/rows", func() *manyRows { return &manyRows{} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/rows")
	// fill clicks a button and times it until its last row is in the page:
	// the server's render, the socket and the browser's apply.
	fill := func(id string, n int) float64 {
		var ms float64
		js := fmt.Sprintf(`new Promise((done) => {
			const first = +(document.querySelector("tbody tr td") || {textContent: "0"}).textContent;
			const t0 = performance.now();
			document.getElementById(%q).click();
			const check = () => {
				const rows = document.querySelectorAll("tbody tr");
				if (rows.length === %d && +rows[0].firstChild.textContent !== first) done(performance.now() - t0);
				else requestAnimationFrame(check);
			};
			requestAnimationFrame(check);
		})`, id, n)
		if err := p.Eval(js, &ms); err != nil {
			t.Fatal(err)
		}
		return ms
	}
	best := func(id string, n int) float64 {
		m := fill(id, n)
		for range 2 {
			m = min(m, fill(id, n))
		}
		return m
	}
	fill("fill500", 500) // warm up
	small, large := best("fill500", 500), best("fill2k", 2000)
	t.Logf("500 new rows: %.0fms, 2,000: %.0fms (%.1f×)", small, large, large/small)
	if large > 6*small {
		t.Fatalf("2,000 new keyed rows took %.0fms, %.1f× the %.0fms of 500: the insert isn't linear", large, large/small, small)
	}
}
