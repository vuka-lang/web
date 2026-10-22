package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/web/v2/internal/chrome"
	"github.com/vuka-lang/web/v2/internal/livetree"
	"github.com/vuka-lang/web/v2/internal/testenv"
)

// Until the Vuka live session speaks protocol v2, the web tests answer v2
// joins with livetree's stand-in, which emits the spec's messages.
func init() { livetree.Install() }

type v2msg struct {
	Type  string           `json:"type"`
	Ref   int              `json:"ref"`
	V     int              `json:"v"`
	HTML  string           `json:"html"`
	Trees []livetree.Entry `json:"trees"`
	Error string           `json:"error"`
}

func recv2(t *testing.T, c *websocket.Conn) (v2msg, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m v2msg
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m, string(b)
}

func TestLiveV2Join(t *testing.T) {
	srv := httptest.NewServer(liveApp().Handler())
	defer srv.Close()
	_, tok := pageToken(t, srv, "/two")
	c, err := dial(t, srv, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1, "v": 2})
	m, raw := recv2(t, c)
	if m.Type != "render" || m.V != 2 || m.Ref != 1 || len(m.Trees) != 3 || m.Trees[0].ID != "c0" || !m.Trees[2].Full {
		t.Fatalf("v2 join: %s", raw)
	}
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": "c2:0", "event": "click"})
	m, raw = recv2(t, c)
	if m.Type != "patch" || m.Ref != 2 || len(m.Trees) != 1 || m.Trees[0].ID != "c2" || m.Trees[0].Full || !strings.Contains(raw, `b11`) || strings.Contains(raw, `"s":`) {
		t.Fatalf("v2 patch: %s", raw)
	}
	send(t, c, map[string]any{"type": "event", "ref": 3, "target": "c9:0", "event": "click"})
	if m, raw := recv2(t, c); m.Type != "error" || m.Ref != 3 {
		t.Fatalf("unknown handler: %s", raw)
	}

	srv1 := httptest.NewServer(liveApp(LiveProtocol(1)).Handler())
	defer srv1.Close()
	_, tok = pageToken(t, srv1, "/two")
	c1, err := dial(t, srv1, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c1, map[string]any{"type": "join", "ref": 1, "v": 2})
	if m, raw := recv2(t, c1); m.Type != "render" || m.V != 0 || m.Trees != nil || !strings.Contains(m.HTML, "<span>b10</span>") {
		t.Fatalf("LiveProtocol(1) answers a v2 join in v1: %s", raw)
	}
}

func TestLiveV2Broadcast(t *testing.T) {
	srv := httptest.NewServer(liveApp().Handler())
	defer srv.Close()
	conns := make([]*websocket.Conn, 2)
	for i, v := range []int{2, 0} {
		_, tok := pageToken(t, srv, "/chat/go")
		c, err := dial(t, srv, tok, nil)
		if err != nil {
			t.Fatal(err)
		}
		send(t, c, map[string]any{"type": "join", "ref": 1, "v": v})
		recv2(t, c)
		conns[i] = c
	}
	send(t, conns[0], map[string]any{"type": "event", "ref": 2, "target": "c1:0", "event": "submit", "form": map[string][]string{"text": {"hi"}}})
	if m, raw := recv2(t, conns[0]); m.Ref != 2 || m.Error != "" {
		t.Fatalf("send: %s", raw)
	}
	if m, raw := recv2(t, conns[0]); m.Type != "patch" || m.Ref != 0 || len(m.Trees) != 1 || !strings.Contains(raw, `hi`) {
		t.Fatalf("v2 broadcast: %s", raw)
	}
	if m, raw := recv2(t, conns[1]); m.Type != "patch" || m.Trees != nil || !strings.Contains(raw, `>hi</form>`) {
		t.Fatalf("v1 broadcast beside v2: %s", raw)
	}
}

// The browser runtime's tree code, over the spec's worked examples.
func TestLiveJSSpecFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		testenv.Needs(t, "no node")
	}
	out, err := exec.Command(node, "testdata/livejs/run.js", "live.js", "testdata/livejs/spec.json").CombinedOutput()
	if err != nil {
		t.Fatalf("%s", out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

func TestBrowserProtocols(t *testing.T) {
	for _, v := range []int{1, 2} {
		t.Run("v"+string(rune('0'+v)), func(t *testing.T) {
			app := liveApp(LiveProtocol(v))
			srv := httptest.NewServer(app.Handler())
			defer srv.Close()
			b := chrome.Launch(t)
			p := b.Open(srv.URL + "/two")
			p.Wait(`document.documentElement.dataset.vkProtocol === "` + string(rune('0'+v)) + `"`)
			p.Click(`[data-vk-id="c2"] button`)
			p.WaitText(`[data-vk-id="c2"] span`, "b11")
			p.Click(`[data-vk-id="c1"] button`)
			p.WaitText(`[data-vk-id="c1"] span`, "a1")
			p.WaitText(`[data-vk-id="c2"] span`, "b11")

			ann, bob := b.Open(srv.URL+"/chat/go"), b.Open(srv.URL+"/chat/go")
			ann.Eval(`(() => { const f = document.querySelector("form"); const i = document.createElement("input"); i.name = "text"; i.value = "yo"; f.append(i); f.requestSubmit(); })()`, nil)
			bob.Wait(`document.querySelector("form").textContent === "yo"`)
			ann.Wait(`document.querySelector("form").textContent === "yo"`)
		})
	}
}
