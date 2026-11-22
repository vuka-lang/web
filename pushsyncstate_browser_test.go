package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/uigen"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// searchLive is the live twin of searchStatic: q is synced (Find reads it),
// draft is browser-local (never read by Go).
type searchLive struct {
	ui.Live
	q     ui.State[string]
	draft ui.State[string]
	found string
}

func init() { uigen.SyncFields[searchLive]("q") }

func (p *searchLive) Find() { p.found = "found:" + p.q.Get() }

func (p *searchLive) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("input", []ui.Attr{attr("id", "q"), ui.BindAttr("value", p.q)}),
		ui.El("input", []ui.Attr{attr("id", "draft"), ui.BindAttr("value", p.draft)}),
		ui.El("button", []ui.Attr{attr("id", "set-q"), attr("onClick", ui.On(ui.JS.Set(p.q, "tea")))}, ui.Text("set q")),
		ui.El("button", []ui.Attr{attr("id", "set-draft"), attr("onClick", ui.On(ui.JS.Set(p.draft, "memo")))}, ui.Text("set draft")),
		ui.El("button", []ui.Attr{attr("id", "find"), attr("onClick", ui.On(p.Find))}, ui.Text("find")),
		ui.El("p", []ui.Attr{attr("id", "found")}, ui.Text(p.found)),
	)
}

// statesLog records the _vk_states each HTTP push call carried ("" for none).
type statesLog struct {
	mu   sync.Mutex
	sent []string
}

func (l *statesLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(PushHeader) != "" && r.Method == http.MethodPost {
			r.ParseForm()
			l.mu.Lock()
			l.sent = append(l.sent, r.PostForm.Get("_vk_states"))
			l.mu.Unlock()
		}
		h.ServeHTTP(w, r)
	})
}

func (l *statesLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sent
	l.sent = nil
	return s
}

// wsSpy records every WebSocket frame the page sends, before the runtime loads.
const wsSpy = `window.__sentFrames = [];
(() => { const send = WebSocket.prototype.send; WebSocket.prototype.send = function (d) { try { __sentFrames.push(String(d)); } catch (_) {} return send.call(this, d); }; })();`

// TestBrowserSyncedStates: one state on both page kinds — a browser write
// of a synced state rides the next push (the HTTP request on a plain page,
// the socket event on a live one) so the handler reads browser truth; the
// reply's states are adopted by version; a browser-local state never rides.
func TestBrowserSyncedStates(t *testing.T) {
	a := New(Only("none"))
	a.Get("/search", func() *searchStatic {
		p := &searchStatic{}
		uigen.States(p)
		return p
	}, PageRoute())
	a.Live("/live-search", &searchLive{})
	log := &statesLog{}
	srv := httptest.NewServer(log.wrap(a.Handler()))
	defer srv.Close()
	b := chrome.Launch(t)

	t.Run("static", func(t *testing.T) {
		p := b.Visit(srv.URL + "/search")
		// A browser-local write rides nothing.
		p.Click("#toggle-open")
		p.Click("#find")
		p.WaitText("#out", "found:")
		if got := log.take(); len(got) != 1 || got[0] != "" {
			t.Fatalf("a push after a browser-local write carried %q", got)
		}
		// A synced write rides the push; the handler reads it; the echo
		// settles the dirty mark, so the next push carries nothing.
		p.Click("#set-q")
		p.Click("#find")
		p.WaitText("#out", "found:tea")
		if got := log.take(); len(got) != 1 || !strings.Contains(got[0], `"tea"`) {
			t.Fatalf("the push didn't carry the synced write: %q", got)
		}
		p.Click("#find")
		p.WaitText("#out", "found:tea")
		if got := log.take(); len(got) != 1 || got[0] != "" {
			t.Fatalf("a push after the echo still carried %q", got)
		}
		// A server write rides the reply and is adopted: the bound field
		// takes it (the version advanced past the browser's base).
		p.Click("#reset")
		p.Wait(`document.querySelector("#q").value === ""`)
		p.Click("#find")
		p.WaitText("#out", "found:")
		if got := log.take(); len(got) != 2 || got[1] != "" {
			t.Fatalf("after adopting the server write the push carried %q", got)
		}
	})

	t.Run("live", func(t *testing.T) {
		p := b.Start(srv.URL+"/live-search", `!!document.querySelector("#find")`, wsSpy)
		frames := func() string {
			var got string
			p.Eval(`__sentFrames.splice(0).join("\n")`, &got)
			return got
		}
		p.Wait(`window.__sentFrames.some((f) => f.includes("join"))`)
		frames()
		// A browser-local write: the event carries no states.
		p.Click("#set-draft")
		p.Click("#find")
		p.WaitText("#found", "found:")
		if f := frames(); strings.Contains(f, `"states"`) {
			t.Fatalf("a browser-local write rode an event:\n%s", f)
		}
		// A synced write rides the event first; the handler reads it.
		p.Click("#set-q")
		p.Click("#find")
		p.WaitText("#found", "found:tea")
		if f := frames(); !strings.Contains(f, `"states"`) || !strings.Contains(f, `"tea"`) {
			t.Fatalf("the event didn't carry the synced write:\n%s", f)
		}
		// The patch's echo settles the mark: the next event carries nothing.
		p.Click("#find")
		p.WaitText("#found", "found:tea")
		if f := frames(); strings.Contains(f, `"states"`) {
			t.Fatalf("an event after the echo still carried states:\n%s", f)
		}
	})
}
