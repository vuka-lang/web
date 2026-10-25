package web

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// rowList is a live list page: 200 rows a page, turned by Next.
type rowList struct {
	ui.Live
	page int
}

func (l *rowList) Next() { l.page++ }
func (l *rowList) Render() ui.Node {
	rows := make([]ui.Node, 0, 200)
	for i := range 200 {
		n := l.page*200 + i
		rows = append(rows, ui.El("tr", nil,
			ui.El("td", []ui.Attr{{Name: "class", Value: "px-3 py-2 font-mono text-sm"}}, ui.Text(n)),
			ui.El("td", []ui.Attr{{Name: "class", Value: "px-3 py-2"}}, ui.Text(fmt.Sprintf("Pet %d", n))),
			ui.El("td", []ui.Attr{{Name: "class", Value: "px-3 py-2 text-muted-foreground"}}, ui.Text(fmt.Sprintf("owner%d@example.com", n%37)))))
	}
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(l.Next)}}, ui.Text("next")),
		ui.El("table", []ui.Attr{{Name: "class", Value: "w-full text-left"}}, ui.El("tbody", nil, rows...)))
}

func deflateApp(opts ...Option) *App {
	a := liveApp(append([]Option{LiveMaxConnsPerIP(0), LiveMaxConnsPerPage(0)}, opts...)...)
	a.Live("/rows", &rowList{})
	return a
}

// wireCount counts the bytes a server's connections carry, after any
// compression.
type wireCount struct {
	net.Listener
	out, in atomic.Int64
}

type countedConn struct {
	net.Conn
	w *wireCount
}

func (c countedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.w.out.Add(int64(n))
	return n, err
}

func (c countedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.w.in.Add(int64(n))
	return n, err
}

func (w *wireCount) Accept() (net.Conn, error) {
	c, err := w.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return countedConn{c, w}, nil
}

func countedServer(h http.Handler) (*httptest.Server, *wireCount) {
	srv := httptest.NewUnstartedServer(h)
	w := &wireCount{Listener: srv.Listener}
	srv.Listener = w
	srv.Start()
	return srv, w
}

func dialMode(t testing.TB, srv *httptest.Server, tok string, mode websocket.CompressionMode, header http.Header) (*websocket.Conn, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if header == nil {
		header = http.Header{}
	}
	header.Set("Origin", srv.URL)
	c, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+liveSocket+"?t="+tok, &websocket.DialOptions{HTTPHeader: header, CompressionMode: mode})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(16 << 20)
	t.Cleanup(func() { c.CloseNow() })
	return c, res
}

type liveMode struct {
	name string
	opt  Option
	ext  string // the extension the server answers a client offering it
}

var liveModes = []liveMode{
	{"default", nil, "permessage-deflate; client_no_context_takeover; server_no_context_takeover"},
	{"NoContextTakeover", LiveCompression(NoContextTakeover), "permessage-deflate; client_no_context_takeover; server_no_context_takeover"},
	{"ContextTakeover", LiveCompression(ContextTakeover), "permessage-deflate"},
	{"Off", LiveCompression(Off), ""},
}

func (m liveMode) options() []Option {
	if m.opt == nil {
		return nil
	}
	return []Option{m.opt}
}

func TestLiveCompressionNegotiates(t *testing.T) {
	for _, m := range liveModes {
		t.Run(m.name, func(t *testing.T) {
			srv := httptest.NewServer(deflateApp(m.options()...).Handler())
			defer srv.Close()
			for _, client := range []websocket.CompressionMode{websocket.CompressionContextTakeover, websocket.CompressionNoContextTakeover, websocket.CompressionDisabled} {
				_, tok := pageToken(t, srv, "/rows")
				c, res := dialMode(t, srv, tok, client, nil)
				want := m.ext
				switch {
				case client == websocket.CompressionDisabled:
					want = ""
				case client == websocket.CompressionNoContextTakeover && want != "":
					want = "permessage-deflate; client_no_context_takeover; server_no_context_takeover"
				}
				if got := res.Header.Get("Sec-WebSocket-Extensions"); got != want {
					t.Errorf("client mode %d: Sec-WebSocket-Extensions %q, want %q", client, got, want)
				}
				send(t, c, map[string]any{"type": "join", "ref": 1})
				if r := must(t, c); !strings.Contains(r.HTML, "owner36@example.com") {
					t.Errorf("client mode %d: join %+v", client, r)
				}
			}
		})
	}
}

// The live upgrade comes with the Accept-Encoding browsers send on it; it
// must reach the socket past any HTTP response compression.
func TestLiveUpgradeAcceptEncoding(t *testing.T) {
	srv := httptest.NewServer(deflateApp().Handler())
	defer srv.Close()
	_, tok := pageToken(t, srv, "/rows")
	c, res := dialMode(t, srv, tok, websocket.CompressionNoContextTakeover, http.Header{"Accept-Encoding": {"gzip, deflate, br, zstd"}})
	if res.StatusCode != http.StatusSwitchingProtocols || res.Header.Get("Content-Encoding") != "" || res.Header.Get("Sec-WebSocket-Extensions") == "" {
		t.Fatalf("upgrade: %d %v", res.StatusCode, res.Header)
	}
	send(t, c, map[string]any{"type": "join", "ref": 1})
	if r := must(t, c); !strings.Contains(r.HTML, "Pet 199") {
		t.Fatalf("join: %+v", r)
	}
}

func TestBrowserLiveCompression(t *testing.T) {
	b := chrome.Launch(t)
	for _, m := range liveModes {
		srv := httptest.NewServer(deflateApp(m.options()...).Handler())
		p := b.Open(srv.URL + "/rows")
		var ext string
		p.Eval("__wire.sockets[0].extensions", &ext)
		if ext != m.ext {
			t.Errorf("%s: Chrome negotiated %q, want %q", m.name, ext, m.ext)
		}
		p.Click("button")
		p.WaitText("tbody tr:first-child td", "200")
		srv.Close()
	}
}

var tokenField = regexp.MustCompile(`"token":"[^"]*"`)

// liveSession runs a long session on the list page: its join, 100 events
// and a navigation to another page and an event there. It returns the
// server's messages, tokens left out (each is signed afresh).
func liveSession(t testing.TB, srv *httptest.Server, mode websocket.CompressionMode) []string {
	t.Helper()
	_, tok := pageToken(t, srv, "/rows")
	c, _ := dialMode(t, srv, tok, mode, nil)
	var got []string
	next := func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		s := tokenField.ReplaceAllString(string(b), `"token":""`)
		got = append(got, s)
		return s
	}
	send(t, c, map[string]any{"type": "join", "ref": 1})
	html := must(t, c).HTML
	got = append(got, html)
	target := handlerOf(t, html, "click", 0)
	for i := range 100 {
		send(t, c, map[string]any{"type": "event", "ref": 2 + i, "target": target, "event": "click"})
		next()
	}
	send(t, c, map[string]any{"type": "navigate", "ref": 102, "url": "/counter"})
	nav := next()
	send(t, c, map[string]any{"type": "event", "ref": 103, "target": handlerOf(t, strings.ReplaceAll(nav, `\"`, `"`), "click", 0), "event": "click"})
	next()
	return got
}

func TestLiveCompressionSameMessages(t *testing.T) {
	var want []string
	for _, m := range liveModes {
		srv := httptest.NewServer(deflateApp(m.options()...).Handler())
		got := liveSession(t, srv, websocket.CompressionContextTakeover)
		srv.Close()
		if !strings.Contains(got[len(got)-1], "n=6") || !strings.Contains(got[100], "Pet 20199") {
			t.Fatalf("%s: the session went wrong: %.200s / %.200s", m.name, got[100], got[len(got)-1])
		}
		if want == nil {
			want = got
			continue
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d messages, want %d", m.name, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: message %d differs:\n%.300s\nwant\n%.300s", m.name, i, got[i], want[i])
			}
		}
	}
}

// Off sends every message as it is, even to a client offering compression:
// the bytes on the wire are the messages' and their framing.
func TestLiveCompressionOff(t *testing.T) {
	srv, w := countedServer(deflateApp(LiveCompression(Off)).Handler())
	defer srv.Close()
	_, tok := pageToken(t, srv, "/rows")
	c, res := dialMode(t, srv, tok, websocket.CompressionContextTakeover, nil)
	if ext := res.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		t.Fatalf("Off negotiated %q", ext)
	}
	before := w.out.Load()
	send(t, c, map[string]any{"type": "join", "ref": 1})
	html := must(t, c).HTML
	if sent := w.out.Load() - before; sent < int64(len(html)) {
		t.Fatalf("Off: %d bytes on the wire for a %d-byte page", sent, len(html))
	}
}

// BenchmarkLiveWire reports the bytes on the wire, server to client, of a
// long live session (liveSession) in each mode.
func BenchmarkLiveWire(b *testing.B) {
	for _, m := range liveModes {
		b.Run(m.name, func(b *testing.B) {
			srv, w := countedServer(deflateApp(m.options()...).Handler())
			defer srv.Close()
			var wire, raw int64
			for range b.N {
				before := w.out.Load()
				for _, s := range liveSession(b, srv, websocket.CompressionContextTakeover) {
					raw += int64(len(s))
				}
				wire += w.out.Load() - before
			}
			b.ReportMetric(float64(wire)/float64(b.N)/1024, "wire-KB/session")
			b.ReportMetric(float64(raw)/float64(b.N)/1024, "raw-KB/session")
		})
	}
}

// BenchmarkLiveConnMemory measures the server's memory for each open live
// socket in each mode: 1,000 sockets join the list page and take one event,
// from minimal clients holding nothing but their TCP connection, then the
// heap and stacks in use are compared with before they opened. Run it with
// -bench LiveConnMemory -benchtime 1x.
func BenchmarkLiveConnMemory(b *testing.B) {
	const conns = 1000
	for _, m := range liveModes {
		b.Run(m.name, func(b *testing.B) {
			srv := httptest.NewServer(deflateApp(append(m.options(), LiveMaxConns(0))...).Handler())
			defer srv.Close()
			_, tok := pageToken(b, srv, "/rows")
			target := ""
			{
				c, _ := dialMode(b, srv, tok, websocket.CompressionDisabled, nil)
				send(b, c, map[string]any{"type": "join", "ref": 1})
				target = handlerOf(b, must(b, c).HTML, "click", 0)
				c.CloseNow()
			}
			var per float64
			for range b.N {
				base := inUse()
				open := make([]net.Conn, conns)
				var wg sync.WaitGroup
				errs := make(chan error, conns)
				for i := range conns {
					wg.Go(func() {
						c, err := rawLive(srv, tok, target)
						open[i] = c
						if err != nil {
							errs <- err
						}
					})
				}
				wg.Wait()
				close(errs)
				if err := <-errs; err != nil {
					b.Fatal(err)
				}
				per = float64(inUse()-base) / conns
				for _, c := range open {
					c.Close()
				}
			}
			b.ReportMetric(per/1024, "KB/conn")
		})
	}
}

func inUse() int64 {
	runtime.GC()
	runtime.GC()
	var s runtime.MemStats
	runtime.ReadMemStats(&s)
	return int64(s.HeapInuse + s.StackInuse)
}

// rawLive opens a live socket by hand, offering permessage-deflate, joins its page and sends it one event, reading the replies'
// frames without inflating them: a client costing the test's process next
// to nothing.
func rawLive(srv *httptest.Server, tok, target string) (net.Conn, error) {
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	rand.Read(key)
	ext := "Sec-WebSocket-Extensions: permessage-deflate\r\n"
	fmt.Fprintf(c, "GET %s?t=%s HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n%s\r\n",
		liveSocket, tok, strings.TrimPrefix(srv.URL, "http://"), srv.URL, base64.StdEncoding.EncodeToString(key), ext)
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		return c, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		return c, fmt.Errorf("upgrade: %s", res.Status)
	}
	for _, msg := range []string{`{"type":"join","ref":1}`, `{"type":"event","ref":2,"target":"` + target + `","event":"click"}`} {
		if err := rawFrame(c, msg); err != nil {
			return c, err
		}
		if err := skipFrame(br); err != nil {
			return c, err
		}
	}
	return c, nil
}

func rawFrame(w io.Writer, msg string) error {
	h := []byte{0x81, 0x80}
	switch n := len(msg); {
	case n < 126:
		h[1] |= byte(n)
	default:
		h[1] |= 126
		h = binary.BigEndian.AppendUint16(h, uint16(n))
	}
	h = append(h, 0, 0, 0, 0) // a zero mask
	_, err := w.Write(append(h, msg...))
	return err
}

func skipFrame(r *bufio.Reader) error {
	for {
		var h [2]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return err
		}
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var l [2]byte
			if _, err := io.ReadFull(r, l[:]); err != nil {
				return err
			}
			n = uint64(binary.BigEndian.Uint16(l[:]))
		case 127:
			var l [8]byte
			if _, err := io.ReadFull(r, l[:]); err != nil {
				return err
			}
			n = binary.BigEndian.Uint64(l[:])
		}
		if _, err := r.Discard(int(n)); err != nil {
			return err
		}
		if op := h[0] & 0x0f; h[0]&0x80 != 0 && op != 0x9 && op != 0xa {
			return nil // the end of a data message, not a ping or pong
		}
	}
}
