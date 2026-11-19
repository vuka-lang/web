package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
)

var boardDraws atomic.Int32

type boardRow struct {
	ui.Live
	N, Seq int
}

func (r *boardRow) Render() ui.Node {
	return ui.Fragment(ui.El("li", nil, ui.Text(fmt.Sprintf("%d:%d", r.N, r.Seq))), ui.El("li", nil, ui.Text("-")))
}

type wideRow struct {
	ui.Live
	Seq int
}

func (r *wideRow) Render() ui.Node {
	return ui.F(0x77, "Es(h)", ui.El("li", []ui.Attr{{Name: "class", Value: "wide"}}, ui.Text(fmt.Sprint("wide", r.Seq))))
}

type priceBoard struct {
	ui.SharedLive
	seq int
}

func (b *priceBoard) Update(n int) { b.seq = n }
func (b *priceBoard) Render() ui.Node {
	boardDraws.Add(1)
	rows := make([]ui.Node, 3)
	for i := range rows {
		rows[i] = ui.Key(i, &boardRow{N: i, Seq: b.seq})
	}
	if b.seq >= 100 {
		rows = append(rows, ui.Key("wide", &wideRow{Seq: b.seq}))
	}
	return ui.El("ul", []ui.Attr{{Name: "id", Value: "board"}}, rows...)
}

type boardPage struct{ ui.Live }

func (p *boardPage) Render() ui.Node {
	return ui.El("main", nil, ui.Component("board", nil, &priceBoard{}))
}

type sortPage struct {
	ui.Live
	sort string
}

func (p *sortPage) SortIt() { p.sort = "name" }
func (p *sortPage) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "id", Value: "sort"}, {Name: "onClick", Value: ui.On(p.SortIt)}}, ui.Text("sort")),
		ui.El("p", []ui.Attr{{Name: "id", Value: "sorted"}}, ui.Text("sort:"+p.sort)),
		ui.Component("board", nil, &priceBoard{}))
}

func sharedApp(t *testing.T) (*App, *httptest.Server) {
	app := New(Only("none"))
	app.Live("/board", &boardPage{})
	app.Live("/sort", &sortPage{})
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	drained(t, app)
	return app, srv
}

func bcast(app *App, msg any) error {
	return ui.Broadcast[*priceBoard](ui.WithSharedBroadcaster(context.Background(), app), msg)
}

func joinV2(t *testing.T, srv *httptest.Server, path string) (*websocket.Conn, []byte) {
	t.Helper()
	_, tok := pageToken(t, srv, path)
	c, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1, "v": 2})
	return c, raw(t, c)
}

func raw(t *testing.T, c *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func silent(t *testing.T, c *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, b, err := c.Read(ctx); err == nil {
		t.Fatalf("a frame arrived that should not have: %s", b)
	}
}

func TestSharedOneRenderOneEncodingForEveryConnection(t *testing.T) {
	app, srv := sharedApp(t)
	var cs []*websocket.Conn
	for range 6 {
		c, first := joinV2(t, srv, "/board")
		if !bytes.Contains(first, []byte(`"id":"s1.c1"`)) || !bytes.Contains(first, []byte("0:0")) {
			t.Fatalf("the join carries the fragment: %s", first)
		}
		cs = append(cs, c)
	}
	before := boardDraws.Load()
	if err := bcast(app, 7); err != nil {
		t.Fatal(err)
	}
	var first []byte
	for i, c := range cs {
		b := raw(t, c)
		if !bytes.Contains(b, []byte("0:7")) || !bytes.Contains(b, []byte(`"type":"patch"`)) {
			t.Fatalf("conn %d: %s", i, b)
		}
		if first == nil {
			first = b
		} else if !bytes.Equal(first, b) {
			t.Errorf("conn %d got other bytes:\n%s\n%s", i, first, b)
		}
	}
	if n := boardDraws.Load() - before; n != 1 {
		t.Errorf("rendered %d times for 6 connections, want 1", n)
	}
	if bytes.Contains(first, []byte(`"s":[`)) {
		t.Errorf("a frame to a browser that holds the statics carries none: %s", first)
	}
}

func TestSharedLateJoinerAndSecondConnection(t *testing.T) {
	app, srv := sharedApp(t)
	a, _ := joinV2(t, srv, "/board")
	bcast(app, 3)
	raw(t, a)
	b, first := joinV2(t, srv, "/board")
	if !bytes.Contains(first, []byte("0:3")) || !bytes.Contains(first, []byte(`"s":[`)) {
		t.Fatalf("a late joiner gets the current fragment, statics inline: %s", first)
	}
	bcast(app, 4)
	for _, c := range []*websocket.Conn{a, b} {
		if m := raw(t, c); !bytes.Contains(m, []byte("0:4")) || bytes.Contains(m, []byte(`"s":[`)) {
			t.Errorf("after the join both get the patch: %s", m)
		}
	}
}

func TestSharedVersionMismatchAndMissingStaticsGetASnapshot(t *testing.T) {
	app, srv := sharedApp(t)
	a, _ := joinV2(t, srv, "/board")
	b, _ := joinV2(t, srv, "/board")
	cs := connsOf(app)
	if len(cs) != 2 {
		t.Fatalf("conns: %d", len(cs))
	}
	r := app.sharedRunOf(reflectTypeOf[*priceBoard]())
	// a browser that missed a frame: its version is behind
	cs[0].smu.Lock()
	cs[0].sh[r].ver = 0
	cs[0].smu.Unlock()
	bcast(app, 1)
	var snaps, patches int
	for _, c := range []*websocket.Conn{a, b} {
		m := raw(t, c)
		if bytes.Contains(m, []byte(`"s":[`)) {
			snaps++
		} else {
			patches++
		}
	}
	if snaps != 1 || patches != 1 {
		t.Errorf("snapshots %d, patches %d: want 1 and 1", snaps, patches)
	}
	// a browser that hasn't the statics a frame needs
	cs = connsOf(app)
	for _, c := range cs {
		c.smu.Lock()
		c.known = nil
		c.smu.Unlock()
	}
	bcast(app, 150)
	for _, c := range []*websocket.Conn{a, b} {
		if m := raw(t, c); !bytes.Contains(m, []byte(`"s":[`)) || !bytes.Contains(m, []byte("0:150")) {
			t.Errorf("a snapshot for a browser without the statics: %s", m)
		}
	}
	bcast(app, 151)
	for _, c := range []*websocket.Conn{a, b} {
		if m := raw(t, c); bytes.Contains(m, []byte(`"s":[`)) || !bytes.Contains(m, []byte("0:151")) {
			t.Errorf("then patches again: %s", m)
		}
	}
}

func TestSharedEventOnSharedInstanceIsRefused(t *testing.T) {
	app, srv := sharedApp(t)
	a, _ := joinV2(t, srv, "/board")
	b, _ := joinV2(t, srv, "/board")
	for i, target := range []string{"s1.c1:0", "s1.c1:Update", "s1.c2:0"} {
		send(t, a, map[string]any{"type": "event", "ref": i + 2, "target": target, "event": "click"})
		m := raw(t, a)
		if !bytes.Contains(m, []byte(`"type":"error"`)) || !bytes.Contains(m, []byte("no handler")) {
			t.Errorf("%s: %s", target, m)
		}
	}
	silent(t, b)
	sh, _ := app.shared.ForType(reflectTypeOf[*priceBoard]())
	if !strings.Contains(sh.HTML(), "0:0") {
		t.Errorf("the shared state changed: %s", sh.HTML())
	}
}

func TestSharedWrapperEventTouchesOnlyItsPage(t *testing.T) {
	app, srv := sharedApp(t)
	a, first := joinV2(t, srv, "/sort")
	b, _ := joinV2(t, srv, "/sort")
	_ = first
	before := boardDraws.Load()
	send(t, a, map[string]any{"type": "event", "ref": 2, "target": "c1:0", "event": "click"})
	m := raw(t, a)
	if !bytes.Contains(m, []byte("sort:name")) || bytes.Contains(m, []byte("s1.c1")) && bytes.Contains(m, []byte(`"full"`)) {
		t.Errorf("the page's reply: %s", m)
	}
	silent(t, b)
	if boardDraws.Load() != before {
		t.Errorf("the shared fragment rendered for a page's event")
	}
	_ = app
}

func TestSharedReadingTheViewerFailsInDevelopment(t *testing.T) {
	t.Setenv("VUKA_DEV", "1")
	app := New(Only("none"))
	_, err := app.shared.ForType(reflectTypeOf[*peeker]())
	if err == nil || !strings.Contains(err.Error(), "differs per viewer") {
		t.Fatalf("err: %v", err)
	}
}

type peeker struct{ ui.SharedLive }

func (p *peeker) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }
func (p *peeker) mount(ctx context.Context) error {
	Request(ctx)
	return nil
}
func (p *peeker) Render() ui.Node { return ui.Text("x") }

func reflectTypeOf[T any]() reflect.Type { return reflect.TypeFor[T]() }
