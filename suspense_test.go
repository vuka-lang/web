package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2/live"
)

func suspenseGet(t *testing.T, srv *httptest.Server) (string, time.Duration) {
	t.Helper()
	start := time.Now()
	res, err := http.Get(srv.URL + "/s")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b), time.Since(start)
}

// A loader that finishes within Wait ships its content in the page: the
// fallback is not in the first response.
func TestSuspenseWaitHTTP(t *testing.T) {
	srv := httptest.NewServer(susApp(&susCtl{wait: 2 * time.Second}).Handler())
	defer srv.Close()
	html, took := suspenseGet(t, srv)
	if strings.Contains(html, `id="fallback"`) || !strings.Contains(html, `<ul id="rows"><li>a</li><li>b</li></ul>`) {
		t.Fatalf("got %s", html)
	}
	if took > time.Second {
		t.Fatalf("the response waited %v for a loader that returns at once", took)
	}
}

// A loader that misses Wait renders its fallback, the response held for
// about Wait; the loader keeps running and the socket's join gets its
// content as a patch, the loader run once.
func TestSuspenseWaitMissed(t *testing.T) {
	ctl := &susCtl{wait: 200 * time.Millisecond, release: make(chan struct{})}
	srv := httptest.NewServer(susApp(ctl).Handler())
	defer srv.Close()
	html, took := suspenseGet(t, srv)
	if took < 180*time.Millisecond || took > 2*time.Second {
		t.Fatalf("the response waited %v, want about 200ms", took)
	}
	if !strings.Contains(html, `id="fallback"`) || strings.Contains(html, `id="rows"`) {
		t.Fatalf("got %s", html)
	}
	m := tokenRe.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("no token")
	}
	c, err := dial(t, srv, m[1], nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1, "v": 2})
	if j := must(t, c); j.Type != live.Render || strings.Contains(string(live.Marshal(j)), "rows") {
		t.Fatalf("join: %s", live.Marshal(j))
	}
	close(ctl.release)
	p := must(t, c)
	if p.Type != live.PatchMsg || !strings.Contains(string(live.Marshal(p)), "rows") {
		t.Fatalf("patch: %s", live.Marshal(p))
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if ctl.runs != 1 {
		t.Fatalf("the loader ran %d times", ctl.runs)
	}
}

// A join that mounts afresh holds too: its first reply carries the content
// of a loader that returns in time, and the fallback is not on the wire.
func TestSuspenseWaitJoin(t *testing.T) {
	ctl := &susCtl{wait: 300 * time.Millisecond, blockFirst: true}
	srv := httptest.NewServer(susApp(ctl, LiveAdopt(0)).Handler())
	defer srv.Close()
	html, _ := suspenseGet(t, srv) // its loader never returns: the fallback
	if !strings.Contains(html, `id="fallback"`) {
		t.Fatalf("got %s", html)
	}
	m := tokenRe.FindStringSubmatch(html)
	c, err := dial(t, srv, m[1], nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1, "v": 2})
	j := string(live.Marshal(must(t, c)))
	if strings.Contains(j, "loading") || !strings.Contains(j, "rows") {
		t.Fatalf("join: %s", j)
	}
}
