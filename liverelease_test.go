package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
)

type eventSeen struct {
	method, path, agent, cookie, live string
	hasW                              bool
	body                              any
	scheme                            string
	wrote                             bool
}

type whoPage struct {
	ui.Live
}

var whoSeen = make(chan eventSeen, 4)

func (p *whoPage) Who(ctx context.Context) {
	r := Request(ctx)
	s := eventSeen{method: r.Method, path: r.URL.Path, agent: r.UserAgent(), live: r.Header.Get("X-Vuka-Live"), body: r.Body}
	if c, err := r.Cookie("user"); err == nil {
		s.cookie = c.Value
	}
	if w := ResponseWriter(ctx); w != nil {
		s.hasW = true
		w.Header().Set("X-Test", "1")
		_, err := w.Write([]byte("ignored"))
		s.wrote = err == nil
	}
	whoSeen <- s
}

func (p *whoPage) Render() ui.Node {
	return ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Who)}}, ui.Text("who"))
}

func joinWho(t *testing.T, srv *httptest.Server, hdr http.Header) *websocket.Conn {
	t.Helper()
	html, tok := pageToken(t, srv, "/who", "Cookie", "user=ann")
	c, err := dial(t, srv, tok, hdr)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1, "v": 2})
	recv2(t, c)
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", 0), "event": "click"})
	return c
}

// After the join the session keeps a trimmed copy of the upgrade request:
// the handler has returned and the request itself is collectable, while
// web.Request in an event still has the URL, method and the headers the
// upgrade carried, and no body.
func TestLiveJoinReleasesRequest(t *testing.T) {
	var (
		mu       sync.Mutex
		weakReq  weak.Pointer[http.Request]
		returned = make(chan struct{})
	)
	app := liveApp()
	app.Live("/who", &whoPage{})
	h := app.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != liveSocket {
			h.ServeHTTP(w, r)
			return
		}
		mu.Lock()
		weakReq = weak.Make(r)
		mu.Unlock()
		h.ServeHTTP(w, r)
		close(returned)
	}))
	defer srv.Close()
	c := joinWho(t, srv, http.Header{"Origin": {srv.URL}, "User-Agent": {"probe/1"}, "Cookie": {"user=ann"}})
	defer c.CloseNow()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("a middleware wrapping the live handler never saw it return after the upgrade")
	}
	var s eventSeen
	select {
	case s = <-whoSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("the event never ran")
	}
	if s.method != http.MethodGet || s.path != "/who" || s.agent != "probe/1" || s.cookie != "ann" || s.live == "" {
		t.Errorf("web.Request in an event: %+v", s)
	}
	if s.body != nil && s.body != http.NoBody {
		t.Errorf("the body is %v, want none", s.body)
	}
	if !s.hasW || !s.wrote {
		t.Errorf("ResponseWriter in an event: %+v, want a writer discarding writes", s)
	}
	for range 20 {
		runtime.GC()
		mu.Lock()
		gone := weakReq.Value() == nil
		mu.Unlock()
		if gone {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Error("the upgrade request is still reachable after the join")
}

// App shutdown still closes sockets whose handler has returned.
func TestLiveDetachedSocketsCloseOnShutdown(t *testing.T) {
	app := liveApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	_, tok := pageToken(t, srv, "/counter")
	c, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1, "v": 2})
	recv2(t, c)
	if n := app.LiveConns(); n != 1 {
		t.Fatalf("%d live conns, want 1", n)
	}
	go app.hub.closeAll()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("read after shutdown: %v", err)
	}
	for range 100 {
		if app.LiveConns() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the connection is still counted after shutdown")
}

// A socket that closes ends its session even though the handler returned.
func TestLiveDetachedSocketEndsWithSocket(t *testing.T) {
	app := liveApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	_, tok := pageToken(t, srv, "/counter")
	c, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1, "v": 2})
	recv2(t, c)
	c.CloseNow()
	for range 100 {
		if app.LiveConns() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the session outlived its socket")
}
