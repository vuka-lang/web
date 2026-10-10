package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/vuka/live"
	"github.com/vuka-lang/web/internal/chrome"
)

type counter struct {
	vuka.Live
	Start int
	Label string
	n     int
}

func (c *counter) Mount() { c.n = c.Start }
func (c *counter) Inc()   { c.n++ }
func (c *counter) Render() vuka.Node {
	return vuka.El("div", nil, vuka.El("span", nil, vuka.Text(c.Label), vuka.Text(c.n)),
		vuka.El("button", []vuka.Attr{{Name: "onClick", Value: vuka.On(c.Inc)}}, vuka.Text("+")))
}

type todoForm struct {
	Name string `form:"name"`
}

type todo struct {
	vuka.Live
	items []string
}

func (t *todo) Add(f todoForm) error {
	if f.Name == "" {
		return errors.New("a name, please")
	}
	t.items = append(t.items, f.Name)
	return nil
}

func (t *todo) Render() vuka.Node {
	var lis []vuka.Node
	for _, it := range t.items {
		lis = append(lis, vuka.El("li", nil, vuka.Text(it)))
	}
	return vuka.El("form", []vuka.Attr{{Name: "onSubmit", Value: vuka.On(t.Add)}},
		vuka.El("input", []vuka.Attr{{Name: "name", Value: "name"}}), vuka.El("ul", nil, lis...))
}

type said struct{ Text string }

type chat struct {
	vuka.Live
	Room string
	log  []string
}

func (c *chat) Mount()        { c.Subscribe("room:" + c.Room) }
func (c *chat) Update(m said) { c.log = append(c.log, m.Text) }
func (c *chat) Render() vuka.Node {
	return vuka.El("form", []vuka.Attr{{Name: "onSubmit", Value: vuka.On(c.Send)}}, vuka.Text(strings.Join(c.log, "|")))
}
func (c *chat) Send(ctx context.Context, f chatForm) error {
	return Broadcast(ctx, "room:"+c.Room, said{f.Text})
}

type chatForm struct {
	Text string `form:"text"`
}

func liveApp(opts ...Option) *App {
	app := New(append([]Option{Only("none")}, opts...)...)
	app.Live("/counter", &counter{Start: 5, Label: "n="})
	app.Live("/todo", &todo{})
	app.Live("/chat/{room}", &chat{})
	app.Get("/two", func() vuka.Node {
		return vuka.El("main", nil, vuka.Component("two#1", nil, &counter{Label: "a"}), vuka.Component("two#2", nil, &counter{Label: "b", Start: 10}))
	})
	app.Get("/plain", func() vuka.Node { return vuka.El("p", nil, vuka.Text("static")) })
	app.Get("/json", func() Pet { return Pet{1, "Rex"} })
	app.Post("/two", func() vuka.Node { return vuka.Component("p#1", nil, &counter{}) })
	needCookie := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := r.Cookie("user"); err != nil {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	app.Get("/private", func() vuka.Node { return vuka.Component("private#1", nil, &counter{}) }, Wrap(needCookie))
	app.Live("/guarded", &counter{}, Use(needCookie))
	return app
}

var tokenRe = regexp.MustCompile(`data-vk-token="([^"]+)"`)

func pageToken(t *testing.T, srv *httptest.Server, path string, header ...string) (string, string) {
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
	m := tokenRe.FindStringSubmatch(string(b))
	if m == nil {
		return string(b), ""
	}
	return string(b), m[1]
}

func dial(t *testing.T, srv *httptest.Server, token string, header http.Header) (*websocket.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if header == nil {
		header = http.Header{"Origin": {srv.URL}}
	}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+liveSocket+"?t="+token, &websocket.DialOptions{HTTPHeader: header})
	if err == nil {
		t.Cleanup(func() { c.CloseNow() })
	}
	return c, err
}

func send(t *testing.T, c *websocket.Conn, m map[string]any) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func recv(c *websocket.Conn, wait time.Duration) (live.ServerMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var m live.ServerMessage
	_, b, err := c.Read(ctx)
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	return m, err
}

func must(t *testing.T, c *websocket.Conn) live.ServerMessage {
	t.Helper()
	m, err := recv(c, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func joined(t *testing.T, srv *httptest.Server, path string) (*websocket.Conn, string) {
	t.Helper()
	_, tok := pageToken(t, srv, path)
	if tok == "" {
		t.Fatalf("%s: not a live page", path)
	}
	c, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1})
	m := must(t, c)
	if m.Type != live.Render || m.Ref != 1 {
		t.Fatalf("join: %+v", m)
	}
	return c, m.HTML
}

func handlerOf(t *testing.T, html, event string, i int) string {
	t.Helper()
	ms := regexp.MustCompile(`data-vk-on-`+event+`="(c\d+:\d+)"`).FindAllStringSubmatch(html, -1)
	if i >= len(ms) {
		t.Fatalf("no %d-th %s handler in %s", i, event, html)
	}
	return ms[i][1]
}

func TestLivePageRenders(t *testing.T) {
	srv := httptest.NewServer(liveApp().Handler())
	defer srv.Close()
	html, tok := pageToken(t, srv, "/counter?start=7")
	if tok == "" || !strings.Contains(html, "<span>n=7</span>") || !strings.Contains(html, "<!DOCTYPE html>") ||
		!strings.Contains(html, `src="/_vuka/live.js?v=`) || strings.Index(html, "<script") > strings.Index(html, "</body>") {
		t.Fatalf("live page: %s", html)
	}
	if html, tok := pageToken(t, srv, "/plain"); tok != "" || strings.Contains(html, "<script") {
		t.Errorf("a page without stateful components is live: %s", html)
	}
	res, err := http.Post(srv.URL+"/two", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(b), "<script") {
		t.Errorf("a POST page is live: %s", b)
	}
	res, err = http.Get(srv.URL + liveScript)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Content-Type") != "text/javascript; charset=utf-8" || res.Header.Get("ETag") == "" {
		t.Errorf("live.js: %v", res.Header)
	}
}

func TestLiveEventPatchesOneComponent(t *testing.T) {
	srv := httptest.NewServer(liveApp().Handler())
	defer srv.Close()
	c, html := joined(t, srv, "/two")
	if !strings.Contains(html, "<span>a0</span>") || !strings.Contains(html, "<span>b10</span>") {
		t.Fatalf("render: %s", html)
	}
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", 1), "event": "click"})
	m := must(t, c)
	if m.Type != live.PatchMsg || m.Ref != 2 || len(m.Updates) != 1 || m.Updates[0].ID != "c2" ||
		!strings.Contains(m.Updates[0].HTML, "<span>b11</span>") || strings.Contains(m.Updates[0].HTML, "a0") {
		t.Fatalf("patch: %+v", m)
	}
	send(t, c, map[string]any{"type": "event", "ref": 3, "target": "c9:0", "event": "click"})
	if m := must(t, c); m.Type != live.Error || m.Ref != 3 {
		t.Fatalf("unknown handler: %+v", m)
	}
	send(t, c, map[string]any{"type": "event", "ref": 4, "target": handlerOf(t, html, "click", 1), "event": "click"})
	if m := must(t, c); len(m.Updates) != 1 || !strings.Contains(m.Updates[0].HTML, "<span>b12</span>") {
		t.Fatalf("second click: %+v", m)
	}
}

func TestLiveFormSubmit(t *testing.T) {
	srv := httptest.NewServer(liveApp().Handler())
	defer srv.Close()
	c, html := joined(t, srv, "/todo")
	target := handlerOf(t, html, "submit", 0)
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": target, "event": "submit", "form": map[string][]string{"name": {"milk"}}})
	if m := must(t, c); m.Error != "" || len(m.Updates) != 1 || !strings.Contains(m.Updates[0].HTML, "<li>milk</li>") {
		t.Fatalf("submit: %+v", m)
	}
	send(t, c, map[string]any{"type": "event", "ref": 3, "target": target, "event": "submit", "form": map[string][]string{"name": {""}}})
	if m := must(t, c); m.Type != live.PatchMsg || m.Error != "a name, please" || len(m.Updates) != 0 {
		t.Fatalf("handler error: %+v", m)
	}
}

func TestLiveBroadcast(t *testing.T) {
	srv := httptest.NewServer(liveApp().Handler())
	defer srv.Close()
	a, html := joined(t, srv, "/chat/go")
	b, _ := joined(t, srv, "/chat/go")
	other, _ := joined(t, srv, "/chat/rust")
	counter, _ := joined(t, srv, "/counter")
	send(t, a, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "submit", 0), "event": "submit", "form": map[string][]string{"text": {"hi"}}})
	if m := must(t, a); m.Ref != 2 || m.Error != "" {
		t.Fatalf("send: %+v", m)
	}
	for _, c := range []*websocket.Conn{a, b} {
		if m := must(t, c); m.Type != live.PatchMsg || m.Ref != 0 || len(m.Updates) != 1 || !strings.Contains(m.Updates[0].HTML, ">hi</form>") {
			t.Fatalf("broadcast: %+v", m)
		}
	}
	for _, c := range []*websocket.Conn{other, counter} {
		if m, err := recv(c, 200*time.Millisecond); err == nil {
			t.Fatalf("a page not subscribed got %+v", m)
		}
	}
	if err := Broadcast(context.Background(), "x", 1); err == nil {
		t.Error("Broadcast without an app's context")
	}
}

func TestLiveTokenAndOrigin(t *testing.T) {
	app := liveApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	_, tok := pageToken(t, srv, "/counter")
	for _, bad := range []string{"", "x", tok[:len(tok)-2] + "AA", app.signToken("//evil.example/")} {
		c, err := dial(t, srv, bad, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = recv(c, 2*time.Second)
		if websocket.CloseStatus(err) != closeReload {
			t.Errorf("token %q: %v", bad, err)
		}
	}
	other := New(Only("none"), LiveKey([]byte("k")))
	if _, err := other.verifyToken(tok); err == nil {
		t.Error("a token verified under another key")
	}
	if _, err := dial(t, srv, tok, http.Header{"Origin": {"http://evil.example"}}); err == nil {
		t.Error("another origin connected")
	}
	srv2 := httptest.NewServer(liveApp(LiveOrigins("evil.example")).Handler())
	defer srv2.Close()
	_, tok2 := pageToken(t, srv2, "/counter")
	if _, err := dial(t, srv2, tok2, http.Header{"Origin": {"http://evil.example"}}); err != nil {
		t.Errorf("an allowed origin: %v", err)
	}
	expired := New(Only("none"), LiveTokenTTL(-time.Second))
	if _, err := expired.verifyToken(expired.signToken("/x")); !errors.Is(err, errExpiredToken) {
		t.Errorf("expired token: %v", err)
	}
}

func TestLiveRejoinAndMiddleware(t *testing.T) {
	app := liveApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	c, html := joined(t, srv, "/counter")
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", 0), "event": "click"})
	if m := must(t, c); !strings.Contains(m.Updates[0].HTML, "n=6") {
		t.Fatalf("click: %+v", m)
	}
	c.Close(websocket.StatusNormalClosure, "")
	_, tok := pageToken(t, srv, "/counter")
	c2, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c2, map[string]any{"type": "event", "ref": 1, "target": "c1:0", "event": "click"})
	if m := must(t, c2); m.Type != live.Error {
		t.Fatalf("an event before the join: %+v", m)
	}
	send(t, c2, map[string]any{"type": "join", "ref": 2})
	if m := must(t, c2); m.Type != live.Render || !strings.Contains(m.HTML, "n=5") {
		t.Fatalf("rejoin: %+v", m)
	}

	if html, _ := pageToken(t, srv, "/guarded"); strings.Contains(html, "data-vk-id") {
		t.Errorf("Use on a live page: %s", html)
	}
	_, tok = pageToken(t, srv, "/private", "Cookie", "user=7")
	if tok == "" {
		t.Fatal("/private with the cookie isn't live")
	}
	c3, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c3, map[string]any{"type": "join", "ref": 1})
	if m := must(t, c3); m.Type != live.Redirect || m.URL != "/login" {
		t.Fatalf("join without the cookie: %+v", m)
	}
	c4, err := dial(t, srv, tok, http.Header{"Origin": {srv.URL}, "Cookie": {"user=7"}})
	if err != nil {
		t.Fatal(err)
	}
	send(t, c4, map[string]any{"type": "join", "ref": 1})
	if m := must(t, c4); m.Type != live.Render {
		t.Fatalf("join with the cookie: %+v", m)
	}
	c5, err := dial(t, srv, app.signToken("/json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c5, map[string]any{"type": "join", "ref": 1})
	m := must(t, c5)
	if _, err := recv(c5, time.Second); m.Type != live.Error || websocket.CloseStatus(err) != closeRefused {
		t.Fatalf("join of a page that isn't live: %+v, %v", m, err)
	}
}

func TestLiveStopClosesConnections(t *testing.T) {
	app := liveApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	c, _ := joined(t, srv, "/counter")
	go app.Stop(context.Background())
	if _, err := recv(c, 5*time.Second); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("after Stop: %v", err)
	}
}

func dropConnections(a *App) {
	a.hub.mu.Lock()
	defer a.hub.mu.Unlock()
	for c := range a.hub.conns {
		c.ws.CloseNow()
	}
}

func TestBrowserReconnect(t *testing.T) {
	for _, v := range []int{1, 2} {
		t.Run(fmt.Sprint("v", v), func(t *testing.T) { browserReconnect(t, v) })
	}
}

func browserReconnect(t *testing.T, v int) {
	app := liveApp(LiveTokenTTL(2*time.Second), LiveProtocol(v))
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/counter")
	p.Wait(fmt.Sprintf(`document.documentElement.dataset.vkProtocol === "%d"`, v))
	p.Click("button")
	p.WaitText("span", "n=6")
	p.Eval(`window.marker = 1`, nil)
	dropConnections(app)
	p.Wait(`document.documentElement.dataset.vkState !== "connected"`)
	p.WaitText("span", "n=5") // rejoined: the state starts over
	p.Wait(`window.marker === 1 && document.documentElement.dataset.vkState === "connected"`)
	p.Click("button")
	p.WaitText("span", "n=6")

	time.Sleep(3100 * time.Millisecond) // the token (whole seconds) expires: reconnecting reloads the page
	dropConnections(app)
	p.Wait(`window.marker === undefined && document.documentElement.dataset.vkState === "connected"`)
	p.WaitText("span", "n=5")
}
