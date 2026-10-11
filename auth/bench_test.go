package auth_test

import (
	"context"
	"testing"

	"github.com/coder/websocket"
)

// A live event's round trip in an app with auth: an ungated handler, and a
// gated one let through.
func BenchmarkLiveEvent(b *testing.B) {
	t := &testing.T{}
	c := serve(t, newApp(t, users(), nil))
	c.login("ada@example.com", "correct horse", "")
	ws, m := c.socket("/board", c.srv.URL)
	for i, name := range []string{"ungated", "gated"} {
		target := handler(t, m.HTML, []int{0, 1}[i])
		msg := []byte(`{"type":"event","ref":2,"target":"` + target + `","event":"click"}`)
		b.Run(name, func(b *testing.B) {
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				ws.Write(ctx, websocket.MessageText, msg)
				if _, _, err := ws.Read(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
