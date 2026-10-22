package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/ui/live"
	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/web/internal/chrome"
)

// withToolbar turns the toolbar on for the test's development apps, which
// test binaries otherwise don't get.
func withToolbar(t testing.TB) {
	toolbarInTests = true
	t.Cleanup(func() { toolbarInTests = false })
}

func fullPage(body ui.Node) ui.Node {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("t"))), ui.El("body", nil, body))
}

func toolbarApp(opts ...Option) *App {
	a := New(append([]Option{Only("none")}, opts...)...)
	a.Get("/page", func() ui.Node { return fullPage(ui.El("p", nil, ui.Text("hi"))) })
	a.Post("/page", func() ui.Node { return fullPage(ui.El("p", nil, ui.Text("posted"))) })
	a.Get("/frag", func() ui.Node { return ui.El("p", nil, ui.Text("a fragment")) })
	a.Get("/json", func() Pet { return Pet{1, "Rex"} })
	a.Live("/counter", &counter{Label: "n="})
	return a
}

type answer struct {
	status int
	header http.Header
	body   string
}

func tbDo(t testing.TB, h http.Handler, method, path string, header ...string) answer {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return answer{w.Code, w.Header(), w.Body.String()}
}

var toolbarTagRe = regexp.MustCompile(`<script src="/_vuka/toolbar\.js\?v=[^"]+" data-vk-toolbar="([^"]+)"( data-vk-build)? defer></script>`)

func TestToolbarInjection(t *testing.T) {
	withToolbar(t)
	plain := toolbarApp().Handler()
	dev := toolbarApp(Dev()).Handler()

	got, want := tbDo(t, dev, "GET", "/page"), tbDo(t, plain, "GET", "/page")
	m := toolbarTagRe.FindStringSubmatch(got.body)
	if m == nil || m[1] != got.header.Get(ToolbarHeader) {
		t.Fatalf("a development page: %s (header %q)", got.body, got.header.Get(ToolbarHeader))
	}
	if strings.Index(got.body, m[0]) > strings.Index(got.body, "</head>") {
		t.Errorf("the toolbar's script isn't in the head: %s", got.body)
	}
	if strings.Replace(got.body, m[0], "", 1) != want.body {
		t.Errorf("the page differs beyond the toolbar's script:\n%s\n%s", got.body, want.body)
	}
	if want.header.Get(ToolbarHeader) != "" || strings.Contains(want.body, "toolbar") {
		t.Errorf("a production page has the toolbar: %s %v", want.body, want.header)
	}
	if tbDo(t, plain, "GET", toolbarJS).status != http.StatusNotFound || tbDo(t, plain, "GET", "/_vuka/toolbar/build").status != http.StatusNotFound {
		t.Error("a production app serves the toolbar")
	}
	if js := tbDo(t, dev, "GET", toolbarJS); js.status != 200 || !strings.Contains(js.body, ".drawer") || strings.Contains(js.body, "__CSS__") {
		t.Errorf("toolbar.js: %d %.200s", js.status, js.body)
	}

	for _, c := range []struct {
		method, path string
		header       []string
	}{
		{"GET", "/frag", nil},
		{"GET", "/json", nil},
		{"POST", "/page", nil},
		{"GET", "/page", []string{"Sec-Fetch-Dest", "empty"}}, // fetched by a script
	} {
		a := tbDo(t, dev, c.method, c.path, c.header...)
		if a.header.Get(ToolbarHeader) == "" || strings.Contains(a.body, "toolbar.js") {
			t.Errorf("%s %s %v: %s (header %q)", c.method, c.path, c.header, a.body, a.header.Get(ToolbarHeader))
		}
	}

	live := tbDo(t, dev, "GET", "/counter")
	id := live.header.Get(ToolbarHeader)
	if !strings.Contains(live.body, `data-vk-toolbar="`+id+`" defer></script></head>`) || !regexp.MustCompile(`live\.js\?v=[^"]+" data-vk-token="[^"]+" data-vk-toolbar="`+id+`"`).MatchString(live.body) {
		t.Errorf("a live page: the toolbar's script and the socket's record: %s", live.body)
	}

	for name, h := range map[string]http.Handler{
		"NoToolbar":            toolbarApp(Dev(), NoToolbar()).Handler(),
		"VUKA_ENV=development": nil,
	} {
		if h == nil {
			t.Setenv("VUKA_ENV", "development")
			h = toolbarApp().Handler()
			os.Unsetenv("VUKA_ENV")
			if a := tbDo(t, h, "GET", "/page"); !toolbarTagRe.MatchString(a.body) {
				t.Errorf("%s: no toolbar: %s", name, a.body)
			}
			continue
		}
		if a := tbDo(t, h, "GET", "/page"); a.header.Get(ToolbarHeader) != "" || strings.Contains(a.body, "toolbar") || tbDo(t, h, "GET", toolbarJS).status != 404 {
			t.Errorf("%s: a toolbar: %s", name, a.body)
		}
	}

	toolbarInTests = false
	if a := tbDo(t, toolbarApp(Dev()).Handler(), "GET", "/page"); strings.Contains(a.body, "toolbar") {
		t.Errorf("a test binary's app has a toolbar: %s", a.body)
	}
	t.Setenv(DevDirEnv, t.TempDir()) // vuka run
	if a := tbDo(t, toolbarApp().Handler(), "GET", "/page"); !strings.Contains(a.body, " data-vk-build defer") {
		t.Errorf("under vuka run: %s", a.body)
	}
}

func TestToolbarRing(t *testing.T) {
	tb := &toolbar{byID: map[string]*tbEntry{}}
	var ids []string
	for i := range toolbarKeep + 30 {
		e := &tbEntry{tbRecord: tbRecord{ID: fmt.Sprint("r", i)}}
		tb.add(e)
		ids = append(ids, e.ID)
	}
	for i, id := range ids {
		if kept := tb.get(id) != nil; kept != (i >= 30) {
			t.Errorf("%s kept: %v", id, kept)
		}
	}
	if len(tb.byID) != toolbarKeep {
		t.Errorf("%d records indexed, want %d", len(tb.byID), toolbarKeep)
	}

	withToolbar(t)
	a := toolbarApp(Dev())
	h := a.Handler()
	first := tbDo(t, h, "GET", "/json").header.Get(ToolbarHeader)
	for range toolbarKeep {
		tbDo(t, h, "GET", "/json")
	}
	if r := tbDo(t, h, "GET", "/_vuka/toolbar/r/"+first); r.status != 404 || !strings.Contains(r.body, "latest 100 requests") {
		t.Errorf("a record no longer kept: %d %s", r.status, r.body)
	}
	if r := tbDo(t, h, "GET", "/_vuka/toolbar/r/nope"); r.status != 404 {
		t.Errorf("an unknown record: %d %s", r.status, r.body)
	}
}

type tbStore struct{}

type lookup struct {
	Key string
	Hit bool
}

func audit(c *vuka.Call) { c.Next() }

// greet is a decorated function, as Vuka generates one: @audit runs around it.
func greet(name string) string {
	fn := &vuka.Func{Name: "main.greet", ErrIndex: -1, CtxIndex: -1, Zero: func() []any { return []any{""} }}
	c := vuka.NewCall(fn, []vuka.Decorator{audit}, nil, []any{name}, func(c *vuka.Call) []any { return []any{"hello " + c.Args[0].(string)} })
	c.Run()
	return c.Results[0].(string)
}

func stamp(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Stamp", "1")
		h.ServeHTTP(w, r)
	})
}

// fetchRecord fetches a request's record with its panels.
func fetchRecord(t testing.TB, h http.Handler, id string) (map[string]any, string) {
	t.Helper()
	r := tbDo(t, h, "GET", "/_vuka/toolbar/r/"+id+"?panels")
	var out map[string]any
	if err := json.Unmarshal([]byte(r.body), &out); err != nil || r.status != 200 {
		t.Fatalf("record %s: %d %s", id, r.status, r.body)
	}
	html, _ := out["html"].(string)
	return out, html
}

// tbShowPet's names are recorded as the compiler records a handler's: q
// binds the query by its name.
func tbShowPet(ctx context.Context, id int, q string, s *tbStore) ui.Node {
	for i := range 4 {
		NoteStatement(ctx, Statement{SQL: "SELECT * FROM owners WHERE id = ?", Args: []any{i}, Start: time.Now(), Duration: time.Millisecond, Rows: -1})
	}
	NoteStatement(ctx, Statement{SQL: "SELECT * FROM pets WHERE id = ?", Args: []any{id}, Start: time.Now(), Duration: 3 * time.Millisecond, Rows: -1})
	NoteStatement(ctx, Statement{SQL: "UPDATE pets SET seen = 1", Err: errors.New("database is locked"), Start: time.Now(), Rows: 0})
	slog.InfoContext(ctx, "showing a pet", "id", id)
	slog.Warn("no context, the same goroutine") // reaches the request through its goroutine
	Note(ctx, "Cache", lookup{"pet:7", true})
	Note(ctx, "Plain", map[string]int{"a": 1})
	return fullPage(ui.El("p", nil, ui.Text(greet(q))))
}

func init() { vuka.RegisterParams(tbShowPet, "ctx", "id", "q", "s") }

func TestToolbarRecord(t *testing.T) {
	withToolbar(t)
	AddPanel("Cache", func(notes []any) ui.Node {
		var lis []ui.Node
		for _, n := range notes {
			l := n.(lookup)
			lis = append(lis, ui.El("li", nil, ui.Text(fmt.Sprintf("%s hit=%v", l.Key, l.Hit))))
		}
		return ui.El("ul", []ui.Attr{{Name: "class", Value: "cache"}}, lis...)
	})
	AddPanel("Broken", func([]any) ui.Node { panic("no") })
	t.Cleanup(func() { panels = nil })

	a := New(Only("none"), Dev(), Use(stamp), Provide(&tbStore{}))
	a.Get("/pets/{id}", tbShowPet, Wrap(stamp))
	h := a.Handler()

	res := tbDo(t, h, "GET", "/pets/7?q=Ada", "Cookie", "session=secret")
	if !strings.Contains(res.body, "hello Ada") {
		t.Fatalf("page: %s", res.body)
	}
	sum, html := fetchRecord(t, h, res.header.Get(ToolbarHeader))
	if sum["status"] != 200.0 || sum["queries"] != 6.0 || sum["tone"] != "err" || sum["method"] != "GET" || sum["uri"] != "/pets/7?q=Ada" {
		t.Errorf("summary: %v", sum)
	}
	for _, want := range []string{
		// Request
		"<code>GET /pets/{id}</code>", "web.tbShowPet", "toolbar_test.go:",
		"<code>id</code></td><td class=\"dim\">path</td><td><code class=\"wrap\">7</code>",
		"<code>q</code></td><td class=\"dim\">query</td><td><code class=\"wrap\">Ada</code>",
		"<code>s</code></td><td class=\"dim\">dependency</td><td><code class=\"wrap\">*web.tbStore</code>",
		"<th>Cookie</th><td>(hidden)</td>", "<th>X-Stamp</th><td>1</td>",
		// SQL
		"<b>6</b> statements", "<b>4</b> repeated", "<b>1</b> failed", "<b>N+1</b> — this statement ran 4 times",
		"<span class=\"times\">×4</span>", "database is locked", "<b>SELECT</b> * <b>FROM</b> pets",
		// Timeline
		"<span class=\"kind\">middleware</span> web.stamp", "<span class=\"kind\">handler</span> web.tbShowPet<",
		"<span class=\"kind\">decorator</span> @audit", "<span class=\"kind\">func</span> greet",
		"<span class=\"kind\">render</span> page", "<span class=\"kind\">sql</span> SELECT * FROM owners",
		// Logs
		"showing a pet <code class=\"attrs\">id=&#34;7&#34;</code>", "no context, the same goroutine", "level warn",
		// Custom panels
		`data-tab="Cache"`, `<ul class="cache"><li>pet:7 hit=true</li></ul>`, `data-tab="Plain"`, "map[a:1]",
		`data-tab="Broken"`, "the panel panicked: no",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the panels have no %q", want)
		}
	}
	if n := strings.Count(html, `<span class="kind">middleware</span> web.stamp`); n != 2 {
		t.Errorf("app-wide and route middleware: %d spans", n)
	}
	if strings.Contains(html, "session=secret") {
		t.Error("a cookie's value is shown")
	}

	// A context the toolbar doesn't watch records nothing.
	NoteStatement(context.Background(), Statement{SQL: "x"})
	Note(context.Background(), "Cache", 1)
}

func TestToolbarLive(t *testing.T) {
	withToolbar(t)
	app := liveApp(Dev())
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/chat/lobby")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	id := res.Header.Get(ToolbarHeader)
	tok := tokenRe.FindStringSubmatch(string(b))
	if id == "" || tok == nil {
		t.Fatalf("a live page: %s", b)
	}
	c, err := dial(t, srv, tok[1]+"&tb="+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1})
	m := must(t, c)
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, m.HTML, "submit", 0), "event": "submit", "form": map[string]any{"text": []string{"hi"}}})
	must(t, c) // the broadcast's patch
	must(t, c) // the event's reply
	send(t, c, map[string]any{"type": "event", "ref": 3, "target": "c9:9", "event": "click"})
	if m := must(t, c); m.Type != live.Error {
		t.Fatalf("an unknown handler: %+v", m)
	}

	sum, html := fetchRecord(t, app.Handler(), id)
	if sum["live"] != 4.0 || sum["tone"] != "err" {
		t.Errorf("summary: %v", sum)
	}
	for _, want := range []string{
		`data-tab="Live"`,
		`<span class="type t-join">join</span> <code>/chat/lobby</code>`,
		`<span class="type t-event">event</span> <code>chat.Send (submit)</code>`,
		`<span class="type t-broadcast">broadcast</span> <code>room:lobby web.said</code>`,
		`data-ref="3" data-err`, `live: no handler &#34;c9:9&#34;`,
		`<span class="kind">handler</span> live page web.chat`, // the join ran the route
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the Live panel has no %q", want)
		}
	}
	if regexp.MustCompile(`t-event">event</span> <code>chat.Send \(submit\)</code>\s*<span class="meta">[^<]+ · 0 q · (\d+) B`).FindStringSubmatch(html) == nil {
		t.Errorf("the event's reply size isn't shown: %s", html)
	}

	// A page an earlier build rendered gets a record of its own.
	c2, err := dial(t, srv, tok[1]+"&tb=earlier-page", nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c2, map[string]any{"type": "join", "ref": 1})
	must(t, c2)
	if sum, html := fetchRecord(t, app.Handler(), "earlier-page"); sum["earlier"] != true || !strings.Contains(html, "t-join") || !strings.Contains(html, "earlier build") {
		t.Errorf("an earlier build's page: %v %s", sum, html)
	}
}

func TestToolbarBuildState(t *testing.T) {
	withToolbar(t)
	dir := t.TempDir()
	t.Setenv(DevDirEnv, dir)
	h := toolbarApp().Handler()
	state := func() buildState {
		var st buildState
		json.Unmarshal([]byte(tbDo(t, h, "GET", "/_vuka/toolbar/build").body), &st)
		return st
	}
	if st := state(); st.State != "" {
		t.Errorf("before any build state: %+v", st)
	}
	write := func(s string) { os.WriteFile(filepath.Join(dir, "build.json"), []byte(s), 0o644) }
	write(fmt.Sprintf(`{"state":"building","since":%q}`, time.Now().Add(-1200*time.Millisecond).Format(time.RFC3339Nano)))
	if st := state(); st.State != "building" || st.Elapsed < 1.2 || st.Elapsed > 5 {
		t.Errorf("building: %+v", st)
	}
	write(`{"state":"failed","output":"./main.vuka:3:1: undefined: x\n"}`)
	if st := state(); st.State != "failed" || !strings.Contains(st.Output, "undefined: x") {
		t.Errorf("failed: %+v", st)
	}
	write(`{"state":"ok","took":1.5}`)
	if st := state(); st.State != "ok" || st.Took != 1.5 {
		t.Errorf("ok: %+v", st)
	}
	write(`{"state":`)
	if st := state(); st.State != "" {
		t.Errorf("a file half written: %+v", st)
	}
}

func TestShortNames(t *testing.T) {
	for in, want := range map[string]string{
		"main.timed at main.vuka:12":              "main.timed",
		"github.com/x/orm/ormweb.UsingFrom.func1": "ormweb.UsingFrom",
		"main.retry.func2.1":                      "main.retry",
		"github.com/x/y.(*Counter).Inc-fm":        "y.(*Counter).Inc",
	} {
		if got := shortName(in); got != want {
			t.Errorf("shortName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := handlerName((&counter{}).Inc); got != "counter.Inc" {
		t.Errorf("handlerName: %q", got)
	}
}

// BenchmarkToolbar measures a page and a JSON answer without the toolbar
// and with it.
func BenchmarkToolbar(b *testing.B) {
	for _, on := range []bool{false, true} {
		toolbarInTests = on
		h := toolbarApp(Dev()).Handler()
		toolbarInTests = false
		for _, path := range []string{"/page", "/json"} {
			b.Run(fmt.Sprintf("%s/toolbar=%v", path, on), func(b *testing.B) {
				req := httptest.NewRequest("GET", path, nil)
				b.ReportAllocs()
				for b.Loop() {
					h.ServeHTTP(httptest.NewRecorder(), req)
				}
			})
		}
	}
}

// TestBrowserToolbar opens a development page in Chrome: the handle shows
// the request, the drawer its panels, a live click joins the Live panel, a
// fetch the list of calls — and the app's styles are the same as without
// the toolbar. VUKA_SCREENSHOTS names a directory for screenshots.
func TestBrowserToolbar(t *testing.T) {
	withToolbar(t)
	css := filepath.Join(t.TempDir(), "app.css")
	os.WriteFile(css, []byte(`.btn { background-color: rgb(16, 185, 129); padding: 8px 16px; font: 18px Georgia, serif; border-radius: 6px }
.handle, .drawer, .tab { color: rgb(255, 0, 0); display: block; position: static }
* { box-sizing: content-box }`), 0o644)
	t.Setenv(CSSEnv, css)
	app := func(opts ...Option) *App {
		a := New(append([]Option{Only("none")}, opts...)...)
		a.Get("/", func(ctx context.Context) ui.Node {
			for i := range 3 {
				NoteStatement(ctx, Statement{SQL: "SELECT name FROM pets WHERE owner_id = ?", Args: []any{i}, Start: time.Now(), Duration: 2 * time.Millisecond, Rows: -1})
			}
			slog.WarnContext(ctx, "a pet without an owner", "pet", 7)
			return fullPage(ui.El("main", nil,
				ui.El("button", []ui.Attr{{Name: "class", Value: "btn handle"}, {Name: "id", Value: "styled"}}, ui.Text("Styled")),
				ui.Component("home#1", nil, &counter{Label: "n="})))
		})
		a.Get("/json", func() Pet { return Pet{1, "Rex"} })
		return a
	}
	dev := httptest.NewServer(app(Dev()).Handler())
	t.Cleanup(dev.Close) // after Chrome closes: the stylesheet's event stream holds a connection
	prod := httptest.NewServer(app().Handler())
	t.Cleanup(prod.Close)

	b := chrome.Launch(t)
	style := `(() => { const s = getComputedStyle(document.getElementById("styled"));
		return [s.backgroundColor, s.padding, s.fontFamily, s.fontSize, s.color, s.display, s.position, s.boxSizing, s.borderRadius].join(" | "); })()`
	var want, got string
	pp := b.Open(prod.URL + "/")
	pp.Wait(`getComputedStyle(document.getElementById("styled")).backgroundColor === "rgb(16, 185, 129)"`)
	pp.Eval(style, &want)

	p := b.Open(dev.URL + "/")
	p.Viewport(1400, 800, false) // the drawer beside the page
	tb := `document.querySelector("vuka-toolbar").shadowRoot`
	p.Wait(tb + `.querySelector(".handle .num")`)
	p.Wait(`getComputedStyle(document.getElementById("styled")).backgroundColor === "rgb(16, 185, 129)"`)
	p.Eval(style, &got)
	if got != want {
		t.Errorf("the app's styles changed with the toolbar:\n%s\nwithout it:\n%s", got, want)
	}
	var handle string
	p.Eval(`(() => { const h = `+tb+`.querySelector(".handle"); const s = getComputedStyle(h); return [h.textContent, s.position, s.color].join(" | "); })()`, &handle)
	if !strings.Contains(handle, "ms·3 q") || !strings.Contains(handle, "| absolute |") || strings.Contains(handle, "rgb(255, 0, 0)") {
		t.Errorf("the handle: %q", handle)
	}
	if !strings.Contains(p.Text("main"), "n=0") {
		t.Fatalf("the page: %s", p.Text("main"))
	}

	// A small indicator remains until the edge is approached or focused: 6px
	// with a mouse, a 44px touch target where nothing hovers (headless Chrome
	// on Linux reports hover: none).
	p.Wait(`(() => { const h = ` + tb + `.querySelector(".handle");
		const peek = matchMedia("(hover: none), (pointer: coarse)").matches ? 44 : 6;
		return Math.abs(innerWidth - h.getBoundingClientRect().left - peek) < 1; })()`)
	p.Eval(tb+`.querySelector(".handle").focus()`, nil)
	p.Wait(`(() => { const h = ` + tb + `.querySelector(".handle"); return Math.abs(innerWidth - h.getBoundingClientRect().right) < 1; })()`)
	p.ClickElement(tb + `.querySelector(".handle")`)
	p.Wait(tb + `.querySelector('.panel[data-panel="Request"]:not([hidden])')`)
	text := func() string {
		var s string
		p.Eval(tb+`.querySelector(".panel:not([hidden])").textContent`, &s)
		return s
	}
	if s := text(); !strings.Contains(s, "GET /") || !strings.Contains(s, "web.TestBrowserToolbar") {
		t.Errorf("the Request panel: %s", s)
	}
	p.ClickElement(tb + `.querySelector('.tab[data-tab="SQL"]')`)
	p.Wait(tb + `.querySelector('.panel[data-panel="SQL"]:not([hidden])')`)
	if s := text(); !strings.Contains(s, "SELECT name FROM pets WHERE owner_id = ?") || !strings.Contains(s, "N+1") {
		t.Errorf("the SQL panel: %s", s)
	}
	if dir := os.Getenv("VUKA_SCREENSHOTS"); dir != "" {
		p.Screenshot(filepath.Join(dir, "toolbar-sql.png"))
	}

	p.Click("main button[data-vk-on-click]")
	p.WaitText("main span", "n=1")
	p.Wait(tb + `.querySelector('.tab[data-tab="Live"]')`)
	p.ClickElement(tb + `.querySelector('.tab[data-tab="Live"]')`)
	p.Wait(tb + `.querySelector('.panel[data-panel="Live"]:not([hidden])') && ` + tb + `.querySelector('.panel[data-panel="Live"]').textContent.includes("counter.Inc (click)")`)
	if s := text(); !strings.Contains(s, "join") {
		t.Errorf("the Live panel: %s", s)
	}

	// The click's entry, opened: its frames decoded, fetched on opening.
	entry := tb + `.querySelector('.panel[data-panel="Live"] details')`
	p.Eval(`(() => { const d = `+entry+`; d.open = true; })()`, nil)
	p.Wait(entry + `.querySelector('.wpanel[data-wpanel="summary"]:not([hidden])')`)
	wpanel := func(name string) string {
		var s string
		p.ClickElement(entry + `.querySelector('.wtab[data-wtab="` + name + `"]')`)
		p.Wait(entry + `.querySelector('.wpanel[data-wpanel="` + name + `"]:not([hidden])')`)
		p.Eval(entry+`.querySelector('.wpanel[data-wpanel="`+name+`"]').textContent`, &s)
		return s
	}
	if s := wpanel("summary"); !strings.Contains(s, "c1") || !strings.Contains(s, "counter") || !strings.Contains(s, "n=1") || !strings.Contains(s, "dynamics") {
		t.Errorf("Summary: %s", s)
	}
	if s := wpanel("diff"); !strings.Contains(s, "c1 › u.") || !strings.Contains(s, "n=0") || !strings.Contains(s, "n=1") {
		t.Errorf("Diff: %s", s)
	}
	if s := wpanel("raw"); !strings.Contains(s, `"type": "patch"`) || !strings.Contains(s, `"type": "event"`) || !strings.Contains(s, `"trees"`) {
		t.Errorf("Raw: %s", s)
	}
	if dir := os.Getenv("VUKA_SCREENSHOTS"); dir != "" {
		p.Screenshot(filepath.Join(dir, "toolbar-raw.png"))
	}
	if dir := os.Getenv("VUKA_SCREENSHOTS"); dir != "" {
		p.Screenshot(filepath.Join(dir, "toolbar-live.png"))
	}

	p.Eval(`fetch("/json").then((r) => r.json())`, nil)
	p.Wait(tb + `.querySelectorAll("select option").length === 2`)
	p.Eval(`(() => { const s = `+tb+`.querySelector("select"); s.value = s.options[1].value; s.dispatchEvent(new Event("change")); })()`, nil)
	p.Wait(tb + `.querySelector('.panel[data-panel="Request"]:not([hidden])') && ` + tb + `.querySelector(".panel:not([hidden])").textContent.includes("/json")`)
	var option string
	p.Eval(tb+`.querySelector("select").selectedOptions[0].textContent`, &option)
	if option != "GET /json → 200" {
		t.Errorf("the fetch's entry: %q", option)
	}
}

// TestBrowserToolbarNavigate follows a live navigation: a link from / to
// /counter moves the handle and the Request panel to /counter's record,
// and back returns to /'s.
func TestBrowserToolbarNavigate(t *testing.T) {
	withToolbar(t)
	a := New(Only("none"), Dev())
	a.Get("/", func(ctx context.Context) ui.Node {
		NoteStatement(ctx, Statement{SQL: "SELECT 1", Start: time.Now(), Rows: -1})
		return fullPage(ui.El("main", nil, ui.Link(ui.LinkProps{Href: "/counter", ID: "go", Children: ui.Text("Counter")}),
			ui.Component("home#1", nil, &counter{Label: "home="})))
	})
	a.Live("/counter", &counter{Label: "n="}, Layout(func(page ui.Node) ui.Node {
		return fullPage(ui.El("main", nil, ui.Link(ui.LinkProps{Href: "/", ID: "home", Children: ui.Text("Home")}), page))
	}))
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)

	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/")
	p.Viewport(1400, 800, false)
	tb := `document.querySelector("vuka-toolbar").shadowRoot`
	p.Wait(tb + `.querySelector(".handle .num") && document.documentElement.getAttribute("data-vk-state") === "connected"`)
	handle := func() string {
		var s string
		p.Eval(tb+`.querySelector(".handle").textContent`, &s)
		return s
	}
	if h := handle(); !strings.Contains(h, "1 q") {
		t.Errorf("the handle on /: %q", h)
	}
	p.ClickElement(tb + `.querySelector(".handle")`)
	p.Wait(tb + `.querySelector('.panel[data-panel="Request"]:not([hidden])')`)
	var first string
	p.Eval(tb+`.querySelector("select").value`, &first)

	p.Click("#go")
	p.WaitText("main span", "n=0")
	p.Wait(`location.pathname === "/counter" && ` + tb + `.querySelector("select").selectedOptions[0].textContent === "page /counter"`)
	p.Wait(tb + `.querySelector('.panel[data-panel="Request"]:not([hidden])') && ` + tb + `.querySelector('.panel[data-panel="Request"]').textContent.includes("GET /counter")`)
	p.Wait(`!` + tb + `.querySelector(".handle").textContent.includes("loading")`)
	if h := handle(); !strings.Contains(h, "0 q") {
		t.Errorf("the handle on /counter: %q", h)
	}
	var req string
	p.Eval(tb+`.querySelector('.panel[data-panel="Request"]').textContent`, &req)
	if !strings.Contains(req, "live page web.counter") {
		t.Errorf("the Request panel: %s", req)
	}
	p.ClickElement(tb + `.querySelector('.tab[data-tab="Live"]')`)
	p.Wait(tb + `.querySelector('.panel[data-panel="Live"]:not([hidden])') && ` + tb + `.querySelector('.panel[data-panel="Live"]').textContent.includes("navigate")`)
	if dir := os.Getenv("VUKA_SCREENSHOTS"); dir != "" {
		p.Screenshot(filepath.Join(dir, "toolbar-navigate.png"))
	}

	p.Eval(`history.back()`, nil)
	p.Wait(`location.pathname === "/" && document.querySelector("#go") !== null`)
	p.Wait(tb + `.querySelector("select").value === ` + strconv.Quote(first))
	var options int
	p.Eval(tb+`.querySelectorAll("select option").length`, &options)
	if options != 2 {
		t.Errorf("%d entries in the list", options)
	}
}

// A page hears of each build state over one stream, which Stop ends.
func TestToolbarBuildStream(t *testing.T) {
	withToolbar(t)
	dir := t.TempDir()
	t.Setenv(DevDirEnv, dir)
	write := func(s string) { os.WriteFile(filepath.Join(dir, "build.json"), []byte(s), 0o644) }
	write(`{"state":"ok","took":1.5}`)
	app := toolbarApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/_vuka/toolbar/build", nil)
	req.Header.Set("Accept", "text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	events := make(chan buildState)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var st buildState
				json.Unmarshal([]byte(d), &st)
				events <- st
			}
		}
		close(events)
	}()
	next := func() (buildState, bool) {
		select {
		case st, ok := <-events:
			return st, ok
		case <-time.After(2 * time.Second):
			t.Fatal("no event")
			return buildState{}, false
		}
	}
	if st, _ := next(); st.State != "ok" || st.Took != 1.5 {
		t.Errorf("on connect: %+v", st)
	}
	write(fmt.Sprintf(`{"state":"building","since":%q}`, time.Now().Format(time.RFC3339Nano)))
	if st, _ := next(); st.State != "building" || st.Since.IsZero() || st.Elapsed != 0 {
		t.Errorf("building: %+v", st)
	}
	write(`{"state":"failed","output":"./main.vuka:3:1: undefined: x\n"}`)
	if st, _ := next(); st.State != "failed" || !strings.Contains(st.Output, "undefined: x") {
		t.Errorf("failed: %+v", st)
	}
	app.Stop(context.Background())
	if _, ok := next(); ok {
		t.Error("the stream outlived Stop")
	}
}
