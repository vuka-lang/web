package web

import (
	"bufio"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2"
)

// countPage counts what a page view runs: its route's guard, the page's
// construction, a method that isn't deferred, its deferred one.
type countPage struct {
	n       *viewCounts
	release chan struct{}
}

type viewCounts struct {
	guard, made, plain, stats atomic.Int32
	canceled                  atomic.Bool
}

func (p *countPage) Plain() string { p.n.plain.Add(1); return "orders" }

func (p *countPage) Stats(ctx context.Context) (int, error) {
	p.n.stats.Add(1)
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			p.n.canceled.Store(true)
			return 0, ctx.Err()
		}
	}
	return 7, nil
}

func init() { vuka.RegisterFunc((*countPage).Stats, ui.Defer("")) }

func (p *countPage) Render(ctx context.Context, w io.Writer) error {
	return fullPage(ui.Group{
		ui.El("h1", nil, ui.Text(p.Plain())),
		ui.Part(p, (*countPage).Stats, nil, func(n int) ui.Node { return ui.El("p", nil, ui.Textf("%d open", n)) }),
	}).Render(ctx, w)
}

// countApp serves countPage at /orders to the visitor whose sid cookie is
// "ann" (a session cookie), refusing anyone else.
func countApp(n *viewCounts, release chan struct{}, opts ...Option) *App {
	a := New(append([]Option{Only("none"), SessionCookies("sid")}, opts...)...)
	a.Get("/orders", func() *countPage { n.made.Add(1); return &countPage{n: n, release: release} }, Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.guard.Add(1)
			if c, err := r.Cookie("sid"); err != nil || c.Value != "ann" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			h.ServeHTTP(w, r)
		})
	}))
	return a
}

var partsTokRE = regexp.MustCompile(` data-vk-parts="([^"]+)"`)

// viewPage loads the page at url as ann, whole, and returns it and its parts'
// token.
func viewPage(t *testing.T, url string) (string, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Cookie", "sid=ann")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	done := make(chan []byte)
	go func() { b, _ := io.ReadAll(res.Body); done <- b }()
	var body string
	select {
	case b := <-done:
		body = string(b)
	case <-time.After(5 * time.Second):
		t.Fatal("the page's response didn't end while its part's method ran")
	}
	m := partsTokRE.FindStringSubmatch(body)
	if m == nil || !strings.HasSuffix(body, "</body></html>") {
		t.Fatalf("no parts token, or the page isn't whole: %s", body)
	}
	if res.Header.Get("Cache-Control") != "private" {
		t.Errorf("Cache-Control: %q", res.Header.Get("Cache-Control"))
	}
	return body, m[1]
}

// pick is the runtime's request for the parts of page at url.
func pick(t *testing.T, url, page, tok, sid string) *http.Response {
	t.Helper()
	req := askParts(t, url, partIDs(page))
	req.Header.Set(partsTokenHeader, tok)
	req.Header.Set("Cookie", "sid="+sid)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (n *viewCounts) String() string {
	return "guard " + itoa(int(n.guard.Load())) + ", made " + itoa(int(n.made.Load())) + ", plain " + itoa(int(n.plain.Load())) + ", stats " + itoa(int(n.stats.Load()))
}

// A page view runs everything once: the page's request starts its
// deferred method and ends at once; the runtime's request for the parts,
// with the page's token, gets the part that method renders — no guard,
// construction or other method run again.
func TestPartsHandOff(t *testing.T) {
	n := &viewCounts{}
	release := make(chan struct{})
	srv := httptest.NewServer(countApp(n, release).Handler())
	defer srv.Close()
	page, tok := viewPage(t, srv.URL+"/orders")
	for i := 0; i < 500 && n.stats.Load() == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	if got := n.String(); got != "guard 1, made 1, plain 1, stats 1" {
		t.Fatalf("the page's request: %s", got)
	}
	res := pick(t, srv.URL+"/orders", page, tok, "ann")
	if res.Header.Get(PartsHeader) != "1" || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("the reply's headers: %v", res.Header)
	}
	close(release)
	if body := readBody(t, res); !strings.Contains(body, "><p>7 open</p></template><!--vk-parts:") {
		t.Errorf("the parts: %s", body)
	}
	if got := n.String(); got != "guard 1, made 1, plain 1, stats 1" {
		t.Errorf("a page view ran: %s", got)
	}
	// The token is spent: its next use renders the page again.
	if body := readBody(t, pick(t, srv.URL+"/orders", page, tok, "ann")); !strings.Contains(body, "<p>7 open</p>") || n.guard.Load() != 2 || n.stats.Load() != 2 {
		t.Errorf("a token used twice: %s; %s", n, body)
	}
}

// A token presented by another visitor, for another URL, or after its
// window isn't taken for its parts: the route runs for the request, its
// guard answering.
func TestPartsHandOffRefused(t *testing.T) {
	log.SetOutput(io.Discard)
	n := &viewCounts{}
	srv := httptest.NewServer(countApp(n, nil, PartsWindow(100*time.Millisecond)).Handler())
	defer srv.Close()
	page, tok := viewPage(t, srv.URL+"/orders")
	if res := pick(t, srv.URL+"/orders", page, tok, "bob"); res.StatusCode != http.StatusForbidden || res.Header.Get(PartsHeader) != "" {
		t.Errorf("another visitor: %d %v", res.StatusCode, res.Header)
	}
	if body := readBody(t, pick(t, srv.URL+"/orders", page, tok, "ann")); n.guard.Load() != 3 || !strings.Contains(body, "<p>7 open</p>") {
		t.Errorf("the token another visitor presented is spent: %s; %s", n, body)
	}
	page, tok = viewPage(t, srv.URL+"/orders")
	if body := readBody(t, pick(t, srv.URL+"/orders?all=1", page, tok, "ann")); n.guard.Load() != 5 || !strings.Contains(body, "<p>7 open</p>") {
		t.Errorf("another URL: %s; %s", n, body)
	}
	page, tok = viewPage(t, srv.URL+"/orders")
	time.Sleep(300 * time.Millisecond)
	if body := readBody(t, pick(t, srv.URL+"/orders", page, tok, "ann")); n.guard.Load() != 7 || !strings.Contains(body, "<p>7 open</p>") {
		t.Errorf("after the window: %s; %s", n, body)
	}
}

// Unclaimed parts are canceled when their window runs out, or when the
// cap on parked pages drops them; a claimed one when its request goes
// away.
func TestPartsHandOffCanceled(t *testing.T) {
	n := &viewCounts{}
	app := countApp(n, make(chan struct{}), PartsWindow(50*time.Millisecond))
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	viewPage(t, srv.URL+"/orders")
	waitUntil(t, "the window's end cancels the method", n.canceled.Load)

	n = &viewCounts{}
	app = countApp(n, make(chan struct{}))
	app.partsPark.max = 1
	srv2 := httptest.NewServer(app.Handler())
	defer srv2.Close()
	viewPage(t, srv2.URL+"/orders")
	viewPage(t, srv2.URL+"/orders")
	waitUntil(t, "the cap drops the oldest", n.canceled.Load)

	n = &viewCounts{}
	srv3 := httptest.NewServer(countApp(n, make(chan struct{})).Handler())
	defer srv3.Close()
	page, tok := viewPage(t, srv3.URL+"/orders")
	res := pick(t, srv3.URL+"/orders", page, tok, "ann")
	if res.Header.Get(PartsHeader) != "1" {
		t.Fatalf("not the parts: %v", res.Header)
	}
	time.Sleep(20 * time.Millisecond)
	if n.canceled.Load() {
		t.Fatal("canceled before the request went away")
	}
	res.Body.Close()
	waitUntil(t, "the request going away cancels the method", n.canceled.Load)
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 500 && !ok(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !ok() {
		t.Error(what + ": it didn't")
	}
}

// A navigation's reply carries the token in a header, and has no ETag.
func TestPartsHandOffNavigation(t *testing.T) {
	n := &viewCounts{}
	app := countApp(n, nil)
	h := app.Handler()
	res := tbDo(t, h, "GET", "/orders", NavigateHeader, app.buildID(), "Cookie", "sid=ann")
	if res.header.Get(partsTokenHeader) == "" || res.header.Get("ETag") != "" || !headerHasToken(res.header, "Vary", PartsHeader) {
		t.Errorf("a navigation: %v", res.header)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	r := bufio.NewReader(pick(t, srv.URL+"/orders", res.body, res.header.Get(partsTokenHeader), "ann").Body)
	if got := readUntil(t, r, "</template>"); !strings.Contains(got, "<p>7 open</p>") || n.guard.Load() != 1 {
		t.Errorf("its parts: %s; %s", n, got)
	}
}
