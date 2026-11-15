package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2/live"
)

// sseEvent is one event of a live connection's stream: its name ("" for a
// message) and its data.
type sseEvent struct{ name, data string }

// sseConnTest is a live connection over an event stream, read by hand: what
// a socket's tests use dial/send/must/joined for, a stream's use
// dialSSE/sendSSE/mustSSE/joinedSSE for.
type sseConnTest struct {
	srv    *httptest.Server
	id     string
	res    *http.Response
	events chan sseEvent
}

// dialSSE opens a stream with token: the response, and the connection once
// its ready event named it (nil for a stream refused).
func dialSSE(t testing.TB, srv *httptest.Server, token string, header http.Header) (*sseConnTest, *http.Response) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+liveSocket+"?transport=sse&t="+token, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, res
	}
	t.Cleanup(func() { res.Body.Close() })
	c := &sseConnTest{srv: srv, res: res, events: make(chan sseEvent, 64)}
	go func() {
		defer close(c.events)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(nil, 4<<20)
		var ev sseEvent
		var data []string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if data != nil {
					ev.data = strings.Join(data, "\n")
					c.events <- ev
				}
				ev, data = sseEvent{}, nil
			case strings.HasPrefix(line, ":"): // a comment: the pad, a ping
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	ready := c.next(t, 5*time.Second)
	var r struct{ ID string }
	if ready.name != "ready" || json.Unmarshal([]byte(ready.data), &r) != nil || r.ID == "" {
		t.Fatalf("the stream's first event: %+v", ready)
	}
	c.id = r.ID
	return c, res
}

func (c *sseConnTest) next(t testing.TB, wait time.Duration) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-c.events:
		if !ok {
			t.Fatal("the stream ended")
		}
		return ev
	case <-time.After(wait):
		t.Fatal("no event in time")
	}
	return sseEvent{}
}

// post POSTs one message for the connection: the response's status.
func (c *sseConnTest) post(t testing.TB, m any) int {
	t.Helper()
	b, _ := json.Marshal(m)
	res, err := http.Post(c.srv.URL+liveSocket+"?c="+c.id, "text/plain;charset=UTF-8", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func sendSSE(t testing.TB, c *sseConnTest, m map[string]any) {
	t.Helper()
	if s := c.post(t, m); s != http.StatusNoContent {
		t.Fatalf("POST %v: %d", m, s)
	}
}

func mustSSE(t testing.TB, c *sseConnTest) live.ServerMessage {
	t.Helper()
	ev := c.next(t, 5*time.Second)
	if ev.name != "" {
		t.Fatalf("a message, not %+v", ev)
	}
	var m live.ServerMessage
	if err := json.Unmarshal([]byte(ev.data), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// closedSSE reads the stream's close event: its code and reason. The stream
// ends after it.
func closedSSE(t testing.TB, c *sseConnTest) (int, string) {
	t.Helper()
	ev := c.next(t, 5*time.Second)
	var cl struct {
		Code   int
		Reason string
	}
	if ev.name != "close" || json.Unmarshal([]byte(ev.data), &cl) != nil {
		t.Fatalf("a close event, not %+v", ev)
	}
	select {
	case ev, ok := <-c.events:
		if ok {
			t.Fatalf("an event after the close: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream didn't end after its close event")
	}
	return cl.Code, cl.Reason
}

func joinedSSE(t *testing.T, srv *httptest.Server, path string) (*sseConnTest, string) {
	t.Helper()
	_, tok := pageToken(t, srv, path)
	if tok == "" {
		t.Fatalf("%s: not a live page", path)
	}
	c, res := dialSSE(t, srv, tok, nil)
	if c == nil {
		t.Fatalf("stream: %d", res.StatusCode)
	}
	sendSSE(t, c, map[string]any{"type": "join", "ref": 1})
	m := mustSSE(t, c)
	if m.Type != live.Render || m.Ref != 1 {
		t.Fatalf("join: %+v", m)
	}
	return c, m.HTML
}

// A live page over an event stream: the join, an event's patch, a
// navigation and a broadcast arrive as messages, the same as a socket's,
// and CloseLive's 4001 arrives as the stream's close event.
func TestLiveStream(t *testing.T) {
	app := liveApp()
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close) // after the streams' bodies close: it waits for their handlers
	c, html := joinedSSE(t, srv, "/two")
	if !strings.Contains(html, "<span>a0</span>") || !strings.Contains(html, "<span>b10</span>") {
		t.Fatalf("render: %s", html)
	}
	sendSSE(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", 1), "event": "click"})
	if m := mustSSE(t, c); m.Type != live.PatchMsg || m.Ref != 2 || len(m.Updates) != 1 || !strings.Contains(m.Updates[0].HTML, "<span>b11</span>") {
		t.Fatalf("patch: %+v", m)
	}

	p, _ := joinedSSE(t, srv, "/pager?page=1")
	sendSSE(t, p, map[string]any{"type": "navigate", "ref": 2, "url": "/pager?page=2", "patch": true})
	if m := mustSSE(t, p); m.Type != live.PatchMsg || m.Ref != 2 || len(m.Updates) != 1 || !strings.Contains(m.Updates[0].HTML, "page 2, mounts 1, got [2]") {
		t.Fatalf("navigate: %+v", m)
	}

	a, chat := joinedSSE(t, srv, "/chat/go")
	b, _ := joined(t, srv, "/chat/go") // a socket's page hears it too
	sendSSE(t, a, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, chat, "submit", 0), "event": "submit", "form": map[string][]string{"text": {"hi"}}})
	if m := mustSSE(t, a); m.Ref != 2 || m.Error != "" {
		t.Fatalf("send: %+v", m)
	}
	if m := mustSSE(t, a); m.Type != live.PatchMsg || m.Ref != 0 || !strings.Contains(m.Updates[0].HTML, ">hi</form>") {
		t.Fatalf("broadcast over the stream: %+v", m)
	}
	if m := must(t, b); m.Type != live.PatchMsg || !strings.Contains(m.Updates[0].HTML, ">hi</form>") {
		t.Fatalf("broadcast over the socket: %+v", m)
	}

	if n := app.CloseLive(func(context.Context) bool { return true }); n != 4 {
		t.Fatalf("closed %d pages", n)
	}
	if code, _ := closedSSE(t, c); code != int(closeReload) {
		t.Fatalf("close: %d", code)
	}
	if s := c.post(t, map[string]any{"type": "event", "ref": 4}); s != http.StatusConflict {
		t.Fatalf("a POST for a closed stream: %d", s)
	}
}

// Messages POSTed back to back are answered in the order they were sent.
func TestLiveStreamOrder(t *testing.T) {
	srv := httptest.NewServer(liveApp().Handler())
	t.Cleanup(srv.Close) // after the streams' bodies close: it waits for their handlers
	c, html := joinedSSE(t, srv, "/counter")
	for i := range 3 {
		sendSSE(t, c, map[string]any{"type": "event", "ref": 2 + i, "target": handlerOf(t, html, "click", 0), "event": "click"})
	}
	for i := range 3 {
		m := mustSSE(t, c)
		if m.Ref != 2+i || len(m.Updates) != 1 || !strings.Contains(m.Updates[0].HTML, fmt.Sprintf("<span>n=%d</span>", 6+i)) {
			t.Fatalf("reply %d: %+v", i, m)
		}
	}
}

// A stream's token, origin, join deadline and the connection it names are
// checked as a socket's are.
func TestLiveStreamRefusals(t *testing.T) {
	app := liveApp(LiveJoinTimeout(200 * time.Millisecond))
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close) // after the streams' bodies close: it waits for their handlers

	c, _ := dialSSE(t, srv, "x", nil)
	if code, _ := closedSSE(t, c); code != int(closeReload) {
		t.Fatalf("a bad token: %d", code)
	}

	_, tok := pageToken(t, srv, "/counter")
	if _, res := dialSSE(t, srv, tok, http.Header{"Origin": {"https://evil.example"}}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("another origin: %d", res.StatusCode)
	}
	c, _ = dialSSE(t, srv, tok, http.Header{"Origin": {srv.URL}})
	if s := (&sseConnTest{srv: srv, id: "nope"}).post(t, map[string]any{"type": "join", "ref": 1}); s != http.StatusConflict {
		t.Fatalf("an unknown connection: %d", s)
	}
	req, _ := http.NewRequest("POST", srv.URL+liveSocket+"?c="+c.id, strings.NewReader(`{"type":"join","ref":1}`))
	req.Header.Set("Origin", "https://evil.example")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("a POST from another origin: %v %v", res, err)
	}
	if code, _ := closedSSE(t, c); code != 1008 { // StatusPolicyViolation: no join in time
		t.Fatalf("no join: %d", code)
	}
}

// A stream takes a place under the live caps as a socket does, and gives it
// back when it ends.
func TestLiveStreamLimits(t *testing.T) {
	app := liveApp(LiveMaxConnsPerPage(2))
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close) // after the streams' bodies close: it waits for their handlers
	_, tok := pageToken(t, srv, "/counter")
	if c, _ := dialSSE(t, srv, tok, nil); c == nil {
		t.Fatal("a stream refused")
	}
	if _, err := dial(t, srv, tok, nil); err != nil {
		t.Fatal(err)
	}
	if _, res := dialSSE(t, srv, tok, nil); res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("a third connection of one page, a stream: %d", res.StatusCode)
	}
	if _, err := dial(t, srv, tok, nil); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("a third connection of one page, a socket: %v", err)
	}
	dropConnections(app)
	waitFor(t, func() bool {
		app.hub.mu.Lock()
		defer app.hub.mu.Unlock()
		n := 0
		app.hub.streams.Range(func(any, any) bool { n++; return true })
		return app.hub.open == 0 && n == 0
	})
	if c, res := dialSSE(t, srv, tok, nil); c == nil {
		t.Fatalf("a place freed: %d", res.StatusCode)
	}
}
