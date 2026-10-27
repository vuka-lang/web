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
		ui.Part(p.Stats, ui.Text("…"), func(n int) ui.Node { return ui.El("p", nil, ui.Textf("%d open", n)) }),
		ui.Part(p.Recent, nil, func(rs []string) ui.Node { return ui.El("ul", nil, ui.Text(strings.Join(rs, ","))) }),
		ui.Part(p.Broken, nil, func(s string) ui.Node { return ui.Text(s) }),
	}).Render(ctx, w)
}

func init() { streamInTests = true }

func partsApp(page func() *partsPage, opts ...Option) *App {
	a := New(append([]Option{Only("none")}, opts...)...)
	a.Get("/apps", func() *partsPage { return page() })
	return a
}

// A page's deferred parts stream after it: the page up to </body> first,
// flushed before any deferred method returns, then each group's parts, an
// error's message in place of a failing one's, then </body></html>.
func TestPartsStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(partsApp(func() *partsPage { return &partsPage{release: release} }).Handler())
	defer srv.Close()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(io.Discard)
	res, err := http.Get(srv.URL + "/apps")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("X-Accel-Buffering: %q", res.Header.Get("X-Accel-Buffering"))
	}
	r := bufio.NewReader(res.Body)
	head := readUntil(t, r, ui.PartsScript)
	if !strings.Contains(head, `<h1>Apps</h1><!--vk-part:vk-p1-->…<!--/vk-part:vk-p1--><!--vk-part:vk-p2--><span aria-busy="true"></span><!--/vk-part:vk-p2-->`) ||
		strings.Contains(head, "</body>") || !strings.Contains(head, `<script src="/_vuka/live.js`) {
		t.Fatalf("the page before its parts: %s", head)
	}
	// Broken's part arrives while Stats waits; Recent waits with its group.
	broken := readUntil(t, r, `__vkP("vk-p3")</script>`)
	if want := `<template data-vk-part="vk-p3"><span role="alert">This part couldn&#39;t load.</span></template><script>__vkP("vk-p3")</script>`; broken != want {
		t.Errorf("the failing part: %s", broken)
	}
	close(release)
	rest, _ := io.ReadAll(r)
	if want := `<template data-vk-part="vk-p1"><p>3 open</p></template><script>__vkP("vk-p1")</script><template data-vk-part="vk-p2"><ul>a,b</ul></template><script>__vkP("vk-p2")</script></body></html>`; string(rest) != want {
		t.Errorf("the group, then the end: %s", rest)
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

// A part's error fallback, a public message, the error's text in
// development; a navigation gets the stream with no ETag.
func TestPartsErrorsAndNavigation(t *testing.T) {
	log.SetOutput(io.Discard)
	h := partsApp(func() *partsPage { return &partsPage{fail: BadRequest("no such shelf")} }).Handler()
	res := tbDo(t, h, "GET", "/apps")
	if !strings.Contains(res.body, `<template data-vk-part="vk-p3"><span role="alert">no such shelf</span></template>`) {
		t.Errorf("a public message: %s", res.body)
	}
	dev := partsApp(func() *partsPage { return &partsPage{} }, Dev(), NoToolbar()).Handler()
	if res := tbDo(t, dev, "GET", "/apps"); !strings.Contains(res.body, `<span role="alert">store: connection refused</span>`) {
		t.Errorf("development: %s", res.body)
	}
	app := partsApp(func() *partsPage { return &partsPage{} })
	h = app.Handler()
	res = tbDo(t, h, "GET", "/apps", NavigateHeader, app.buildID())
	if res.status != 200 || res.header.Get(navPageHeader) != "-" || res.header.Get("ETag") != "" || !strings.Contains(res.body, ui.PartsScript) || !strings.Contains(res.body, `<template data-vk-part="vk-p3">`) {
		t.Errorf("a navigation: %d %v %s", res.status, res.header, res.body)
	}
	if res := tbDo(t, h, "HEAD", "/apps"); res.status != 200 || strings.Contains(res.body, "<template") { // HEAD has no body: its parts never run
		t.Errorf("HEAD: %d %s", res.status, res.body)
	}
}

// The client going away cancels the methods still running.
func TestPartsCanceled(t *testing.T) {
	var canceled atomic.Bool
	srv := httptest.NewServer(partsApp(func() *partsPage { return &partsPage{release: make(chan struct{}), canceled: &canceled} }).Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/apps")
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, bufio.NewReader(res.Body), ui.PartsScript)
	res.Body.Close()
	for i := 0; i < 500 && !canceled.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !canceled.Load() {
		t.Error("Stats still runs after the client went away")
	}
}

// The page is flushed through the compression before its parts arrive.
func TestPartsCompressed(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(partsApp(func() *partsPage { return &partsPage{release: release} }).Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/apps", nil)
	req.Header.Set("Accept-Encoding", "br")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Encoding") != "br" {
		t.Fatalf("Content-Encoding: %q", res.Header.Get("Content-Encoding"))
	}
	readUntil(t, bufio.NewReader(brotli.NewReader(res.Body)), ui.PartsScript)
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
	res := tbDo(t, h, "GET", "/apps")
	_, html := fetchRecord(t, h, res.header.Get(ToolbarHeader))
	for _, want := range []string{
		`<span class="kind">part</span> partsPage.Stats <span class="dim">· 1 SQL</span>`,
		`<span class="kind">part</span> partsPage.Recent<`,
		`<span class="kind">part</span> partsPage.Broken <span class="errtext">store: connection refused</span>`,
		`from <code>part partsPage.Stats</code>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the record has no %q", want)
		}
	}
}

// partsHome links to the parts page.
type partsHome struct{}

func (p *partsHome) Render(ctx context.Context, w io.Writer) error {
	return fullPage(ui.Link(ui.LinkProps{Href: "/apps", ID: "apps", Children: ui.Text("apps")})).Render(ctx, w)
}
