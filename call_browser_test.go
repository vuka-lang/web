package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// stockroom is what the product pages' components change: a dependency, so
// the pages over HTTP and the live one share it.
type stockroom struct {
	mu    sync.Mutex
	saves int
}

func (s *stockroom) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
}

func (s *stockroom) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

// productPreview is stateless: it opens over HTTP on a page that isn't
// live, and on the live page's session.
type productPreview struct {
	ID     int64
	Query  any
	OnSave ui.Action
}

func (p productPreview) Done(ctx context.Context, s *stockroom) {
	s.save()
	ui.Close(ctx)
}

func (p productPreview) View() ui.Node {
	q, _ := p.Query.(string)
	return ui.El("section", nil,
		ui.El("p", []ui.Attr{attr("id", "preview")}, ui.Text(fmt.Sprintf("product %d %s", p.ID, q))),
		ui.El("input", []ui.Attr{attr("id", "preview-field")}),
		ui.El("button", []ui.Attr{attr("id", "preview-save"), attr("type", "button"), attr("onClick", ui.On(p.OnSave))}, ui.Text("Save")),
		ui.El("button", []ui.Attr{attr("id", "preview-done"), attr("type", "button"), attr("onClick", ui.On(ui.JS.Push(p.Done)))}, ui.Text("Done")),
		ui.El("button", []ui.Attr{attr("id", "preview-cancel"), attr("type", "button"), attr("onClick", ui.On(ui.JS.Close()))}, ui.Text("Cancel")))
}

func (p productPreview) Render(ctx context.Context, w io.Writer) error {
	return p.View().Render(ctx, w)
}

// stockEditor is stateful, in a ui.Dialog: it opens on a live page.
type stockEditor struct {
	ui.Live
	ID   int64
	n    int64
	room *stockroom // injected
}

func (e *stockEditor) LiveSchema(l *ui.LiveSchema) { l.Mount(e.mount) }
func (e *stockEditor) mount(ctx context.Context) error {
	if e.ID == 0 {
		return BadRequest("no such product")
	}
	e.n = e.ID
	return nil
}
func (e *stockEditor) More() { e.n++ }
func (e *stockEditor) Save(ctx context.Context) {
	e.room.save()
	ui.Close(ctx)
}
func (e *stockEditor) Render() ui.Node {
	return ui.HTML("{d}", ui.Dialog{Title: ui.Text("Stock"), Children: ui.Fragment(
		ui.El("output", []ui.Attr{attr("id", "stock-n")}, ui.Text(fmt.Sprint(e.n))),
		ui.El("button", []ui.Attr{attr("id", "stock-more"), attr("type", "button"), attr("onClick", ui.On(e.More))}, ui.Text("+")),
		ui.El("button", []ui.Attr{attr("id", "stock-save"), attr("type", "button"), attr("onClick", ui.On(e.Save))}, ui.Text("Save")),
	)})
}

func init() {
	ui.RegisterCall(productPreview{})
	ui.RegisterCall(stockEditor{})
	ui.RegisterPush(productPreview.Done)
	ui.RegisterPush((*productsPage).Saved)
}

func openLog(s string) ui.Commands { return ui.JS.Call("log", s) }

// productsMarkup is what both product pages render.
func productsMarkup(onSave any, extra ...ui.Node) ui.Node {
	b := func(id string, ch ui.Commands) ui.Node {
		return ui.El("button", []ui.Attr{attr("id", id), attr("type", "button"), attr("onClick", ui.On(ch))}, ui.Text(id))
	}
	open := ui.JS.Call(productPreview{ID: 7, Query: ui.Sel("#q").Value(), OnSave: onSave}).
		Before(openLog("before")).Success(openLog("success")).Error(openLog("error")).After(openLog("after")).Closed(openLog("closed"))
	return ui.Fragment(append([]ui.Node{
		ui.El("input", []ui.Attr{attr("id", "q"), attr("value", "red")}),
		b("open", open),
		ui.El("p", []ui.Attr{attr("id", "err")}),
	}, extra...)...)
}

type productsPage struct{}

func (p *productsPage) Saved(s *stockroom) { s.save() }

func (p *productsPage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Products"))),
		ui.El("body", nil, ui.El("main", nil, productsMarkup(p.Saved)))).Render(ctx, w)
}

type liveProducts struct {
	ui.Live
	Shelf *stockroom
	saved int
}

func (p *liveProducts) Render() ui.Node {
	b := func(id string, ch ui.Commands) ui.Node {
		return ui.El("button", []ui.Attr{attr("id", id), attr("type", "button"), attr("onClick", ui.On(ch))}, ui.Text(id))
	}
	return ui.El("main", nil, productsMarkup(func() { p.saved++ },
		b("stock", ui.JS.Call(stockEditor{ID: 4}).Success(openLog("success")).Closed(openLog("closed"))),
		b("broken", ui.JS.Call(stockEditor{}).Error(ui.JS.SetText("#err", ui.LoadError))),
		ui.El("i", []ui.Attr{attr("id", "saved")}, ui.Text(fmt.Sprint(p.saved)))))
}

// TestBrowserCall: a component a Call opens shows modal over the page, on
// a page that isn't live (over HTTP) and on a live one (on its session):
// the hooks in order, Success once it is on screen, its Action prop
// running the page's method or closure, Escape and the close buttons and
// the server's ui.Close closing it, the focus back on what opened it,
// Closed once it is gone.
func TestBrowserCall(t *testing.T) {
	sh := &stockroom{}
	a := New(Only("none"), Provide(func() *stockroom { return sh }))
	a.Get("/products", func() *productsPage { return &productsPage{} }, PageRoute())
	a.Live("/live-products", &liveProducts{Shelf: sh})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	for _, path := range []string{"/products", "/live-products"} {
		t.Run(strings.Trim(path, "/"), func(t *testing.T) {
			live := path == "/live-products"
			var p *chrome.Page
			if live {
				p = b.Open(srv.URL + path)
			} else {
				p = b.Visit(srv.URL + path)
			}
			p.Eval(`window.steps = []; window.log = (ctx, s) => { window.steps.push(s); }`, nil)
			var got any
			state := func() string {
				t.Helper()
				p.Eval(`(() => { const d = document.querySelector("[data-vk-call]"); return [window.steps.join(","), d ? d.open + " " + d.matches(":modal") + " " + (d.parentElement.closest("#vk-overlays") !== null) : "none", document.activeElement && document.activeElement.id].join(" | "); })()`, &got)
				return fmt.Sprint(got)
			}
			open := func() {
				t.Helper()
				p.Eval(`window.steps = []; document.querySelector("#open").focus()`, nil)
				p.Click("#open")
				p.Wait(`window.steps.includes("after")`)
			}

			open()
			if s := state(); !strings.HasPrefix(s, "before,success,after | true true true") {
				t.Fatalf("opened modal in the overlay host after Success: %s", s)
			}
			if s := p.Text("#preview"); s != "product 7 red" {
				t.Errorf("its props and the read: %q", s)
			}
			p.Eval(`document.activeElement && document.querySelector("[data-vk-call]").contains(document.activeElement)`, &got)
			if got != true {
				t.Errorf("the focus is in the dialog: %v", state())
			}
			p.Press("Escape")
			p.Wait(`window.steps.includes("closed")`)
			p.Wait(`!document.querySelector("[data-vk-call]")`)
			if s := state(); s != "before,success,after,closed | none | open" {
				t.Errorf("Escape: closed, the focus back on the button: %s", s)
			}

			open()
			p.Click("#preview-cancel")
			p.Wait(`!document.querySelector("[data-vk-call]") && window.steps.includes("closed")`)

			saves := sh.count()
			open()
			p.Click("#preview-done")
			p.Wait(`!document.querySelector("[data-vk-call]") && window.steps.includes("closed")`)
			if sh.count() != saves+1 {
				t.Errorf("the component's pushed method ran: %d", sh.count())
			}
			if s := state(); !strings.HasSuffix(s, "| none | open") {
				t.Errorf("ui.Close from its method: %s", s)
			}

			open()
			p.Click("#preview-save")
			if live {
				p.WaitText("#saved", "1")
			} else {
				for deadline := time.Now().Add(5 * time.Second); sh.count() == saves+1 && time.Now().Before(deadline); {
					time.Sleep(10 * time.Millisecond)
				}
				if sh.count() != saves+2 {
					t.Errorf("the Action prop pushed the page's method: %d", sh.count())
				}
			}
			p.Click(".vk-call__close")
			p.Wait(`!document.querySelector("[data-vk-call]")`)
			if !live {
				return
			}

			p.Eval(`window.steps = []`, nil)
			p.Click("#stock")
			p.Wait(`window.steps.includes("success")`)
			p.Eval(`(() => { const d = document.querySelector("[data-vk-call]"); return d.className + " " + d.matches(":modal"); })()`, &got)
			if got != "vk-dialog true" {
				t.Errorf("a ui.Dialog root is the presentation: %v", got)
			}
			p.Click("#stock-more")
			p.WaitText("#stock-n", "5")
			saves = sh.count()
			p.Click("#stock-save")
			p.Wait(`!document.querySelector("[data-vk-call]") && window.steps.includes("closed")`)
			if sh.count() != saves+1 {
				t.Errorf("the dependency field injected: %d", sh.count())
			}

			p.Click("#broken")
			p.WaitText("#err", "no such product")
			p.Eval(`document.querySelector("[data-vk-call] .vk-call__error").textContent`, &got)
			if got != "This couldn't open." {
				t.Errorf("a Mount that fails shows the error state: %v", got)
			}
		})
	}
}

var openToken = regexp.MustCompile(`&#34;t&#34;:&#34;([^&]+)&#34;`)
var openCSRF = regexp.MustCompile(`&#34;csrf&#34;:&#34;([^&]+)&#34;`)

// TestCallRefusedOverHTTP: an open over HTTP takes only a token the page
// rendered for this visitor, as rendered, naming a component a Call names.
func TestCallRefusedOverHTTP(t *testing.T) {
	sh := &stockroom{}
	a := New(Only("none"), Provide(func() *stockroom { return sh }))
	a.Get("/products", func() *productsPage { return &productsPage{} }, PageRoute())
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	visit := func() (*http.Client, string, string) {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar}
		res, err := c.Get(srv.URL + "/products")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return c, html.UnescapeString(openToken.FindStringSubmatch(string(body))[1]), html.UnescapeString(openCSRF.FindStringSubmatch(string(body))[1])
	}
	open := func(c *http.Client, tok, csrf string) (int, string) {
		form := url.Values{"_vk_key": {"k1"}, "_vk_reads": {`{"Query":"blue"}`}}
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/products", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set(CallHeader, tok)
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	c1, tok, csrf := visit()
	if code, body := open(c1, tok, csrf); code != 200 || !strings.Contains(body, `data-vk-call=\"k1\"`) || !strings.Contains(body, "product 7 blue") {
		t.Fatalf("its own visitor: %d %s", code, body)
	}
	c2, _, csrf2 := visit()
	if code, _ := open(c2, tok, csrf2); code != http.StatusForbidden {
		t.Errorf("another session's token: %d", code)
	}
	body, sig, _ := strings.Cut(tok, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	parts := strings.Split(string(raw), "\x00")
	forge := func(i int, v string) string {
		p := append([]string(nil), parts...)
		p[i] = v
		return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(p, "\x00"))) + "." + sig
	}
	if code, _ := open(c1, forge(2, strings.Replace(parts[2], `"ID":7`, `"ID":8`, 1)), csrf); code != http.StatusForbidden {
		t.Errorf("a changed prop: %d", code)
	}
	if code, _ := open(c1, forge(1, "AAAAAAAAAAAA"), csrf); code != http.StatusForbidden {
		t.Errorf("an unregistered component: %d", code)
	}
}

// catalogPage isn't live, but a Call of a stateful component makes it so:
// the component opens on its session.
type catalogPage struct{}

func (p *catalogPage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("body", nil, ui.El("button", []ui.Attr{attr("onClick", ui.On(ui.JS.Call(stockEditor{ID: 1})))}, ui.Text("Stock")))).Render(ctx, w)
}

func TestCallMakesPageLive(t *testing.T) {
	a := New(Only("none"), Provide(func() *stockroom { return &stockroom{} }))
	a.Get("/catalog", func() *catalogPage { return &catalogPage{} }, PageRoute())
	a.Get("/products", func() *productsPage { return &productsPage{} }, PageRoute())
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	for path, live := range map[string]bool{"/catalog": true, "/products": false} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if got := strings.Contains(string(b), "data-vk-token="); got != live {
			t.Errorf("%s: live %v, want %v:\n%s", path, got, live, b)
		}
		if !strings.Contains(string(b), `<div id="vk-overlays" data-vk-overlays></div>`) {
			t.Errorf("%s: no overlay host:\n%s", path, b)
		}
	}
}
