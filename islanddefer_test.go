package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/ui/live"
	"github.com/vuka-lang/vuka"
)

// orders is a stateless page with deferred fields: Stats in its own group,
// Rows and Total loaded together, Recent failing.
type orders struct {
	Title  string         `json:"title"`
	Stats  map[string]int `json:"stats" defer:""`
	Rows   []string       `json:"rows" defer:"lists"`
	Total  int            `json:"total,omitempty" defer:"lists"`
	Recent []string       `json:"recent" defer:""`
	Loads  int            `json:"loads"`
	db     *applicantDB
}

func (o *orders) Mount() { o.Title = "Orders" }

func (o *orders) LoadStats(ctx context.Context) error {
	o.Stats = map[string]int{"open": len(o.db.names)}
	o.Loads++ // not the group's: dropped
	return nil
}

func (o *orders) LoadLists(ctx context.Context) error {
	o.Rows = o.db.find("a")
	return nil
}

func (o *orders) LoadRecent(ctx context.Context) error {
	return errors.New("recent: connection refused")
}

func (o *orders) Rename(t string) { o.Title = t }

// liveOrders is the live variant; LoadStats waits, so the page's first
// render and its join never wait for it.
type liveOrders struct {
	ui.Live
	Title  string         `json:"title"`
	Stats  map[string]int `json:"stats" defer:""`
	Rows   []string       `json:"rows" defer:""`
	Recent []string       `json:"recent" defer:""`
	db     *applicantDB
}

func (o *liveOrders) Render() ui.Node { return ui.IslandPage(o) }
func (o *liveOrders) Mount()          { o.Title = "Orders" }

func (o *liveOrders) LoadStats(ctx context.Context) error {
	select {
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	o.Stats = map[string]int{"open": len(o.db.names)}
	return nil
}

func (o *liveOrders) LoadRows(ctx context.Context) error {
	o.Rows = o.db.find("a")
	return nil
}

func (o *liveOrders) LoadRecent(ctx context.Context) error {
	return errors.New("recent: connection refused")
}

func init() {
	ui.Island("components/Orders.vue")(vuka.TypeOf[orders]("web.orders"))
	ui.Island("components/Orders.vue")(vuka.TypeOf[liveOrders]("web.liveOrders"))
}

func loadBody(props, sig string) string {
	b, _ := json.Marshal(map[string]any{"method": "", "load": true, "props": props, "sig": sig})
	return string(b)
}

func TestDeferredStateless(t *testing.T) {
	h := pageApp().Page("/orders", &orders{}).Handler()
	code, body, _ := do(t, h, "GET", "/orders", "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	_, raw, sig := rendered(t, body)
	if raw != `{"loads":0,"title":"Orders"}` {
		t.Errorf("first render's props: %s", raw)
	}
	if !strings.Contains(body, `data-vk-defer="{&#34;e&#34;:{},&#34;p&#34;:[&#34;stats&#34;,&#34;rows&#34;,&#34;total&#34;,&#34;recent&#34;]}"`) {
		t.Errorf("no data-vk-defer: %s", body)
	}
	if strings.Contains(body, "loadStats") {
		t.Errorf("a loader is callable: %s", body)
	}

	code, resp, _ := do(t, h, "POST", "/orders", loadBody(raw, sig), "Content-Type", "application/json")
	var out struct {
		Props, Sig string
		Defer      map[string]any
	}
	if code != 200 || json.Unmarshal([]byte(resp), &out) != nil {
		t.Fatalf("load: %d %s", code, resp)
	}
	if out.Props != `{"loads":0,"rows":["Ada","Alan","Grace"],"stats":{"open":3},"title":"Orders","total":0}` {
		t.Errorf("loaded props: %s", out.Props)
	}
	if d, _ := json.Marshal(out.Defer); string(d) != `{"e":{"recent":{"message":"recent: connection refused"}},"p":[]}` && string(d) != `{"e":{"recent":{"message":"error"}},"p":[]}` {
		t.Errorf("defer: %s", d)
	}

	// The loaded props make the next call: the loaded fields stay, the
	// failed one stays out.
	code, resp, _ = do(t, h, "POST", "/orders", callBody("rename", []any{"Sales"}, out.Props, out.Sig), "Content-Type", "application/json")
	if code != 200 || !strings.Contains(resp, `\"stats\":{\"open\":3}`) || !strings.Contains(resp, `\"title\":\"Sales\"`) || strings.Contains(resp, "recent") || strings.Contains(resp, `"defer"`) {
		t.Errorf("a call after the load: %d %s", code, resp)
	}

	// The load is a call: signed, same-origin.
	expect(t, h, "POST", "/orders", loadBody(raw, sig+"x"), 403, "aren't the server's", "Content-Type", "application/json")
	expect(t, h, "POST", "/orders", loadBody(strings.Replace(raw, `"loads":0`, `"loads":1`, 1), sig), 403, "aren't the server's", "Content-Type", "application/json")
	expect(t, h, "POST", "/orders", loadBody(raw, sig), 403, "cross-origin", "Content-Type", "application/json", "Sec-Fetch-Site", "cross-site")
}

func TestDeferredLive(t *testing.T) {
	srv := httptest.NewServer(pageApp().Page("/live/orders", &liveOrders{}).Handler())
	defer srv.Close()
	start := time.Now()
	code, body, _ := do(t, srv.Config.Handler, "GET", "/live/orders", "")
	if code != 200 || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("the first render waited: %d %v", code, time.Since(start))
	}
	if props, raw, _ := rendered(t, body); len(props) != 1 || props["title"] != "Orders" {
		t.Errorf("first render's props: %s", raw)
	}
	start = time.Now()
	c, html := joined(t, srv, "/live/orders")
	if time.Since(start) > 250*time.Millisecond {
		t.Errorf("the join waited for a loader: %v", time.Since(start))
	}
	if strings.Contains(html, "open") || !strings.Contains(html, "data-vk-defer") {
		t.Errorf("the join's render: %s", html)
	}
	var last map[string]any
	var dstate string
	for i := 0; i < 3; i++ {
		m := must(t, c)
		if m.Type != live.PatchMsg || len(m.Updates) != 1 {
			t.Fatalf("a load's patch: %+v", m)
		}
		last, _, _ = rendered(t, m.Updates[0].HTML)
		dstate = deferAttr.FindStringSubmatch(m.Updates[0].HTML)[1]
	}
	if _, ok := last["recent"]; ok || last["stats"] == nil || len(last["rows"].([]any)) != 3 {
		t.Errorf("after the loads: %v", last)
	}
	if !strings.Contains(dstate, `&#34;p&#34;:[]`) || !strings.Contains(dstate, `&#34;recent&#34;:{&#34;message&#34;:`) {
		t.Errorf("defer: %s", dstate)
	}
	// The page is fine: it renders and takes events.
	if m, err := recv(c, 200*time.Millisecond); err == nil {
		t.Errorf("more after the loads: %+v", m)
	}
}
