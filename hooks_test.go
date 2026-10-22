package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2"
)

type role string

func (role) RouteWrapper() string { return "roles" }

func TestWrapRoutes(t *testing.T) {
	var seen []RouteInfo
	tag := func(name string) func(RouteInfo) Middleware {
		return func(info RouteInfo) Middleware {
			if name == "outer" {
				seen = append(seen, info)
			}
			return func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("X-Order", name)
					for _, a := range info.Attrs {
						if a == role("admin") && r.Header.Get("X-Role") != "admin" {
							w.WriteHeader(http.StatusForbidden)
							return
						}
					}
					next.ServeHTTP(w, r)
				})
			}
		}
	}
	a := New(Only("none"), WrapRoutes("roles", tag("outer")), WrapRoutes("other", tag("inner")))
	a.Get("/open", func() string { return "open" })
	a.Get("/admin", func() string { return "admin" }, Attrs(role("admin"), vuka.Compose(role("x"))), Wrap(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Add("X-Order", "route"); next.ServeHTTP(w, r) })
	}))
	a.Live("/counter", &counter{}, Attrs(role("admin")))
	h := a.Handler()
	code, _, hdr := do(t, h, "GET", "/admin", "", "X-Role", "admin")
	if code != 200 || strings.Join(hdr.Values("X-Order"), " ") != "outer inner route" {
		t.Errorf("order: %d %v", code, hdr.Values("X-Order"))
	}
	if code, _, _ := do(t, h, "GET", "/admin", ""); code != 403 {
		t.Errorf("refused: %d", code)
	}
	if code, _, _ := do(t, h, "GET", "/counter", ""); code != 403 {
		t.Errorf("a live page's attributes: %d", code)
	}
	if len(seen) != 3 {
		t.Fatalf("routes seen: %+v", seen)
	}
	for _, info := range seen {
		switch info.Path {
		case "/admin":
			if len(info.Attrs) != 2 || info.Attrs[1] != role("x") || info.HTML || info.Live {
				t.Errorf("/admin: %+v", info)
			}
		case "/counter":
			if !info.Live || !info.HTML || len(info.Attrs) != 1 {
				t.Errorf("/counter: %+v", info)
			}
		}
	}

	// A RouteAttr without its WrapRoutes fails Build.
	b := New(Only("none"))
	b.Get("/admin", func() string { return "" }, Attrs(role("admin")))
	if err := b.Build(); err == nil || !strings.Contains(err.Error(), "its attribute web.role needs roles") {
		t.Errorf("no wrapper: %v", err)
	}
}

func TestParamFromAndImplementation(t *testing.T) {
	type who struct{ Name string }
	a := New(Only("none"), ParamFrom(func(r *http.Request) (who, error) {
		if n := r.Header.Get("X-Who"); n != "" {
			return who{n}, nil
		}
		return who{}, Unauthorized
	}), Provide(NewStore))
	a.Get("/who", func(w who) string { return w.Name })
	h := a.Handler()
	if _, body, _ := do(t, h, "GET", "/who", "", "X-Who", "ada"); body != `"ada"`+"\n" {
		t.Errorf("bound: %s", body)
	}
	if code, _, _ := do(t, h, "GET", "/who", ""); code != 401 {
		t.Errorf("the binder's error: %d", code)
	}
	type finder interface{ Find(int) vuka.Result[Pet] }
	if s, err := Implementation[finder](a); err != nil || s == nil {
		t.Errorf("Implementation: %v", err)
	}
	if _, err := Implementation[interface{ Nope() }](a); err == nil {
		t.Error("nothing implements Nope")
	}
}

func TestCSRF(t *testing.T) {
	a := New(Only("none"), CSRF("https://trusted.example"))
	a.Post("/do", func() string { return "done" })
	a.Get("/do", func() string { return "read" })
	h := a.Handler()
	for _, c := range []struct {
		method string
		header []string
		want   int
	}{
		{"POST", []string{"Sec-Fetch-Site", "cross-site"}, 403},
		{"POST", []string{"Origin", "https://evil.example"}, 403},
		{"POST", []string{"Origin", "https://trusted.example"}, 200},
		{"POST", []string{"Sec-Fetch-Site", "same-origin"}, 200},
		{"POST", nil, 200},
		{"POST", []string{"Sec-Fetch-Site", "cross-site", "Authorization", "Bearer x"}, 200},
		{"POST", []string{"Sec-Fetch-Site", "cross-site", "Authorization", "Basic eDp5"}, 403},
		{"GET", []string{"Sec-Fetch-Site", "cross-site"}, 200},
	} {
		if code, body, _ := do(t, h, c.method, "/do", "", c.header...); code != c.want {
			t.Errorf("%s %v: %d %s", c.method, c.header, code, body)
		}
	}
	if code, body, _ := do(t, h, "POST", "/do", "", "Origin", "https://evil.example", "Accept", "text/html"); code != 403 || !strings.Contains(body, "cross-origin request refused") {
		t.Errorf("a page: %d %s", code, body)
	}
}

// guarded is a counter whose Inc a decorator refuses unless the page was
// opened with ?ok=1.
type guarded struct {
	ui.Live
	n int
}

// guardedSeen is CurrentLiveEvent's Handling where guarded saw an event:
// "inc" in Inc's decorator, "render" in the render after it.
var guardedSeen struct {
	sync.Mutex
	inc, render []bool
}

var guardedInc = vuka.Func{Name: "guarded.Inc", ErrIndex: -1, CtxIndex: -1, Zero: func() []any { return nil }}

func (g *guarded) Inc() {
	refuse := func(c *vuka.Call) {
		ev := CurrentLiveEvent()
		if ev == nil || ev.Handler() != "guarded.Inc" {
			c.Next()
			return
		}
		guardedSeen.Lock()
		guardedSeen.inc = append(guardedSeen.inc, ev.Handling())
		guardedSeen.Unlock()
		if Request(ev.Context()).URL.Query().Get("ok") != "1" {
			ev.Refuse(Status(http.StatusForbidden, "not ok"))
			c.Return()
			return
		}
		c.Next()
	}
	vuka.NewCall(&guardedInc, []vuka.Decorator{refuse}, g, nil, func(*vuka.Call) []any { g.n++; return nil }).Run()
}

func (g *guarded) Render() ui.Node {
	if ev := CurrentLiveEvent(); ev != nil {
		guardedSeen.Lock()
		guardedSeen.render = append(guardedSeen.render, ev.Handling())
		guardedSeen.Unlock()
	}
	return ui.El("p", nil, ui.Text(g.n), ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(g.Inc)}}))
}

func TestLiveEventAndCheck(t *testing.T) {
	var ended sync.Map
	a := New(Only("none"), LiveCheckEvery(50*time.Millisecond), LiveCheck(func(ctx context.Context) error {
		if _, ok := ended.Load(Request(ctx).URL.Query().Get("who")); ok {
			return errors.New("ended")
		}
		return nil
	}))
	a.Live("/g", &guarded{})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	c, html := joined(t, srv, "/g?who=a")
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", 0), "event": "click"})
	if m := must(t, c); m.Error != "not ok" || len(m.Updates) != 0 {
		t.Errorf("refused: %+v", m)
	}
	if CurrentLiveEvent() != nil {
		t.Error("an event outside one")
	}
	c, html = joined(t, srv, "/g?who=b&ok=1")
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", 0), "event": "click"})
	if m := must(t, c); m.Error != "" || len(m.Updates) != 1 {
		t.Errorf("allowed: %+v", m)
	}
	guardedSeen.Lock()
	if !reflect.DeepEqual(guardedSeen.inc, []bool{true, true}) || !reflect.DeepEqual(guardedSeen.render, []bool{false, false}) {
		t.Errorf("Handling: in the handler %v, in the render after it %v", guardedSeen.inc, guardedSeen.render)
	}
	guardedSeen.Unlock()
	ended.Store("b", true) // the periodic check closes the page, no message needed
	if _, err := recv(c, 5e9); websocket.CloseStatus(err) != closeReload {
		t.Errorf("LiveCheck: %v", err)
	}
}

func TestToolbarRequestNotes(t *testing.T) {
	withToolbar(t)
	type login struct {
		Login    string
		Password string
		APIToken string `json:"api_token"`
	}
	a := New(Only("none"), Dev(), WrapRoutes("note", func(RouteInfo) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				NoteRequest(r.Context(), RequestNote{Section: "Auth", Name: "Identity", Value: "ada"})
				NoteRequest(r.Context(), RequestNote{Section: "Auth", Name: "Gate", Value: "refused <x>", Refused: true})
				next.ServeHTTP(w, r)
			})
		}
	}))
	a.Post("/login", func(in login) ui.Node { return fullPage(ui.Text("hi " + in.Login)) })
	h := a.Handler()
	req := httptest.NewRequest("POST", "/login", strings.NewReader("login=ada&password=hunter22&api_token=tok123"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	_, html := fetchRecord(t, h, w.Header().Get(ToolbarHeader))
	for _, want := range []string{"<h4>Auth</h4>", "<th>Identity</th><td>ada</td>", `<span class="errtext">refused &lt;x&gt;</span>`, "Login:ada", "Password:(hidden)", "APIToken:(hidden)"} {
		if !strings.Contains(html, want) {
			t.Errorf("no %q in %s", want, html)
		}
	}
	if strings.Contains(html, "hunter22") || strings.Contains(html, "tok123") {
		t.Error("a secret is shown")
	}
	NoteRequest(context.Background(), RequestNote{Name: "x"})
}

type redirectMount struct{ ui.Live }

func (r *redirectMount) LiveSchema(l *ui.LiveSchema)   { l.Mount(r.mount) }
func (r *redirectMount) mount(_ context.Context) error { return Redirect("/login") }
func (r *redirectMount) Render() ui.Node               { return ui.El("p", nil, ui.Text("x")) }

// A redirect returned from a live view's Mount redirects, rather than 500ing.
func TestLiveMountRedirect(t *testing.T) {
	a := New(Only("none"))
	a.Live("/gated", &redirectMount{})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(srv.URL + "/gated")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Fatalf("live Mount redirect: got %d Location=%q, want 302 /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}
