package web

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// deferCtl steers a statsPage's loaders: rows wait for release (nil: they
// don't) and report a context ended first on ended.
type deferCtl struct {
	release chan struct{}
	ended   chan error
	fail    bool
}

// statsPage is a live page rendered by its own markup with two deferred
// groups, an embedded component deferring a third, and one more the page
// mounts on a click.
type statsPage struct {
	ui.Live
	Page  int
	ctl   *deferCtl
	stats map[string]int
	rows  []string
	more  bool
}

type statsParams struct {
	URLParams
	Page int `query:"page"`
}

func (p *statsPage) LiveSchema(l *ui.LiveSchema) {
	l.Field(&p.stats, ui.Defer("stats"))
	l.Field(&p.rows, ui.Defer("rows"))
	l.Loads("stats", p.loadStats)
	l.Loads("rows", p.loadRows)
}

func (p *statsPage) loadStats(ctx context.Context) error {
	if p.ctl.fail {
		return errors.New("stats: connection refused")
	}
	p.stats = map[string]int{"open": 3}
	return nil
}

func (p *statsPage) loadRows(ctx context.Context) error {
	if p.ctl.release != nil {
		select {
		case <-p.ctl.release:
		case <-ctx.Done():
			p.ctl.ended <- ctx.Err()
			return ctx.Err()
		}
	}
	p.rows = []string{"Ada", "Alan"}
	return nil
}

func (p *statsPage) Update(q statsParams) { p.Page = q.Page }
func (p *statsPage) More()                { p.more = true }

func (p *statsPage) Render() ui.Node {
	var stats, rows ui.Node
	switch {
	case p.Loading("stats"):
		stats = ui.El("div", []ui.Attr{{Name: "class", Value: "skeleton"}}, ui.Text("stats loading"))
	case p.LoadErr("stats") != nil:
		stats = ui.El("p", []ui.Attr{{Name: "role", Value: "alert"}}, ui.Text("stats failed: "+p.LoadErr("stats").Error()))
	default:
		stats = ui.Text(fmt.Sprintf("open %d", p.stats["open"]))
	}
	if p.Loading("rows") {
		rows = ui.Text("rows loading")
	} else {
		rows = ui.Text("rows " + strings.Join(p.rows, ","))
	}
	kids := []ui.Node{
		ui.El("section", []ui.Attr{{Name: "id", Value: "stats"}}, stats),
		ui.El("section", []ui.Attr{{Name: "id", Value: "rows"}}, rows),
		ui.El("p", []ui.Attr{{Name: "id", Value: "page"}}, ui.Child(p.Page)),
		ui.El("button", []ui.Attr{{Name: "id", Value: "more"}, {Name: "onClick", Value: ui.On(p.More)}}, ui.Text("more")),
		ui.Component("stats#users", nil, &usersCard{ID: "users"}),
	}
	if p.more {
		kids = append(kids, ui.Component("stats#more", nil, &usersCard{ID: "more-users"}))
	}
	return ui.El("main", nil, kids...)
}

// usersCard is an embedded live component deferring its own group.
type usersCard struct {
	ui.Live
	ID    string
	Users []string
}

func (c *usersCard) LiveSchema(l *ui.LiveSchema) {
	l.Field(&c.Users, ui.Defer("users"))
	l.Loads("users", c.loadUsers)
}

func (c *usersCard) loadUsers(ctx context.Context) error {
	c.Users = []string{"Grace"}
	return nil
}

func (c *usersCard) Render() ui.Node {
	text := "users loading"
	if !c.Loading("users") {
		text = "users " + strings.Join(c.Users, ",")
	}
	return ui.El("aside", []ui.Attr{{Name: "id", Value: c.ID}}, ui.Text(text))
}

func statsApp(ctl *deferCtl) *App {
	a := New(Only("none"))
	a.Live("/stats", func() *statsPage { return &statsPage{ctl: ctl} })
	a.Get("/elsewhere", func() ui.Node { return ui.El("main", nil, ui.Text("elsewhere")) })
	return a
}

// patchesUntil reads the connection's messages until the page as patched
// holds every want, failing on an error or a timeout.
func patchesUntil(t *testing.T, c *websocket.Conn, want ...string) string {
	t.Helper()
	var seen strings.Builder
	for {
		m, err := recv(c, 3*time.Second)
		if err != nil {
			t.Fatalf("waiting for %q: %v; got %s", want, err, seen.String())
		}
		if m.Type == live.Error {
			t.Fatalf("an error: %+v", m)
		}
		for _, u := range m.Updates {
			seen.WriteString(u.HTML)
		}
		all := true
		for _, w := range want {
			all = all && strings.Contains(seen.String(), w)
		}
		if all {
			return seen.String()
		}
	}
}

func TestLiveDeferredTemplate(t *testing.T) {
	ctl := &deferCtl{release: make(chan struct{}), ended: make(chan error, 1)}
	srv := httptest.NewServer(statsApp(ctl).Handler())
	defer srv.Close()

	// The first render: every group loading, whatever the loaders do.
	body, _ := pageToken(t, srv, "/stats")
	for _, want := range []string{"stats loading", "rows loading", "users loading"} {
		if !strings.Contains(body, want) {
			t.Errorf("the first render lacks %q: %s", want, body)
		}
	}

	c, html := joined(t, srv, "/stats")
	if !strings.Contains(html, "rows loading") {
		t.Errorf("the join: %s", html)
	}
	// stats and users arrive while rows still wait: the groups load
	// concurrently, each patching on its own.
	got := patchesUntil(t, c, "open 3", "users Grace")
	if strings.Contains(got, "rows Ada") {
		t.Errorf("rows came before their release: %s", got)
	}
	close(ctl.release)
	patchesUntil(t, c, "rows Ada,Alan")

	// A component an event mounts loads its own group.
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", 0), "event": "click"})
	patchesUntil(t, c, `id="more-users" data-vk-id="c3">users Grace`)
}

func TestLiveDeferredTemplateError(t *testing.T) {
	srv := httptest.NewServer(statsApp(&deferCtl{fail: true}).Handler())
	defer srv.Close()
	c, _ := joined(t, srv, "/stats")
	got := patchesUntil(t, c, `<p role="alert">stats failed: Internal Server Error</p>`, "rows Ada,Alan")
	if strings.Contains(got, "connection refused") {
		t.Errorf("the cause reached the browser: %s", got)
	}
}

// Leaving the page ends its loads; a patch keeps them.
func TestLiveDeferredTemplateNavigation(t *testing.T) {
	ctl := &deferCtl{release: make(chan struct{}), ended: make(chan error, 1)}
	srv := httptest.NewServer(statsApp(ctl).Handler())
	defer srv.Close()

	c, _ := joined(t, srv, "/stats")
	patchesUntil(t, c, "open 3")
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/stats?page=2", "patch": true})
	select {
	case err := <-ctl.ended:
		t.Fatalf("a patch ended the load: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(ctl.release)
	patchesUntil(t, c, `<p id="page">2</p>`, "rows Ada,Alan")

	ctl.release = make(chan struct{})
	c, _ = joined(t, srv, "/stats")
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/elsewhere"})
	select {
	case err := <-ctl.ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ended with %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leaving the page didn't end its load")
	}
}

// A page rendered for a form posted without script has no connection to
// receive its deferred fields: they load before it is sent.
type deferFormPage struct {
	ui.Live
	Form  joinForm
	stats int
}

func (p *deferFormPage) LiveSchema(l *ui.LiveSchema) {
	l.Field(&p.stats, ui.Defer("stats"))
	l.Loads("stats", p.loadStats)
}

func (p *deferFormPage) loadStats(ctx context.Context) error { p.stats = 42; return nil }
func (p *deferFormPage) Save(f joinForm) error               { return nil }

func (p *deferFormPage) Render() ui.Node {
	text := "stats loading"
	if !p.Loading("stats") {
		text = fmt.Sprint("stats ", p.stats)
	}
	return ui.El("main", nil, ui.El("p", nil, ui.Text(text)), ui.Form{For: ui.For(&p.Form), OnSubmit: p.Save})
}

func TestLiveDeferredTemplateFormPost(t *testing.T) {
	a := New(Only("none"))
	a.Live("/signup", func() *deferFormPage { return &deferFormPage{} })
	h := a.Handler()
	v, cookie := formOf(t, h, "/signup")
	v.Set("Name", "") // fails its check: the page again, with its errors
	code, body, _ := post(t, h, "/signup", v, "Cookie", cookie)
	if code != 422 || !strings.Contains(body, "stats 42") {
		t.Fatalf("%d: %s", code, body)
	}
}

func TestBrowserDeferredTemplate(t *testing.T) {
	ctl := &deferCtl{release: make(chan struct{}), ended: make(chan error, 1)}
	srv := httptest.NewServer(statsApp(ctl).Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Open(srv.URL + "/stats")
	p.WaitText("#stats", "open 3")
	p.WaitText("#users", "users Grace")
	if got := p.Text("#rows"); got != "rows loading" {
		t.Errorf("rows before their release: %q", got)
	}
	close(ctl.release)
	p.WaitText("#rows", "rows Ada,Alan")
	p.Click("#more")
	p.WaitText("#more-users", "users Grace")
}
