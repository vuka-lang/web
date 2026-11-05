package web

import (
	"fmt"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// scrollGate is what a scrollPage's viewport handlers were called with, in
// order; a held handler waits for release before it queues its rows, so a
// test can look at the page while the reply is pending.
type scrollGate struct {
	calls   chan string
	release chan struct{}
	hold    map[string]bool // "older", "newer"
}

func newScrollGate(hold ...string) *scrollGate {
	g := &scrollGate{calls: make(chan string, 64), release: make(chan struct{}), hold: map[string]bool{}}
	for _, h := range hold {
		g.hold[h] = true
	}
	return g
}

func (g *scrollGate) called(what string, key int) {
	g.calls <- fmt.Sprint(what, " ", key)
	if g.hold[what] {
		select {
		case <-g.release:
		case <-time.After(20 * time.Second):
		}
	}
}

// next is the next call, failing the test without one.
func (g *scrollGate) next(t *testing.T) string {
	t.Helper()
	select {
	case c := <-g.calls:
		return c
	case <-time.After(chrome.Timeout):
		t.Fatal("no viewport event reached the server")
		return ""
	}
}

// none fails the test when a viewport event reaches the server within d.
func (g *scrollGate) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case c := <-g.calls:
		t.Fatalf("a viewport event %q reached the server", c)
	case <-time.After(d):
	}
}

// scrollPage is a stream over the records 1 to N, 40px rows, loading Page
// rows older than its first at the top and newer than its last at the
// bottom, the browser keeping Max. Box makes the container its own
// scroller, 400px high; else the page scrolls.
type scrollPage struct {
	ui.Live
	G                    *scrollGate
	N, From, Count, Page int
	Max                  int
	Box                  bool
	rows                 ui.Stream[feedItem]
}

func (s *scrollPage) LiveSchema(l *ui.LiveSchema) { l.Mount(s.mount) }

func (s *scrollPage) mount() error {
	s.rows.Limit(s.Max)
	s.rows.Reset(scrollItems(s.From, s.From+s.Count-1)...)
	return nil
}

func scrollItems(from, to int) []feedItem {
	var out []feedItem
	for id := from; id <= to; id++ {
		out = append(out, feedItem{id, fmt.Sprint("r", id)})
	}
	return out
}

func (s *scrollPage) Older(first int) {
	s.G.called("older", first)
	s.rows.Prepend(scrollItems(max(first-s.Page, 1), first-1)...)
}

func (s *scrollPage) Newer(last int) {
	s.G.called("newer", last)
	s.rows.Insert(scrollItems(last+1, min(last+s.Page, s.N))...)
}

func (s *scrollPage) Render() ui.Node {
	style := "margin:0;padding:0;list-style:none"
	if s.Box {
		style += ";height:400px;overflow:auto"
	}
	var lis []ui.Node
	for _, it := range s.rows.Rows() {
		lis = append(lis, ui.HTML(`<li key={id} style="height:40px;margin:0">{text}</li>`, it.ID, it.Text))
	}
	rows := s.rows.Block(lis)
	return ui.HTML(`<main>
<ul id="w" style={style} data-vk-viewport-margin="200px" onViewportTop={older} onViewportBottom={newer} {attrs...}>{rows}</ul>
</main>`, style, s.Older, s.Newer, rows.Attrs(), rows)
}

type scrollView struct {
	t *testing.T
	p *chrome.Page
}

func openScroll(t *testing.T, page *scrollPage) scrollView {
	t.Helper()
	a := New(Only("none"))
	a.Live("/w", func() *scrollPage { cp := *page; return &cp })
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Start(srv.URL+"/w", `document.documentElement.dataset.vkState === "connected"`)
	p.Front()
	p.Viewport(800, 600, false) // the edges seen at once may change: the events wait for the change
	return scrollView{t, p}
}

// rows is the keys the container holds, as one string.
func (v scrollView) rows() string {
	v.t.Helper()
	var ks []string
	if err := v.p.Eval(`[...document.querySelectorAll("#w > li")].map((l) => l.dataset.vkRow)`, &ks); err != nil {
		v.t.Fatal(err)
	}
	return strings.Join(ks, " ")
}

// span waits for the container to hold the rows from to to, in order, with
// no reply pending.
func (v scrollView) span(from, to int) {
	v.t.Helper()
	var ks []string
	for id := from; id <= to; id++ {
		ks = append(ks, fmt.Sprint(id))
	}
	want := strings.Join(ks, " ")
	v.p.Wait(`[...document.querySelectorAll("#w > li")].map((l) => l.dataset.vkRow).join(" ") === ` + fmt.Sprintf("%q", want) + ` && !document.querySelector("#w[aria-busy]")`)
}

// top is where row key is on screen.
func (v scrollView) top(key int) float64 {
	v.t.Helper()
	var y float64
	if err := v.p.Eval(fmt.Sprintf(`document.querySelector('#w > li[data-vk-row="%d"]').getBoundingClientRect().top`, key), &y); err != nil {
		v.t.Fatal(err)
	}
	return y
}

// stays fails when row key isn't where it was, at y, now and two frames on:
// the browser's own scroll anchoring and the runtime's mustn't both move it.
func (v scrollView) stays(key int, y float64) {
	v.t.Helper()
	if got := v.top(key); math.Abs(got-y) > 1 {
		v.t.Fatalf("row %d moved from %v to %v", key, y, got)
	}
	var got float64
	if err := v.p.Eval(fmt.Sprintf(`new Promise((ok) => requestAnimationFrame(() => requestAnimationFrame(() => ok(document.querySelector('#w > li[data-vk-row="%d"]').getBoundingClientRect().top))))`, key), &got); err != nil {
		v.t.Fatal(err)
	}
	if math.Abs(got-y) > 1 {
		v.t.Fatalf("row %d moved from %v to %v a frame after the batch", key, y, got)
	}
}

func (v scrollView) eval(js string) {
	v.t.Helper()
	if err := v.p.Eval(js, nil); err != nil {
		v.t.Fatal(err)
	}
}

// both waits for the calls a and b, in either order.
func both(t *testing.T, g *scrollGate, a, b string) {
	t.Helper()
	x, y := g.next(t), g.next(t)
	if !(x == a && y == b || x == b && y == a) {
		t.Fatalf("the edges fired %q and %q, want %q and %q", x, y, a, b)
	}
}

func wantCall(t *testing.T, g *scrollGate, want string) {
	t.Helper()
	if got := g.next(t); got != want {
		t.Fatalf("the server's viewport handler was called with %q, want %q", got, want)
	}
}

// TestBrowserStreamScrollBottom: scrolling the page to the bottom loads the
// next rows, each page once, until a reply adds none: the end, after which
// scrolling fires nothing.
func TestBrowserStreamScrollBottom(t *testing.T) {
	g := newScrollGate()
	v := openScroll(t, &scrollPage{G: g, N: 100, From: 1, Count: 20, Page: 20})
	both(t, g, "older 1", "newer 20") // the page opens with both edges in view; nothing is older than 1
	v.span(1, 40)
	for _, last := range []int{40, 60, 80, 100} {
		v.eval(`scrollTo(0, document.scrollingElement.scrollHeight)`)
		wantCall(t, g, fmt.Sprint("newer ", last))
		v.span(1, min(last+20, 100))
	}
	v.eval(`scrollTo(0, 0)`)
	v.eval(`scrollTo(0, document.scrollingElement.scrollHeight)`)
	g.none(t, 400*time.Millisecond)
	v.span(1, 100)
}

// TestBrowserStreamScrollTop: rows loaded at the top of the page go above
// what the reader sees, which stays put; the top fires once while its reply
// is pending, however the reader scrolls, and again only for another first
// row.
func TestBrowserStreamScrollTop(t *testing.T) {
	g := newScrollGate("older")
	v := openScroll(t, &scrollPage{G: g, N: 200, From: 181, Count: 20, Page: 20})
	wantCall(t, g, "older 181") // the page opens with both edges in view, the bottom's event behind the top's on the server
	var busy bool
	v.p.Eval(`document.querySelector("#w").getAttribute("aria-busy") === "true"`, &busy)
	if !busy {
		t.Fatal("the container isn't aria-busy while its top's reply is pending")
	}
	v.eval(`scrollBy(0, 30)`)
	v.eval(`new Promise((ok) => setTimeout(ok, 150))`)
	v.eval(`scrollBy(0, -30)`)
	g.none(t, 400*time.Millisecond)

	y := v.top(181)
	g.release <- struct{}{}
	v.span(161, 200)
	v.stays(181, y)
	wantCall(t, g, "newer 200")     // nothing newer: the end
	g.none(t, 400*time.Millisecond) // row 161 is far above the view

	v.eval(`scrollTo(0, 0)`)
	wantCall(t, g, "older 161")
	y = v.top(161)
	g.release <- struct{}{}
	v.span(141, 200)
	v.stays(161, y)
}

// TestBrowserStreamTwoWay: a container that scrolls itself, keeping 30 rows,
// loads at both ends: rows trimmed from the end away from the load, the row
// the reader sees staying put each time, and nothing fired while a reply is
// pending.
func TestBrowserStreamTwoWay(t *testing.T) {
	g := newScrollGate("older", "newer")
	v := openScroll(t, &scrollPage{G: g, N: 300, From: 101, Count: 20, Page: 10, Max: 30, Box: true})
	wantCall(t, g, "older 101")
	y := v.top(101)
	g.release <- struct{}{}
	v.span(91, 120)
	v.stays(101, y)
	g.none(t, 300*time.Millisecond)

	for _, last := range []int{120, 130} {
		v.eval(`(() => { const w = document.querySelector("#w"); w.scrollTop = w.scrollHeight; })()`)
		wantCall(t, g, fmt.Sprint("newer ", last))
		v.eval(`document.querySelector("#w").scrollTop -= 20`) // still in view: no second event
		g.none(t, 300*time.Millisecond)
		y := v.top(last - 5)
		g.release <- struct{}{}
		v.span(last-30+11, last+10) // the top trimmed
		v.stays(last-5, y)
	}

	for _, first := range []int{111, 101} {
		v.eval(`document.querySelector("#w").scrollTop = 0`)
		wantCall(t, g, fmt.Sprint("older ", first))
		y := v.top(first)
		g.release <- struct{}{}
		v.span(first-10, first+19) // the bottom trimmed
		v.stays(first, y)
	}
	if n := len(strings.Fields(v.rows())); n != 30 {
		t.Fatalf("the container holds %d rows, past its limit of 30", n)
	}
}

// TestBrowserStreamInsertAt: InsertAt's rows go before the browser's row i,
// in their order; past the rows at the end; a limit trims the end.
func TestBrowserStreamInsertAt(t *testing.T) {
	a := New(Only("none"))
	a.Live("/feed", func() *feedPage {
		return &feedPage{Log: &keyedLog{}, Max: 5, Start: []feedItem{{1, "one"}, {2, "two"}}}
	})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Start(srv.URL+"/feed", `document.documentElement.dataset.vkState === "connected"`)
	want := func(ks string) {
		t.Helper()
		p.Wait(`[...document.querySelectorAll("#feed > li")].map((l) => l.dataset.vkRow).join(" ") === ` + fmt.Sprintf("%q", ks) + ` && !document.querySelector("[aria-busy]")`)
	}
	want("1 2")
	p.Click("#atend")
	want("1 2 101")
	p.Click("#at")
	want("1 102 103 2 101")
	p.Click("#at") // past the limit: the end trimmed
	want("1 104 105 102 103")
}

// TestBrowserStreamFocus: a focused row the batch removes hands focus to
// the next row, else the previous, else the container; a trimmed row too;
// focus outside the rows stays where it is.
func TestBrowserStreamFocus(t *testing.T) {
	a := New(Only("none"))
	a.Live("/feed", func() *feedPage {
		return &feedPage{Log: &keyedLog{}, Max: 3, Start: []feedItem{{1, "one"}, {2, "two"}, {3, "three"}}}
	})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Start(srv.URL+"/feed", `document.documentElement.dataset.vkState === "connected"`)
	p.Front()
	want := func(ks string) {
		t.Helper()
		p.Wait(`[...document.querySelectorAll("#feed > li")].map((l) => l.dataset.vkRow).join(" ") === ` + fmt.Sprintf("%q", ks) + ` && !document.querySelector("[aria-busy]")`)
	}
	press := func(sel string) { // focus and click, as a keyboard does
		t.Helper()
		if err := p.Eval(fmt.Sprintf(`(() => { const b = document.querySelector(%q); b.focus(); b.click(); })()`, sel), nil); err != nil {
			t.Fatal(err)
		}
	}
	focused := func(want string) {
		t.Helper()
		p.Wait(fmt.Sprintf(`document.activeElement === document.querySelector(%q)`, want))
	}

	press(`#feed > li[data-vk-row="2"] .del`)
	want("1 3")
	focused(`#feed > li[data-vk-row="3"]`) // the next row
	press(`#feed > li[data-vk-row="3"] .del`)
	want("1")
	focused(`#feed > li[data-vk-row="1"]`) // the previous
	press(`#feed > li[data-vk-row="1"] .del`)
	want("")
	focused(`#feed`) // the container

	p.Click("#reload")
	want("1 2")
	p.Click("#add")
	want("1 2 101")
	press("#add") // row 1 trimmed while #add has focus: it keeps it
	want("2 101 102")
	focused("#add")

	if err := p.Eval(`document.querySelector('#feed > li[data-vk-row="2"] input').focus()`, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Eval(`document.querySelector("#add").click()`, nil); err != nil { // focus stays in row 2
		t.Fatal(err)
	}
	want("101 102 103")
	focused(`#feed > li[data-vk-row="101"]`) // the trimmed row's next
}
