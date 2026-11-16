package web

import (
	"fmt"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/liveproto"
)

// report is a page holding two thousand rows of about a hundred bytes in a
// Temporary region (ui.Temporary): the browser keeps them, the server
// forgets them once sent.
type report struct {
	ui.Live
	rows   []string
	clicks int
	picked string
}

func (r *report) LiveSchema(l *ui.LiveSchema) { l.Mount(r.mount) }
func (r *report) mount() error                { r.rows = reportRows("row"); return nil }
func (r *report) Inc()                        { r.clicks++ }
func (r *report) Reload()                     { r.rows = reportRows("new") }

func reportRows(prefix string) []string {
	rows := make([]string, 2000)
	for i := range rows {
		rows[i] = fmt.Sprintf("%s %d %s", prefix, i, strings.Repeat("-", 80))
	}
	return rows
}

func (r *report) Render() ui.Node {
	rows := make([]ui.Node, len(r.rows))
	for i, row := range r.rows {
		pick := ui.On(func() { r.picked = row })
		rows[i] = ui.HTML(`<tr key={i}><td>{row}</td><td><button class={"p" + strconv.Itoa(i)} onClick={pick}>pick</button></td></tr>`, i, row, "p"+strconv.Itoa(i), pick)
	}
	body := ui.Temporary(ui.TemporaryProps{Of: r.rows, Children: ui.HTML(`<table>{rows}</table>`, rows)})
	return ui.HTML(`<section>
  <p id="count">clicks {r.clicks} picked {r.picked}</p>
  <button id="inc" onClick={r.Inc}>+</button>
  <button id="reload" onClick={r.Reload}>reload</button>
  <div id="box">{body}</div>
</section>`, r.clicks, r.picked, ui.On(r.Inc), ui.On(r.Reload), body)
}

func reportApp() *App {
	app := New(Only("none"))
	app.Live("/report", &report{})
	return app
}

// Over a real v2 connection: the rows arrive with the join, an unrelated
// event's patch doesn't carry them, a row's handler still answers, and a
// new value sends them again, once.
func TestLiveTemporary(t *testing.T) {
	v2 := liveproto.V2
	defer func() { liveproto.V2 = v2 }()
	liveproto.V2 = nil
	if !liveproto.Native() {
		t.Skip("this ui's session speaks only v1")
	}
	srv := httptest.NewServer(reportApp().Handler())
	defer srv.Close()
	html, tok := pageToken(t, srv, "/report")
	if !strings.Contains(html, "row 1999 ") {
		t.Fatal("the page's HTML lacks the rows")
	}
	c, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(4 << 20)
	send(t, c, map[string]any{"type": "join", "ref": 1, "v": 2})
	m, raw := recv2(t, c)
	if m.Type != "render" || m.V != 2 || !strings.Contains(raw, "row 1999 ") {
		t.Fatalf("v2 join (%d bytes) lacks the rows: %.300s", len(raw), raw)
	}
	t.Logf("join: %d bytes", len(raw))
	inc, reload, pick := handlerOf(t, html, "click", 0), handlerOf(t, html, "click", 1), ""
	if m := regexp.MustCompile(`class="p7" data-vk-on-click="([^"]+)"`).FindStringSubmatch(html); m != nil {
		pick = m[1]
	} else {
		t.Fatal("row 7's button has no handler")
	}
	event := func(ref int, target string) string {
		t.Helper()
		send(t, c, map[string]any{"type": "event", "ref": ref, "target": target, "event": "click"})
		m, raw := recv2(t, c)
		if m.Type != "patch" || m.Error != "" {
			t.Fatalf("event %s: %s", target, raw)
		}
		return raw
	}
	if raw := event(2, inc); len(raw) > 300 || strings.Contains(raw, "row ") || !strings.Contains(raw, `"1"`) {
		t.Fatalf("an unrelated event's patch (%d bytes): %s", len(raw), raw)
	}
	if raw := event(3, pick); len(raw) > 300 || strings.Count(raw, "row ") != 1 || !strings.Contains(raw, "row 7 ") {
		t.Fatalf("a row's event's patch (%d bytes): %.300s", len(raw), raw)
	}
	raw = event(4, reload)
	if !strings.Contains(raw, "new 1999 ") || strings.Count(raw, "new ") != 2000 {
		t.Fatalf("a new value's patch (%d bytes) doesn't carry its rows once: %.300s", len(raw), raw)
	}
	t.Logf("new value: %d bytes", len(raw))
	if raw := event(5, inc); len(raw) > 300 || strings.Contains(raw, "new ") {
		t.Fatalf("an unrelated event's patch after the new value (%d bytes): %s", len(raw), raw)
	}
}
