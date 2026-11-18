package web

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/ui/v2/uigen"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// deferCtl steers a statsPage's loaders: rows wait for release (nil: they
// don't) and report a context ended first on ended.
type deferCtl struct {
	release chan struct{}
	ended   chan error
	fail    bool
}

// statsPage is a live page rendered by its own markup with two deferred
// groups, an embedded component deferring a third, and one more the page
// mounts on a click.
type statsPage struct {
	ui.Live
	Page  int
	ctl   *deferCtl
	stats map[string]int
	rows  []string
	more  bool
}

type statsParams struct {
	URLParams
	Page int `query:"page"`
}

func (p *statsPage) LiveSchema(l *ui.LiveSchema) {
	l.Field(&p.stats, ui.Defer("stats"))
	l.Field(&p.rows, ui.Defer("rows"))
	l.Loads("stats", p.loadStats)
	l.Loads("rows", p.loadRows)
}

func (p *statsPage) loadStats(ctx context.Context) error {
	if p.ctl.fail {
		return errors.New("stats: connection refused")
	}
	p.stats = map[string]int{"open": 3}
	return nil
}

func (p *statsPage) loadRows(ctx context.Context) error {
	if p.ctl.release != nil {
		select {
		case <-p.ctl.release:
		case <-ctx.Done():
			p.ctl.ended <- ctx.Err()
			return ctx.Err()
		}
	}
	p.rows = []string{"Ada", "Alan"}
	return nil
}

func (p *statsPage) Update(q statsParams) { p.Page = q.Page }
func (p *statsPage) More()                { p.more = true }

func (p *statsPage) Render() ui.Node {
	var stats, rows ui.Node
	switch {
	case p.Loading("stats"):
		stats = ui.El("div", []ui.Attr{{Name: "class", Value: "skeleton"}}, ui.Text("stats loading"))
	case p.LoadErr("stats") != nil:
		stats = ui.El("p", []ui.Attr{{Name: "role", Value: "alert"}}, ui.Text("stats failed: "+p.LoadErr("stats").Error()))
	default:
		stats = ui.Text(fmt.Sprintf("open %d", p.stats["open"]))
	}
	if p.Loading("rows") {
		rows = ui.Text("rows loading")
	} else {
		rows = ui.Text("rows " + strings.Join(p.rows, ","))
	}
	kids := []ui.Node{
		ui.El("section", []ui.Attr{{Name: "id", Value: "stats"}}, stats),
		ui.El("section", []ui.Attr{{Name: "id", Value: "rows"}}, rows),
		ui.El("p", []ui.Attr{{Name: "id", Value: "page"}}, ui.Child(p.Page)),
		ui.El("button", []ui.Attr{{Name: "id", Value: "more"}, {Name: "onClick", Value: ui.On(p.More)}}, ui.Text("more")),
		ui.Component("stats#users", nil, &usersCard{ID: "users"}),
	}
	if p.more {
		kids = append(kids, ui.Component("stats#more", nil, &usersCard{ID: "more-users"}))
	}
	return ui.El("main", nil, kids...)
}

// usersCard is an embedded live component deferring its own group.
type usersCard struct {
	ui.Live
	ID    string
	Users []string
}

func (c *usersCard) LiveSchema(l *ui.LiveSchema) {
	l.Field(&c.Users, ui.Defer("users"))
	l.Loads("users", c.loadUsers)
}

func (c *usersCard) loadUsers(ctx context.Context) error {
	c.Users = []string{"Grace"}
	return nil
}

func (c *usersCard) Render() ui.Node {
	text := "users loading"
	if !c.Loading("users") {
		text = "users " + strings.Join(c.Users, ",")
	}
	return ui.El("aside", []ui.Attr{{Name: "id", Value: c.ID}}, ui.Text(text))
}

func statsApp(ctl *deferCtl) *App {
	a := New(Only("none"))
	a.Live("/stats", func() *statsPage { return &statsPage{ctl: ctl} })
	a.Get("/elsewhere", func() ui.Node { return ui.El("main", nil, ui.Text("elsewhere")) })
	return a
}

// patchesUntil reads the connection's messages until the page as patched
// holds every want, failing on an error or a timeout.
func patchesUntil(t *testing.T, c *websocket.Conn, want ...string) string {
	t.Helper()
	var seen strings.Builder
	for {
		m, err := recv(c, 3*time.Second)
		if err != nil {
			t.Fatalf("waiting for %q: %v; got %s", want, err, seen.String())
		}
		if m.Type == live.Error {
			t.Fatalf("an error: %+v", m)
		}
		for _, u := range m.Updates {
			seen.WriteString(u.HTML)
		}
		all := true
		for _, w := range want {
			all = all && strings.Contains(seen.String(), w)
		}
		if all {
			return seen.String()
		}
	}
}

func TestLiveDeferredTemplateError(t *testing.T) {
	srv := httptest.NewServer(statsApp(&deferCtl{fail: true}).Handler())
	defer srv.Close()
	c, _ := joined(t, srv, "/stats")
	got := patchesUntil(t, c, `<p role="alert">stats failed: Internal Server Error</p>`, "rows Ada,Alan")
	if strings.Contains(got, "connection refused") {
		t.Errorf("the cause reached the browser: %s", got)
	}
}

// Leaving the page ends its loads; a patch keeps them.
func TestLiveDeferredTemplateNavigation(t *testing.T) {
	ctl := &deferCtl{release: make(chan struct{}), ended: make(chan error, 1)}
	srv := httptest.NewServer(statsApp(ctl).Handler())
	defer srv.Close()

	c, _ := joined(t, srv, "/stats")
	patchesUntil(t, c, "open 3")
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/stats?page=2", "patch": true})
	select {
	case err := <-ctl.ended:
		t.Fatalf("a patch ended the load: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(ctl.release)
	patchesUntil(t, c, `<p id="page">2</p>`, "rows Ada,Alan")

	ctl.release = make(chan struct{})
	c, _ = joined(t, srv, "/stats")
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/elsewhere"})
	select {
	case err := <-ctl.ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ended with %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leaving the page didn't end its load")
	}
}

// Leaving the page ends its loads before the next page's route runs, so a
// slow load's query hands the database to the navigation instead of racing it.
func TestLiveDeferredNavigationEndsLoadsFirst(t *testing.T) {
	ctl := &deferCtl{release: make(chan struct{}), ended: make(chan error, 1)}
	a := New(Only("none"))
	a.Live("/stats", func() *statsPage { return &statsPage{ctl: ctl} })
	a.Get("/next", func() ui.Node {
		select {
		case err := <-ctl.ended:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("the load ended with %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("the old page's load still ran when the next page's route did")
		}
		return ui.El("main", nil, ui.Text("next"))
	})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	c, _ := joined(t, srv, "/stats")
	patchesUntil(t, c, "open 3")
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/next"})
	for {
		m, err := recv(c, 3*time.Second)
		if err != nil {
			t.Fatalf("waiting for the navigation's reply: %v", err)
		}
		if m.Type == live.Error {
			t.Fatalf("the navigation failed: %+v", m)
		}
		if m.Ref == 2 {
			break
		}
	}
}

// tabsPage is a live page with a blocking deferred load and no Update: a
// patch can never keep it, so a tab click remounts it. probe, once set,
// makes its mount report on mounted whether the load had ended by then.
type tabsPage struct {
	ui.Live
	ctl     *deferCtl
	probe   *atomic.Bool
	mounted chan error
	rows    []string
}

func (p *tabsPage) LiveSchema(l *ui.LiveSchema) {
	l.Mount(p.mount)
	l.Field(&p.rows, ui.Defer("rows"))
	l.Loads("rows", p.loadRows)
}

func (p *tabsPage) mount(ctx context.Context) error {
	if !p.probe.Load() {
		return nil
	}
	select {
	case err := <-p.ctl.ended:
		p.mounted <- err
	case <-time.After(3 * time.Second):
		p.mounted <- errors.New("the old page's load still ran when the tab's route did")
	}
	return nil
}

func (p *tabsPage) loadRows(ctx context.Context) error {
	select {
	case <-p.ctl.release:
	case <-ctx.Done():
		p.ctl.ended <- ctx.Err()
		return ctx.Err()
	}
	p.rows = []string{"Ada"}
	return nil
}

func (p *tabsPage) Render() ui.Node {
	text := "rows loading"
	if !p.Loading("rows") {
		text = "rows " + strings.Join(p.rows, ",")
	}
	return ui.El("main", nil, ui.Text(text))
}

// A tab's click asks for a patch, but a page without an Update can't take
// one: the navigation remounts it, so its loads end before the route runs,
// as any other navigation's do.
func TestLiveDeferredPatchWithoutUpdateEndsLoadsFirst(t *testing.T) {
	ctl := &deferCtl{release: make(chan struct{}), ended: make(chan error, 1)}
	probe := &atomic.Bool{}
	mounted := make(chan error, 1)
	a := New(Only("none"))
	a.Live("/tabs", func() *tabsPage { return &tabsPage{ctl: ctl, probe: probe, mounted: mounted} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	c, _ := joined(t, srv, "/tabs")
	probe.Store(true)
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/tabs?tab=2", "patch": true})
	select {
	case err := <-mounted:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("mounting again saw %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the tab's route never ran")
	}
	for {
		m, err := recv(c, 3*time.Second)
		if err != nil {
			t.Fatalf("waiting for the navigation's reply: %v", err)
		}
		if m.Type == live.Error {
			t.Fatalf("the navigation failed: %+v", m)
		}
		if m.Ref == 2 {
			break
		}
	}
}

// slowRenderPage defers "rows", whose load is instant; the render after the
// apply runs a slow query (it waits on its context, 3s at most) — a
// DataTable loading its rows does the same.
type slowRenderPage struct {
	ui.Live
	ctl  *deferCtl
	rows []string
}

func (p *slowRenderPage) LiveSchema(l *ui.LiveSchema) {
	l.Field(&p.rows, ui.Defer("rows"))
	l.Loads("rows", p.loadRows)
}

func (p *slowRenderPage) loadRows(ctx context.Context) error {
	p.rows = []string{"Ada"}
	return nil
}

func (p *slowRenderPage) Render() ui.Node {
	if p.Loading("rows") {
		return ui.El("main", nil, ui.Text("rows loading"))
	}
	return ui.El("main", nil, ui.Ctx(func(ctx context.Context) ui.Node {
		select {
		case <-ctx.Done():
			p.ctl.ended <- ctx.Err()
			return ui.Text("rows gone")
		case <-time.After(3 * time.Second):
			p.ctl.ended <- nil
			return ui.Text("rows slow")
		}
	}))
}

func (p *slowRenderPage) Update(statsParams) {}

// A navigate arriving while a deferred load's apply renders — a read — ends
// the page's context, so the slow query the render runs stops instead of
// making the navigation wait behind it.
func TestLiveDeferredNavigateInterruptsASlowApplyRender(t *testing.T) {
	ctl := &deferCtl{ended: make(chan error, 1)}
	a := New(Only("none"))
	a.Live("/slow", func() *slowRenderPage { return &slowRenderPage{ctl: ctl} })
	a.Get("/next", func() ui.Node { return ui.El("main", nil, ui.Text("next")) })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	c, _ := joined(t, srv, "/slow")
	time.Sleep(300 * time.Millisecond) // the load applied; its render is in the slow query
	start := time.Now()
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/next"})
	select {
	case err := <-ctl.ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the render's query ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the render's query was not ended")
	}
	for {
		m, err := recv(c, 2*time.Second)
		if err != nil {
			t.Fatalf("waiting for the navigation's reply: %v", err)
		}
		if m.Type == live.Error {
			t.Fatalf("the navigation failed: %+v", m)
		}
		if m.Ref == 2 {
			break
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the navigation waited %v for the render", d)
	}
}

// searchCtl steers a searchPage: each handler works for work, then the
// render after it runs a query of slow, unless its context ends first.
type searchCtl struct {
	work, slow time.Duration
	mu         sync.Mutex
	handled    []error // each handler's context's error once its work was done, in order
	ended      chan error
}

// searchPage is a search box whose every event's handler only edits the
// page's state, and whose render then runs a slow query — a DataTable with
// a Load searching as the user types.
type searchPage struct {
	ui.Live
	ctl *searchCtl
	n   int
}

func (p *searchPage) Search(ctx context.Context) error {
	select {
	case <-ctx.Done():
	case <-time.After(p.ctl.work):
	}
	p.ctl.mu.Lock()
	p.ctl.handled = append(p.ctl.handled, ctx.Err())
	p.n = len(p.ctl.handled)
	p.ctl.mu.Unlock()
	return nil
}

func (p *searchPage) Update(statsParams) {}

func (p *searchPage) Render() ui.Node {
	results := ui.Text("no search")
	if p.n > 0 {
		results = ui.Ctx(func(ctx context.Context) ui.Node {
			select {
			case <-ctx.Done():
				p.ctl.ended <- ctx.Err()
				return ui.Text("results gone")
			case <-time.After(p.ctl.slow):
				p.ctl.ended <- nil
				return ui.Text(fmt.Sprint("results ", p.n))
			}
		})
	}
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Search)}}, ui.Text("search")),
		ui.El("section", nil, results))
}

func searchApp(ctl *searchCtl) *App {
	a := New(Only("none"))
	a.Live("/search", func() *searchPage { return &searchPage{ctl: ctl} })
	a.Get("/next", func() ui.Node { return ui.El("main", nil, ui.Text("next")) })
	return a
}

// navigatedBy reads the connection's messages until the reply to the
// navigate ref, failing on an error, and returns the events' replies seen
// before it.
func navigatedBy(t *testing.T, c *websocket.Conn, ref int, wait time.Duration) []int {
	t.Helper()
	var before []int
	for {
		m, err := recv(c, wait)
		if err != nil {
			t.Fatalf("waiting for the navigation's reply: %v (replies seen: %v)", err, before)
		}
		if m.Type == live.Error {
			t.Fatalf("an error: %+v", m)
		}
		if m.Ref == ref {
			return before
		}
		before = append(before, m.Ref)
	}
}

// A navigate leaving the page behind queued events: each event's handler
// still runs, in order, with a live context — the one working when the
// navigate arrives too — while the renders after them, reads, are ended, so
// the navigation doesn't wait for every queued search's query.
func TestLiveNavigateEndsTheRendersOfQueuedEvents(t *testing.T) {
	ctl := &searchCtl{work: 150 * time.Millisecond, slow: 3 * time.Second, ended: make(chan error, 8)}
	srv := httptest.NewServer(searchApp(ctl).Handler())
	defer srv.Close()
	c, html := joined(t, srv, "/search")
	target := handlerOf(t, html, "click", 0)
	start := time.Now()
	for ref := 2; ref <= 4; ref++ {
		send(t, c, map[string]any{"type": "event", "ref": ref, "target": target, "event": "click"})
	}
	send(t, c, map[string]any{"type": "navigate", "ref": 5, "url": "/next"})
	before := navigatedBy(t, c, 5, 3*time.Second)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("the navigation waited %v for the queued renders", d)
	}
	if len(before) != 3 || before[0] != 2 || before[1] != 3 || before[2] != 4 {
		t.Errorf("the events' replies before the navigation's: %v", before)
	}
	ctl.mu.Lock()
	handled := slices.Clone(ctl.handled)
	ctl.mu.Unlock()
	if len(handled) != 3 {
		t.Fatalf("%d handlers ran, want 3", len(handled))
	}
	for i, err := range handled {
		if err != nil {
			t.Errorf("handler %d ran with its context ended: %v", i+1, err)
		}
	}
	for i := range 3 {
		if err := <-ctl.ended; !errors.Is(err, context.Canceled) {
			t.Errorf("render %d's query ended with %v", i+1, err)
		}
	}
}

// A render in progress after an event's handler is ended by a navigate that
// arrives while it runs.
func TestLiveNavigateEndsARenderAfterAHandler(t *testing.T) {
	ctl := &searchCtl{slow: 3 * time.Second, ended: make(chan error, 2)}
	srv := httptest.NewServer(searchApp(ctl).Handler())
	defer srv.Close()
	c, html := joined(t, srv, "/search")
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", 0), "event": "click"})
	time.Sleep(200 * time.Millisecond) // the handler returned; its render is in the slow query
	start := time.Now()
	send(t, c, map[string]any{"type": "navigate", "ref": 3, "url": "/next"})
	navigatedBy(t, c, 3, 3*time.Second)
	if d := time.Since(start); d > time.Second {
		t.Errorf("the navigation waited %v for the render", d)
	}
	if err := <-ctl.ended; !errors.Is(err, context.Canceled) {
		t.Errorf("the render's query ended with %v", err)
	}
}

// A patch of a page with an Update keeps it: the renders of the events
// before it run to the end, and later events' handlers see a live context.
func TestLivePatchKeepsTheRendersOfQueuedEvents(t *testing.T) {
	ctl := &searchCtl{slow: 300 * time.Millisecond, ended: make(chan error, 4)}
	srv := httptest.NewServer(searchApp(ctl).Handler())
	defer srv.Close()
	c, html := joined(t, srv, "/search")
	target := handlerOf(t, html, "click", 0)
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": target, "event": "click"})
	send(t, c, map[string]any{"type": "navigate", "ref": 3, "url": "/search?page=2", "patch": true})
	if before := navigatedBy(t, c, 3, 3*time.Second); len(before) != 1 || before[0] != 2 {
		t.Errorf("the event's reply before the patch's: %v", before)
	}
	send(t, c, map[string]any{"type": "event", "ref": 4, "target": target, "event": "click"})
	patchesUntil(t, c, "results 2")
	for i := range 2 {
		if err := <-ctl.ended; err != nil {
			t.Errorf("render %d's query ended with %v", i+1, err)
		}
	}
}

// A page rendered for a form posted without script has no connection to
// receive its deferred fields: they load before it is sent.
type deferFormPage struct {
	ui.Live
	Form  joinForm
	stats int
}

func (p *deferFormPage) LiveSchema(l *ui.LiveSchema) {
	l.Field(&p.stats, ui.Defer("stats"))
	l.Loads("stats", p.loadStats)
}

func (p *deferFormPage) loadStats(ctx context.Context) error { p.stats = 42; return nil }
func (p *deferFormPage) Save(f joinForm) error               { return nil }

func (p *deferFormPage) Render() ui.Node {
	text := "stats loading"
	if !p.Loading("stats") {
		text = fmt.Sprint("stats ", p.stats)
	}
	return ui.El("main", nil, ui.El("p", nil, ui.Text(text)), ui.Form{For: ui.For(&p.Form), OnSubmit: p.Save})
}

func TestLiveDeferredTemplateFormPost(t *testing.T) {
	a := New(Only("none"))
	a.Live("/signup", func() *deferFormPage { return &deferFormPage{} })
	h := a.Handler()
	v, cookie := formOf(t, h, "/signup")
	v.Set("Name", "") // fails its check: the page again, with its errors
	code, body, _ := post(t, h, "/signup", v, "Cookie", cookie)
	if code != 422 || !strings.Contains(body, "stats 42") {
		t.Fatalf("%d: %s", code, body)
	}
}

func TestBrowserDeferredTemplate(t *testing.T) {
	ctl := &deferCtl{release: make(chan struct{}), ended: make(chan error, 1)}
	srv := httptest.NewServer(statsApp(ctl).Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Open(srv.URL + "/stats")
	p.WaitText("#stats", "open 3")
	p.WaitText("#users", "users Grace")
	if got := p.Text("#rows"); got != "rows loading" {
		t.Errorf("rows before their release: %q", got)
	}
	close(ctl.release)
	p.WaitText("#rows", "rows Ada,Alan")
	p.Click("#more")
	p.WaitText("#more-users", "users Grace")
}

// searchURLPage keeps its search in the URL: each event's handler only edits
// it, and the render that follows runs a slow query (3s at most, unless its
// context ends) — a DataTable with a Load searching as the user types.
type searchURLPage struct {
	ui.Live
	Q string
	n int
}

type searchURLParams struct {
	URLParams
	Q string `query:"q"`
}

func (p *searchURLPage) Update(q searchURLParams) { p.Q = q.Q }

func (p *searchURLPage) Search(ctx context.Context) error {
	p.n++
	ui.EditURL(ctx, func(v url.Values) { v.Set("q", strings.Repeat("a", p.n)) }, false)
	return nil
}

func (p *searchURLPage) Render() ui.Node {
	results := ui.Text("no search")
	if p.Q != "" || p.n > 0 {
		results = ui.Ctx(func(ctx context.Context) ui.Node {
			select {
			case <-ctx.Done():
				return ui.Text("results gone")
			case <-time.After(3 * time.Second):
				return ui.Text("results for " + p.Q)
			}
		})
	}
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "id", Value: "search"}, {Name: "onClick", Value: ui.On(p.Search)}}, ui.Text("search")),
		ui.Link(ui.LinkProps{Href: "/next", ID: "to-next", Children: ui.Text("next")}),
		ui.El("section", nil, results))
}

// nextPage is a live page a search page's link goes to.
type nextPage struct{ ui.Live }

func (p *nextPage) Render() ui.Node {
	return ui.El("main", nil, ui.El("p", attrs("id", "next"), ui.Text("next")))
}

// A click on a link behind searches still rendering leaves the page at once,
// and stays left: the URL edits the searches' replies carry belong to the
// page left, not to the visitor's next move.
func TestBrowserNavigateLeavesBehindQueuedSearches(t *testing.T) {
	a := New(Only("none"))
	a.Live("/search", func() *searchURLPage { return &searchURLPage{} })
	a.Live("/next", func() *nextPage { return &nextPage{} })
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Open(srv.URL + "/search")
	p.Wait(`document.documentElement.dataset.vkState === "connected"`)
	start := time.Now()
	p.Eval(`(async () => {
		const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
		for (let i = 0; i < 3; i++) { document.querySelector("#search").click(); await sleep(150); }
		document.querySelector("#to-next").click();
	})()`, nil)
	p.WaitText("#next", "next")
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("the navigation took %v behind the searches", d)
	}
	time.Sleep(300 * time.Millisecond) // a search's URL edit would have moved it back by now
	var path string
	p.Eval(`location.pathname`, &path)
	if path != "/next" {
		t.Errorf("the page went back to %q", path)
	}
}

// Patches the browser sent before a navigate leaving the page — a search's
// URL edits — were superseded there: the one rendering when the navigate
// arrives is ended, those queued behind it don't run, and each still gets a
// reply that is no error.
func TestLiveNavigateSupersedesQueuedPatches(t *testing.T) {
	a := New(Only("none"))
	a.Live("/search", func() *searchURLPage { return &searchURLPage{} })
	a.Get("/next", func() ui.Node { return ui.El("main", nil, ui.Text("next")) })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	c, _ := joined(t, srv, "/search")
	send(t, c, map[string]any{"type": "navigate", "ref": 2, "url": "/search?q=a", "patch": true})
	time.Sleep(200 * time.Millisecond) // its render is in the slow query
	start := time.Now()
	send(t, c, map[string]any{"type": "navigate", "ref": 3, "url": "/search?q=aa", "patch": true})
	send(t, c, map[string]any{"type": "navigate", "ref": 4, "url": "/next"})
	if before := navigatedBy(t, c, 4, 3*time.Second); len(before) != 2 || before[0] != 2 || before[1] != 3 {
		t.Errorf("the patches' replies before the navigation's: %v", before)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the navigation waited %v behind the patches", d)
	}
}

// urlDeferPage keeps the open group in the URL and loads its rows deferred;
// it has no Update, so a navigation to it mounts it again.
type urlDeferPage struct {
	ui.Live
	open   uigen.Assign[int]
	rows   []string
	mounts *atomic.Int32
	loads  *atomic.Int32
}

func (p *urlDeferPage) LiveSchema(l *ui.LiveSchema) {
	l.Field(&p.open, ui.URL("open"))
	l.Field(&p.rows, ui.Defer("rows"))
	l.Mount(func(context.Context) error { p.mounts.Add(1); return nil })
	l.Loads("rows", func(context.Context) error {
		p.loads.Add(1)
		p.rows = []string{"Ada"}
		return nil
	})
}

func (p *urlDeferPage) Toggle() { p.open.Set(p.open.Get() + 1) }

func (p *urlDeferPage) Render() ui.Node {
	text := "rows loading"
	if !p.Loading("rows") {
		text = fmt.Sprintf("rows %s open %d", strings.Join(p.rows, ","), p.open.Get())
	}
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Toggle)}}, ui.Text("toggle")),
		ui.El("section", []ui.Attr{{Name: "id", Value: "rows"}}, ui.Text(text)))
}

// Assigning a @ui.URL field in an event is a change of state: the page's
// reply has the new state, and the navigate its URL write makes runs
// neither Mount nor the loaders, and renders nothing. A link to another
// query is a navigation as ever.
func TestLiveURLFieldAssignKeepsLoads(t *testing.T) {
	var mounts, loads atomic.Int32
	a := New(Only("none"))
	a.Live("/groups", func() *urlDeferPage { return &urlDeferPage{mounts: &mounts, loads: &loads} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	c, html := joined(t, srv, "/groups")
	patchesUntil(t, c, "rows Ada open 0")
	toggle := handlerOf(t, html, "click", 0)

	send(t, c, map[string]any{"type": "event", "ref": 2, "target": toggle, "event": "click"})
	m := must(t, c)
	got := fmt.Sprint(m.Updates, m.Trees)
	if !strings.Contains(got, "open 1") || strings.Contains(got, "loading") {
		t.Fatalf("the event's reply: %s", got)
	}
	if len(m.JS) == 0 || !strings.Contains(string(m.JS[0].Ops), `"/groups?open=1"`) {
		t.Fatalf("the reply writes the URL: %+v", m.JS)
	}

	send(t, c, map[string]any{"type": "navigate", "ref": 3, "url": "/groups?open=1", "patch": true})
	m = must(t, c)
	if m.Type != live.PatchMsg || m.Ref != 3 || m.URL != "/groups?open=1" || len(m.Updates) > 0 || len(m.Trees) > 0 {
		t.Fatalf("the URL write's navigate: %+v", m)
	}
	if mounts.Load() != 1 || loads.Load() != 1 {
		t.Fatalf("mounts %d, loads %d: the URL write mounted or loaded again", mounts.Load(), loads.Load())
	}

	// the same address again is not the page's own write
	send(t, c, map[string]any{"type": "navigate", "ref": 4, "url": "/groups?open=1", "patch": true})
	if m = must(t, c); m.Ref != 4 {
		t.Fatalf("something rendered after the URL write: %+v", m)
	}
	patchesUntil(t, c, "rows Ada open 1")
	if mounts.Load() != 2 || loads.Load() != 2 {
		t.Fatalf("mounts %d, loads %d after a link to the address, want 2 each", mounts.Load(), loads.Load())
	}

	send(t, c, map[string]any{"type": "navigate", "ref": 5, "url": "/groups?open=7", "patch": true})
	patchesUntil(t, c, "open 7")
	if mounts.Load() != 3 || loads.Load() != 3 {
		t.Fatalf("mounts %d, loads %d after links, want 3 each", mounts.Load(), loads.Load())
	}
}
