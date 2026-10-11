package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// livePage opens path's page on srv, its live connection joined in protocol
// v, the toolbar recording it: the page's record id and the connection.
func livePage(t *testing.T, app *App, srv *httptest.Server, path string, v int) (string, *websocket.Conn) {
	t.Helper()
	res := tbDo(t, app.Handler(), "GET", path)
	id, tok := res.header.Get(ToolbarHeader), tokenRe.FindStringSubmatch(res.body)
	if id == "" || tok == nil {
		t.Fatalf("%s: %s", path, res.body)
	}
	c, err := dial(t, srv, tok[1]+"&tb="+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1, "v": v})
	recv2(t, c)
	return id, c
}

func liveRecord(t *testing.T, app *App, id string) tbRecord {
	t.Helper()
	e := app.tb.get(id)
	if e == nil {
		t.Fatalf("no record %s", id)
	}
	return e.snapshot()
}

// Each socket message keeps the client's frame and every server frame it
// sent, decoded: join, an event, a broadcast, in either protocol.
func TestToolbarWireFrames(t *testing.T) {
	withToolbar(t)
	for _, v := range []int{2, 1} {
		t.Run(fmt.Sprint("v", v), func(t *testing.T) {
			app := liveApp(Dev(), LiveProtocol(v))
			srv := httptest.NewServer(app.Handler())
			defer srv.Close()
			id, c := livePage(t, app, srv, "/two", 2)
			send(t, c, map[string]any{"type": "event", "ref": 2, "target": "c2:0", "event": "click"})
			recv2(t, c)
			cid, cc := livePage(t, app, srv, "/chat/lobby", 2)
			send(t, cc, map[string]any{"type": "event", "ref": 2, "target": "c1:0", "event": "submit", "form": map[string][]string{"text": {"hi"}}})
			recv2(t, cc)
			recv2(t, cc)

			live := liveRecord(t, app, id).Live
			if len(live) != 2 {
				t.Fatalf("%d messages", len(live))
			}
			join, ev := live[0].snapshot(), live[1].snapshot()
			if len(join.Frames) != 2 || join.Frames[0].Dir != "client" || !strings.Contains(join.Frames[0].Body, `"type":"join"`) ||
				join.Frames[1].Report.Type != "render" {
				t.Fatalf("join's frames: %+v", join.Frames)
			}
			if len(ev.Frames) != 2 || !strings.Contains(ev.Frames[0].Body, `"target":"c2:0"`) || ev.Frames[1].Size != ev.Bytes {
				t.Fatalf("event's frames: %+v", ev.Frames)
			}
			r := ev.Frames[1].Report
			if len(r.Insts) != 1 || r.Insts[0].ID != "c2" || r.Insts[0].Type != "counter" || len(r.Rows) != 1 {
				t.Fatalf("the event's reply: %+v", r)
			}
			row := r.Rows[0]
			if !row.HasOld || !strings.Contains(row.Old, "b10") || !strings.Contains(row.New, "b11") {
				t.Errorf("the change: %+v", row)
			}
			if kind := map[int]string{1: "html", 2: "diff"}[v]; r.Insts[0].Kind != kind {
				t.Errorf("sent as %s, want %s", r.Insts[0].Kind, kind)
			}

			var bc tbRecord
			for _, m := range liveRecord(t, app, cid).Live {
				if s := m.snapshot(); s.Method == "broadcast" {
					bc = s
				}
			}
			if len(bc.Frames) != 1 || bc.Frames[0].Dir != "server" || bc.Frames[0].Report.Type != "patch" || len(bc.Frames[0].Report.Rows) == 0 ||
				!strings.Contains(bc.Frames[0].Report.Rows[0].New, "hi") {
				t.Fatalf("the broadcast's frames: %+v", bc.Frames)
			}

			// The views, fetched when an entry opens.
			res := tbDo(t, app.Handler(), "GET", fmt.Sprintf("/_vuka/toolbar/r/%s?wire=%d", id, ev.Seq))
			var out struct{ HTML string }
			json.Unmarshal([]byte(res.body), &out)
			for _, want := range []string{`data-wpanel="summary"`, `data-wpanel="diff"`, `data-wpanel="raw"`, `<code>c2</code> counter`,
				`<code>c2:0 click</code>`, `<span class="jk">&#34;type&#34;</span>: <span class="js">&#34;patch&#34;</span>`, `<code>c2`} {
				if !strings.Contains(out.HTML, want) {
					t.Errorf("the views have no %q:\n%s", want, out.HTML)
				}
			}
			if res := tbDo(t, app.Handler(), "GET", "/_vuka/toolbar/r/"+id+"?wire=99999"); res.status != 404 {
				t.Errorf("a message that's gone: %d", res.status)
			}
		})
	}
}

func TestToolbarWireTruncates(t *testing.T) {
	big := []byte(`{"type":"render","html":"` + strings.Repeat("x", 70_000) + `"}`)
	f := newFrame("server", big)
	if !f.Truncated || f.Size != len(big) || len(f.Body) != wireFrameMax {
		t.Errorf("a big frame: truncated %v, size %d, kept %d", f.Truncated, f.Size, len(f.Body))
	}
	if f := newFrame("server", []byte(`{"type":"patch"}`)); f.Truncated || f.Body != `{"type":"patch"}` {
		t.Errorf("a small frame: %+v", f)
	}
	if h := string(highlightJSON(f.Body, true)); strings.Contains(h, "\n  ") {
		t.Error("a truncated frame is pretty-printed")
	}
}

func TestToolbarWireMasksSecrets(t *testing.T) {
	m := newWireMirror(func(string) string { return "" })
	m.apply([]byte(`{"type":"render","html":"<form data-vk-on-submit=\"c0:0\"><input name=\"user\"><input type=\"password\" name=\"pw\" data-vk-on-input=\"c0:1\"></form>"}`))
	got := string(m.maskClient([]byte(`{"type":"event","ref":2,"target":"c0:0","event":"submit","form":{"user":["ada"],"pw":["hunter2"],"api_token":["t0k"],"client_secret":["s"]}}`)))
	for _, leak := range []string{"hunter2", "t0k", `"s"`} {
		if strings.Contains(got, leak) {
			t.Errorf("%s is shown: %s", leak, got)
		}
	}
	if !strings.Contains(got, `"user":["ada"]`) || strings.Count(got, hidden) != 3 {
		t.Errorf("masked: %s", got)
	}
	if got := string(m.maskClient([]byte(`{"type":"event","ref":3,"target":"c0:1","event":"input","value":"hunter2"}`))); strings.Contains(got, "hunter2") {
		t.Errorf("a password input's value: %s", got)
	}
	plain := `{"type":"event","ref":4,"target":"c0:0","event":"click","value":"3"}`
	if got := string(m.maskClient([]byte(plain))); got != plain {
		t.Errorf("nothing to mask: %s", got)
	}
}

// A reply's bytes split by what they carry.
func TestToolbarWireByteSplit(t *testing.T) {
	m := newWireMirror(func(id string) string { return map[string]string{"c0": "page", "c1": "Counter"}[id] })
	join := `{"type":"render","ref":1,"v":2,"trees":[{"id":"c0","full":true,"tree":{"t":0,"s":["<main>","</main>"],"d":[{"id":"c1"}]}},{"id":"c1","full":true,"tree":{"t":1,"s":["<p>","</p>"],"d":["4"]}}]}`
	r := m.apply([]byte(join))
	// statics: ["<main>","</main>"] (20) and ["<p>","</p>"] (14), each with ,"s": (5); dynamics "4" (3)
	want := fmt.Sprintf("%d B: statics 44 B (new fingerprints 2), dynamics 3 B, framing %d B", len(join), len(join)-47)
	if got := splitText(r); got != want {
		t.Errorf("join:\n got %s\nwant %s", got, want)
	}
	if r.Insts[1].ID != "c1" || r.Insts[1].Type != "Counter" || r.Insts[1].Kind != "full" || r.Insts[1].Statics != 1 || r.Insts[1].SBytes != 19 { // 14 + 5
		t.Errorf("c1: %+v", r.Insts[1])
	}
	patch := `{"type":"patch","ref":2,"trees":[{"id":"c1","tree":{"u":{"0":"5"}}}],"js":[{"id":"c1","ops":[["focus",{}]]}]}`
	r = m.apply([]byte(patch))
	// js: the list (34) with ,"js": (6)
	want = fmt.Sprintf("%d B: statics 0 B, dynamics 3 B, js 40 B, framing %d B", len(patch), len(patch)-43)
	if got := splitText(r); got != want {
		t.Errorf("patch:\n got %s\nwant %s", got, want)
	}
	if len(r.JS) != 1 || r.JS[0] != "c1: 1 command" || r.Insts[0].Dyn != 1 {
		t.Errorf("patch: %+v", r)
	}
}

// The Diff view's rows: each changed dynamic's path, with the value the
// browser held.
func TestToolbarWireDiffPaths(t *testing.T) {
	m := newWireMirror(func(string) string { return "" })
	m.apply([]byte(`{"type":"render","v":2,"trees":[{"id":"c1","full":true,"tree":{"t":0,"s":["<table",">","","</table>"],"d":[" data-vk-id=\"c1\"",` +
		`{"c":[{"t":1,"s":["<tr","><td>","</td><td>","</td></tr>"],"d":[" data-vk-key=\"1\"","apples","3"]},{"t":1,"d":[" data-vk-key=\"2\"","pears","5"]},` +
		`{"t":1,"d":[" data-vk-key=\"3\"","figs","2"]},{"t":1,"d":[" data-vk-key=\"4\"","kiwis","8"]}]},` +
		`{"c":[{"t":2,"s":["<p>none</p>"],"d":[]}]}]}}]}`))
	r := m.apply([]byte(`{"type":"patch","ref":2,"trees":[{"id":"c1","tree":{"u":{"1":{"k":[{"m":3},{"u":{"2":"4"}},-1,1]},"2":{"c":[{"t":3,"s":["<p>row ","</p>"],"d":["2"]}]}}}}]}`))
	var got []string
	for _, row := range r.Rows {
		s := []string{row.Path}
		if row.Note != "" {
			s = append(s, row.Note)
		}
		if row.HasOld {
			s = append(s, fmt.Sprintf("%q → %q", row.Old, row.New))
		}
		got = append(got, strings.Join(s, " "))
	}
	want := []string{
		"c1 › u.1 › k.0 moved from k.3",
		`c1 › u.1 › k.1 › u.2 "3" → "4"`,
		`c1 › u.1 › k.2 1 item removed "<tr data-vk-key=\"2\"><td>pears</td><td>5</td></tr>" → ""`,
		`c1 › u.2 the whole list "<p>none</p>" → "<p>row 2</p>"`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r.Rows[1].At != "<td>" || r.Insts[0].Steps != 4 || r.Insts[0].Statics != 1 {
		t.Errorf("place %q, steps %d, statics %d", r.Rows[1].At, r.Insts[0].Steps, r.Insts[0].Statics)
	}
	if html := m.html(m.inst["c1"], 0); html != `<table data-vk-id="c1"><tr data-vk-key="4"><td>kiwis</td><td>8</td></tr><tr data-vk-key="1"><td>apples</td><td>4</td></tr><tr data-vk-key="3"><td>figs</td><td>2</td></tr><p>row 2</p></table>` {
		t.Errorf("the copy: %s", html)
	}

	// A kept string, then a token patch against it.
	long := strings.Repeat("<li>x</li>", 120)
	m.apply([]byte(`{"type":"render","v":2,"trees":[{"id":"c0","full":true,"tree":{"t":0,"s":["<ul>","</ul>"],"d":[{"r":0,"v":"` + long + `"}]}}]}`))
	r = m.apply([]byte(`{"type":"patch","trees":[{"id":"c0","tree":{"u":{"0":{"p":[4,-1,"y",355]}}}}]}`))
	if len(r.Rows) != 1 || r.Rows[0].Note != "token patch" || r.Insts[0].Patches != 1 || !strings.HasPrefix(r.Rows[0].New, "<li>x</li><li>y</li><li>x") {
		t.Errorf("token patch: %+v", r)
	}
}

// A navigation over the socket gives the page it moved to a record of its
// own — route, handler, its server work and the navigate message — which
// the page's record names as the one on screen; a patch updates the page's
// URL; going back finds the page's record again.
func TestToolbarNavigate(t *testing.T) {
	withToolbar(t)
	app := liveApp(Dev())
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	id, c := livePage(t, app, srv, "/pager?page=1", 2)
	nav := func(ref int, url string, patch bool) {
		t.Helper()
		send(t, c, map[string]any{"type": "navigate", "ref": ref, "url": url, "patch": patch})
		if m, raw := recv2(t, c); m.Type != "patch" {
			t.Fatalf("navigate %s: %s", url, raw)
		}
	}
	nav(2, "/pager?page=2", true)
	if p := liveRecord(t, app, id); p.URI != "/pager?page=2" || p.Now != "" || len(p.Live) != 2 {
		t.Errorf("a patch: %s now %q, %d messages", p.URI, p.Now, len(p.Live))
	}

	nav(3, "/counter", false)
	page := liveRecord(t, app, id)
	if page.Now == "" || page.Now == id || len(page.Live) != 2 {
		t.Fatalf("the page after a navigate: now %q, %d messages", page.Now, len(page.Live))
	}
	sum, html := fetchRecord(t, app.Handler(), id)
	if sum["now"] != page.Now {
		t.Errorf("summary: %v", sum)
	}
	next := liveRecord(t, app, page.Now)
	if next.URI != "/counter" || next.Route != "GET /counter" || !strings.Contains(next.Handler, "live page") || next.Status != 200 ||
		len(next.Live) != 1 || next.Live[0].snapshot().Method != "navigate" || next.Req.Get("X-Vuka-Live") != "navigate" || next.Req.Get("Sec-Websocket-Key") != "" {
		t.Fatalf("the navigation's record: %+v", next)
	}
	_, html = fetchRecord(t, app.Handler(), page.Now)
	if !strings.Contains(html, "<code>GET /counter</code>") || !strings.Contains(html, `<span class="type t-navigate">navigate</span> <code>/counter</code>`) {
		t.Errorf("its panels: %s", html)
	}

	// An event on the new page is recorded under it.
	send(t, c, map[string]any{"type": "event", "ref": 4, "target": "c1:0", "event": "click"})
	recv2(t, c)
	if n := len(liveRecord(t, app, page.Now).Live); n != 2 {
		t.Errorf("the new page has %d messages", n)
	}

	// Back: the first page's record again.
	nav(5, "/pager?page=2", false)
	if p := liveRecord(t, app, id); p.Now != id || p.Route != "GET /pager" || p.Live[len(p.Live)-1].snapshot().Method != "navigate" {
		t.Errorf("back: now %q, route %q", p.Now, p.Route)
	}
}

// The cost of recording frames: a live event's round trip with and without
// the toolbar.
func BenchmarkToolbarLiveEvent(b *testing.B) {
	for _, on := range []bool{false, true} {
		b.Run(fmt.Sprintf("toolbar=%v", on), func(b *testing.B) {
			toolbarInTests = on
			app := liveApp(Dev())
			toolbarInTests = false
			srv := httptest.NewServer(app.Handler())
			defer srv.Close()
			t := &testing.T{}
			res := tbDo(t, app.Handler(), "GET", "/two")
			tok := tokenRe.FindStringSubmatch(res.body)[1]
			if id := res.header.Get(ToolbarHeader); id != "" {
				tok += "&tb=" + id
			}
			ctx := context.Background()
			c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+liveSocket+"?t="+tok, &websocket.DialOptions{HTTPHeader: map[string][]string{"Origin": {srv.URL}}})
			if err != nil {
				b.Fatal(err)
			}
			defer c.CloseNow()
			c.Write(ctx, websocket.MessageText, []byte(`{"type":"join","ref":1,"v":2}`))
			c.Read(ctx)
			msg := []byte(`{"type":"event","ref":2,"target":"c2:0","event":"click"}`)
			b.ReportAllocs()
			for b.Loop() {
				c.Write(ctx, websocket.MessageText, msg)
				if _, _, err := c.Read(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestToolbarWirePageBudget(t *testing.T) {
	p := &tbEntry{}
	body := strings.Repeat("x", 300<<10)
	for range 5 {
		m := &tbEntry{tbRecord: tbRecord{Frames: []tbFrame{{Dir: "server", Body: body, Size: len(body)}}}}
		p.Live = append(p.Live, m)
		p.trimFrames()
	}
	var kept []bool
	for _, m := range p.Live {
		kept = append(kept, m.Frames != nil && !m.FramesDropped)
	}
	if fmt.Sprint(kept) != "[false false true true true]" {
		t.Errorf("frames kept: %v", kept)
	}
}
