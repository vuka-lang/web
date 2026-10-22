package web

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
)

// secOrders records the orders the pages' events cancelled.
var secOrders struct {
	sync.Mutex
	mounts, cancelled []string
}

// secOrder is /orders/{id}; the visitor may see order 1 only, which Mount
// checks.
type secOrder struct {
	ui.Live
	ID string
}

func (o *secOrder) LiveSchema(l *ui.LiveSchema) { l.Mount(o.mount) }
func (o *secOrder) mount(_ context.Context) error {
	secOrders.Lock()
	secOrders.mounts = append(secOrders.mounts, o.ID)
	secOrders.Unlock()
	if o.ID != "1" {
		return Status(http.StatusForbidden, "not your order")
	}
	return nil
}

func (o *secOrder) Cancel() {
	secOrders.Lock()
	secOrders.cancelled = append(secOrders.cancelled, o.ID)
	secOrders.Unlock()
}

func (o *secOrder) Render() ui.Node {
	return ui.El("main", nil, ui.Text("order "+o.ID),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(o.Cancel)}}, ui.Text("cancel")))
}

// secCheckedOrder takes a patch's URL in an Update, which checks again.
type secCheckedOrder struct{ secOrder }

type secOrderParams struct {
	URLParams
	ID string
}

func (o *secCheckedOrder) Update(p secOrderParams) error {
	if p.ID != "1" {
		return Status(http.StatusForbidden, "not your order")
	}
	return nil
}

// A patch the browser asks for doesn't skip Mount: a page whose type has no
// Update taking URLParams mounts again, and Mount refuses another visitor's
// order; one with such an Update keeps its instance only when the Update
// takes the URL. Either way no event acts on the refused order.
func TestLivePatchMountsUnlessUpdate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		comp   ui.Stateful
		mounts string
	}{
		// the GETs of /orders/2 and /orders/1 (the join adopts the GET's
		// mount), then the patch's
		{"no Update", &secOrder{}, "[2 1 2]"},
		{"Update taking URLParams", &secCheckedOrder{}, "[2 1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secOrders.Lock()
			secOrders.mounts, secOrders.cancelled = nil, nil
			secOrders.Unlock()
			app := New(Only("none"))
			app.Live("/orders/{id}", tc.comp)
			srv := httptest.NewServer(app.Handler())
			defer srv.Close()
			if res, err := http.Get(srv.URL + "/orders/2"); err != nil || res.StatusCode != http.StatusForbidden {
				t.Fatalf("GET /orders/2: %v %v", res, err)
			}
			c, html := joined(t, srv, "/orders/1")
			target := handlerOf(t, html, "click", 0)
			send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/orders/2", "patch": true})
			if m := must(t, c); m.Type != live.Error || m.Error != "not your order" {
				t.Fatalf("a patch to another visitor's order: %+v", m)
			}
			send(t, c, map[string]any{"type": "event", "ref": 3, "target": target, "event": "click"})
			must(t, c)
			secOrders.Lock()
			defer secOrders.Unlock()
			if len(secOrders.cancelled) != 0 || len(secOrders.mounts) == 0 || "["+strings.Join(secOrders.mounts, " ")+"]" != tc.mounts {
				t.Fatalf("cancelled %v, mounts %v (want %s)", secOrders.cancelled, secOrders.mounts, tc.mounts)
			}
		})
	}
}

// A redirect the browser would run (an app's open redirect to a
// javascript: URL) is never sent: neither for a navigation nor a join.
func TestLiveRedirectOnlyHTTP(t *testing.T) {
	app := New(Only("none"))
	app.Get("/out", func(r *http.Request) Redirect { return Redirect(r.URL.Query().Get("to")) })
	app.Live("/counter", &counter{})
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	c, _ := joined(t, srv, "/counter")
	for to, ok := range map[string]bool{
		"/elsewhere": true, "https://example.com/": true,
		"javascript:void(window.pwned=1)": false, "JavaScript:alert(1)": false, "data:text/html,x": false, "vbscript:x": false,
	} {
		send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/out?to=" + to})
		m := must(t, c)
		if ok && (m.Type != live.Redirect || m.URL != to) || !ok && (m.Type != live.Error || strings.Contains(m.URL+m.Error, to)) {
			t.Errorf("redirect to %q: %+v", to, m)
		}
	}
	for _, to := range []string{"javascript:alert(1)", "data:text/html,x"} {
		send(t, c, map[string]any{"type": "navigate", "ref": 3, "url": to})
		if m := must(t, c); m.Type != live.Error {
			t.Errorf("navigate to %q: %+v", to, m)
		}
	}
	c2, err := dial(t, srv, app.signToken("/out?to=javascript:alert(1)"), nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c2, map[string]any{"type": "join", "ref": 1})
	if m := must(t, c2); m.Type != live.Error || strings.Contains(m.URL, "javascript") {
		t.Fatalf("join redirecting to javascript: %+v", m)
	}
}

// secErrs is a page whose handlers and Mount fail in ways whose text
// shouldn't reach the browser.
type secErrs struct {
	ui.Live
	Fail string // ?fail=mount
}

func (p *secErrs) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }
func (p *secErrs) mount(_ context.Context) error {
	if p.Fail == "mount" {
		return errors.New(`dial tcp 10.0.0.7:5432: password authentication failed for user "app"`)
	}
	return nil
}

func (p *secErrs) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(func() error {
			return errors.New(`pq: duplicate key (SELECT password_hash FROM users WHERE email='a@b.c')`)
		})}}),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(func() { panic("open /srv/app/secrets.env: permission denied") })}}),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(func() error { return BadRequest("that email is taken") })}}),
		ui.El("input", []ui.Attr{{Name: "onInput", Value: ui.On(func(n int) {})}}))
}

// Outside development a live page's errors reach the browser as an HTTP
// route's do: a *web.Error's message, else "Internal Server Error", the
// error itself going to the log with the page and the handler.
func TestLiveErrorsHidden(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	for _, dev := range []bool{false, true} {
		logs.Reset()
		opts := []Option{Only("none")}
		if dev {
			opts = append(opts, Dev(), NoToolbar())
		}
		app := New(opts...)
		app.Live("/p", &secErrs{})
		srv := httptest.NewServer(app.Handler())
		c, html := joined(t, srv, "/p")
		for i, want := range []string{"password_hash", "secrets.env", "that email is taken"} {
			send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", i), "event": "click"})
			m := must(t, c)
			if hidden := !dev && i < 2; hidden && m.Error != "Internal Server Error" || !hidden && !strings.Contains(m.Error, want) {
				t.Errorf("dev %v: %s: %q", dev, want, m.Error)
			}
		}
		send(t, c, map[string]any{"type": "event", "ref": 3, "target": handlerOf(t, html, "input", 0), "event": "input", "value": "x"})
		if m := must(t, c); !strings.Contains(m.Error, `"x" isn't a whole number`) {
			t.Errorf("dev %v: a field that doesn't parse: %q", dev, m.Error)
		}
		_, tok := pageToken(t, srv, "/p")
		c2, err := dial(t, srv, strings.Replace(tok, tok, app.signToken("/p?fail=mount"), 1), nil)
		if err != nil {
			t.Fatal(err)
		}
		send(t, c2, map[string]any{"type": "join", "ref": 1})
		if m := must(t, c2); dev != strings.Contains(m.Error, "password authentication") {
			t.Errorf("dev %v: Mount's error at the join: %+v", dev, m)
		}
		srv.Close()
		for _, want := range []string{"password_hash", "secrets.env", "password authentication", "page=/p", "handler="} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("dev %v: the log lacks %q:\n%s", dev, want, logs.String())
			}
		}
		if strings.Contains(logs.String(), "that email is taken") {
			t.Errorf("a public error is the browser's, not the log's:\n%s", logs.String())
		}
	}
}

// The live socket's limits: per page token, per client address, in all,
// a join deadline, an idle one, and the checks run periodically and
// before broadcasts.
func TestLiveLimits(t *testing.T) {
	t.Run("per page", func(t *testing.T) {
		app := liveApp(LiveMaxConnsPerPage(2))
		srv := httptest.NewServer(app.Handler())
		defer srv.Close()
		_, tok := pageToken(t, srv, "/counter")
		for i := 0; i < 2; i++ {
			if _, err := dial(t, srv, tok, nil); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := dial(t, srv, tok, nil); err == nil || !strings.Contains(err.Error(), "429") {
			t.Fatalf("a third socket of one page: %v", err)
		}
		_, other := pageToken(t, srv, "/counter?start=3")
		if _, err := dial(t, srv, other, nil); err != nil {
			t.Fatalf("another page: %v", err)
		}
	})
	t.Run("per address and in all", func(t *testing.T) {
		for _, tc := range []struct {
			opt    Option
			status string
		}{{LiveMaxConnsPerIP(3), "429"}, {LiveMaxConns(3), "503"}} {
			app := liveApp(tc.opt)
			srv := httptest.NewServer(app.Handler())
			for i := 0; i < 3; i++ {
				_, tok := pageToken(t, srv, "/counter?start="+string(rune('0'+i)))
				if _, err := dial(t, srv, tok, nil); err != nil {
					t.Fatal(err)
				}
			}
			_, tok := pageToken(t, srv, "/counter?start=9")
			if _, err := dial(t, srv, tok, nil); err == nil || !strings.Contains(err.Error(), tc.status) {
				t.Fatalf("over the cap: %v", err)
			}
			dropConnections(app)
			waitFor(t, func() bool { app.hub.mu.Lock(); defer app.hub.mu.Unlock(); return app.hub.open == 0 })
			if _, err := dial(t, srv, tok, nil); err != nil {
				t.Fatalf("a place freed: %v", err)
			}
			srv.Close()
		}
	})
	t.Run("per address behind a trusted proxy", func(t *testing.T) {
		app := liveApp(LiveMaxConnsPerIP(1), TrustedProxies("127.0.0.1"))
		srv := httptest.NewServer(app.Handler())
		defer srv.Close()
		from := func(ip string) http.Header {
			return http.Header{"Origin": {srv.URL}, "X-Forwarded-For": {ip}}
		}
		for i, ip := range []string{"203.0.113.1", "203.0.113.2"} {
			_, tok := pageToken(t, srv, "/counter?start="+string(rune('0'+i)))
			if _, err := dial(t, srv, tok, from(ip)); err != nil {
				t.Fatalf("%s: %v", ip, err)
			}
		}
		_, tok := pageToken(t, srv, "/counter?start=9")
		if _, err := dial(t, srv, tok, from("203.0.113.1")); err == nil || !strings.Contains(err.Error(), "429") {
			t.Fatalf("a second socket from one client behind the proxy: %v", err)
		}
	})
	t.Run("join deadline", func(t *testing.T) {
		app := liveApp(LiveJoinTimeout(200 * time.Millisecond))
		srv := httptest.NewServer(app.Handler())
		defer srv.Close()
		_, tok := pageToken(t, srv, "/counter")
		c, err := dial(t, srv, tok, nil)
		if err != nil {
			t.Fatal(err)
		}
		send(t, c, map[string]any{"type": "event", "ref": 1, "target": "c1:0", "event": "click"}) // not a join
		must(t, c)
		if _, err := recv(c, 2*time.Second); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("no join: %v", err)
		}
	})
	t.Run("idle", func(t *testing.T) {
		app := liveApp(LiveIdleTimeout(300 * time.Millisecond))
		srv := httptest.NewServer(app.Handler())
		defer srv.Close()
		c, html := joined(t, srv, "/counter")
		for i := 0; i < 3; i++ { // messages keep it open
			time.Sleep(150 * time.Millisecond)
			send(t, c, map[string]any{"type": "event", "ref": 2 + i, "target": handlerOf(t, html, "click", 0), "event": "click"})
			must(t, c)
		}
		if _, err := recv(c, 2*time.Second); websocket.CloseStatus(err) != closeIdle {
			t.Fatalf("idle: %v", err)
		}
	})
	t.Run("checks", func(t *testing.T) {
		var mu sync.Mutex
		ended := false
		check := LiveCheck(func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			if ended {
				return errors.New("ended")
			}
			return nil
		})
		end := func(v bool) { mu.Lock(); ended = v; mu.Unlock() }

		app := liveApp(check, LiveCheckEvery(100*time.Millisecond))
		srv := httptest.NewServer(app.Handler())
		defer srv.Close()
		c, _ := joined(t, srv, "/counter")
		end(true)
		if _, err := recv(c, 2*time.Second); websocket.CloseStatus(err) != closeReload {
			t.Fatalf("periodic check: %v", err)
		}

		// Messages and broadcasts trust what the page mounted with: no
		// checks, only the periodic ones (none here).
		end(false)
		var calls atomic.Int32
		count := LiveCheck(func(context.Context) error { calls.Add(1); return nil })
		app3 := liveApp(count, LiveCheckEvery(0))
		srv3 := httptest.NewServer(app3.Handler())
		defer srv3.Close()
		c3, _ := joined(t, srv3, "/chat/x")
		app3.Broadcast(context.Background(), "room:x", said{"hi"})
		must(t, c3)
		send(t, c3, map[string]any{"type": "navigate", "ref": 2, "url": "/chat/y"})
		must(t, c3)
		if n := calls.Load(); n != 0 {
			t.Fatalf("checks for a join, a broadcast and a navigation: %d", n)
		}
	})
	t.Run("CloseLive", func(t *testing.T) {
		app := liveApp(LiveCheckEvery(0))
		srv := httptest.NewServer(app.Handler())
		defer srv.Close()
		x, _ := joined(t, srv, "/chat/x")
		y, _ := joined(t, srv, "/chat/y")
		n := app.CloseLive(func(ctx context.Context) bool { return Request(ctx).URL.Path == "/chat/x" })
		if n != 1 {
			t.Errorf("CloseLive closed %d pages", n)
		}
		if _, err := recv(x, 2*time.Second); websocket.CloseStatus(err) != closeReload {
			t.Fatalf("the matched page: %v", err)
		}
		app.Broadcast(context.Background(), "room:y", said{"hi"})
		if m, err := recv(y, 2*time.Second); err != nil || m.Type == "" {
			t.Fatalf("the other page: %+v %v", m, err)
		}
	})
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}
