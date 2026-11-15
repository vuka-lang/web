package web

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// connKinds is the kinds of a record's connection events, in order.
func connKinds(evs []tbConn) []string {
	var ks []string
	for _, e := range evs {
		ks = append(ks, e.Kind)
	}
	return ks
}

// waitConns waits for the page's record to hold n connection events.
func waitConns(t *testing.T, app *App, id string, n int) []tbConn {
	t.Helper()
	for range 200 {
		if evs := liveRecord(t, app, id).Conns; len(evs) >= n {
			return evs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the record has %v, want %d events", connKinds(liveRecord(t, app, id).Conns), n)
	return nil
}

// A page's record keeps its live connections' lives: each socket opened,
// its joins (adopted, then fresh), the server's closes with their codes,
// reasons and causes, and the browser closing one; the summary says the
// state now, the Connection panel draws it.
func TestToolbarConnection(t *testing.T) {
	withToolbar(t)
	var refuse atomic.Bool
	app := liveApp(Dev(), LiveIdleTimeout(400*time.Millisecond), LiveCheckEvery(40*time.Millisecond),
		LiveCheck(func(context.Context) error {
			if refuse.Load() {
				return errors.New("the session ended")
			}
			return nil
		}))
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	id, c := livePage(t, app, srv, "/counter", 2)
	if sum, _ := fetchRecord(t, app.Handler(), id); sum["conn"] != "connected" {
		t.Errorf("joined: %v", sum)
	}

	// CloseLive: 4001, its cause in words.
	closeAll(t, app)
	if _, err := recv(c, 5*time.Second); websocket.CloseStatus(err) != closeReload {
		t.Fatalf("CloseLive: %v", err)
	}
	waitConns(t, app, id, 3)
	if sum, _ := fetchRecord(t, app.Handler(), id); sum["conn"] != "recovering" || !strings.Contains(sum["connText"].(string), "the page must load again") {
		t.Errorf("after CloseLive: %v", sum)
	}

	// The browser connects again, joins twice and sits idle: 4002.
	res := tbDo(t, app.Handler(), "GET", "/counter")
	freshTok := tokenRe.FindStringSubmatch(res.body)[1]
	c2, err := dial(t, srv, freshTok+"&tb="+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c2, map[string]any{"type": "join", "ref": 1, "v": 2})
	recv2(t, c2)
	send(t, c2, map[string]any{"type": "join", "ref": 2, "v": 2})
	recv2(t, c2)
	if _, err := recv(c2, 5*time.Second); websocket.CloseStatus(err) != closeIdle {
		t.Fatalf("idle: %v", err)
	}

	// Again, refused by a LiveCheck.
	c3, err := dial(t, srv, freshTok+"&tb="+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c3, map[string]any{"type": "join", "ref": 1, "v": 2})
	recv2(t, c3)
	refuse.Store(true)
	if _, err := recv(c3, 5*time.Second); websocket.CloseStatus(err) != closeReload {
		t.Fatalf("a LiveCheck: %v", err)
	}
	refuse.Store(false)

	// Again, the browser leaving.
	c4, err := dial(t, srv, freshTok+"&tb="+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c4, map[string]any{"type": "join", "ref": 1, "v": 2})
	recv2(t, c4)
	c4.Close(websocket.StatusGoingAway, "")

	want := []string{"open", "join", "close", "reconnect", "join", "rejoin", "close", "reconnect", "join", "close", "reconnect", "join", "drop"}
	evs := waitConns(t, app, id, len(want))
	if got := connKinds(evs); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("events %v, want %v", got, want)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].At.Before(evs[i-1].At) {
			t.Errorf("event %d is before the one before it", i)
		}
	}
	check := func(i int, conn, code int, text, detail string) {
		t.Helper()
		e := evs[i]
		if e.Conn != conn || e.Code != code || !strings.Contains(e.Text, text) || !strings.Contains(e.Detail, detail) {
			t.Errorf("event %d: %+v, want #%d %d %q %q", i, e, conn, code, text, detail)
		}
	}
	first := evs[0].Conn
	check(0, first, 0, "opened a socket", "WebSocket")
	check(1, first, 0, "parked was adopted", "protocol v2")
	check(2, first, 4001, "the server closed it: live: the page must load again", "CloseLive closed the page; the browser loads the page again")
	check(3, first+1, 0, "opened a socket again", "")
	check(4, first+1, 0, "parked was adopted", "") // the fresh GET parked one
	check(5, first+1, 0, "again on the same socket", "")
	check(6, first+1, 4002, "the page sat idle", "LiveIdleTimeout")
	check(8, first+2, 0, "mounted fresh", "") // its token's session was taken
	check(9, first+2, 4001, "must load again", "a LiveCheck refused the page: the session ended")
	check(12, first+3, 1001, "the browser closed it", "the page went away")

	sum, html := fetchRecord(t, app.Handler(), id)
	if sum["conn"] != "disconnected" || sum["connTone"] != "warn" || !strings.HasPrefix(sum["connText"].(string), "disconnected since ") {
		t.Errorf("after the browser left: %v", sum)
	}
	for _, s := range []string{`data-tab="Connection"`, `<span class="cstate c-disconnected">disconnected</span>`, `<code class="code">4002</code>`,
		`<span class="type t-drop">dropped</span>`, "a LiveCheck refused the page: the session ended", "CloseLive closed the page"} {
		if !strings.Contains(html, s) {
			t.Errorf("the Connection panel has no %q", s)
		}
	}
	if strings.Index(html, `t-drop`) > strings.Index(html, `t-open`) {
		t.Error("the panel doesn't list the latest first")
	}
}

// A socket whose token is bad is refused as it opens, on the page's record.
func TestToolbarConnectionRefused(t *testing.T) {
	withToolbar(t)
	app := liveApp(Dev())
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	id := tbDo(t, app.Handler(), "GET", "/counter").header.Get(ToolbarHeader)
	c, err := dial(t, srv, "forged&tb="+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recv(c, 5*time.Second); websocket.CloseStatus(err) != closeReload {
		t.Fatalf("a bad token: %v", err)
	}
	evs := waitConns(t, app, id, 1)
	if e := evs[0]; e.Kind != "refused" || e.Code != 4001 || !strings.Contains(e.Text, "closed the socket as it opened") {
		t.Errorf("%+v", e)
	}
}

// A page keeps its latest toolbarConns events.
func TestToolbarConnectionCap(t *testing.T) {
	p := &tbEntry{}
	for i := range toolbarConns + 50 {
		p.addConn(tbConn{At: time.Now(), Conn: i, Kind: "join"})
	}
	r := p.snapshot()
	if len(r.Conns) != toolbarConns || r.Conns[0].Conn != 50 || r.Conns[len(r.Conns)-1].Conn != toolbarConns+49 {
		t.Errorf("%d events, %d to %d", len(r.Conns), r.Conns[0].Conn, r.Conns[len(r.Conns)-1].Conn)
	}
}

// In the browser the handle shows the connection's state, and the
// Connection panel why it closed: a page CloseLive closes recovers in place,
// its new record saying why the connection before it closed, which the
// earlier record lists with its code and cause.
func TestBrowserToolbarConnection(t *testing.T) {
	withToolbar(t)
	app := meterApp(Dev())
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/meter")
	p.Viewport(1400, 800, false)
	tb := `document.querySelector("vuka-toolbar").shadowRoot`
	p.Wait(`document.documentElement.dataset.vkState === "connected"`)
	p.Wait(tb + `.querySelector(".handle .conn.c-connected")`)
	p.ClickElement(tb + `.querySelector(".handle")`)
	p.Wait(tb + `.querySelector('.tab[data-tab="Connection"]')`)
	p.ClickElement(tb + `.querySelector('.tab[data-tab="Connection"]')`)
	panel := tb + `.querySelector('.panel[data-panel="Connection"]:not([hidden])')`
	p.Wait(panel + ` && ` + panel + `.textContent.includes("the session the page's render parked was adopted")`)

	closeAll(t, app)
	p.WaitText("span", "m2 n0")
	p.Wait(`document.documentElement.dataset.vkState === "connected" && ` + tb + `.querySelector("select").selectedOptions[0].textContent === "page /meter (recovered)"`)
	p.Wait(panel + ` && ` + panel + `.textContent.includes("This page recovered in place after its connection closed: recovering since ") && ` +
		panel + `.textContent.includes("the page must load again")`)
	p.Wait(tb + `.querySelector(".handle .conn.c-connected")`)
	if dir := os.Getenv("VUKA_SCREENSHOTS"); dir != "" {
		p.Screenshot(filepath.Join(dir, "toolbar-connection.png"))
	}

	p.ClickElement(tb + `.querySelector(".record-open")`)
	p.Wait(panel + ` && ` + panel + `.textContent.includes("CloseLive closed the page; the browser loads the page again") && ` +
		panel + `.querySelector(".cstate").textContent === "recovering" && ` + panel + `.querySelector("code.code").textContent === "4001"`)
}
