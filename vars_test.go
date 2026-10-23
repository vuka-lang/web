package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/vuka/v2/diag"
)

type siteVars struct {
	SiteName string
	Theme    string
}

var siteRuns atomic.Int64

func siteContext(ctx context.Context) (siteVars, error) {
	siteRuns.Add(1)
	return siteVars{SiteName: "Shop", Theme: "dark"}, nil
}

type greeter struct{ hello string }

type greetVars struct{ Greeting string }

func greetContext(ctx context.Context, g *greeter) (greetVars, error) {
	return greetVars{Greeting: g.hello}, nil
}

// show writes what reading name gives, as compiled template code would.
func show[T any](name string) ui.Node {
	return ui.NodeFunc(func(ctx context.Context, w io.Writer) error {
		v, err := VarErr[T](ctx, name)
		if err != nil {
			_, err = fmt.Fprintf(w, "[%s: %v]", name, err)
			return err
		}
		_, err = fmt.Fprintf(w, "%s=%v;", name, v)
		return err
	})
}

func TestLowerCamel(t *testing.T) {
	for in, want := range map[string]string{"User": "user", "CSRFToken": "csrfToken", "ID": "id", "Me": "me", "HTTPServer": "httpServer", "URL": "url", "SiteName": "siteName", "X": "x"} {
		if got := lowerCamel(in); got != want {
			t.Errorf("lowerCamel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestProcessorVars: a page reads processors' variables by name; a
// processor runs once per request whatever the reads, and not at all when
// nothing reads it; its dependencies come from the container.
func TestProcessorVars(t *testing.T) {
	siteRuns.Store(0)
	app := New(Only("none"), ContextProcessors(siteContext, greetContext))
	app.Provide(&greeter{hello: "hi"})
	app.Get("/", func() ui.Node {
		return ui.El("p", nil, show[string]("siteName"), show[string]("theme"), show[string]("siteName"), show[string]("greeting"))
	})
	app.Get("/none", func() ui.Node { return ui.Text("nothing") })
	h := app.Handler()
	_, body, _ := do(t, h, "GET", "/", "")
	if !strings.Contains(body, "siteName=Shop;theme=dark;siteName=Shop;greeting=hi;") {
		t.Errorf("body: %s", body)
	}
	if n := siteRuns.Load(); n != 1 {
		t.Errorf("siteContext ran %d times, want once", n)
	}
	do(t, h, "GET", "/", "")
	if n := siteRuns.Load(); n != 2 {
		t.Errorf("a second request: %d runs, want 2", n)
	}
	do(t, h, "GET", "/none", "")
	if n := siteRuns.Load(); n != 2 {
		t.Errorf("a page reading nothing ran the processor: %d", n)
	}
}

// TestVarErrors: a wrong type, a typo and a context of no app say so.
func TestVarErrors(t *testing.T) {
	app := New(Only("none"), ContextProcessors(siteContext))
	app.Get("/", func() ui.Node {
		return ui.El("p", nil, show[int]("siteName"), show[string]("siteNme"), show[string]("missing"))
	})
	_, body, _ := do(t, app.Handler(), "GET", "/", "")
	for _, want := range []string{
		`the template variable "siteName" is a string (from web.siteContext), read as a int`,
		`no template variable "siteNme"; did you mean "siteName"?`,
		`no template variable "missing": no processor in web.ContextProcessors gives one (siteName, theme)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if _, err := VarErr[string](context.Background(), "siteName"); err == nil || !strings.Contains(err.Error(), "web.ContextProcessors") {
		t.Errorf("no app: %v", err)
	}
	if v := Var[string](context.Background(), "siteName"); v != "" {
		t.Errorf("Var without an app: %q", v)
	}
}

type slowVars struct{ Slow int }

var slowRuns atomic.Int64

func slowContext(ctx context.Context) (slowVars, error) {
	slowRuns.Add(1)
	return slowVars{Slow: 7}, nil
}

// TestConcurrentReadsShareOneRun: reads at once wait for the one run.
func TestConcurrentReadsShareOneRun(t *testing.T) {
	slowRuns.Store(0)
	app := New(Only("none"), ContextProcessors(slowContext))
	var wg sync.WaitGroup
	app.Get("/", func(ctx context.Context) ui.Node {
		var bad atomic.Int64
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if Var[int](ctx, "slow") != 7 {
					bad.Add(1)
				}
			}()
		}
		wg.Wait()
		return ui.Text(fmt.Sprint("bad=", bad.Load()))
	})
	_, body, _ := do(t, app.Handler(), "GET", "/", "")
	if body != "bad=0" || slowRuns.Load() != 1 {
		t.Errorf("body %q, runs %d", body, slowRuns.Load())
	}
}

// TestWithVars: an override replaces a processor's result below it.
func TestWithVars(t *testing.T) {
	siteRuns.Store(0)
	app := New(Only("none"), ContextProcessors(siteContext))
	app.Get("/", func(ctx context.Context) ui.Node {
		return ui.El("p", nil, show[string]("siteName"), ui.NodeFunc(func(ctx context.Context, w io.Writer) error {
			return show[string]("siteName").Render(WithVars(ctx, siteVars{SiteName: "Other"}), w)
		}))
	})
	_, body, _ := do(t, app.Handler(), "GET", "/", "")
	if !strings.Contains(body, "siteName=Shop;siteName=Other;") {
		t.Errorf("body: %s", body)
	}
	// with no app at all: the override's fields
	ctx := WithVars(context.Background(), &siteVars{Theme: "light"})
	if v, err := VarErr[string](ctx, "theme"); v != "light" || err != nil {
		t.Errorf("override alone: %q, %v", v, err)
	}
	if _, err := VarErr[int](ctx, "theme"); err == nil {
		t.Error("an override read as another type")
	}
	defer func() {
		if recover() == nil {
			t.Error("WithVars of a non-struct didn't panic")
		}
	}()
	WithVars(ctx, 3)
}

type badVars struct{ Name string }

func noCtxProc() (badVars, error)                           { return badVars{}, nil }
func noErrProc(context.Context) badVars                     { return badVars{} }
func notStructProc(context.Context) (string, error)         { return "", nil }
func clashProc(context.Context) (siteVars2, error)          { return siteVars2{}, nil }
func missingDep(context.Context, *greeter) (badVars, error) { return badVars{}, nil }

type siteVars2 struct{ SiteName string }

// TestProcessorBootChecks: Build reports each processor of the wrong
// shape, two giving one variable (naming both), a dependency nothing
// provides and a wrapper that means nothing.
func TestProcessorBootChecks(t *testing.T) {
	for _, c := range []struct {
		name  string
		procs []any
		code  diag.Code
		want  string
	}{
		{"not a function", []any{3}, "E0425", "ContextProcessors' 1st processor, int, isn't a function"},
		{"no context", []any{noCtxProc}, "E0425", "web.noCtxProc doesn't take a context.Context first"},
		{"no error", []any{noErrProc}, "E0425", "web.noErrProc doesn't return (S, error)"},
		{"not a struct", []any{notStructProc}, "E0425", "web.notStructProc returns string, not a struct"},
		{"clash", []any{siteContext, clashProc}, "E0421", `the template variable "siteName" is given by web.siteContext and by web.clashProc`},
		{"twice", []any{siteContext, siteContext}, "E0421", "context processors web.siteContext and web.siteContext both return web.siteVars"},
		{"missing dep", []any{missingDep}, "E0407", "the context processor web.missingDep's 2nd parameter, *web.greeter, is provided by nothing"},
		{"personal alone", []any{Personal(siteContext)}, "E0425", "web.Personal(web.siteContext) outside web.Browser"},
		{"auth without gates", []any{Auth(siteContext)}, "E0425", "web.Auth(web.siteContext) has no gate"},
		{"auth without guard", []any{Auth(siteContext, "gate")}, "E0404", "web.Auth(web.siteContext): the app has nothing guarding variables"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := New(Only("none"), ContextProcessors(c.procs...)).Build()
			if err == nil || !strings.Contains(err.Error(), c.want) || diag.Of(err) != c.code {
				t.Errorf("Build: %v; want %s %q", err, c.code, c.want)
			}
		})
	}
	err := New(Only("none"), ContextProcessors(missingDep)).Build()
	if ds, _ := diag.From(err); len(ds) != 1 || !strings.HasSuffix(ds[0].Span.File, "vars_test.go") || ds[0].Span.Line == 0 {
		t.Errorf("missing dep's span: %+v", ds)
	}
}

// TestBrowserRefusesPersonal: a Browser processor of a package that knows
// the visitor is refused unless Personal.
func TestBrowserRefusesPersonal(t *testing.T) {
	err := New(Only("none"), PersonalPackage("github.com/vuka-lang/web/v2"), ContextProcessors(Browser(siteContext))).Build()
	if err == nil || !strings.Contains(err.Error(), "web.Browser(web.siteContext) would send what github.com/vuka-lang/web/v2 knows of the visitor to the browser") {
		t.Errorf("Build: %v", err)
	}
	if err := New(Only("none"), PersonalPackage("github.com/vuka-lang/web/v2"), ContextProcessors(Browser(Personal(siteContext)))).Build(); err != nil {
		t.Errorf("Personal: %v", err)
	}
}

type secretList []string

func (secretList) NotForBrowser() {}

type shopVars struct {
	Currency string
	Secret   secretList
}

func shopContext(context.Context) (shopVars, error) {
	return shopVars{Currency: "TZS", Secret: secretList{"x"}}, nil
}

// TestBrowserVars: an island carries the Browser processors' variables,
// never another's, nor a NotForBrowser value.
func TestBrowserVars(t *testing.T) {
	app := New(Only("none"), ContextProcessors(Browser(shopContext), siteContext))
	if err := app.Build(); err != nil {
		t.Fatal(err)
	}
	rq := &request{app: app, r: httptest.NewRequest("GET", "/", nil)}
	rq.ctx = context.WithValue(context.Background(), ctxKey{}, rq)
	app.withVars(rq)
	got := browserVars(rq.ctx).(map[string]any)
	if len(got) != 1 || got["currency"] != "TZS" {
		t.Errorf("browserVars = %v", got)
	}
}

// RequestContext gives the request, and its input is the URL.
func TestRequestContext(t *testing.T) {
	app := New(Only("none"), ContextProcessors(RequestContext))
	app.Get("/users/{id}", func(ctx context.Context) ui.Node {
		r := Var[RequestInfo](ctx, "request")
		return ui.Text(fmt.Sprintf("%s %s %v %s %q", r.Path, r.Params["id"], r.Query["tab"], r.Pattern, r.Name))
	})
	_, body, _ := do(t, app.Handler(), "GET", "/users/42?tab=perms", "")
	if !strings.Contains(body, "/users/42 42 [perms] GET /users/{id}") {
		t.Errorf("body: %s", body)
	}
}

var navLoads atomic.Int64

// pathShell stays across navigations and shows the request's path, read
// from the variable in its markup.
type pathShell struct{ Children ui.Node }

func (l *pathShell) Load(ctx context.Context) error { navLoads.Add(1); return nil }
func (l *pathShell) Render(ctx context.Context, w io.Writer) error {
	return ui.El("div", nil, ui.El("p", nil, ui.Text("at "+Var[RequestInfo](ctx, "request").Path)), l.Children).Render(ctx, w)
}

// TestLayoutShowsRequestAndStays: a layout reading request.Path stays on a
// navigation (its Load doesn't run again), and its spot shows the new path.
func TestLayoutShowsRequestAndStays(t *testing.T) {
	navLoads.Store(0)
	app := New(Only("none"), ContextProcessors(RequestContext))
	shell := LayoutOf[pathShell]()
	app.Live("/a", func() *titledPage { return &titledPage{title: "a", text: "page a"} }, shell)
	app.Live("/b", func() *titledPage { return &titledPage{title: "b", text: "page b"} }, shell)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	c, html := joined(t, srv, "/a")
	defer c.CloseNow()
	if !strings.Contains(html, "at /a") {
		t.Fatalf("join: %s", html)
	}
	loads := navLoads.Load()
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/b"})
	m := must(t, c)
	if m.Type != live.PatchMsg || len(m.Updates) == 0 || !strings.Contains(m.Updates[0].HTML, "at /b") || !strings.Contains(m.Updates[0].HTML, "page b") {
		t.Fatalf("navigate: %+v", m)
	}
	if n := navLoads.Load(); n != loads {
		t.Errorf("the layout loaded again: %d, want %d", n, loads)
	}
}

// tick is an input a test moves.
var tick atomic.Int64

type tickVars struct{ Tick int64 }

func tickContext(ctx context.Context) (tickVars, error) {
	RecordInput(ctx, Input{Key: "tick", Now: func(context.Context) string { return fmt.Sprint(tick.Load()) }})
	return tickVars{Tick: tick.Load()}, nil
}

var tickRenders, otherRenders atomic.Int64

// tickShow reads tick in its markup; state all Assigns (none).
type tickShow struct{ ui.Live }

func (s *tickShow) Render() ui.Node {
	tickRenders.Add(1)
	return ui.El("p", nil, show[int64]("tick"))
}

// otherShow reads siteName.
type otherShow struct{ ui.Live }

func (s *otherShow) Render() ui.Node {
	otherRenders.Add(1)
	return ui.El("p", nil, show[string]("siteName"))
}

type tickPage struct{ ui.Live }

func (p *tickPage) Poke() {}
func (p *tickPage) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Poke)}}, ui.Text("poke")),
		ui.Component("tick", nil, &tickShow{}),
		ui.Component("other", nil, &otherShow{}))
}

// TestLiveSpotRerender: when a processor's input moves, a live event
// renders again only the components that read its variables.
func TestLiveSpotRerender(t *testing.T) {
	defer func(v bool) { live.Verify = v }(live.Verify)
	live.Verify = false
	tick.Store(1)
	app := New(Only("none"), ContextProcessors(tickContext, siteContext))
	app.Live("/", func() *tickPage { return &tickPage{} })
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	c, html := joined(t, srv, "/")
	defer c.CloseNow()
	if !strings.Contains(html, "tick=1;") {
		t.Fatalf("join: %s", html)
	}
	poke := handlerOf(t, html, "click", 0)
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": poke, "event": "click"})
	if m := must(t, c); len(m.Updates) != 0 {
		t.Errorf("nothing moved, yet updates: %+v", m.Updates)
	}
	ticks, others := tickRenders.Load(), otherRenders.Load()
	tick.Store(2)
	send(t, c, map[string]any{"type": "event", "ref": 3, "target": poke, "event": "click"})
	m := must(t, c)
	if len(m.Updates) != 1 || !strings.Contains(m.Updates[0].HTML, "tick=2;") {
		t.Fatalf("tick moved: %+v", m)
	}
	if tickRenders.Load() != ticks+1 || otherRenders.Load() != others {
		t.Errorf("renders: tick %d→%d, other %d→%d; want only tick's", ticks, tickRenders.Load(), others, otherRenders.Load())
	}
}

type deniedVars struct{ Hidden string }

func deniedContext(context.Context) (deniedVars, error) { return deniedVars{Hidden: "x"}, nil }

// TestAuthGatedVars: a read a gate refuses answers the page with the
// guard's error, a redirect here.
func TestAuthGatedVars(t *testing.T) {
	guard := GuardVars(func(ctx context.Context, gates []any) error {
		if len(gates) == 1 && gates[0] == "signed-in" {
			return Redirect("/login")
		}
		return errors.New("unknown gate")
	})
	app := New(Only("none"), guard, ContextProcessors(Auth(deniedContext, "signed-in")))
	app.Get("/", func() ui.Node { return ui.El("p", nil, show[string]("hidden")) })
	code, _, h := do(t, app.Handler(), "GET", "/", "", "Accept", "text/html")
	if code != http.StatusSeeOther && code != http.StatusFound || h.Get("Location") != "/login" {
		t.Errorf("GET: %d %v", code, h)
	}
}

// TestFlashMessages: a message flashed before a redirect shows on the next
// page, once.
func TestFlashMessages(t *testing.T) {
	app := New(Only("none"), ContextProcessors(MessagesContext))
	app.Post("/save", func(ctx context.Context) ui.Node {
		if err := Flash(ctx, "success", "Saved"); err != nil {
			t.Error(err)
		}
		return Redirect("/")
	})
	app.Get("/", func(ctx context.Context) ui.Node {
		var b strings.Builder
		for _, m := range Var[[]Message](ctx, "messages") {
			b.WriteString(m.Level + ":" + m.Text + ";")
		}
		return ui.Text("messages " + b.String())
	})
	h := app.Handler()
	_, _, hdr := do(t, h, "POST", "/save", "")
	cookie := ""
	for _, c := range (&http.Response{Header: hdr}).Cookies() {
		if c.Name == messagesCookie {
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatalf("no messages cookie: %v", hdr)
	}
	_, body, hdr := do(t, h, "GET", "/", "", "Cookie", cookie)
	if body != "messages success:Saved;" {
		t.Errorf("next page: %q", body)
	}
	if !strings.Contains(hdr.Get("Set-Cookie"), messagesCookie+"=;") {
		t.Errorf("not cleared: %v", hdr)
	}
	if _, body, _ := do(t, h, "GET", "/", "", "Cookie", messagesCookie+"=forged.sig"); body != "messages " {
		t.Errorf("a forged cookie: %q", body)
	}
}

// TestCSRFContext: the token is the browser's, from the cookie it sets.
func TestCSRFContext(t *testing.T) {
	app := New(Only("none"), ContextProcessors(CSRFContext))
	app.Get("/", func(ctx context.Context) ui.Node { return ui.Text(Var[string](ctx, "csrfToken")) })
	app.Post("/check", func(ctx context.Context, f struct {
		Token string `form:"token"`
	}) ui.Node {
		return ui.Text(fmt.Sprint(ValidCSRFToken(ctx, f.Token)))
	})
	h := app.Handler()
	_, tok, hdr := do(t, h, "GET", "/", "")
	set := (&http.Response{Header: hdr}).Cookies()
	if tok == "" || len(set) != 1 || set[0].Name != csrfCookie {
		t.Fatalf("token %q, cookies %v", tok, set)
	}
	cookie := csrfCookie + "=" + set[0].Value
	if _, again, _ := do(t, h, "GET", "/", "", "Cookie", cookie); again != tok {
		t.Errorf("another token for the same browser: %q, %q", again, tok)
	}
	form := "token=" + tok
	ct := "application/x-www-form-urlencoded"
	if _, ok, _ := do(t, h, "POST", "/check", form, "Content-Type", ct, "Cookie", cookie); ok != "true" {
		t.Errorf("valid token: %q", ok)
	}
	if _, ok, _ := do(t, h, "POST", "/check", form, "Content-Type", ct, "Cookie", csrfCookie+"=another-browser-cookie-value-x"); ok != "false" {
		t.Errorf("another browser's token: %q", ok)
	}
}

// TestLiveFlash: a message flashed by a live event shows in its reply,
// and goes at the next event.
func TestLiveFlash(t *testing.T) {
	app := New(Only("none"), ContextProcessors(MessagesContext))
	app.Live("/", func() *flashPage { return &flashPage{} })
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	c, html := joined(t, srv, "/")
	defer c.CloseNow()
	save := handlerOf(t, html, "click", 0)
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": save, "event": "click"})
	m := must(t, c)
	if len(m.Updates) == 0 || !strings.Contains(m.Updates[0].HTML, "info:Saved;") {
		t.Fatalf("flash: %+v", m)
	}
	noop := handlerOf(t, html, "click", 1)
	send(t, c, map[string]any{"type": "event", "ref": 3, "target": noop, "event": "click"})
	m = must(t, c)
	if len(m.Updates) == 0 || strings.Contains(m.Updates[0].HTML, "Saved") {
		t.Errorf("still shown: %+v", m)
	}
}

type flashPage struct{ ui.Live }

func (p *flashPage) Save(ctx context.Context) error { return Flash(ctx, "info", "Saved") }
func (p *flashPage) Noop()                          {}
func (p *flashPage) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Save)}}, ui.Text("save")),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Noop)}}, ui.Text("noop")),
		ui.NodeFunc(func(ctx context.Context, w io.Writer) error {
			io.WriteString(w, "messages ")
			for _, m := range Var[[]Message](ctx, "messages") {
				io.WriteString(w, m.Level+":"+m.Text+";")
			}
			return nil
		}))
}

type gatedPage struct{ ui.Live }

func (p *gatedPage) Poke() {}
func (p *gatedPage) Render() ui.Node {
	return ui.El("main", nil, ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Poke)}}, ui.Text("poke")), show[string]("hidden"))
}

// TestAuthGatedVarsLive: a live page whose read a gate refuses closes, to
// load again, its route answering the browser.
func TestAuthGatedVarsLive(t *testing.T) {
	var deny atomic.Bool
	guard := GuardVars(func(ctx context.Context, gates []any) error {
		if deny.Load() {
			return Unauthorized
		}
		return nil
	})
	app := New(Only("none"), guard, ContextProcessors(Auth(deniedContext, "signed-in")))
	app.Live("/", func() *gatedPage { return &gatedPage{} })
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	c, html := joined(t, srv, "/")
	defer c.CloseNow()
	if !strings.Contains(html, "hidden=x;") {
		t.Fatalf("join: %s", html)
	}
	deny.Store(true)
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", 0), "event": "click"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != closeReload {
		t.Errorf("after a refused read: %v", err)
	}
}
