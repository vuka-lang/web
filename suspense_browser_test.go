package web

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// susCtl steers a susPage: its loader waits for release (nil: it doesn't),
// counting its runs, and the section's timings are the test's.
type susCtl struct {
	release                chan struct{}
	wait, delay, minShow   time.Duration
	mu                     sync.Mutex
	runs                   int
	first                  chan struct{} // closed when the first run starts
	blockFirst, firstEnded bool
}

type susPage struct {
	ui.Live
	ctl  *susCtl
	rows []string
	n    int
}

func (p *susPage) LiveSchema(l *ui.LiveSchema) {
	l.Field(&p.rows, ui.Defer("rows"))
	l.Loads("rows", p.load)
}

func (p *susPage) load(ctx context.Context) error {
	c := p.ctl
	c.mu.Lock()
	c.runs++
	run := c.runs
	c.mu.Unlock()
	if c.blockFirst && run == 1 {
		<-ctx.Done()
		return ctx.Err()
	}
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.rows = []string{"a", "b"}
	return nil
}

func (p *susPage) Inc()  { p.n++ }
func (p *susPage) Edit() { p.rows = []string{"c", "d"} }

func (p *susPage) Render() ui.Node {
	section := ui.Await{Of: p, Key: "rows", Wait: p.ctl.wait, Delay: p.ctl.delay, MinShow: p.ctl.minShow,
		Pending: ui.El("p", []ui.Attr{{Name: "id", Value: "fallback"}}, ui.Text("loading")),
		Children: func() ui.Node {
			var lis []ui.Node
			for _, r := range p.rows {
				lis = append(lis, ui.El("li", nil, ui.Text(r)))
			}
			return ui.El("ul", []ui.Attr{{Name: "id", Value: "rows"}}, lis...)
		}}
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "id", Value: "inc"}, {Name: "onClick", Value: p.Inc}}, ui.Text("inc")),
		ui.El("button", []ui.Attr{{Name: "id", Value: "edit"}, {Name: "onClick", Value: p.Edit}}, ui.Text("edit")),
		ui.El("span", []ui.Attr{{Name: "id", Value: "n"}}, ui.Textf("%d", p.n)),
		section)
}

func susApp(ctl *susCtl, opts ...Option) *App {
	app := New(append([]Option{Only("none")}, opts...)...)
	app.Live("/s", func() *susPage { return &susPage{ctl: ctl} })
	return app
}

// susSampler records, on every frame, when the fallback was visible and
// which rows the page showed.
const susSampler = `(() => {
  window.sus = { t0: performance.now(), fbSeen: null, fbVisible: null, content: null, shown: [] };
  const frame = () => {
    const now = performance.now() - sus.t0;
    const fb = document.getElementById("fallback"), rows = document.getElementById("rows");
    if (fb && sus.fbSeen === null) sus.fbSeen = now;
    if (fb && getComputedStyle(fb).visibility !== "hidden" && sus.fbVisible === null) sus.fbVisible = now;
    if (rows) {
      if (sus.content === null) sus.content = now;
      const text = rows.textContent;
      if (sus.shown[sus.shown.length - 1] !== text) sus.shown.push(text);
    }
    requestAnimationFrame(frame);
  };
  frame();
  return true;
})()`

func openSus(t *testing.T, ctl *susCtl) *chrome.Page {
	t.Helper()
	srv := httptest.NewServer(susApp(ctl).Handler())
	t.Cleanup(srv.Close)
	p := chrome.Launch(t).Open(srv.URL + "/s")
	p.Wait(`document.documentElement.getAttribute("data-vk-state") === "connected"`)
	var ok bool
	p.Eval(susSampler, &ok)
	return p
}

type susLog struct {
	FbSeen, FbVisible, Content *float64
	Shown                      []string
}

func sampled(t *testing.T, p *chrome.Page) susLog {
	t.Helper()
	var l susLog
	if err := p.Eval(`window.sus`, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// A load faster than Delay never shows the fallback: it is on the page,
// reserving its space, but never visible, and the content replaces it.
func TestBrowserSuspenseFastLoadNeverFlashes(t *testing.T) {
	ctl := &susCtl{release: make(chan struct{}), delay: 600 * time.Millisecond}
	p := openSus(t, ctl)
	time.Sleep(100 * time.Millisecond)
	var hidden bool
	p.Eval(`(() => { const f = document.getElementById("fallback"); return !!f && getComputedStyle(f).visibility === "hidden" && f.getBoundingClientRect().height > 0; })()`, &hidden)
	if !hidden {
		t.Fatal("the fallback is not on the page, hidden, with its space")
	}
	close(ctl.release)
	p.Wait(`document.getElementById("rows")`)
	time.Sleep(900 * time.Millisecond) // past Delay: nothing may appear late
	l := sampled(t, p)
	if l.FbVisible != nil {
		t.Fatalf("the fallback was visible at %vms", *l.FbVisible)
	}
	if l.Content == nil {
		t.Fatal("no content")
	}
	var gone bool
	p.Eval(`!document.getElementById("fallback")`, &gone)
	if !gone {
		t.Fatal("the fallback is still on the page")
	}
}

// A slower load shows the fallback once Delay has passed, and the content
// replaces it when it arrives.
func TestBrowserSuspenseSlowLoadShowsAfterDelay(t *testing.T) {
	ctl := &susCtl{release: make(chan struct{}), delay: 250 * time.Millisecond}
	p := openSus(t, ctl)
	p.Wait(`window.sus.fbVisible !== null`)
	l := sampled(t, p)
	if l.FbSeen == nil || *l.FbVisible-*l.FbSeen < 100 {
		t.Fatalf("visible at %v, seen at %v", ptr(l.FbVisible), ptr(l.FbSeen))
	}
	time.Sleep(100 * time.Millisecond)
	close(ctl.release)
	p.Wait(`document.getElementById("rows") && !document.getElementById("fallback")`)
}

func ptr(f *float64) float64 {
	if f == nil {
		return -1
	}
	return *f
}

// Once visible, the fallback stays MinShow before the content replaces it;
// the rest of the page updates at once, and the newest content wins.
func TestBrowserSuspenseMinShow(t *testing.T) {
	ctl := &susCtl{release: make(chan struct{}), delay: ui.Instant, minShow: 800 * time.Millisecond}
	p := openSus(t, ctl)
	p.Wait(`window.sus.fbVisible !== null`)
	close(ctl.release) // the content arrives at once
	time.Sleep(150 * time.Millisecond)
	var held bool
	p.Eval(`!!document.getElementById("fallback") && !document.getElementById("rows")`, &held)
	if !held {
		t.Fatal("the content replaced the fallback before MinShow")
	}
	p.Click("#inc")
	p.WaitText("#n", "1") // the rest of the patch applied meanwhile
	p.Eval(`!!document.getElementById("fallback") && !document.getElementById("rows")`, &held)
	if !held {
		t.Fatal("the click's patch released the held section")
	}
	p.Click("#edit") // a newer content while the older is held
	p.Wait(`window.sus.content !== null`)
	l := sampled(t, p)
	if l.FbVisible == nil || l.Content == nil || *l.Content-*l.FbVisible < 700 {
		t.Fatalf("fallback visible at %v, content at %v", ptr(l.FbVisible), ptr(l.Content))
	}
	if len(l.Shown) != 1 || l.Shown[0] != "cd" {
		t.Fatalf("the content showed %q, want only the newest, cd", l.Shown)
	}
}

// A load that finishes before the fallback is visible is not held: MinShow
// counts from the fallback becoming visible.
func TestBrowserSuspenseMinShowNotVisibleYet(t *testing.T) {
	ctl := &susCtl{release: make(chan struct{}), delay: 500 * time.Millisecond, minShow: 2 * time.Second}
	p := openSus(t, ctl)
	close(ctl.release)
	start := time.Now()
	p.Wait(`document.getElementById("rows")`)
	if time.Since(start) > time.Second {
		t.Fatalf("the content waited %v", time.Since(start))
	}
	if l := sampled(t, p); l.FbVisible != nil {
		t.Fatalf("the fallback was visible at %v", *l.FbVisible)
	}
}

// Without a Delay or MinShow the section is not wrapped and behaves as
// before: the fallback shows at once and the content replaces it.
func TestBrowserSuspenseInstant(t *testing.T) {
	ctl := &susCtl{release: make(chan struct{}), delay: ui.Instant}
	p := openSus(t, ctl)
	var wrapped bool
	p.Eval(`!!document.querySelector("vk-await")`, &wrapped)
	if wrapped {
		t.Fatal("a section with no timings is wrapped")
	}
	p.Wait(`window.sus.fbVisible !== null`)
	close(ctl.release)
	p.Wait(`document.getElementById("rows") && !document.getElementById("fallback")`)
}
