package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/web/internal/chrome"
)

type Counter struct{ n int }

func (c *Counter) Tick(ctx context.Context) error { c.n++; return nil }

// cronPkg is a package of its own for each run of a test: decorators
// record into the program's registry.
func cronPkg(t *testing.T) string { return fmt.Sprint(pkgOf(t), "/", line.Add(1)) }

// captureSlog collects what slog's default logger writes during the test.
// The logger is replaced once, for the binary, by one writing to the
// test's buffer (and to nothing between tests): swapping it back and forth
// would race with the log package, which slog's built-in handler writes
// through.
func captureSlog(t testing.TB) *syncBuffer {
	captureOnce.Do(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	})
	buf := &syncBuffer{}
	captured.set(buf)
	t.Cleanup(func() { captured.set(nil) })
	return buf
}

var (
	captureOnce sync.Once
	captured    = &logSink{}
)

type logSink struct {
	mu sync.Mutex
	w  *syncBuffer
}

func (s *logSink) set(w *syncBuffer) { s.mu.Lock(); s.w = w; s.mu.Unlock() }

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	w := s.w
	s.mu.Unlock()
	if w == nil {
		return len(p), nil
	}
	return w.Write(p)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestCronDecorator(t *testing.T) {
	p := cronPkg(t)
	var got []string
	Cron("@every 1m")(decl(p, "cleanUp", func(ctx context.Context, s *Store) error {
		got = append(got, s.pets[1].Name)
		return nil
	}, "ctx", "s"))
	Cron("@daily")(decl(p, "fails", func() error { return errors.New("no luck") }))
	Cron("0 * * * *")(vuka.MethodDecl(vuka.Decl{Name: "Counter.Tick", Pkg: p, File: "t.vuka", Line: 7, Func: (*Counter).Tick}, "ctx"))
	cnt := &Counter{}
	a := New(Only(p), Provide(NewStore, cnt))
	if err := a.RunCron(context.Background(), "cleanUp"); err != nil || len(got) != 1 || got[0] != "Rex" {
		t.Fatalf("cleanUp: %v %v", err, got)
	}
	if err := a.RunCron(context.Background(), "Counter.Tick"); err != nil || cnt.n != 1 {
		t.Fatalf("a method: %v %d", err, cnt.n)
	}
	if err := a.RunCron(context.Background(), "fails"); err == nil || err.Error() != "web: cron fails: no luck" {
		t.Errorf("fails: %v", err)
	}
	if err := a.RunCron(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), `no cron named "nope"; the app has cleanUp, fails, Counter.Tick`) {
		t.Errorf("an unknown cron: %v", err)
	}
	if w := a.sched.job("cleanUp").where; !strings.HasPrefix(w, "main.cleanUp at t.vuka:") {
		t.Errorf("where: %s", w)
	}
}

func TestCronBuildErrors(t *testing.T) {
	p := cronPkg(t)
	bad := decl(p, "bad", func() {})
	Cron("61 * * * *")(bad)
	Cron("@every 1m")(decl(p, "needsDB", func(db *DB) {}, "db"))
	Cron("@every 1m")(decl(p, "takesInt", func(n int) {}, "n"))
	Cron("@every 1m")(decl(p, "returns", func() int { return 1 }))
	err := New(Only(p)).Build()
	for _, want := range []string{
		`error[E0411]: @web.Cron("61 * * * *") on main.bad: cron: "61 * * * *": minute: 61 is out of range 0-59` + "\n --> t.vuka:" + itoa(bad.Line),
		"error[E0411]: cron needsDB (main.needsDB): ",
		"parameter db *web.DB: a cron takes a context.Context and provided types; no provider for *web.DB",
		"parameter n int: a cron takes",
		"returns func() int; a cron returns nothing or an error",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Build: %v\nwant %q", err, want)
		}
	}

	a := New(Only("none"))
	a.Cron("x", "@every 1m", func() {})
	a.Cron("x", "@hourly", func() {})
	a.Cron("y", "every minute", func() {})
	a.Cron("z", "@hourly", 42)
	err = a.Build()
	for _, want := range []string{"error[E0412]: cron x is declared twice", `error[E0411]: cron y (github.com/vuka-lang/web.TestCronBuildErrors.func`, `want five fields`, "error[E0411]: cron z: int is not a function"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Build: %v\nwant %q", err, want)
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestCronPanicAndLogs(t *testing.T) {
	buf := captureSlog(t)
	a := New(Only("none"))
	a.Cron("boom", "@hourly", func() { panic("kaboom") })
	a.Cron("fine", "@hourly", func(ctx context.Context) error { slog.InfoContext(ctx, "working"); return nil })
	err := a.RunCron(context.Background(), "boom")
	if err == nil || err.Error() != "web: cron boom: panic: kaboom" {
		t.Errorf("a panic: %v", err)
	}
	if err := a.RunCron(context.Background(), "fine"); err != nil {
		t.Error(err)
	}
	out := buf.String()
	for _, want := range []string{`msg="cron panicked" cron=boom panic=kaboom stack="goroutine`, "crons_test.go", `msg="cron failed" cron=boom err="panic: kaboom"`, `msg="cron ran" cron=fine`} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
}

func TestCronToolbar(t *testing.T) {
	withToolbar(t)
	a := New(Only("none"), Dev())
	a.Cron("sweep", "@every 1h", func(ctx context.Context) error {
		NoteStatement(ctx, Statement{SQL: "DELETE FROM sessions WHERE expired", Rows: 3})
		slog.InfoContext(ctx, "swept", "n", 3)
		Note(ctx, "Notes", "a note")
		return errors.New("disk full")
	})
	a.Get("/page", func() string { return "p" })
	h := a.Handler()
	if err := a.RunCron(context.Background(), "sweep"); err == nil {
		t.Fatal("no error")
	}
	if len(a.tb.crons) != 1 {
		t.Fatalf("cron records: %d", len(a.tb.crons))
	}
	rec := a.tb.crons[0].snapshot()
	if rec.Method != "CRON" || rec.URI != "sweep" || rec.Route != "cron @every 1h" || rec.Status != 500 || rec.Err != "disk full" || !rec.Done ||
		len(rec.Queries) != 1 || len(rec.Notes) != 1 || len(rec.Logs) < 2 {
		t.Fatalf("the record: %+v", rec)
	}

	var list struct{ Runs []struct{ ID, Label string } }
	if r := tbDo(t, h, "GET", toolbarCrons); json.Unmarshal([]byte(r.body), &list) != nil || len(list.Runs) != 1 || list.Runs[0].ID != rec.ID ||
		!strings.HasPrefix(list.Runs[0].Label, "cron sweep · ") || !strings.HasSuffix(list.Runs[0].Label, " · failed") {
		t.Errorf("the list: %s", r.body)
	}
	panels := panelsHTML(t, h, rec.ID)
	for _, want := range []string{`<p class="errtext">disk full</p>`, "DELETE", "swept", `data-tab="Crons"`, `data-cron="sweep"`, "Run now", "not scheduled", `data-id="` + rec.ID + `"`} {
		if !strings.Contains(panels, want) {
			t.Errorf("the panels lack %q: %s", want, panels)
		}
	}
	// A request's record has the Crons panel too.
	page := tbDo(t, h, "GET", "/page")
	if p := panelsHTML(t, h, page.header.Get(ToolbarHeader)); !strings.Contains(p, "Run now") {
		t.Errorf("a request's panels: %s", p)
	}

	// Run now, from the toolbar only.
	if r := tbDo(t, h, "POST", toolbarCronRun+"?name=sweep"); r.status != http.StatusForbidden {
		t.Errorf("without the header: %d %s", r.status, r.body)
	}
	if r := tbDo(t, h, "POST", toolbarCronRun+"?name=nope", ToolbarHeader, "1"); r.status != http.StatusNotFound {
		t.Errorf("an unknown cron: %d %s", r.status, r.body)
	}
	r := tbDo(t, h, "POST", toolbarCronRun+"?name=sweep", ToolbarHeader, "1")
	var run struct{ ID string }
	if json.Unmarshal([]byte(r.body), &run) != nil || run.ID == "" || run.ID == rec.ID {
		t.Fatalf("run now: %d %s", r.status, r.body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e := a.tb.get(run.ID); !e.snapshot().Done; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the run doesn't end")
		}
	}

	// A busy cron answers 409.
	release := make(chan struct{})
	b := New(Only("none"), Dev())
	b.Cron("slow", "@hourly", func() { <-release })
	hb := b.Handler()
	go b.RunCron(context.Background(), "slow")
	for !b.sched.job("slow").running.Load() {
		time.Sleep(time.Millisecond)
	}
	if r := tbDo(t, hb, "POST", toolbarCronRun+"?name=slow", ToolbarHeader, "1"); r.status != http.StatusConflict || !strings.Contains(r.body, "slow is still running") {
		t.Errorf("busy: %d %s", r.status, r.body)
	}
	close(release)

	// Without a toolbar there are no records, routes or panel.
	toolbarInTests = false
	c := New(Only("none"))
	c.Cron("quiet", "@hourly", func() {})
	hc := c.Handler()
	c.RunCron(context.Background(), "quiet")
	if c.tb != nil || tbDo(t, hc, "GET", toolbarCrons).status != 404 {
		t.Error("a production app records crons")
	}
}

func panelsHTML(t *testing.T, h http.Handler, id string) string {
	t.Helper()
	var out struct{ HTML string }
	r := tbDo(t, h, "GET", "/_vuka/toolbar/r/"+id+"?panels")
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatalf("%v: %s", err, r.body)
	}
	return out.HTML
}

func TestCronRecordInsideRequest(t *testing.T) {
	withToolbar(t)
	a := New(Only("none"), Dev())
	a.Cron("inner", "@hourly", func(ctx context.Context) { slog.InfoContext(ctx, "inner ran") })
	a.Get("/run", func(ctx context.Context) error {
		a.RunCron(context.Background(), "inner") // without the request's context: the goroutine's
		slog.Info("after the cron")
		return nil
	})
	page := tbDo(t, a.Handler(), "GET", "/run")
	req := a.tb.get(page.header.Get(ToolbarHeader)).snapshot()
	if len(req.Logs) != 1 || req.Logs[0].Msg != "after the cron" {
		t.Errorf("the request's logs: %+v", req.Logs)
	}
	if run := a.tb.crons[0].snapshot(); len(run.Logs) == 0 || run.Logs[0].Msg != "inner ran" {
		t.Errorf("the cron's logs: %+v", run.Logs)
	}
}

// TestBrowserToolbarCrons runs a cron from the toolbar's Crons panel: its
// run becomes the selected record, listed under Cron runs.
func TestBrowserToolbarCrons(t *testing.T) {
	withToolbar(t)
	a := New(Only("none"), Dev())
	a.Get("/", func() ui.Node {
		return fullPage(ui.El("main", nil, ui.Component("home#1", nil, &counter{Label: "n="})))
	})
	a.Cron("sweep", "@daily", func(ctx context.Context) error {
		NoteStatement(ctx, Statement{SQL: "DELETE FROM sessions", Start: time.Now(), Rows: 4})
		return nil
	})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)

	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/")
	p.Viewport(1400, 800, false)
	tb := `document.querySelector("vuka-toolbar").shadowRoot`
	p.Wait(tb + `.querySelector(".handle .num")`)
	p.ClickElement(tb + `.querySelector(".handle")`)
	p.Wait(tb + `.querySelector('.tab[data-tab="Crons"]')`)
	p.ClickElement(tb + `.querySelector('.tab[data-tab="Crons"]')`)
	p.Wait(tb + `.querySelector('.panel[data-panel="Crons"]:not([hidden]) .cron-run')`)
	p.ClickElement(tb + `.querySelector('.cron-run[data-cron="sweep"]')`)
	p.Wait(tb + `.querySelector('select optgroup[label="Cron runs"] option') && ` + tb + `.querySelector("select").selectedOptions[0].textContent.startsWith("cron sweep")`)
	p.ClickElement(tb + `.querySelector('.tab[data-tab="SQL"]')`)
	p.Wait(tb + `.querySelector('.panel[data-panel="SQL"]:not([hidden])') && ` + tb + `.querySelector('.panel[data-panel="SQL"]').textContent.includes("DELETE FROM sessions")`)
	p.ClickElement(tb + `.querySelector('.tab[data-tab="Request"]')`)
	p.Wait(tb + `.querySelector('.panel[data-panel="Request"]:not([hidden])').textContent.includes("cron @daily")`)
	var handle string
	p.Eval(tb+`.querySelector(".handle").textContent`, &handle)
	if strings.Contains(handle, "+") {
		t.Errorf("cron runs counted as the page's calls: %q", handle)
	}
	if dir := os.Getenv("VUKA_SCREENSHOTS"); dir != "" {
		p.Screenshot(filepath.Join(dir, "toolbar-crons.png"))
	}
}
