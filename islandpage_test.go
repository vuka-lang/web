package web

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/vuka/v2"
)

// applicantDB is a dependency of a page backed by its island.
type applicantDB struct{ names []string }

func (d *applicantDB) find(q string) []string {
	var out []string
	for _, n := range d.names {
		if strings.Contains(strings.ToLower(n), strings.ToLower(q)) {
			out = append(out, n)
		}
	}
	return out
}

// applicants is a stateless page backed by its island: Q bound to ?q=, Rows
// a prop, db the server's own.
type applicants struct {
	Q      string   `json:"q" url:"q"`
	Rows   []string `json:"rows"`
	Mounts int      `json:"mounts"`
	db     *applicantDB
	secret string
}

func (a *applicants) LiveSchema(l *ui.LiveSchema) { l.Mount(a.mount) }
func (a *applicants) mount(_ context.Context) error {
	a.Mounts++
	a.secret = "never sent"
	a.Rows = a.db.find(a.Q)
	return nil
}

func (a *applicants) Search(ctx context.Context, q string) error {
	if len(q) == 1 {
		return Invalid{"q": "type at least 2 letters"}
	}
	a.Q, a.Rows = q, a.db.find(q)
	return nil
}

func (a *applicants) Clear() { a.Q, a.Rows = "", nil }

func (a *applicants) Away() error { return Redirect("/elsewhere") }

func (a *applicants) Boom() error { return http.ErrAbortHandler }

// liveApplicants is the live variant: ui.Live embedded, Q bound to ?q= and
// the Mount hook in its LiveSchema; its Render is the compiler's, written
// here.
type liveApplicants struct {
	ui.Live
	Q    string   `json:"q"`
	Rows []string `json:"rows"`
	db   *applicantDB
}

func (a *liveApplicants) LiveSchema(l *ui.LiveSchema) {
	l.Field(&a.Q, ui.URL("q"))
	l.Mount(a.mount)
}

func (a *liveApplicants) Render() ui.Node { return ui.IslandPage(a) }

func (a *liveApplicants) mount(ctx context.Context) error {
	a.Rows = a.db.find(a.Q)
	return nil
}

func (a *liveApplicants) Search(q string) { a.Q, a.Rows = q, a.db.find(q) }

func init() {
	ui.Island("components/Applicants.vue")(vuka.TypeOf[applicants]("web.applicants"))
	ui.Island("components/Applicants.vue")(vuka.TypeOf[liveApplicants]("web.liveApplicants"))
}

func pageApp(opts ...Option) *App {
	db := &applicantDB{names: []string{"Ada", "Alan", "Grace"}}
	return New(append([]Option{Only("none"), Provide(func() *applicantDB { return db })}, opts...)...).
		Page("/applicants", &applicants{}).
		Page("/live/applicants", &liveApplicants{})
}

var (
	propsAttr = regexp.MustCompile(`data-vk-props="([^"]*)"`)
	sigAttr   = regexp.MustCompile(`data-vk-sig="([^"]*)"`)
	deferAttr = regexp.MustCompile(`data-vk-defer="([^"]*)"`)
)

// rendered is a page's island props and signature.
func rendered(t *testing.T, body string) (props map[string]any, raw, sig string) {
	t.Helper()
	m := propsAttr.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no data-vk-props in %s", body)
	}
	raw = html.UnescapeString(m[1])
	if err := json.Unmarshal([]byte(raw), &props); err != nil {
		t.Fatal(err)
	}
	if s := sigAttr.FindStringSubmatch(body); s != nil {
		sig = html.UnescapeString(s[1])
	}
	return props, raw, sig
}

func callBody(method string, args []any, props, sig string) string {
	b, _ := json.Marshal(map[string]any{"method": method, "args": args, "props": props, "sig": sig})
	return string(b)
}

func TestIslandPageStateless(t *testing.T) {
	h := pageApp().Handler()
	code, body, _ := do(t, h, "GET", "/applicants?q=a", "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	props, raw, sig := rendered(t, body)
	if props["q"] != "a" || props["mounts"] != 1.0 || len(props["rows"].([]any)) != 3 {
		t.Errorf("props %v", props)
	}
	for _, no := range []string{"never sent", "secret", "db"} {
		if strings.Contains(raw, no) {
			t.Errorf("an unexported field reached the props: %s", raw)
		}
	}
	for _, want := range []string{`data-vk-island="applicants"`, `data-vk-client="load"`, `data-vk-call="/applicants"`,
		`data-vk-methods="[&#34;away&#34;,&#34;boom&#34;,&#34;clear&#34;,&#34;search&#34;]"`, `data-vk-url="{&#34;q&#34;:&#34;q&#34;}"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "data-vk-on-m-") {
		t.Errorf("a stateless page registers no live handlers: %s", body)
	}

	// A call: rebuilt from the signed props, the dependency injected again,
	// Mount and the method run, the new props signed.
	code, resp, _ := do(t, h, "POST", "/applicants", callBody("search", []any{"gr"}, raw, sig), "Content-Type", "application/json")
	var out struct{ Props, Sig string }
	if code != 200 || json.Unmarshal([]byte(resp), &out) != nil {
		t.Fatalf("call: %d %s", code, resp)
	}
	var np map[string]any
	json.Unmarshal([]byte(out.Props), &np)
	if np["q"] != "gr" || np["mounts"] != 2.0 || len(np["rows"].([]any)) != 1 {
		t.Errorf("after search: %v", np)
	}
	// The new props and signature make the next call.
	code, resp, _ = do(t, h, "POST", "/applicants", callBody("clear", nil, out.Props, out.Sig), "Content-Type", "application/json")
	if code != 200 || !strings.Contains(resp, `\"q\":\"\"`) {
		t.Errorf("clear: %d %s", code, resp)
	}

	// Field errors, a redirect, a hidden error.
	expect(t, h, "POST", "/applicants", callBody("search", []any{"g"}, raw, sig), 422, `"fields":{"q":"type at least 2 letters"}`, "Content-Type", "application/json")
	expect(t, h, "POST", "/applicants", callBody("away", nil, raw, sig), 200, `"redirect":"/elsewhere"`, "Content-Type", "application/json")
	code, resp, _ = do(t, h, "POST", "/applicants", callBody("boom", nil, raw, sig), "Content-Type", "application/json")
	if code != 500 || strings.Contains(resp, "abort") {
		t.Errorf("boom: %d %s", code, resp)
	}
	expect(t, h, "POST", "/applicants", callBody("nope", nil, raw, sig), 400, `has no method`, "Content-Type", "application/json")
}

func TestIslandPageTampering(t *testing.T) {
	h := pageApp().Handler()
	_, body, _ := do(t, h, "GET", "/applicants?q=a", "")
	_, raw, sig := rendered(t, body)
	forged := strings.Replace(raw, `"mounts":1`, `"mounts":99`, 1)
	expect(t, h, "POST", "/applicants", callBody("clear", nil, forged, sig), 403, "aren't the server's", "Content-Type", "application/json")
	expect(t, h, "POST", "/applicants", callBody("clear", nil, raw, sig+"x"), 403, "aren't the server's", "Content-Type", "application/json")
	expect(t, h, "POST", "/applicants", callBody("clear", nil, raw, ""), 403, "aren't the server's", "Content-Type", "application/json")
	// Another app's key signs nothing this one takes.
	expect(t, pageApp().Handler(), "POST", "/applicants", callBody("clear", nil, raw, sig), 403, "aren't the server's", "Content-Type", "application/json")
	// A cross-site call is refused.
	expect(t, h, "POST", "/applicants", callBody("clear", nil, raw, sig), 403, "cross-origin", "Content-Type", "application/json", "Sec-Fetch-Site", "cross-site")
}

// A stateless page's props verify only for the visitor they were signed
// for, at the page they were signed at, until they expire.
func TestIslandPageSignedForTheVisitor(t *testing.T) {
	who := LiveIdentity(func(ctx context.Context) string {
		if r := Request(ctx); r != nil {
			return r.Header.Get("X-Who")
		}
		return ""
	})
	key := LiveKey([]byte(strings.Repeat("k", 32)))
	h := pageApp(who, key).Page("/elsewhere", &applicants{}).Handler()
	_, body, _ := do(t, h, "GET", "/applicants?q=a", "", "X-Who", "ada")
	_, raw, sig := rendered(t, body)
	call := callBody("clear", nil, raw, sig)
	expect(t, h, "POST", "/applicants", call, 200, `"props"`, "Content-Type", "application/json", "X-Who", "ada")
	expect(t, h, "POST", "/applicants", call, 403, "aren't the server's", "Content-Type", "application/json", "X-Who", "bob")
	expect(t, h, "POST", "/applicants", call, 403, "aren't the server's", "Content-Type", "application/json")
	expect(t, h, "POST", "/elsewhere", call, 403, "aren't the server's", "Content-Type", "application/json", "X-Who", "ada")

	_, body, _ = do(t, h, "GET", "/applicants?q=a", "")
	_, raw, sig = rendered(t, body)
	expect(t, h, "POST", "/applicants", callBody("clear", nil, raw, sig), 200, `"props"`, "Content-Type", "application/json")
	expect(t, h, "POST", "/applicants", callBody("clear", nil, raw, sig), 403, "aren't the server's", "Content-Type", "application/json", "X-Who", "ada")

	expired := pageApp(who, key, LiveTokenTTL(-time.Second)).Handler()
	_, body, _ = do(t, expired, "GET", "/applicants?q=a", "", "X-Who", "ada")
	_, raw, sig = rendered(t, body)
	expect(t, expired, "POST", "/applicants", callBody("clear", nil, raw, sig), 403, "expired", "Content-Type", "application/json", "X-Who", "ada")
}

func TestIslandPageLive(t *testing.T) {
	h := pageApp().Handler()
	code, body, _ := do(t, h, "GET", "/live/applicants?q=gr", "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	props, raw, _ := rendered(t, body)
	if props["q"] != "gr" || len(props["rows"].([]any)) != 1 || strings.Contains(raw, "db") {
		t.Errorf("props %v", props)
	}
	for _, want := range []string{`data-vk-methods="[&#34;search&#34;]"`, `data-vk-on-m-search="c1:`, `data-vk-token=`} {
		if !strings.Contains(body, want) {
			t.Errorf("live page lacks %s:\n%s", want, body)
		}
	}
}

func TestIslandPageNeedsIsland(t *testing.T) {
	type plain struct{ X int }
	if err := New(Only("none")).Page("/p", &plain{}).Build(); err == nil || !strings.Contains(err.Error(), "add @ui.Island") {
		t.Errorf("got %v", err)
	}
}

// levels is a stateless page whose Q the component sets (bind:); Changed
// runs after.
type levels struct {
	Q       string   `json:"q" url:"q" bind:"q"`
	Rows    []string `json:"rows"`
	Changes int      `json:"changes"`
	db      *applicantDB
}

func (l *levels) LiveSchema(s *ui.LiveSchema) {
	s.Mount(l.mount)
	s.OnChange(l.onChange)
}

func (l *levels) mount(_ context.Context) error { l.Rows = l.db.find(l.Q); return nil }

func (l *levels) onChange(_ context.Context, _ ui.Changed) error {
	if l.Q == "x" {
		return Invalid{"q": "no x"}
	}
	l.Changes++
	l.Rows = l.db.find(l.Q)
	return nil
}

func (l *levels) Clear() { l.Q, l.Rows = "", nil }

type liveLevels struct {
	ui.Live
	Q       string   `json:"q" url:"q" bind:"q"`
	Rows    []string `json:"rows"`
	Changes int      `json:"changes"`
	db      *applicantDB
}

func (l *liveLevels) Render() ui.Node { return ui.IslandPage(l) }
func (l *liveLevels) LiveSchema(s *ui.LiveSchema) {
	s.Mount(l.mount)
	s.OnChange(l.onChange)
}

func (l *liveLevels) mount(_ context.Context) error { l.Rows = l.db.find(l.Q); return nil }
func (l *liveLevels) onChange(_ context.Context, _ ui.Changed) error {
	l.Changes++
	l.Rows = l.db.find(l.Q)
	return nil
}

type clashing struct {
	List []string `json:"search"`
}

func (c *clashing) Search() {}

func init() {
	ui.Island("components/Levels.vue")(vuka.TypeOf[levels]("web.levels"))
	ui.Island("components/Levels.vue")(vuka.TypeOf[liveLevels]("web.liveLevels"))
	ui.Island("components/Levels.vue")(vuka.TypeOf[clashing]("web.clashing"))
}

func setBody(set map[string]any, method string, props, sig string) string {
	b, _ := json.Marshal(map[string]any{"method": method, "set": set, "props": props, "sig": sig})
	return string(b)
}

func TestIslandPageBoundStateless(t *testing.T) {
	h := pageApp().Page("/levels", &levels{}).Handler()
	_, body, _ := do(t, h, "GET", "/levels?q=a", "")
	_, raw, sig := rendered(t, body)
	if !strings.Contains(body, `data-vk-fields="{&#34;changes&#34;:{&#34;w&#34;:&#34;changes&#34;},&#34;q&#34;:{&#34;d&#34;:300,&#34;w&#34;:&#34;q&#34;},&#34;rows&#34;:{&#34;w&#34;:&#34;rows&#34;}}"`) {
		t.Errorf("no data-vk-fields: %s", body)
	}
	code, resp, _ := do(t, h, "POST", "/levels", setBody(map[string]any{"q": "gr"}, "", raw, sig), "Content-Type", "application/json")
	var out struct{ Props, Sig string }
	if code != 200 || json.Unmarshal([]byte(resp), &out) != nil {
		t.Fatalf("set: %d %s", code, resp)
	}
	var np map[string]any
	json.Unmarshal([]byte(out.Props), &np)
	if np["q"] != "gr" || np["changes"] != 1.0 || len(np["rows"].([]any)) != 1 || out.Sig == sig {
		t.Errorf("after the set: %v %s", np, out.Sig)
	}
	// The new props, re-signed, make the next call; a set before a method
	// is applied first.
	code, resp, _ = do(t, h, "POST", "/levels", setBody(map[string]any{"q": "al"}, "clear", out.Props, out.Sig), "Content-Type", "application/json")
	if code != 200 || !strings.Contains(resp, `\"q\":\"\"`) || !strings.Contains(resp, `\"changes\":2`) {
		t.Errorf("set + clear: %d %s", code, resp)
	}

	// A field that isn't bound is refused; so is a set with forged props,
	// and Changed's error is the component's.
	expect(t, h, "POST", "/levels", setBody(map[string]any{"rows": []string{"Eve"}}, "", raw, sig), 403, `"fields":{"rows":"can't be set"}`, "Content-Type", "application/json")
	expect(t, h, "POST", "/levels", setBody(map[string]any{"changes": 9, "q": "b"}, "", raw, sig), 403, `can't be set`, "Content-Type", "application/json")
	expect(t, h, "POST", "/levels", setBody(map[string]any{"q": 7}, "", raw, sig), 422, `"fields":{"q":"not a string"}`, "Content-Type", "application/json")
	expect(t, h, "POST", "/levels", setBody(map[string]any{"q": "gr"}, "", strings.Replace(raw, `"changes":0`, `"changes":5`, 1), sig), 403, "aren't the server's", "Content-Type", "application/json")
	expect(t, h, "POST", "/levels", setBody(map[string]any{"q": "x"}, "", raw, sig), 422, `"fields":{"q":"no x"}`, "Content-Type", "application/json")
	expect(t, h, "POST", "/levels", setBody(nil, "", raw, sig), 400, `has no method`, "Content-Type", "application/json")
	expect(t, h, "POST", "/levels", setBody(map[string]any{"q": "gr"}, "", raw, sig), 403, "cross-origin", "Content-Type", "application/json", "Sec-Fetch-Site", "cross-site")
}

func TestIslandPageBoundLive(t *testing.T) {
	srv := httptest.NewServer(pageApp().Page("/live/levels", &liveLevels{}).Handler())
	defer srv.Close()
	c, html := joined(t, srv, "/live/levels?q=a")
	set := handlerOf(t, html, "m-_set", 0)
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": set, "event": "emit", "data": map[string]any{"q": "gr"}})
	m := must(t, c)
	if m.Type != live.PatchMsg || m.Ref != 2 || len(m.Updates) != 1 {
		t.Fatalf("set: %+v", m)
	}
	props, _, _ := rendered(t, m.Updates[0].HTML)
	if props["q"] != "gr" || props["changes"] != 1.0 || len(props["rows"].([]any)) != 1 || strings.Contains(m.Updates[0].HTML, "data-vk-err") {
		t.Errorf("after the set: %v\n%s", props, m.Updates[0].HTML)
	}
	send(t, c, map[string]any{"type": "event", "ref": 3, "target": set, "event": "emit", "data": map[string]any{"rows": []string{"Eve"}}})
	m = must(t, c)
	if len(m.Updates) != 1 || !strings.Contains(m.Updates[0].HTML, `data-vk-err="{&#34;status&#34;:403,&#34;message&#34;:&#34;rows: can&#39;t be set&#34;`) {
		t.Fatalf("a set of a field that isn't bound: %+v", m)
	}
	if props, _, _ := rendered(t, m.Updates[0].HTML); props["q"] != "gr" || len(props["rows"].([]any)) != 1 {
		t.Errorf("a refused set changed the page: %v", props)
	}
}

func TestIslandPageNameClash(t *testing.T) {
	err := New(Only("none")).Page("/c", &clashing{}).Build()
	if err == nil || !strings.Contains(err.Error(), "the method Search and the field List are both live.search") {
		t.Errorf("got %v", err)
	}
}
