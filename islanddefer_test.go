package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
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

func (o *orders) LiveSchema(l *ui.LiveSchema) {
	l.Mount(o.mount)
	l.Loads("stats", o.loadStats)
	l.Loads("lists", o.loadLists)
	l.Loads("recent", o.loadRecent)
}

func (o *orders) mount(_ context.Context) error { o.Title = "Orders"; return nil }

func (o *orders) loadStats(_ context.Context) error {
	o.Stats = map[string]int{"open": len(o.db.names)}
	o.Loads++ // not the group's: dropped
	return nil
}

func (o *orders) loadLists(_ context.Context) error {
	o.Rows = o.db.find("a")
	return nil
}

func (o *orders) loadRecent(_ context.Context) error {
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
func (o *liveOrders) LiveSchema(l *ui.LiveSchema) {
	l.Mount(o.mount)
	l.Loads("stats", o.loadStats)
	l.Loads("rows", o.loadRows)
	l.Loads("recent", o.loadRecent)
}

func (o *liveOrders) mount(_ context.Context) error { o.Title = "Orders"; return nil }

func (o *liveOrders) loadStats(ctx context.Context) error {
	select {
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	o.Stats = map[string]int{"open": len(o.db.names)}
	return nil
}

func (o *liveOrders) loadRows(_ context.Context) error {
	o.Rows = o.db.find("a")
	return nil
}

func (o *liveOrders) loadRecent(_ context.Context) error {
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

// patchedOrders loads slowly and stays mounted across a patch.
type patchedOrders struct {
	ui.Live
	Page  int      `json:"page" url:"page"`
	Rows  []string `json:"rows" defer:""`
	Stats int      `json:"stats" defer:""`
}

type ordersParams struct {
	URLParams
	Page int
}

func (o *patchedOrders) Render() ui.Node { return ui.IslandPage(o) }
func (o *patchedOrders) LiveSchema(l *ui.LiveSchema) {
	l.Mount(o.mount)
	l.Loads("stats", o.loadStats)
	l.Loads("rows", o.loadRows)
}

func (o *patchedOrders) mount(_ context.Context) error     { o.Subscribe("orders"); return nil }
func (o *patchedOrders) Update(p ordersParams)             { o.Page = p.Page }
func (o *patchedOrders) loadStats(_ context.Context) error { o.Stats = 7; return nil }
func (o *patchedOrders) loadRows(ctx context.Context) error {
	select {
	case <-time.After(200 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	o.Rows = []string{"a"}
	return nil
}

func init() {
	ui.Island("components/Orders.vue", ui.Layout(titledDoc("orders"), shell))(vuka.TypeOf[patchedOrders]("web.patchedOrders"))
}

// A patch arriving while a load runs keeps the load: the page is the same.
func TestDeferredLiveSurvivesPatch(t *testing.T) {
	a := New(Only("none"))
	a.Live("/patched", &patchedOrders{})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	for _, v := range []int{1, 2} {
		_, tok := pageToken(t, srv, "/patched?page=1")
		c, err := dial(t, srv, tok, nil)
		if err != nil {
			t.Fatal(err)
		}
		send(t, c, map[string]any{"type": "join", "ref": 1, "v": v})
		send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/patched?page=2", "patch": true})
		var got string
		for !strings.Contains(got, "&#34;rows&#34;:[&#34;a&#34;]") {
			rctx, done := context.WithTimeout(context.Background(), 3*time.Second)
			_, b, err := c.Read(rctx)
			done()
			if err != nil {
				t.Fatalf("v%d: the rows never came: %v (last %s)", v, err, got)
			}
			if m := (live.ServerMessage{}); json.Unmarshal(b, &m) == nil && m.Type == live.Error {
				t.Fatalf("v%d: %s", v, b)
			}
			got = string(b)
			if strings.Contains(got, `"ref":2`) && !strings.Contains(got, `"patch"`) {
				t.Fatalf("v%d: the navigation wasn't a patch: %s", v, got)
			}
		}
		if !strings.Contains(got, "&#34;page&#34;:2") || !strings.Contains(got, "&#34;stats&#34;:7") {
			t.Errorf("v%d: after the load: %s", v, got)
		}
		c.CloseNow()
	}
}

// slowDash's component module takes longer to load than its loaders.
type slowDash struct {
	ui.Live
	Rows []string `json:"rows" defer:""`
}

func (d *slowDash) Render() ui.Node                  { return ui.IslandPage(d) }
func (d *slowDash) LiveSchema(l *ui.LiveSchema)      { l.Loads("rows", d.loadRows) }
func (d *slowDash) loadRows(_ context.Context) error { d.Rows = []string{"Ada"}; return nil }

func init() {
	ui.Island("components/SlowDash.vue", ui.Layout(fullPage, shell))(vuka.TypeOf[slowDash]("web.slowDash"))
}

const slowDashJS = `await new Promise((r) => setTimeout(r, 600));
export function mount(el, props, ctx) {
  const show = () => { el.textContent = JSON.stringify({ rows: ctx.live.rows.value ?? null, pending: ctx.live.rows.pending }); };
  ctx.live.subscribe(show);
  show();
  return { update() { show(); } };
}`

// Loads done before the component's module loads reach it as it mounts.
func TestBrowserDeferredBeforeMount(t *testing.T) {
	RegisterAssets([]byte(`{"islands":{"slowDash":{"name":"slowDash","module":"slowdash.abcd1234.js","framework":"vanilla"}}}`))
	RegisterIsland("slowdash.abcd1234.js", []byte(slowDashJS))
	t.Cleanup(func() { RegisterAssets(nil); delete(islandReg, "slowdash.abcd1234.js") })
	a := New(Only("none"))
	a.Live("/slow", &slowDash{})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)

	p := chrome.Launch(t).Open(srv.URL + "/slow?x=1")
	p.WaitText("vk-island", `{"rows":["Ada"],"pending":false}`)
}
