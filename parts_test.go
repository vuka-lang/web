package web

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2"
)

// partsPage is a page with deferred parts: Stats waits for release (or its
// request's end), Recent is in Stats's group, Broken fails.
type partsPage struct {
	release  chan struct{}
	canceled *atomic.Bool
	fail     error
}

func (p *partsPage) Stats(ctx context.Context) (int, error) {
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			if p.canceled != nil {
				p.canceled.Store(true)
			}
			return 0, ctx.Err()
		}
	}
	NoteStatement(ctx, Statement{SQL: "SELECT count(*) FROM apps", Start: time.Now()})
	return 3, nil
}

func (p *partsPage) Recent(ctx context.Context) ([]string, error) { return []string{"a", "b"}, nil }

func (p *partsPage) Broken(ctx context.Context) (string, error) {
	if p.fail != nil {
		return "", p.fail
	}
	return "", errors.New("store: connection refused")
}

func init() {
	vuka.RegisterFunc((*partsPage).Stats, ui.Defer("main"))
	vuka.RegisterFunc((*partsPage).Recent, ui.Defer("main"))
	vuka.RegisterFunc((*partsPage).Broken, ui.Defer(""))
}

func (p *partsPage) Render(ctx context.Context, w io.Writer) error {
	return fullPage(ui.Group{
		ui.El("h1", nil, ui.Text("Apps")),
		ui.Part(p, (*partsPage).Stats, ui.Text("…"), func(n int) ui.Node { return ui.El("p", nil, ui.Textf("%d open", n)) }),
		ui.Part(p, (*partsPage).Recent, nil, func(rs []string) ui.Node { return ui.El("ul", nil, ui.Text(strings.Join(rs, ","))) }),
		ui.Part(p, (*partsPage).Broken, nil, func(s string) ui.Node { return ui.Text(s) }),
	}).Render(ctx, w)
}

func init() { streamInTests = true }

func partsApp(page func() *partsPage, opts ...Option) *App {
	a := New(append([]Option{Only("none")}, opts...)...)
	a.Get("/apps", func() *partsPage { return page() })
	return a
}

var partMarkRE = regexp.MustCompile(`<!--vk-part:([^>]+?)-->`)

// partIDs is the ids of the parts a page shows.
func partIDs(html string) []string {
	var ids []string
	for _, m := range partMarkRE.FindAllStringSubmatch(html, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// askParts is the runtime's request for the parts a page shows.
func askParts(t *testing.T, url string, ids []string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(PartsHeader, strings.Join(ids, " "))
	return req
}

// A page with deferred parts is sent whole, each part's fallback in its
// place and no method run; the runtime's request for its parts, the route
// rendering the page again, gets each part as it arrives: a failing one's
// message while another waits, a group's together.
func TestPartsAfterLoad(t *testing.T) {
	release := make(chan struct{})
	var made atomic.Int32
	srv := httptest.NewServer(partsApp(func() *partsPage { made.Add(1); return &partsPage{release: release} }).Handler())
	defer srv.Close()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(io.Discard)
	page := tbDo(t, partsApp(func() *partsPage { return &partsPage{release: release} }).Handler(), "GET", "/apps")
	ids := partIDs(page.body)
	if len(ids) != 3 || !strings.HasSuffix(page.body, "</body></html>") || strings.Contains(page.body, "<template") || strings.Contains(page.body, "__vkP") ||
		!strings.Contains(page.body, `<h1>Apps</h1><!--vk-part:`+ids[0]+`-->…<!--/vk-part:`+ids[0]+`-->`) || !strings.Contains(page.body, `<script src="/_vuka/live.js`) {
		t.Fatalf("the page: %s", page.body)
	}
	if !headerHasToken(page.header, "Vary", PartsHeader) {
		t.Errorf("the page doesn't vary by %s: %v", PartsHeader, page.header)
	}
	res, err := http.DefaultClient.Do(askParts(t, srv.URL+"/apps", ids))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	for k, want := range map[string]string{"X-Accel-Buffering": "no", "Cache-Control": "no-store", PartsHeader: "1", "Content-Type": "text/html; charset=utf-8"} {
		if got := res.Header.Get(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
	if made.Load() != 1 {
		t.Errorf("the parts' request made %d pages, want the route run again once", made.Load())
	}
	r := bufio.NewReader(res.Body)
	// Broken's part arrives while Stats waits; Recent waits with its group.
	broken := readUntil(t, r, `<!--vk-parts:`+ids[2]+`-->`)
	if want := `<template data-vk-part="` + ids[2] + `"><span role="alert">This part couldn&#39;t load.</span></template><!--vk-parts:` + ids[2] + `-->`; broken != want {
		t.Errorf("the failing part: %s", broken)
	}
	close(release)
	rest, _ := io.ReadAll(r)
	if want := `<template data-vk-part="` + ids[0] + `"><p>3 open</p></template><!--vk-parts:` + ids[0] + `-->` +
		`<template data-vk-part="` + ids[1] + `"><ul>a,b</ul></template><!--vk-parts:` + ids[1] + `-->`; string(rest) != want {
		t.Errorf("the group: %s", rest)
	}
	if !strings.Contains(logs.String(), "web: GET /apps: part partsPage.Broken: store: connection refused") {
		t.Errorf("the failing part isn't logged: %q", logs.String())
	}
}

func readUntil(t *testing.T, r *bufio.Reader, end string) string {
	t.Helper()
	var b strings.Builder
	done := make(chan error, 1)
	go func() {
		for !strings.HasSuffix(b.String(), end) {
			c, err := r.ReadByte()
			if err != nil {
				done <- err
				return
			}
			b.WriteByte(c)
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reading up to %q: %v; read %s", end, err, b.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no %q in 5s: the response is held back", end)
	}
	return b.String()
}

// parts is the reply to the runtime's request for the parts of the page
// at path, as h answers it.
func parts(t *testing.T, h http.Handler, path string, header ...string) answer {
	t.Helper()
	page := tbDo(t, h, "GET", path, header...)
	return tbDo(t, h, "GET", path, append([]string{PartsHeader, strings.Join(partIDs(page.body), " ")}, header...)...)
}

// A part's error fallback, a public message, the error's text in
// development; a navigation gets the page, its parts asked for after; a
// part the page no longer has fails.
func TestPartsErrorsAndNavigation(t *testing.T) {
	log.SetOutput(io.Discard)
	h := partsApp(func() *partsPage { return &partsPage{fail: BadRequest("no such shelf")} }).Handler()
	if res := parts(t, h, "/apps"); !strings.Contains(res.body, `<span role="alert">no such shelf</span></template>`) {
		t.Errorf("a public message: %s", res.body)
	}
	dev := partsApp(func() *partsPage { return &partsPage{} }, Dev(), NoToolbar()).Handler()
	if res := parts(t, dev, "/apps"); !strings.Contains(res.body, `<span role="alert">store: connection refused</span>`) {
		t.Errorf("development: %s", res.body)
	}
	app := partsApp(func() *partsPage { return &partsPage{} })
	h = app.Handler()
	res := tbDo(t, h, "GET", "/apps", NavigateHeader, app.buildID())
	if res.status != 200 || res.header.Get(navPageHeader) != "-" || strings.Contains(res.body, "<template") || len(partIDs(res.body)) != 3 {
		t.Errorf("a navigation: %d %v %s", res.status, res.header, res.body)
	}
	if res := tbDo(t, h, "HEAD", "/apps"); res.status != 200 || strings.Contains(res.body, "<template") {
		t.Errorf("HEAD: %d %s", res.status, res.body)
	}
	res = tbDo(t, h, "GET", "/apps", PartsHeader, "vk-p9-zz")
	if res.header.Get(PartsHeader) != "1" || res.body != `<template data-vk-part="vk-p9-zz"><span role="alert">This part couldn&#39;t load.</span></template><!--vk-parts:vk-p9-zz-->` {
		t.Errorf("a part the page doesn't have: %v %s", res.header, res.body)
	}
}

// shopPage shows a shop's figures, from its path, and a card for each of
// its products: components rendered with props, each with a part.
type shopPage struct{ asked *atomic.Int32 }

func (p *shopPage) Figures(ctx context.Context) (string, error) {
	if p.asked != nil {
		p.asked.Add(1)
	}
	return "figures of " + Request(ctx).PathValue("shop"), nil
}

type productCard struct{ SKU string }

func (c productCard) Stock(ctx context.Context) (int, error) { return len(c.SKU), nil }

func (c productCard) Render(ctx context.Context, w io.Writer) error {
	return ui.El("li", []ui.Attr{{Name: "key", Value: c.SKU}},
		ui.Part(c, productCard.Stock, nil, func(n int) ui.Node { return ui.Textf("%s: %d", c.SKU, n) })).Render(ctx, w)
}

func init() {
	vuka.RegisterFunc((*shopPage).Figures, ui.Defer(""))
	vuka.RegisterFunc(productCard.Stock, ui.Defer(""))
}

func (p *shopPage) Render(ctx context.Context, w io.Writer) error {
	return fullPage(ui.Group{
		ui.Part(p, (*shopPage).Figures, nil, func(s string) ui.Node { return ui.El("p", nil, ui.Text(s)) }),
		ui.El("ul", nil, productCard{"pen"}, productCard{"lamp"}),
	}).Render(ctx, w)
}

// The request for a page's parts runs the route as the page's did: its
// guard, its path; the parts of components come from the props the page
// gives them again, nothing of them taken from the browser.
func TestPartsAuthorized(t *testing.T) {
	log.SetOutput(io.Discard)
	var asked atomic.Int32
	app := New(Only("none"))
	app.Get("/shops/{shop}", func() *shopPage { return &shopPage{asked: &asked} }, Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c, err := r.Cookie("user"); err != nil || c.Value != "ann" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			h.ServeHTTP(w, r)
		})
	}))
	h := app.Handler()
	res := parts(t, h, "/shops/north", "Cookie", "user=ann")
	for _, want := range []string{`<p>figures of north</p>`, `>pen: 3</template>`, `>lamp: 4</template>`} {
		if !strings.Contains(res.body, want) {
			t.Errorf("the parts lack %s: %s", want, res.body)
		}
	}
	page := tbDo(t, h, "GET", "/shops/north", "Cookie", "user=ann")
	asked.Store(0)
	res = tbDo(t, h, "GET", "/shops/north", PartsHeader, strings.Join(partIDs(page.body), " "), "Cookie", "user=bob")
	if res.status != http.StatusForbidden || res.header.Get(PartsHeader) != "" || asked.Load() != 0 {
		t.Errorf("another visitor: %d %v, %d calls: %s", res.status, res.header, asked.Load(), res.body)
	}
	// Another shop's URL renders that shop's page: its parts are its own.
	res = tbDo(t, h, "GET", "/shops/south", PartsHeader, strings.Join(partIDs(page.body), " "), "Cookie", "user=ann")
	if !strings.Contains(res.body, "figures of south") || strings.Contains(res.body, "north") {
		t.Errorf("another URL: %s", res.body)
	}
}

// The client going away cancels the methods still running.
func TestPartsCanceled(t *testing.T) {
	var canceled atomic.Bool
	srv := httptest.NewServer(partsApp(func() *partsPage { return &partsPage{release: make(chan struct{}), canceled: &canceled} }).Handler())
	defer srv.Close()
	page := tbDo(t, partsApp(func() *partsPage { return &partsPage{} }).Handler(), "GET", "/apps")
	res, err := http.DefaultClient.Do(askParts(t, srv.URL+"/apps", partIDs(page.body)))
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, bufio.NewReader(res.Body), "<!--vk-parts:")
	res.Body.Close()
	for i := 0; i < 500 && !canceled.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !canceled.Load() {
		t.Error("Stats still runs after the client went away")
	}
}

// The parts are flushed through the compression as they arrive.
func TestPartsCompressed(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(partsApp(func() *partsPage { return &partsPage{release: release} }).Handler())
	defer srv.Close()
	page := tbDo(t, partsApp(func() *partsPage { return &partsPage{} }).Handler(), "GET", "/apps")
	req := askParts(t, srv.URL+"/apps", partIDs(page.body))
	req.Header.Set("Accept-Encoding", "br")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Encoding") != "br" {
		t.Fatalf("Content-Encoding: %q", res.Header.Get("Content-Encoding"))
	}
	readUntil(t, bufio.NewReader(brotli.NewReader(res.Body)), "<!--vk-parts:")
}

// A deferred method that can't be one fails the app's start.
type partsBadSig struct{}

func (p *partsBadSig) Count() int                                    { return 1 }
func (p *partsBadSig) Render(ctx context.Context, w io.Writer) error { return nil }

type partsTwice struct{}

func (p *partsTwice) Stats(ctx context.Context) (int, error)        { return 1, nil }
func (p *partsTwice) Render(ctx context.Context, w io.Writer) error { return nil }

type partsLive struct{ ui.Live }

func (p *partsLive) Stats(ctx context.Context) (int, error) { return 1, nil }
func (p *partsLive) Render() ui.Node                        { return ui.Text("x") }

func init() {
	vuka.RegisterFunc((*partsBadSig).Count, ui.Defer(""))
	vuka.RegisterFunc((*partsTwice).Stats, ui.Defer(""), ui.Defer("g"))
	vuka.RegisterFunc((*partsLive).Stats, ui.Defer(""))
}

func TestPartsMisregistered(t *testing.T) {
	for _, c := range []struct {
		route func(*App)
		want  string
	}{
		{func(a *App) { a.Get("/", func() *partsBadSig { return &partsBadSig{} }) }, "E0347"},
		{func(a *App) { a.Get("/", func() *partsTwice { return &partsTwice{} }) }, "E0345"},
		{func(a *App) { a.Live("/", &partsLive{}) }, "E0346"},
	} {
		a := New(Only("none"))
		c.route(a)
		if err := a.Build(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %s, got %v", c.want, err)
		}
	}
}

// Each deferred method is a span of its request in the toolbar: its time,
// its statements, its error.
func TestPartsToolbar(t *testing.T) {
	withToolbar(t)
	log.SetOutput(io.Discard)
	h := partsApp(func() *partsPage { return &partsPage{} }, Dev()).Handler()
	// The parts' request with the page's token: the methods are the page's.
	page := tbDo(t, h, "GET", "/apps")
	tok := partsTokRE.FindStringSubmatch(page.body)
	if tok == nil {
		t.Fatalf("no token: %s", page.body)
	}
	if res := tbDo(t, h, "GET", "/apps", PartsHeader, strings.Join(partIDs(page.body), " "), partsTokenHeader, tok[1]); !strings.Contains(res.body, "<p>3 open</p>") {
		t.Fatalf("the parts: %s", res.body)
	}
	// Without it: the parts' request renders the page again, its methods its own.
	again := parts(t, h, "/apps")
	for _, id := range []string{page.header.Get(ToolbarHeader), again.header.Get(ToolbarHeader)} {
		_, html := fetchRecord(t, h, id)
		for _, want := range []string{
			`<span class="kind">part</span> partsPage.Stats <span class="dim">· 1 SQL</span>`,
			`<span class="kind">part</span> partsPage.Recent<`,
			`<span class="kind">part</span> partsPage.Broken <span class="errtext">store: connection refused</span>`,
			`from <code>part partsPage.Stats</code>`,
		} {
			if !strings.Contains(html, want) {
				t.Errorf("the record %s has no %q", id, want)
			}
		}
	}
}

// partsHome links to the parts page.
type partsHome struct{}

func (p *partsHome) Render(ctx context.Context, w io.Writer) error {
	return fullPage(ui.Link(ui.LinkProps{Href: "/apps", ID: "apps", Children: ui.Text("apps")})).Render(ctx, w)
}
