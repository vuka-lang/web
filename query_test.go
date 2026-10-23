package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"net/http/httptest"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/ui/v2/uigen"
)

// urlTable keeps its page in the URL, under its tag's urlKey.
type urlTable struct {
	ui.Live
	Name string
	page uigen.Assign[int]
}

func (t *urlTable) LiveSchema(l *ui.LiveSchema) { l.Field(&t.page, ui.URL("page").Default(1)) }
func (t *urlTable) Next()                       { t.page.Set(t.page.Get() + 1) }
func (t *urlTable) Render() ui.Node {
	return ui.El("p", []ui.Attr{{Name: "onClick", Value: ui.On(t.Next)}}, ui.Text(fmt.Sprintf("%s page %d", t.Name, t.page.Get())))
}

// urlTables is a Live page holding two tables, and a status filter it sets
// with Query; it stays mounted as its URL changes.
type urlTables struct {
	ui.Live
	Status string
	mounts int
}

func (p *urlTables) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }
func (p *urlTables) mount(context.Context) error { p.mounts++; return nil }
func (p *urlTables) Update(URLParams)            {}
func (p *urlTables) Filter(ctx context.Context) {
	Query.Set(ctx, "status", "open")
	Query.Del(ctx, "requests.page")
}
func (p *urlTables) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Filter)}}, ui.Text(fmt.Sprintf("status %s mounts %d", p.Status, p.mounts))),
		ui.URLKey("requests", ui.Component("t#1", nil, &urlTable{Name: "requests"})),
		ui.URLKey("users", ui.Component("t#2", nil, &urlTable{Name: "users"})))
}

func TestLiveURLState(t *testing.T) {
	app := New(Only("none"))
	app.Live("/tables", &urlTables{})
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	html, _ := pageToken(t, srv, "/tables?requests.page=3&x=1")
	if !strings.Contains(html, "requests page 3") || !strings.Contains(html, "users page 1") {
		t.Fatalf("the first render fills the fields from the URL: %s", html)
	}
	c, page := joined(t, srv, "/tables?requests.page=3&x=1")
	event := func(ref int, target string) live.ServerMessage {
		t.Helper()
		send(t, c, map[string]any{"type": "event", "ref": ref, "target": target, "event": "click"})
		return must(t, c)
	}
	// the patch an event's reply carries
	patchOf := func(m live.ServerMessage) (string, bool) {
		t.Helper()
		for _, p := range m.JS {
			var ops [][]json.RawMessage
			json.Unmarshal(p.Ops, &ops)
			for _, op := range ops {
				var name string
				json.Unmarshal(op[0], &name)
				var a struct {
					Href    string `json:"href"`
					Replace bool   `json:"replace"`
				}
				json.Unmarshal(op[1], &a)
				if name == "patch" {
					return a.Href, a.Replace
				}
			}
		}
		t.Fatalf("no patch in %+v", m)
		return "", false
	}
	nav := func(ref int, url string) live.ServerMessage {
		t.Helper()
		send(t, c, map[string]any{"type": "navigate", "ref": ref, "url": url, "patch": true})
		m := must(t, c)
		if m.Type != live.PatchMsg {
			t.Fatalf("navigate %s: %+v", url, m)
		}
		return m
	}

	// The users table's next page: the URL keeps the other parameters, in
	// place of the current entry.
	m := event(2, handlerOf(t, page, "click", 2))
	href, replace := patchOf(m)
	if href != "/tables?requests.page=3&users.page=2&x=1" || !replace {
		t.Fatalf("patch %q replace %v", href, replace)
	}
	if !strings.Contains(fmt.Sprint(m.Updates), "users page 2") {
		t.Fatalf("the event's reply shows the new page: %+v", m)
	}
	m = nav(3, href)
	if strings.Contains(fmt.Sprint(m.Updates), "mounts 2") {
		t.Fatalf("an Update taking URLParams keeps the page mounted: %+v", m)
	}

	// Query edits from the page's event, as one patch.
	m = event(4, handlerOf(t, page, "click", 0))
	if href, _ := patchOf(m); href != "/tables?status=open&users.page=2&x=1" {
		t.Fatalf("Query's patch %q", href)
	}
	m = nav(5, "/tables?status=open&users.page=2&x=1")
	if s := fmt.Sprint(m.Updates); !strings.Contains(s, "status open mounts 1") || !strings.Contains(s, "requests page 1") {
		t.Fatalf("the patch binds the page and fills the tables: %s", s)
	}

	// Back to the first URL (popstate's patch): the tables follow it.
	m = nav(6, "/tables?requests.page=3&x=1")
	if s := fmt.Sprint(m.Updates); !strings.Contains(s, "requests page 3") || !strings.Contains(s, "users page 1") {
		t.Fatalf("back: %s", s)
	}
}

func TestQueryOutsideAnEvent(t *testing.T) {
	Query.Set(context.Background(), "a", 1) // nothing to edit: does nothing
	if Query.Get(context.Background(), "a") != "" {
		t.Fatal("no query")
	}
}
