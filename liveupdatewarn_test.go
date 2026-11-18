package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// warnLog collects the records slog's default logger is given.
type warnLog struct {
	mu   sync.Mutex
	msgs []string
}

func (h *warnLog) Enabled(context.Context, slog.Level) bool { return true }
func (h *warnLog) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *warnLog) WithGroup(string) slog.Handler            { return h }
func (h *warnLog) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String() + " " + r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	h.mu.Lock()
	h.msgs = append(h.msgs, b.String())
	h.mu.Unlock()
	return nil
}

func (h *warnLog) take() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.msgs
	h.msgs = nil
	return out
}

// In development a patch navigation to a live page without an Update says
// so, once for each route, in the log and the toolbar's Logs panel; a page
// with an Update, a navigation that asks for no patch and an app that isn't
// in development say nothing.
func TestLivePatchWithoutUpdateWarns(t *testing.T) {
	withToolbar(t)
	logs := &warnLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	run := func(t *testing.T, opts ...Option) (*App, *httptest.Server) {
		app := New(append([]Option{Only("none")}, opts...)...)
		app.Live("/orders/{id}", &secOrder{})
		app.Live("/checked/{id}", &secCheckedOrder{})
		srv := httptest.NewServer(app.Handler())
		t.Cleanup(srv.Close)
		return app, srv
	}
	patch := func(t *testing.T, srv *httptest.Server, from, to string, ref int, patch bool) {
		t.Helper()
		c, _ := joined(t, srv, from)
		defer c.CloseNow()
		send(t, c, map[string]any{"type": "navigate", "ref": ref, "url": to, "patch": patch})
		must(t, c)
	}

	t.Run("development", func(t *testing.T) {
		_, srv := run(t, Dev())
		patch(t, srv, "/orders/1", "/orders/1?tab=2", 2, true)
		got := logs.take()
		if len(got) != 1 || !strings.HasPrefix(got[0], "WARN a patch link targets a live page without Update") ||
			!strings.Contains(got[0], "add Update(ctx, params)") || !strings.Contains(got[0], "route=/orders/{id}") {
			t.Fatalf("logged %q", got)
		}
		patch(t, srv, "/orders/1", "/orders/1?tab=3", 2, true)
		if got := logs.take(); len(got) != 0 {
			t.Errorf("said again for the same route: %q", got)
		}
		patch(t, srv, "/checked/1", "/checked/1?tab=2", 2, true)
		patch(t, srv, "/orders/1", "/orders/1?tab=4", 2, false)
		if got := logs.take(); len(got) != 0 {
			t.Errorf("a page with an Update, or no patch asked for: %q", got)
		}
	})

	t.Run("toolbar", func(t *testing.T) {
		app, srv := run(t, Dev())
		res, err := http.Get(srv.URL + "/orders/1")
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 1<<16)
		n, _ := res.Body.Read(b)
		res.Body.Close()
		id := res.Header.Get(ToolbarHeader)
		tok := tokenRe.FindStringSubmatch(string(b[:n]))
		if id == "" || tok == nil {
			t.Fatalf("not a live page with a toolbar: %s", b[:n])
		}
		c, err := dial(t, srv, tok[1]+"&tb="+id, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		send(t, c, map[string]any{"type": "join", "ref": 1})
		must(t, c)
		send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/orders/1?tab=2", "patch": true})
		must(t, c)
		// The page mounts again, so the toolbar moves to its record.
		now := liveRecord(t, app, id).Now
		if now == "" {
			t.Fatal("the toolbar didn't move to the page's record")
		}
		_, html := fetchRecord(t, app.Handler(), now)
		if !strings.Contains(html, "a patch link targets a live page without Update") {
			t.Errorf("the toolbar doesn't show it: %s", html)
		}
		logs.take()
	})

	t.Run("production", func(t *testing.T) {
		_, srv := run(t)
		patch(t, srv, "/orders/1", "/orders/1?tab=2", 2, true)
		if got := logs.take(); len(got) != 0 {
			t.Errorf("logged outside development: %q", got)
		}
	})
}
