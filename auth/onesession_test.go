package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/auth"
)

// countingSessions counts the store reads of the sessions it keeps.
type countingSessions struct {
	*auth.MemorySessions
	gets atomic.Int64
}

func (s *countingSessions) Get(ctx context.Context, key string) (*auth.Session, error) {
	s.gets.Add(1)
	return s.MemorySessions.Get(ctx, key)
}

type countingTokens struct {
	*auth.MemoryTokens
	gets atomic.Int64
}

func (s *countingTokens) Get(ctx context.Context, key string) (*auth.Token, error) {
	s.gets.Add(1)
	return s.MemoryTokens.Get(ctx, key)
}

func countingApp(t *testing.T, opts ...web.Option) (*web.App, *countingSessions, *countingTokens) {
	t.Helper()
	ss := &countingSessions{MemorySessions: auth.NewMemorySessions()}
	ts := &countingTokens{MemoryTokens: auth.NewMemoryTokens()}
	return newApp(t, users(), func(c *auth.Config) { c.Sessions, c.Tokens = ss, ts }, opts...), ss, ts
}

// reads runs f and returns how many times n moved while it did.
func reads(n *atomic.Int64, f func()) int64 {
	before := n.Load()
	f()
	return n.Load() - before
}

func navigate(t *testing.T, ws *websocket.Conn, ref int, to string) live.ServerMessage {
	t.Helper()
	send(t, ws, map[string]any{"type": "navigate", "ref": ref, "url": to})
	return must(t, ws)
}

// A live page's session is read when the page mounts — its join, each
// navigation — and never for its events.
func TestOneSessionReadPerMount(t *testing.T) {
	app, ss, _ := countingApp(t, web.LiveCheckEvery(0))
	c := serve(t, app)
	c.login("ada@example.com", "correct horse", "")

	for i := 0; i < 2; i++ {
		if n := reads(&ss.gets, func() { c.page("/board") }); n != 1 {
			t.Errorf("a plain GET read the session %d times", n)
		}
	}
	r := c.page("/board")
	ws := dial(t, c, tokenRe.FindStringSubmatch(r.body)[1], http.Header{"Cookie": {"vk_session=" + c.cookie()}})
	var m live.ServerMessage
	if n := reads(&ss.gets, func() {
		send(t, ws, map[string]any{"type": "join", "ref": 1, "v": 1})
		m = must(t, ws)
	}); n != 1 || m.Type != live.Render {
		t.Errorf("a join read the session %d times: %+v", n, m)
	}
	inc := handler(t, m.HTML, 0)
	if n := reads(&ss.gets, func() {
		for i := 0; i < 5; i++ {
			click(t, ws, inc, 2+i)
		}
	}); n != 0 {
		t.Errorf("5 events read the session %d times", n)
	}
	for i, to := range []string{"/who", "/staff", "/board", "/board?x=1"} {
		var m live.ServerMessage
		if n := reads(&ss.gets, func() { m = navigate(t, ws, 10+i, to) }); n != 1 || m.Type == live.Redirect || m.Error != "" {
			t.Errorf("a navigation to %s read the session %d times: %+v", to, n, m)
		}
	}
	if m := navigate(t, ws, 20, "/who"); !strings.Contains(patched(m), "who=1") {
		t.Errorf("the navigation's identity: %+v", m)
	}
}

func TestOneTokenReadPerMount(t *testing.T) {
	app, _, ts := countingApp(t, web.LiveCheckEvery(0))
	c := serve(t, app)
	a, _ := web.Resolve[*auth.Auth](app)
	tok, _, err := a.Issue(context.Background(), "1", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var r answer
	if n := reads(&ts.gets, func() { r = c.do("GET", "/board", "", "Accept", html, "Authorization", "Bearer "+tok) }); n != 1 {
		t.Errorf("a plain GET read the token %d times", n)
	}
	ws := dial(t, c, tokenRe.FindStringSubmatch(r.body)[1], http.Header{"Authorization": {"Bearer " + tok}})
	var m live.ServerMessage
	if n := reads(&ts.gets, func() {
		send(t, ws, map[string]any{"type": "join", "ref": 1, "v": 1})
		m = must(t, ws)
	}); n != 1 || m.Type != live.Render {
		t.Errorf("a join read the token %d times: %+v", n, m)
	}
	if n := reads(&ts.gets, func() { click(t, ws, handler(t, m.HTML, 0), 2); click(t, ws, handler(t, m.HTML, 0), 3) }); n != 0 {
		t.Errorf("events read the token %d times", n)
	}
	if n := reads(&ts.gets, func() { m = navigate(t, ws, 4, "/staff") }); n != 1 || m.Error != "" || m.Type == live.Redirect {
		t.Errorf("a navigation read the token %d times: %+v", n, m)
	}
	// Revoke closes it at once.
	if err := a.Revoke(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	if _, err := recv(ws); websocket.CloseStatus(err) != 4001 {
		t.Errorf("after Revoke: %v", err)
	}
}

// A quiet page's sign-in is read once per web.LiveCheckEvery, which catches
// a session deleted from the store behind auth's back.
func TestPeriodicSessionCheck(t *testing.T) {
	app, ss, _ := countingApp(t, web.LiveCheckEvery(50*time.Millisecond))
	c := serve(t, app)
	c.login("ada@example.com", "correct horse", "")
	ws, _ := c.socket("/board", c.srv.URL)
	before, start := ss.gets.Load(), time.Now()
	for ss.gets.Load()-before < 3 && time.Since(start) < 10*time.Second {
		time.Sleep(10 * time.Millisecond)
	}
	n, took := ss.gets.Load()-before, time.Since(start)
	if most := int64(took/(50*time.Millisecond)) + 1; n < 3 || n > most {
		t.Errorf("a quiet page read its session %d times in %v (at most %d)", n, took, most)
	}
	ss.MemorySessions.DeleteUser(context.Background(), "1")
	if _, err := recv(ws); websocket.CloseStatus(err) != 4001 {
		t.Errorf("a session deleted from the store: %v", err)
	}
}

// Ending a sign-in through auth closes the pages signed in with it at once
// — no message, no periodic check — and no one else's.
func TestSignOutsCloseLivePages(t *testing.T) {
	for _, how := range []string{"SignOut", "RevokeUser", "SignIn again", "expired"} {
		t.Run(how, func(t *testing.T) {
			app, ss, _ := countingApp(t, web.LiveCheckEvery(0))
			a, _ := web.Resolve[*auth.Auth](app)
			ada, ada2, bob := serve(t, app), serve(t, app), serve(t, app)
			ada.login("ada@example.com", "correct horse", "")
			ada2.login("ada@example.com", "correct horse", "")
			bob.login("bob@example.com", "hunter22", "")
			ws, _ := ada.socket("/board", ada.srv.URL)
			ws2, m2 := ada2.socket("/board", ada2.srv.URL)
			wsb, mb := bob.socket("/board", bob.srv.URL)
			sameUser := false
			switch how {
			case "SignOut":
				ada.do("POST", "/auth/logout", "", "Origin", ada.srv.URL)
			case "RevokeUser":
				a.RevokeUser(context.Background(), "1")
				sameUser = true
			case "SignIn again": // the browser's old session ends
				ada.login("ada@example.com", "correct horse", "")
			case "expired": // a read finds it expired
				key := sessionKey(ada.cookie())
				s, _ := ss.MemorySessions.Get(context.Background(), key)
				s.Expires = time.Now().Add(-time.Second)
				ss.MemorySessions.Save(context.Background(), key, *s)
				ada.page("/open")
			}
			if _, err := recv(ws); websocket.CloseStatus(err) != 4001 {
				t.Errorf("the page signed in with it: %v", err)
			}
			if r := click(t, wsb, handler(t, mb.HTML, 0), 2); r.Error != "" || !strings.Contains(patched(r), "n=1") {
				t.Errorf("another user's page: %+v", r)
			}
			if sameUser {
				if _, err := recv(ws2); websocket.CloseStatus(err) != 4001 {
					t.Errorf("the user's other session's page: %v", err)
				}
			} else if r := click(t, ws2, handler(t, m2.HTML, 0), 2); r.Error != "" || !strings.Contains(patched(r), "n=1") {
				t.Errorf("the user's other session's page: %+v", r)
			}
		})
	}
}

func sessionKey(cookie string) string {
	sum := sha256.Sum256([]byte(cookie))
	return hex.EncodeToString(sum[:])
}

// dial opens a live socket for the page token tok, without joining.
func dial(t *testing.T, c *client, tok string, h http.Header) *websocket.Conn {
	t.Helper()
	h.Set("Origin", c.srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(c.srv.URL, "http")+"/_vuka/live?t="+tok, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return ws
}
