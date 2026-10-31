package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// orderDB is what the orders pages' methods read and write: a dependency.
type orderDB struct {
	mu     sync.Mutex
	alerts []int64
	writes int
}

func (d *orderDB) remove(id int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writes++
	for i, a := range d.alerts {
		if a == id {
			d.alerts = append(d.alerts[:i], d.alerts[i+1:]...)
			break
		}
	}
}

func (d *orderDB) list() ui.Node {
	d.mu.Lock()
	defer d.mu.Unlock()
	var items []ui.Node
	for _, a := range d.alerts {
		items = append(items, ui.El("li", nil, ui.Textf("alert %d", a)))
	}
	return ui.Fragment(items...)
}

type orderQuote struct {
	SKU    string `form:"sku"`
	Qty    int    `validate:"min=1"`
	Coupon string
}

type orderPlace struct {
	City   string
	Region string
	Zip    vuka.Option[string]
	Gift   bool
}

// pushKit are the methods both orders pages push: the stateless one
// calls them over HTTP, the live one over its socket (or HTTP with Read).
type pushKit struct{}

func (pushKit) details(shop string, id int64) (ui.Node, error) {
	if id == 0 {
		return nil, BadRequest("no such order")
	}
	return ui.El("p", []ui.Attr{attr("class", "detail")}, ui.Textf("order %d of %s", id, shop)), nil
}

func (pushKit) price(sku string, qty int, coupon string) (string, error) {
	return fmt.Sprintf("%s × %d %s", sku, qty, coupon), nil
}

func (pushKit) lookup(postcode string) (orderPlace, error) {
	if postcode == "" {
		return orderPlace{}, BadRequest("no postcode")
	}
	return orderPlace{City: "Arusha", Region: "North " + postcode, Gift: true}, nil
}

func (pushKit) checkName(name string) ui.Commands {
	if name == "taken" {
		return ui.JS.SetText(ui.Sel("#name-msg"), name+" is taken").AddClass(ui.Sel("#name-msg"), "bad")
	}
	return ui.JS.SetText(ui.Sel("#name-msg"), name+" is free")
}

// pushForm is the markup both pages render, pushing to p's methods.
func pushForm(details, older, newer, price, quote, lookup, check, slow, count, unsub any) ui.Node {
	b := func(id string, ch ui.Commands, label string) ui.Node {
		return ui.El("button", []ui.Attr{attr("id", id), attr("type", "button"), attr("onClick", ui.On(ch))}, ui.Text(label))
	}
	return ui.Fragment(
		b("b-details", ui.JS.Push(details, ui.Value(int64(7)), ui.Read()).Into(ui.Sel("#details")), "view"),
		b("b-details-write", ui.JS.Push(details, int64(8)).Into(ui.Sel("#details")), "view (write)"),
		b("b-details-bad", ui.JS.Push(details, int64(0), ui.Read()).Into(ui.Sel("#details")).OnError(ui.JS.SetText(ui.Sel("#err"), ui.LoadError)), "broken"),
		ui.El("div", []ui.Attr{attr("id", "details")}, ui.Text("none")),
		ui.El("p", []ui.Attr{attr("id", "err")}),
		ui.El("ul", []ui.Attr{attr("id", "posts")}, ui.El("li", nil, ui.Text("post 5"))),
		b("older", ui.JS.Push(older, 5, ui.Read()).Into(ui.Sel("#posts"), ui.Append()), "older"),
		b("newer", ui.JS.Push(newer, 5, ui.Read()).Into(ui.Sel("#posts"), ui.Prepend()), "newer"),
		ui.El("form", []ui.Attr{attr("id", "order")},
			ui.El("input", []ui.Attr{attr("name", "sku"), attr("value", "tea")}),
			ui.El("input", []ui.Attr{attr("id", "qty"), attr("name", "qty"), attr("value", "2"),
				attr("onInput", ui.On(ui.JS.Push(price, "tea", ui.This.Value(), ui.Sel("[name=coupon]").Value(), ui.Read()).Into(ui.Sel("#price"))))}),
			ui.El("input", []ui.Attr{attr("name", "coupon"), attr("value", "SAVE")}),
			b("quote", ui.JS.Push(quote, ui.Read()).Into(ui.Sel("#total")), "quote"),
			ui.El("input", []ui.Attr{attr("id", "total"), attr("name", "total")}),
			ui.El("input", []ui.Attr{attr("id", "postcode"), attr("name", "postcode"),
				attr("onChange", ui.On(ui.JS.Push(lookup, ui.This.Value(), ui.Read()).Into(ui.Closest("form"))))}),
			ui.El("input", []ui.Attr{attr("name", "city")}),
			ui.El("input", []ui.Attr{attr("name", "region")}),
			ui.El("input", []ui.Attr{attr("name", "zip"), attr("value", "kept")}),
			ui.El("input", []ui.Attr{attr("name", "gift"), attr("type", "checkbox")}),
		),
		ui.El("span", []ui.Attr{attr("id", "price")}, ui.Text("-")),
		ui.El("input", []ui.Attr{attr("id", "username"), attr("onInput", ui.On(ui.JS.Push(check, ui.This.Value(), ui.Read())))}),
		ui.El("p", []ui.Attr{attr("id", "name-msg")}),
		b("b-slow", ui.JS.Push(slow, 3, ui.Read()).Into(ui.Sel("#slow")), "slow"),
		b("fast", ui.JS.Push(slow, 1, ui.Read()).Into(ui.Sel("#slow")), "fast"),
		ui.El("span", []ui.Attr{attr("id", "slow")}),
		b("b-count", ui.JS.Push(count, ui.Cache(time.Minute)).Into(ui.Sel("#count")), "count"),
		ui.El("span", []ui.Attr{attr("id", "count")}),
		ui.El("ul", []ui.Attr{attr("id", "alerts")}, ui.El("li", nil, ui.Text("alert 1")), ui.El("li", nil, ui.Text("alert 2"))),
		b("unsub", ui.JS.Push(unsub, int64(1)).Into(ui.Sel("#alerts")), "remove"),
	)
}

// ordersPage is a folder page that isn't live: its path field bound for
// its methods, its guard noted (and refusing ?closed), its Load noted.
type ordersPage struct {
	Shop string
	log  *accessLog
	pushKit
}

func newOrdersPage(l *accessLog) *ordersPage { return &ordersPage{log: l} }

func (p *ordersPage) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }
func (p *ordersPage) mount(context.Context) error { p.log.note("mount"); return nil }

func (p *ordersPage) Details(ctx context.Context, id int64) (ui.Node, error) {
	p.log.note("details " + p.Shop)
	return p.details(p.Shop, id)
}
func (p *ordersPage) Older(before int) ui.Node {
	return ui.Fragment(ui.El("li", nil, ui.Textf("post %d", before-1)), ui.El("li", nil, ui.Textf("post %d", before-2)))
}
func (p *ordersPage) Newer(after int) ui.Node {
	return ui.El("li", nil, ui.Textf("post %d", after+1))
}
func (p *ordersPage) Price(sku string, qty int, coupon string) (string, error) {
	return p.price(sku, qty, coupon)
}
func (p *ordersPage) Quote(q orderQuote) (int, error) { return q.Qty * 100, nil }
func (p *ordersPage) Lookup(postcode string) (orderPlace, error) {
	return p.lookup(postcode)
}
func (p *ordersPage) CheckName(name string) ui.Commands { return p.checkName(name) }
func (p *ordersPage) Slow(n int) string {
	time.Sleep(time.Duration(n) * 150 * time.Millisecond)
	return strconv.Itoa(n)
}
func (p *ordersPage) Count() int { return 42 }
func (p *ordersPage) Unsubscribe(ctx context.Context, id int64, db *orderDB) (ui.Node, error) {
	p.log.note("unsubscribe")
	db.remove(id)
	return db.list(), nil
}

func (p *ordersPage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Orders"))), ui.El("body", nil, ui.El("main", nil,
		pushForm(p.Details, p.Older, p.Newer, p.Price, p.Quote, p.Lookup, p.CheckName, p.Slow, p.Count, p.Unsubscribe)))).Render(ctx, w)
}

type ordersGuard struct{ Shop string }

func (g *ordersGuard) Load(ctx context.Context, l *accessLog) error {
	l.note("guard " + g.Shop)
	if Request(ctx).URL.Query().Get("closed") != "" {
		return Forbidden
	}
	return nil
}

// livePush is the same markup on a live page: its pushes run on its
// instance over the socket, but for the reads, which go over HTTP.
type livePush struct {
	ui.Live
	pushKit
	clicks int
}

func (p *livePush) Inc() { p.clicks++ }
func (p *livePush) Details(ctx context.Context, id int64) (ui.Node, error) {
	return p.details("live", id)
}
func (p *livePush) Older(before int) ui.Node {
	return ui.El("li", nil, ui.Textf("post %d", before-1))
}
func (p *livePush) Newer(after int) ui.Node { return ui.El("li", nil, ui.Textf("post %d", after+1)) }
func (p *livePush) Price(sku string, qty int, coupon string) (string, error) {
	return p.price(sku, qty, coupon)
}
func (p *livePush) Quote(q orderQuote) (int, error) { return q.Qty * 100, nil }
func (p *livePush) Lookup(postcode string) (orderPlace, error) {
	return p.lookup(postcode)
}
func (p *livePush) CheckName(name string) ui.Commands { return p.checkName(name) }
func (p *livePush) Slow(n int) string {
	time.Sleep(time.Duration(n) * 150 * time.Millisecond)
	return strconv.Itoa(n)
}
func (p *livePush) Count() int { return 1 }

// Bump is a socket push changing the page's state and returning a result.
func (p *livePush) Bump(by int) (string, error) {
	p.clicks += by
	return fmt.Sprintf("bumped to %d", p.clicks), nil
}
func (p *livePush) Unsubscribe(ctx context.Context, id int64, db *orderDB) (ui.Node, error) {
	db.remove(id)
	return db.list(), nil
}

func (p *livePush) Render() ui.Node {
	return ui.El("main", nil,
		pushForm(p.Details, p.Older, p.Newer, p.Price, p.Quote, p.Lookup, p.CheckName, p.Slow, p.Count, p.Unsubscribe),
		ui.El("button", []ui.Attr{attr("id", "inc"), attr("onClick", ui.On(p.Inc))}, ui.Text("+")),
		ui.El("button", []ui.Attr{attr("id", "bump"), attr("onClick", ui.On(ui.JS.Push(p.Bump, 10).Into(ui.Sel("#bumped"))))}, ui.Text("bump")),
		ui.El("span", []ui.Attr{attr("id", "bumped")}),
		ui.El("p", []ui.Attr{attr("id", "clicks")}, ui.Textf("clicks %d", p.clicks)))
}

func init() {
	for _, m := range []any{(*ordersPage).Details, (*ordersPage).Older, (*ordersPage).Newer, (*ordersPage).Price, (*ordersPage).Quote,
		(*ordersPage).Lookup, (*ordersPage).CheckName, (*ordersPage).Slow, (*ordersPage).Count, (*ordersPage).Unsubscribe,
		(*livePush).Details, (*livePush).Older, (*livePush).Newer, (*livePush).Price, (*livePush).Quote,
		(*livePush).Lookup, (*livePush).CheckName, (*livePush).Slow, (*livePush).Count} {
		ui.RegisterPush(m)
	}
}

// callLog records the push calls an app gets: method, path.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (c *callLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(PushHeader) != "" {
			c.mu.Lock()
			c.calls = append(c.calls, r.Method+" csrf="+strconv.FormatBool(r.Header.Get("X-CSRF-Token") != ""))
			c.mu.Unlock()
		}
		h.ServeHTTP(w, r)
	})
}

func (c *callLog) take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := strings.Join(c.calls, ",")
	c.calls = nil
	return s
}

func ordersApp() (*App, *orderDB, *accessLog) {
	db, l := &orderDB{alerts: []int64{1, 2}}, &accessLog{errs: map[int]error{}}
	a := New(Only("none"), Provide(func() *orderDB { return db }), Provide(func() *accessLog { return l }))
	a.Get("/orders/{shop}", newOrdersPage, PageRoute(), GuardOf[ordersGuard]())
	a.Live("/live-orders", &livePush{})
	return a, db, l
}

var pushRefRe = regexp.MustCompile(`&#34;m&#34;:&#34;([^&]+)&#34;`)

// pushRefs are the references of the push steps path renders, in order.
func pushRefs(t *testing.T, srv *httptest.Server, path string) []string {
	t.Helper()
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var out []string
	for _, m := range pushRefRe.FindAllStringSubmatch(string(b), -1) {
		out = append(out, m[1])
	}
	return out
}

func pushCall(t *testing.T, srv *httptest.Server, method, path, ref, args string, header ...string) (*http.Response, string) {
	t.Helper()
	var req *http.Request
	if method == "GET" {
		req, _ = http.NewRequest("GET", srv.URL+path, nil)
		q := req.URL.Query()
		q.Set("_vk_args", args)
		req.URL.RawQuery = q.Encode()
	} else {
		req, _ = http.NewRequest("POST", srv.URL+path, strings.NewReader(url.Values{"_vk_args": {args}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	req.Header.Set(PushHeader, ref)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(b)
}

// TestPushCallHTTP: a page answers its push steps' calls: a read as a GET
// (cached only when the step says so), a write only as a POST, which the
// app's CSRF protection checks; guards run, Load doesn't; a forged or
// another route's reference, or a cross-site call, is refused.
func TestPushCallHTTP(t *testing.T) {
	a, db, l := ordersApp()
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	refs := pushRefs(t, srv, "/orders/east")
	if len(refs) != 13 {
		t.Fatalf("%d push steps", len(refs))
	}
	read, write, count := refs[0], refs[1], refs[11]
	l.take()
	res, body := pushCall(t, srv, "GET", "/orders/east", read, `[7]`)
	if res.StatusCode != 200 || !strings.HasPrefix(body, `{"html":`) || !strings.Contains(body, `order 7 of east`) || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("a read: %d %s %q", res.StatusCode, body, res.Header.Get("Cache-Control"))
	}
	if got := l.take(); got != "guard east,details east" {
		t.Errorf("ran %s", got)
	}
	if res, _ := pushCall(t, srv, "GET", "/orders/east", count, `[]`); res.Header.Get("Cache-Control") != "private, max-age=60" {
		t.Errorf("a cached read: %q", res.Header.Get("Cache-Control"))
	}
	if res, body := pushCall(t, srv, "GET", "/orders/east", write, `[8]`); res.StatusCode != 405 {
		t.Errorf("a write as a GET: %d %s", res.StatusCode, body)
	}
	if res, body := pushCall(t, srv, "POST", "/orders/east", write, `[8]`); res.StatusCode != 200 || !strings.Contains(body, "order 8 of east") {
		t.Errorf("a write: %d %s", res.StatusCode, body)
	}
	if res, _ := pushCall(t, srv, "POST", "/orders/east", write, `[8]`, "Sec-Fetch-Site", "cross-site"); res.StatusCode != 403 {
		t.Errorf("a cross-site write: %d", res.StatusCode)
	}
	if res, _ := pushCall(t, srv, "GET", "/orders/east", read, `[7]`, "Sec-Fetch-Site", "cross-site"); res.StatusCode != 403 {
		t.Errorf("a cross-site read: %d", res.StatusCode)
	}
	// an unvouched POST carrying the visitor's cookie needs the token
	jar := &http.Cookie{Name: csrfCookie, Value: strings.Repeat("a", 24)}
	req, _ := http.NewRequest("POST", srv.URL+"/orders/east", strings.NewReader(url.Values{"_vk_args": {`[8]`}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(PushHeader, write)
	req.AddCookie(jar)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 403 {
		t.Errorf("a write without the CSRF token: %d", res.StatusCode)
	}
	// forged, another route's, a stale one
	if res, body := pushCall(t, srv, "GET", "/orders/east", strings.Split(read, ".")[0]+".r.forged", `[7]`); res.StatusCode != 400 || !strings.Contains(body, "names no method this page calls") {
		t.Errorf("a forged reference: %d %s", res.StatusCode, body)
	}
	liveRefs := pushRefs(t, srv, "/live-orders")
	if res, _ := pushCall(t, srv, "GET", "/orders/east", liveRefs[0], `[7]`); res.StatusCode != 400 {
		t.Errorf("another route's reference: %d", res.StatusCode)
	}
	if res, body := pushCall(t, srv, "GET", "/orders/west", read, `[7]`); res.StatusCode != 200 || !strings.Contains(body, "of west") {
		t.Errorf("another path of the route: %d %s", res.StatusCode, body)
	}
	l.take()
	if res, _ := pushCall(t, srv, "GET", "/orders/east?closed=1", read, `[7]`); res.StatusCode != 403 {
		t.Errorf("a refusing guard: %d", res.StatusCode)
	}
	if got := l.take(); strings.Contains(got, "details") {
		t.Errorf("ran %s behind a refusing guard", got)
	}
	// the Options header still works
	if res, body := pushCall(t, srv, "GET", "/orders/east", read, `[7]`, PushHeader, "", OptionsHeader, read); res.StatusCode != 200 || !strings.Contains(body, "order 7") {
		t.Errorf("Vk-Options: %d %s", res.StatusCode, body)
	}
	if db.writes != 0 {
		t.Errorf("%d writes: a refused call ran", db.writes)
	}
	// a page whose path has a POST of the app's own: a write's call is the
	// page's, any other post the app's
	a2, _, _ := ordersApp()
	a2.Post("/orders/{shop}", func() string { return "own post" })
	srv2 := httptest.NewServer(a2.Handler())
	defer srv2.Close()
	w2 := pushRefs(t, srv2, "/orders/north")[1]
	if res, body := pushCall(t, srv2, "POST", "/orders/north", w2, `[8]`); res.StatusCode != 200 || !strings.Contains(body, "order 8 of north") {
		t.Errorf("a write beside the app's POST: %d %s", res.StatusCode, body)
	}
	if res, err := http.Post(srv2.URL+"/orders/north", "text/plain", nil); err != nil || res.StatusCode != 200 {
		t.Errorf("the app's POST: %v %v", res, err)
	} else if b, _ := io.ReadAll(res.Body); !strings.Contains(string(b), "own post") {
		t.Errorf("the app's POST: %s", b)
	}
}

// TestBrowserPush: each kind of result goes Into its target, on a page
// that isn't live (over HTTP: reads GET, writes POST with the CSRF token)
// and on a live one (over the socket, reads over HTTP): markup replacing,
// appended and prepended; a value and text; a struct filling a form; a
// struct from the form; commands; a failure's message read by OnError; a
// later push into a target winning; on the live page, loaded content
// surviving a re-render.
func TestBrowserPush(t *testing.T) {
	a, db, _ := ordersApp()
	calls := &callLog{}
	srv := httptest.NewServer(calls.wrap(a.Handler()))
	defer srv.Close()
	b := chrome.Launch(t)
	for _, path := range []string{"/orders/east", "/live-orders"} {
		t.Run(strings.Trim(strings.ReplaceAll(path, "/", "-"), "-"), func(t *testing.T) {
			stateless := path != "/live-orders"
			db.mu.Lock()
			db.alerts = []int64{1, 2}
			db.mu.Unlock()
			var p *chrome.Page
			if stateless {
				p = b.Visit(srv.URL + path)
			} else {
				p = b.Open(srv.URL + path)
			}
			calls.take()
			var got any
			shop := "east"
			if !stateless {
				shop = "live"
			}
			// a Node, read
			p.Click("#b-details")
			p.WaitText("#details", "order 7 of "+shop)
			if c := calls.take(); c != "GET csrf=false" {
				t.Errorf("a read went %q", c)
			}
			// a Node, written: a POST with the token on a page that isn't
			// live, the socket on a live one
			p.Click("#b-details-write")
			p.WaitText("#details", "order 8 of "+shop)
			want := "POST csrf=true"
			if !stateless {
				want = ""
			}
			if c := calls.take(); c != want {
				t.Errorf("a write went %q, want %q", c, want)
			}
			// a failure: OnError reads its message
			p.Click("#b-details-bad")
			p.WaitText("#err", "no such order")
			// append, prepend
			p.Click("#older")
			p.Wait(`document.querySelectorAll("#posts li").length >= 2`)
			p.Click("#newer")
			p.Wait(`document.querySelector("#posts li").textContent === "post 6"`)
			p.Eval(`Array.from(document.querySelectorAll("#posts li"), (l) => l.textContent).join(",")`, &got)
			wantPosts := "post 6,post 5,post 4,post 3"
			if !stateless {
				wantPosts = "post 6,post 5,post 4"
			}
			if got != wantPosts {
				t.Errorf("posts: %v", got)
			}
			// a string into text, from a Go value and two reads
			p.Eval(`{ const q = document.querySelector("#qty"); q.value = "3"; q.dispatchEvent(new Event("input", { bubbles: true })) }`, nil)
			p.WaitText("#price", "tea × 3 SAVE")
			// a number into a field, its struct from the enclosing form
			p.Eval(`window.totalChanges = 0; document.querySelector("#total").addEventListener("change", () => totalChanges++)`, nil)
			p.Click("#quote")
			p.Wait(`document.querySelector("#total").value === "300"`)
			p.Eval(`totalChanges`, &got)
			if got != float64(1) {
				t.Errorf("the field's change fired %v times", got)
			}
			// a struct filling the form's controls; Zip, None, left alone
			p.Eval(`{ const q = document.querySelector("#postcode"); q.value = "0100"; q.dispatchEvent(new Event("change", { bubbles: true })) }`, nil)
			p.Wait(`document.querySelector("[name=city]").value === "Arusha"`)
			p.Eval(`[document.querySelector("[name=region]").value, document.querySelector("[name=zip]").value, document.querySelector("[name=gift]").checked]`, &got)
			if fmt.Sprint(got) != "[North 0100 kept true]" {
				t.Errorf("the filled form: %v", got)
			}
			// commands decided by the server
			p.Eval(`{ const q = document.querySelector("#username"); q.value = "taken"; q.dispatchEvent(new Event("input", { bubbles: true })) }`, nil)
			p.WaitText("#name-msg", "taken is taken")
			p.Eval(`document.querySelector("#name-msg").classList.contains("bad")`, &got)
			if got != true {
				t.Errorf("the commands' class: %v", got)
			}
			// a later push into a target wins over an earlier one on its way
			p.Click("#b-slow")
			p.Click("#fast")
			p.WaitText("#slow", "1")
			time.Sleep(600 * time.Millisecond)
			if s := p.Text("#slow"); s != "1" {
				t.Errorf("the earlier push won: %q", s)
			}
			// a write re-rendering a list
			p.Click("#unsub")
			p.Wait(`document.querySelectorAll("#alerts li").length === 1`)
			if s := p.Text("#alerts"); s != "alert 2" {
				t.Errorf("alerts: %q", s)
			}
			if stateless {
				return
			}
			// a socket push changing the page's state, with a result
			p.Click("#bump")
			p.WaitText("#bumped", "bumped to 10")
			p.WaitText("#clicks", "clicks 10")
			// what pushes put in places stays through a re-render rendering
			// them as before
			p.Click("#inc")
			p.WaitText("#clicks", "clicks 11")
			p.Eval(`[document.querySelector("#details").textContent, document.querySelector("#price").textContent, document.querySelector("#bumped").textContent, document.querySelector("#total").value, document.querySelectorAll("#posts li").length]`, &got)
			if fmt.Sprint(got) != "[order 8 of live tea × 3 SAVE bumped to 10 300 3]" {
				t.Errorf("after a re-render: %v", got)
			}
		})
	}
}
