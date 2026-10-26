package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
)

// errUsers is what both a Mount and a call in markup fail with.
var errUsers = errors.New("users: dial tcp 10.0.0.7:5432: connection refused")

// usersPage fails in its Mount (?fail=mount), in its markup's call of
// users (?fail=markup), or, once its Break event has run, in the markup's
// call at the re-render; ?status=1 makes the error a *web.Error.
type usersPage struct {
	ui.Live
	Fail   string
	Status string
	broken bool
}

func (p *usersPage) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }

func (p *usersPage) err() error {
	if p.Status != "" {
		return Status(http.StatusServiceUnavailable, "users are unavailable")
	}
	return errUsers
}

func (p *usersPage) mount(context.Context) error {
	if p.Fail == "mount" {
		return p.err()
	}
	return nil
}

func (p *usersPage) users() ([]string, error) {
	if p.Fail == "markup" || p.broken {
		return nil, p.err()
	}
	return []string{"ann", "bob"}, nil
}

func (p *usersPage) Break() { p.broken = true }

func (p *usersPage) Fails() error { return p.err() }

// Render is what the compiler writes for
//
//	<main>
//	  <button onClick={Break}>break</button>
//	  <button onClick={Fails}>fail</button>
//	  for _, u := range users() { <li>{u}</li> }
//	</main>
func (p *usersPage) Render() ui.Node {
	var lis []ui.Node
	for _, u := range ui.Must(p.users()) {
		lis = append(lis, ui.HTML(`<li>{u}</li>`, u))
	}
	return ui.HTML(`<main><button onClick={p.Break}>break</button><button onClick={p.Fails}>fail</button>{lis}</main>`, p.Break, p.Fails, lis)
}

// usersList is a shape a handler returns, its View calling users as it
// renders.
type usersList struct{ p *usersPage }

func (l usersList) View() ui.Node {
	return ui.HTML(`<ul>{ui.Must(l.p.users())}</ul>`, ui.Must(l.p.users()))
}

func (l usersList) Render(ctx context.Context, w io.Writer) error { return l.View().Render(ctx, w) }

// A call in markup returning (T, error) fails the render as Mount does:
// over HTTP the same status and page — the error's text only in
// development, the error in the log —, and on a live re-render the same
// error message a failing join's Mount gets.
func TestMarkupErrorIsMountError(t *testing.T) {
	get := func(t *testing.T, srv *httptest.Server, path string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Accept", "text/html")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	for _, dev := range []bool{false, true} {
		opts := []Option{Only("none")}
		if dev {
			opts = append(opts, Dev(), NoToolbar())
		}
		app := New(opts...)
		app.Live("/p", &usersPage{})
		app.Get("/list", func(r *http.Request) ui.Node {
			return usersList{&usersPage{Fail: r.URL.Query().Get("fail"), Status: r.URL.Query().Get("status")}}
		})
		// A handler's markup, run as the handler returns it.
		app.Get("/eager", func(r *http.Request) ui.Node {
			p := &usersPage{Fail: r.URL.Query().Get("fail"), Status: r.URL.Query().Get("status")}
			return ui.HTML(`<ul>{ui.Must(p.users())}</ul>`, ui.Must(p.users()))
		})
		srv := httptest.NewServer(app.Handler())
		for _, status := range []string{"", "&status=1"} {
			wantCode, wantBody := get(t, srv, "/p?fail=mount"+status)
			for _, path := range []string{"/p?fail=markup" + status, "/list?fail=markup" + status, "/eager?fail=markup" + status} {
				code, body := get(t, srv, path)
				if code != wantCode || body != wantBody {
					t.Errorf("dev %v: %s: %d %q, want Mount's %d %q", dev, path, code, body, wantCode, wantBody)
				}
			}
			detail := strings.Contains(wantBody, "connection refused")
			if status == "" && (wantCode != 500 || detail != dev) || status != "" && (wantCode != 503 || !strings.Contains(wantBody, "users are unavailable")) {
				t.Errorf("dev %v: Mount's error: %d %q", dev, wantCode, wantBody)
			}
		}
		if code, body := get(t, srv, "/list"); code != 200 || !strings.Contains(body, "<ul>[ann bob]</ul>") {
			t.Errorf("dev %v: /list: %d %q", dev, code, body)
		}

		// Live: a join whose Mount fails, and an event after which the
		// re-render's call fails, get the same message.
		c, err := dial(t, srv, app.signToken("/p?fail=mount"), nil)
		if err != nil {
			t.Fatal(err)
		}
		send(t, c, map[string]any{"type": "join", "ref": 1})
		mounted := must(t, c)
		if mounted.Type != live.Error || strings.Contains(mounted.Error, "connection refused") != dev {
			t.Fatalf("dev %v: Mount's error at the join: %+v", dev, mounted)
		}
		c, html := joined(t, srv, "/p")
		send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", 1), "event": "click"})
		failed := must(t, c)
		send(t, c, map[string]any{"type": "event", "ref": 3, "target": handlerOf(t, html, "click", 0), "event": "click"})
		broken := must(t, c)
		if broken.Type != live.Error || broken.Error != mounted.Error || failed.Error != mounted.Error {
			t.Errorf("dev %v: the re-render's error %+v, a handler's %+v, want Mount's %q", dev, broken, failed, mounted.Error)
		}
		srv.Close()
	}
}
