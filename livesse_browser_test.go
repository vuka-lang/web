package web

import (
	"net/http/httptest"
	"testing"

	"github.com/vuka-lang/web/v2/internal/chrome"
)

const liveConnected = `document.documentElement.dataset.vkState === "connected"`

// A browser with no WebSocket goes live over the event stream: events
// round-trip, and a 4001 close takes the recovery path in place, the
// document (and a value a script left on window) still there.
func TestBrowserLiveStream(t *testing.T) {
	app := meterApp()
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close) // after the browser closes: it waits for the page's stream
	b := chrome.Launch(t)
	p := b.Start(srv.URL+"/meter", liveConnected, `delete window.WebSocket`)
	p.WaitText("span", "m1 n0")
	p.Click("button")
	p.WaitText("span", "m1 n1")
	if err := p.Eval(`window.__sentinel = 1`, nil); err != nil {
		t.Fatal(err)
	}

	closeAll(t, app)
	p.WaitText("span", "m2 n0")
	p.Wait(liveConnected)
	var s int
	if err := p.Eval(`window.__sentinel || 0`, &s); err != nil || s != 1 {
		t.Fatalf("the document loaded again (sentinel %d, %v)", s, err)
	}
	p.Click("button")
	p.WaitText("span", "m2 n1")
}

// Sockets that never open (a proxy refusing the upgrade) give way to the
// stream after three tries, and the tab remembers the switch.
func TestBrowserLiveStreamFallback(t *testing.T) {
	app := meterApp()
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close) // after the browser closes: it waits for the page's stream
	b := chrome.Launch(t)
	refused := `(() => {
		window.__tries = 0;
		window.WebSocket = class {
			constructor() { window.__tries++; setTimeout(() => this.onclose && this.onclose({ code: 1006, reason: "" }), 0); }
			send() {}
			close() {}
		};
	})()`
	p := b.Start(srv.URL+"/meter", liveConnected, refused)
	p.Click("button")
	p.WaitText("span", "m1 n1")
	var got struct {
		Tries  int
		Stream bool
	}
	if err := p.Eval(`({ tries: window.__tries, stream: !!sessionStorage.getItem("vk-stream") })`, &got); err != nil || got.Tries != 3 || !got.Stream {
		t.Fatalf("%+v %v", got, err)
	}
}
