package web

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/web/v2/internal/liveproto"
)

// A page left idle past LiveTrimAfter answers its next event, broadcast and
// reconnect as a page never trimmed does: changes, not the whole page again.
func TestLiveTrimAfter(t *testing.T) {
	v2 := liveproto.V2
	defer func() { liveproto.V2 = v2 }()
	liveproto.V2 = nil
	if !liveproto.Native() {
		t.Skip("this ui's session speaks only v1")
	}
	const idle = 20 * time.Millisecond
	replies := func(after time.Duration, opts ...Option) []string {
		srv := httptest.NewServer(liveApp(opts...).Handler())
		defer srv.Close()
		var out []string
		read := func(c *websocket.Conn) {
			t.Helper()
			_, raw := recv2(t, c)
			out = append(out, raw)
		}
		join := func(path string) (*websocket.Conn, string) {
			html, tok := pageToken(t, srv, path)
			c, err := dial(t, srv, tok, nil)
			if err != nil {
				t.Fatal(err)
			}
			send(t, c, map[string]any{"type": "join", "ref": 1, "v": 2})
			read(c)
			return c, html
		}
		c, html := join("/counter")
		target := handlerOf(t, html, "click", 0)
		for ref := 2; ref < 5; ref++ {
			time.Sleep(after)
			send(t, c, map[string]any{"type": "event", "ref": ref, "target": target, "event": "click"})
			read(c)
		}
		// a broadcast reaching a page that sat idle
		a, chat := join("/chat/go")
		b, _ := join("/chat/go")
		time.Sleep(after)
		send(t, a, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, chat, "submit", 0), "event": "submit", "form": map[string][]string{"text": {"hi"}}})
		read(a)
		read(a)
		read(b)
		// and the page's socket reconnecting
		c.CloseNow()
		join("/counter")
		return out
	}
	plain := replies(0, LiveTrimAfter(0))
	trimmed := replies(5*idle, LiveTrimAfter(idle))
	if len(plain) != len(trimmed) {
		t.Fatalf("%d replies, %d trimmed", len(plain), len(trimmed))
	}
	for i := range plain {
		if plain[i] != trimmed[i] {
			t.Errorf("reply %d:\nnot trimmed: %s\ntrimmed:     %s", i, plain[i], trimmed[i])
		}
	}
}
