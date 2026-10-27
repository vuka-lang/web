package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
)

var navShellLoads atomic.Int64

// navShell is a root layout: a header with a search field, the page below.
type navShell struct{ Children ui.Node }

func (l *navShell) Load(ctx context.Context) error { navShellLoads.Add(1); return nil }
func (l *navShell) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil,
		ui.El("head", nil, ui.El("title", nil, ui.Text(ui.PageTitle(ctx)))),
		ui.El("body", nil,
			ui.El("header", nil, ui.El("input", attrs("id", "q")),
				ui.Link(ui.LinkProps{Href: "/nav/a", Children: ui.Text("a")}),
				ui.Link(ui.LinkProps{Href: "/nav/b", Children: ui.Text("b")}),
				ui.Link(ui.LinkProps{Href: "/nav/live", Children: ui.Text("live")})),
			ui.El("main", nil, l.Children))).Render(ctx, w)
}

// navOther is another root layout: another document.
type navOther struct{ Children ui.Node }

func (l *navOther) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("body", nil, l.Children)).Render(ctx, w)
}

func navApp(opts ...Option) *App {
	navShellLoads.Store(0)
	meterMounts.Store(0)
	app := New(append([]Option{Only("none")}, opts...)...)
	shell := LayoutOf[navShell]()
	text := func(s string) func() ui.Node {
		return func() ui.Node { return ui.El("p", attrs("id", "at"), ui.Text(s)) }
	}
	app.Get("/nav/a", text("a"), shell)
	app.Get("/nav/b", text("b"), shell)
	app.Get("/nav/full", text("full"), shell, FullLoad())
	app.Get("/nav/other", text("other"), LayoutOf[navOther]())
	app.Get("/nav/script", func() ui.Node {
		return ui.Safe(`<div><script>window.ran = 1</script></div>`)
	}, shell)
	app.Get("/nav/theme", func() ui.Node {
		return ui.Safe("<div>themed<script\n  src=\"/static/theme.js\"   defer></script></div>")
	}, shell)
	app.Get("/nav/data", func() ui.Node {
		return ui.Safe(`<div><script type="application/json">{"a":1}</script>data</div>`)
	}, shell)
	app.Get("/nav/missing", func() (ui.Node, error) { return nil, NotFound }, shell)
	app.Get("/nav/json", func() map[string]int { return map[string]int{"a": 1} })
	app.Live("/nav/live", &meter{}, shell)
	return app
}

var buildRe = regexp.MustCompile(`data-vk-build-id="([^"]+)"`)

func navGet(t *testing.T, srv *httptest.Server, path string, header ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestNavigationOverHTTP(t *testing.T) {
	srv := httptest.NewServer(navApp().Handler())
	defer srv.Close()

	// An ordinary GET: the page, as ever, its runtime naming the build.
	res, page := navGet(t, srv, "/nav/a")
	m := buildRe.FindStringSubmatch(page)
	if m == nil || res.Header.Get(navPageHeader) != "" || strings.Contains(res.Header.Get("Vary"), NavigateHeader) || res.Header.Get("ETag") != "" {
		t.Fatalf("an ordinary GET: %v\n%s", res.Header, page)
	}
	build := m[1]
	if !strings.Contains(page, ` data-vk-root="`+rootID(typeOf[navShell]())+`"`) {
		t.Errorf("the script names no root layout: %s", page)
	}

	// A navigation: the page, marked; every layout's Load runs.
	loads := navShellLoads.Load()
	res, body := navGet(t, srv, "/nav/b", NavigateHeader, build)
	h := res.Header
	if res.StatusCode != 200 || h.Get(navPageHeader) != rootID(typeOf[navShell]()) || !strings.Contains(h.Get("Vary"), NavigateHeader) ||
		h.Get("Cache-Control") != "private" || h.Get("ETag") == "" || h.Get(navLiveHeader) != "" || !strings.Contains(body, `<p id="at">b</p>`) {
		t.Fatalf("a navigation: %d %v\n%s", res.StatusCode, h, body)
	}
	if n := navShellLoads.Load(); n != loads+1 {
		t.Errorf("the layout loaded %d times, want once more", n-loads)
	}
	res, body = navGet(t, srv, "/nav/b", NavigateHeader, build, "If-None-Match", "W/"+h.Get("ETag"))
	if res.StatusCode != http.StatusNotModified || body != "" {
		t.Errorf("the same page again: %d %q", res.StatusCode, body)
	}

	reloads := []struct{ path, build, why string }{
		{"/nav/a", "another", "build"},
		{"/nav/full", build, "route"},
		{"/nav/script", build, "scripts"},
	}
	for _, c := range reloads {
		res, body := navGet(t, srv, c.path, NavigateHeader, c.build)
		if res.StatusCode != http.StatusNoContent || res.Header.Get(navReload) != c.why || res.Header.Get("Cache-Control") != "no-store" || body != "" || len(res.Header.Values("Vary")) != 1 {
			t.Errorf("%s: %d %v %q, want 204 Vk-Reload %s", c.path, res.StatusCode, res.Header, body, c.why)
		}
	}
	if res, _ := navGet(t, srv, "/nav/full"); res.StatusCode != 200 {
		t.Errorf("FullLoad's ordinary GET: %d", res.StatusCode)
	}

	// A data block isn't a script; another root layout is named; an
	// error page is one under none; JSON is no page.
	if res, body := navGet(t, srv, "/nav/data", NavigateHeader, build); res.Header.Get(navPageHeader) == "" || !strings.Contains(body, "data") {
		t.Errorf("a data block: %v", res.Header)
	}
	if res, _ := navGet(t, srv, "/nav/other", NavigateHeader, build); res.Header.Get(navPageHeader) != rootID(typeOf[navOther]()) {
		t.Errorf("another root layout: %v", res.Header)
	}
	if res, _ := navGet(t, srv, "/nav/missing", NavigateHeader, build); res.StatusCode != 404 || res.Header.Get(navPageHeader) != "-" {
		t.Errorf("an error page: %d %v", res.StatusCode, res.Header)
	}
	if res, _ := navGet(t, srv, "/nav/json", NavigateHeader, build); res.Header.Get(navPageHeader) != "" || !strings.Contains(res.Header.Get("Vary"), NavigateHeader) {
		t.Errorf("JSON: %v", res.Header)
	}
}

// A navigation to a live page carries its join token, and the join adopts
// the session the navigation's render parked: Mount runs once.
func TestNavigationToALivePage(t *testing.T) {
	srv := httptest.NewServer(navApp().Handler())
	defer srv.Close()
	_, page := navGet(t, srv, "/nav/a")
	build := buildRe.FindStringSubmatch(page)[1]
	res, body := navGet(t, srv, "/nav/live", NavigateHeader, build)
	tok := res.Header.Get(navLiveHeader)
	if tok == "" || res.Header.Get("Cache-Control") != "private, no-store" || res.Header.Get("ETag") != "" || !strings.Contains(body, "m1 ") {
		t.Fatalf("a live page: %v\n%s", res.Header, body)
	}
	c, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	send(t, c, map[string]any{"type": "join", "ref": 1})
	m := must(t, c)
	if m.Type != live.Render || !strings.Contains(m.HTML, "m1 ") {
		t.Fatalf("the join mounted again: %+v", m)
	}
}

// In development a navigation that loads in full says why: the app logs it
// once per cause, the reply says it (Vk-Reload-Detail), the toolbar's record
// of the navigation shows it. A production app says nothing.
func TestNavigationFullLoadWhy(t *testing.T) {
	withToolbar(t)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	for _, dev := range []bool{true, false} {
		logs.Reset()
		var opts []Option
		if dev {
			opts = append(opts, Dev())
		}
		h := navApp(opts...).Handler()
		page := tbDo(t, h, "GET", "/nav/a").body
		build := buildRe.FindStringSubmatch(page)[1]
		if strings.Contains(page, " data-vk-dev") != dev {
			t.Errorf("dev %v: the runtime's tag: %s", dev, page)
		}
		const want = `its body runs a script of its own: <script src="/static/theme.js" defer>; move it into <head>, make it a component script, or mark the link reload`
		var id string
		for range 2 {
			a := tbDo(t, h, "GET", "/nav/theme", NavigateHeader, build)
			if a.status != http.StatusNoContent || a.header.Get(navReload) != "scripts" {
				t.Fatalf("dev %v: %d %v", dev, a.status, a.header)
			}
			if got := a.header.Get(navReloadWhy); dev && got != want || !dev && got != "" {
				t.Errorf("dev %v: Vk-Reload-Detail %q", dev, got)
			}
			id = a.header.Get(ToolbarHeader)
		}
		tbDo(t, h, "GET", "/nav/full", NavigateHeader, build)
		got := logs.String()
		if !dev {
			if got != "" {
				t.Errorf("production logged: %s", got)
			}
			continue
		}
		if n := strings.Count(got, "loads in full"); n != 2 || strings.Count(got, "/nav/theme loads in full: "+strings.ReplaceAll(want, `"`, `\"`)) != 1 ||
			!strings.Contains(got, "level=WARN") || !strings.Contains(got, "its route GET /nav/full is web.FullLoad") {
			t.Errorf("the log, want the script once and the route:\n%s", got)
		}
		if _, html := fetchRecord(t, h, id); !strings.Contains(html, "<h4>Navigation</h4>") || !strings.Contains(html, "full load: scripts") || !strings.Contains(html, "/static/theme.js") {
			t.Errorf("the toolbar's record: %s", html)
		}
	}
}

func TestPlainScripts(t *testing.T) {
	for html, want := range map[string]bool{
		`<html><head><script src="/x.js"></script></head><body><p>x</p></body></html>`:   false,
		`<html><body><script>alert(1)</script></body></html>`:                            true,
		`<body><script type="module" src="/m.js"></script></body>`:                       true,
		`<body><script type='text/javascript'>x</script></body>`:                         true,
		`<body><script type="application/json">{}</script></body>`:                       false,
		`<body><script type="application/ld+json">{}</script><scripts></scripts></body>`: false,
		`<p>no body tag</p><SCRIPT>x</SCRIPT>`:                                           true,
		`<p>none</p>`:                                                                    false,
	} {
		if got := plainScripts([]byte(html)) != nil; got != want {
			t.Errorf("plainScripts(%s) = %v, want %v", html, got, want)
		}
	}
}
