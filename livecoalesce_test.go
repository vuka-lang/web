package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
)

// drained waits, at the test's end and after its clients have closed, for the
// app's connections to be gone, so the package variables a test sets are not
// read by a server still winding down.
func drained(t *testing.T, app *App) {
	t.Cleanup(func() {
		for i := 0; i < 300 && app.hub.open.Load() > 0; i++ {
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func connsOf(a *App) (cs []*conn) {
	a.hub.each(func(c *conn) { cs = append(cs, c) })
	return
}

func logOf(m live.ServerMessage) string {
	if len(m.Updates) == 0 {
		return ""
	}
	h := m.Updates[0].HTML
	i := strings.Index(h, ">")
	j := strings.LastIndex(h, "</form>")
	if i < 0 || j < i {
		return h
	}
	return h[i+1 : j]
}

// stalled is a chat that waits, in the Update of its first message, until
// stall is closed, while the rest queue behind it.
type stalled struct {
	ui.Live
	log []string
}

var stall chan struct{}

func (s *stalled) LiveSchema(l *ui.LiveSchema)   { l.Mount(s.mount) }
func (s *stalled) mount(_ context.Context) error { s.Subscribe("room:stalled"); return nil }
func (s *stalled) Update(m said) {
	if len(s.log) == 0 {
		<-stall
	}
	s.log = append(s.log, m.Text)
}
func (s *stalled) Render() ui.Node {
	return ui.El("form", nil, ui.Text(strings.Join(s.log, "|")))
}

// Broadcasts waiting side by side reach Update in the order they arrived and
// are rendered, and sent, together.
func TestLiveBroadcastsCoalesce(t *testing.T) {
	stall = make(chan struct{})
	app := liveApp()
	app.Live("/stalled", &stalled{})
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	drained(t, app)
	c, _ := joined(t, srv, "/stalled")
	ctx := context.Background()
	var want []string
	for i := range 30 {
		want = append(want, fmt.Sprintf("m%d", i))
		app.Broadcast(ctx, "room:stalled", said{want[i]})
		time.Sleep(time.Millisecond)
	}
	close(stall)
	patches := 0
	for {
		m := must(t, c)
		patches++
		got := logOf(m)
		if !strings.HasPrefix(strings.Join(want, "|"), got) || got == "" {
			t.Fatalf("patch %d shows %q: not a prefix, in order, of the messages sent", patches, got)
		}
		if got == strings.Join(want, "|") {
			break
		}
	}
	if patches > 2 {
		t.Errorf("30 broadcasts, 29 of them waiting for the first, made %d patches, want at most 2", patches)
	}
}

// A flood of broadcasts leaves the state exact, whichever way they batch.
func TestLiveBroadcastFloodKeepsStateExact(t *testing.T) {
	app := liveApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	drained(t, app)
	c, _ := joined(t, srv, "/chat/go")
	ctx := context.Background()
	const n = 400
	var want []string
	go func() {
		for i := range n {
			app.Broadcast(ctx, "room:go", said{fmt.Sprint("m", i)})
		}
	}()
	for i := range n {
		want = append(want, fmt.Sprint("m", i))
	}
	full := strings.Join(want, "|")
	for {
		m := must(t, c)
		got := logOf(m)
		if !strings.HasPrefix(full, got) {
			t.Fatalf("a patch shows %.60q…: messages out of order or lost", got)
		}
		if got == full {
			return
		}
	}
}
