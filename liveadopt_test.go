package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// The join of a live page adopts the session its GET mounted (livepark.go):
// Mount runs once per page view. meter renders which Mount call, counted
// across the app, made its instance, so a page's HTML says whether its join
// mounted afresh (the GET's count + 1) or adopted (the GET's own count).

var meterMounts atomic.Int32

type meter struct {
	ui.Live
	m int32 // which app-wide Mount made this instance
	n int   // clicks, for the browser test
}

func (c *meter) LiveSchema(l *ui.LiveSchema)   { l.Mount(c.mount) }
func (c *meter) mount(_ context.Context) error { c.m = meterMounts.Add(1); return nil }
func (c *meter) Inc()                          { c.n++ }
func (c *meter) Render() ui.Node {
	return ui.El("div", nil,
		ui.El("span", nil, ui.Text(fmt.Sprintf("m%d n%d", c.m, c.n))),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(c.Inc)}}, ui.Text("+")))
}

// meterApp is an app serving meter at /meter, the app-wide count reset.
func meterApp(opts ...Option) *App {
	meterMounts.Store(0)
	app := New(append([]Option{Only("none")}, opts...)...)
	app.Live("/meter", &meter{})
	app.Live("/chat/{room}", &chat{})
	app.Live("/guarded", &meter{}, Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := r.Cookie("user"); err != nil {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			h.ServeHTTP(w, r)
		})
	}))
	return app
}

func TestLiveAdoptMountOnce(t *testing.T) {
	srv := httptest.NewServer(meterApp().Handler())
	if _, html := joined(t, srv, "/meter"); !strings.Contains(html, "m1 ") {
		t.Fatalf("the join mounted again: %s", html)
	}
	srv.Close()

	// adoption off: the join mounts fresh, as before
	srv0 := httptest.NewServer(meterApp(LiveAdopt(0)).Handler())
	defer srv0.Close()
	if _, html := joined(t, srv0, "/meter"); !strings.Contains(html, "m2 ") {
		t.Fatalf("LiveAdopt(0) still adopted: %s", html)
	}
}

func TestLiveAdoptWindowExpires(t *testing.T) {
	srv := httptest.NewServer(meterApp(LiveAdopt(30 * time.Millisecond)).Handler())
	defer srv.Close()
	_, tok := pageToken(t, srv, "/meter")
	time.Sleep(200 * time.Millisecond)
	c, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1})
	if m := must(t, c); !strings.Contains(m.HTML, "m2 ") {
		t.Fatalf("an expired park was adopted: %+v", m)
	}
}

func TestLiveAdoptOnce(t *testing.T) {
	srv := httptest.NewServer(meterApp().Handler())
	defer srv.Close()
	_, tok := pageToken(t, srv, "/meter")
	for i, want := range []string{"m1 ", "m2 "} {
		c, err := dial(t, srv, tok, nil)
		if err != nil {
			t.Fatal(err)
		}
		send(t, c, map[string]any{"type": "join", "ref": 1})
		if m := must(t, c); !strings.Contains(m.HTML, want) {
			t.Fatalf("join %d of one token: want %q in %+v", i+1, want, m)
		}
		c.CloseNow()
	}
}

func TestLiveAdoptIdentity(t *testing.T) {
	who := LiveIdentity(func(ctx context.Context) string {
		if r := Request(ctx); r != nil {
			if c, err := r.Cookie("user"); err == nil {
				return c.Value
			}
		}
		return ""
	})
	for _, tc := range []struct {
		render, join, want string
	}{
		{"user=ann", "user=ann", "m1 "}, // the same user adopts
		{"user=ann", "user=bob", "m2 "}, // another user mounts fresh
		{"user=ann", "", "m2 "},         // signed out since the render
		{"", "", "m1 "},                 // anonymous both times
	} {
		srv := httptest.NewServer(meterApp(who).Handler())
		defer srv.Close()
		_, tok := pageToken(t, srv, "/meter", "Cookie", tc.render)
		h := http.Header{"Origin": {srv.URL}}
		if tc.join != "" {
			h.Set("Cookie", tc.join)
		}
		c, err := dial(t, srv, tok, h)
		if err != nil {
			t.Fatal(err)
		}
		send(t, c, map[string]any{"type": "join", "ref": 1})
		if m := must(t, c); !strings.Contains(m.HTML, tc.want) {
			t.Fatalf("render as %q, join as %q: want %q in %+v", tc.render, tc.join, tc.want, m)
		}
		c.CloseNow()
	}
}

// A parked session for another URI than the token's is never adopted — a
// defensive check; the park key is the signed token, which names the URI.
func TestLiveAdoptURIMismatch(t *testing.T) {
	app := meterApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	_, tok := pageToken(t, srv, "/meter")
	p := app.parked.take(tok)
	if p == nil {
		t.Fatal("nothing parked")
	}
	p.uri = "/elsewhere"
	app.parked.park(tok, p, time.Minute)
	c, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1})
	if m := must(t, c); !strings.Contains(m.HTML, "m2 ") {
		t.Fatalf("a parked session for another URI was adopted: %+v", m)
	}
}

// Subscriptions made by Mount during the GET reach the connection once the
// join adopts the session: broadcasts arrive without mounting again.
func TestLiveAdoptKeepsSubscriptions(t *testing.T) {
	srv := httptest.NewServer(liveApp().Handler())
	defer srv.Close()
	ann, _ := joined(t, srv, "/chat/go") // Mount (GET) subscribed; the join adopted
	bob, html := joined(t, srv, "/chat/go")
	send(t, bob, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "submit", 0), "event": "submit", "form": map[string][]string{"text": {"hi"}}})
	must(t, bob)
	m, err := recv(ann, 5*time.Second)
	if err != nil || len(m.Updates) == 0 || !strings.Contains(m.Updates[0].HTML, "hi") {
		t.Fatalf("the adopted page missed the broadcast: %+v %v", m, err)
	}
}

// An adopted v2 join renders the same trees as a fresh one: the browser's
// DOM comes out the same whether or not the GET's session was adopted.
func TestLiveAdoptV2SameRender(t *testing.T) {
	replies := map[bool]string{}
	for _, adopt := range []bool{true, false} {
		opts := []Option{}
		if !adopt {
			opts = append(opts, LiveAdopt(0))
		}
		srv := httptest.NewServer(liveApp(opts...).Handler())
		_, tok := pageToken(t, srv, "/counter?start=7")
		c, err := dial(t, srv, tok, nil)
		if err != nil {
			t.Fatal(err)
		}
		send(t, c, map[string]any{"type": "join", "ref": 1, "v": 2})
		m, raw := recv2(t, c)
		if m.Type != "render" || m.V != 2 {
			t.Fatalf("v2 join (adopt %v): %s", adopt, raw)
		}
		replies[adopt] = raw
		// the page still works: an event patches
		send(t, c, map[string]any{"type": "event", "ref": 2, "target": "c1:0", "event": "click"})
		if m, raw := recv2(t, c); m.Type != "patch" || !strings.Contains(raw, "8") {
			t.Fatalf("event after join (adopt %v): %s", adopt, raw)
		}
		c.CloseNow()
		srv.Close()
	}
	if replies[true] != replies[false] {
		t.Fatalf("an adopted join renders differently:\nadopted: %s\nfresh:   %s", replies[true], replies[false])
	}
}

// The join still runs the route through middleware and gates: a visitor
// whose credential went away between the render and the join is refused,
// parked session or not.
func TestLiveAdoptGatesStillRun(t *testing.T) {
	srv := httptest.NewServer(meterApp().Handler())
	defer srv.Close()
	_, tok := pageToken(t, srv, "/guarded", "Cookie", "user=ann")
	if tok == "" {
		t.Fatal("no token on the guarded page")
	}
	c, err := dial(t, srv, tok, http.Header{"Origin": {srv.URL}}) // no cookie now
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1})
	if m := must(t, c); m.Type != "redirect" || m.URL != "/login" {
		t.Fatalf("the gate didn't run on the join: %+v", m)
	}
}

// A real browser: the page loads, joins and clicks work, and its one
// instance came from the GET's Mount (a fresh join would show m2).
func TestBrowserAdoptMountOnce(t *testing.T) {
	srv := httptest.NewServer(meterApp().Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/meter")
	p.Wait(`!!document.documentElement.dataset.vkProtocol`)
	p.WaitText("span", "m1 n0") // joined, and Mount ran once
	p.Click("button")
	p.WaitText("span", "m1 n1")
	p.Click("button")
	p.WaitText("span", "m1 n2")
}

// tabbed defaults its URL field in its mount hook.
type tabbed struct {
	ui.Live
	Tab string
}

func (c *tabbed) LiveSchema(l *ui.LiveSchema) {
	l.Field(&c.Tab, ui.URL("tab"))
	l.Mount(c.mount)
}
func (c *tabbed) mount(context.Context) error {
	if c.Tab == "" {
		c.Tab = "first"
	}
	return nil
}
func (c *tabbed) Render() ui.Node { return ui.El("p", nil, ui.Text("tab "+c.Tab)) }

// A page opened without its URL fields keeps what its mount hook set on
// them once its join adopts it.
func TestLiveAdoptKeepsMountedURLFields(t *testing.T) {
	app := New(Only("none"))
	app.Live("/tabs", &tabbed{})
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	if _, html := joined(t, srv, "/tabs"); !strings.Contains(html, "tab first") {
		t.Fatalf("the join lost the mount hook's default: %s", html)
	}
}
