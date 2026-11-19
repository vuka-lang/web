package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
)

// fakeSocket is a liveTransport whose writes wait for release while held.
type fakeSocket struct {
	wmu     sync.Mutex // held by a write, as a frame's is: a close waits for it
	mu      sync.Mutex
	written []string
	closed  []websocket.StatusCode
	now     bool
	hold    chan struct{}
	inWrite chan struct{}
}

func (f *fakeSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	<-ctx.Done()
	return 0, nil, ctx.Err()
}

func (f *fakeSocket) Write(ctx context.Context, _ websocket.MessageType, p []byte) error {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if f.inWrite != nil {
		select {
		case f.inWrite <- struct{}{}:
		default:
		}
	}
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	f.written = append(f.written, string(p))
	f.mu.Unlock()
	return nil
}

func (f *fakeSocket) Ping(context.Context) error { return nil }

func (f *fakeSocket) Close(code websocket.StatusCode, _ string) error {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	f.mu.Lock()
	f.closed = append(f.closed, code)
	f.mu.Unlock()
	return nil
}

func (f *fakeSocket) CloseNow() error {
	f.mu.Lock()
	f.now = true
	f.mu.Unlock()
	return nil
}

func (f *fakeSocket) snapshot() (written []string, closed []websocket.StatusCode, now bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.written...), append([]websocket.StatusCode(nil), f.closed...), f.now
}

func fakeConn(f *fakeSocket) *conn {
	return &conn{ws: f, out: outbox{space: make(chan struct{}, 1)}}
}

func setCloseTimeout(t *testing.T, d time.Duration) {
	old := closeTimeout
	closeTimeout = d
	t.Cleanup(func() { closeTimeout = old })
}

func TestOutboxWritesFramesInOrder(t *testing.T) {
	f := &fakeSocket{}
	c := fakeConn(f)
	ctx := context.Background()
	for i := range 500 {
		if !c.write(ctx, i) {
			t.Fatalf("frame %d refused", i)
		}
	}
	c.out.settle(5 * time.Second)
	got, _, _ := f.snapshot()
	if len(got) != 500 {
		t.Fatalf("%d frames written, want 500", len(got))
	}
	for i, g := range got {
		if g != fmt.Sprint(i) {
			t.Fatalf("frame %d is %q", i, g)
		}
	}
}

func TestOutboxIdleHoldsNoGoroutine(t *testing.T) {
	f := &fakeSocket{}
	c := fakeConn(f)
	c.write(context.Background(), 1)
	waitFor(t, func() bool { c.out.mu.Lock(); defer c.out.mu.Unlock(); return !c.out.run })
}

func TestOutboxPushNeverWaitsAndOverflowClosesWithReload(t *testing.T) {
	setCloseTimeout(t, 150*time.Millisecond)
	f := &fakeSocket{hold: make(chan struct{}), inWrite: make(chan struct{}, 1)}
	c := fakeConn(f)
	ctx := context.Background()
	c.push(ctx, "first")
	<-f.inWrite // the writer holds one frame, stuck in the socket
	for i := range liveOutFrames {
		if !c.push(ctx, i) {
			t.Fatalf("frame %d refused with room in the queue", i)
		}
	}
	start := time.Now()
	if c.push(ctx, "over") {
		t.Fatal("a frame past the queue was accepted")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the overflow waited %v", d)
	}
	_, closed, now := f.snapshot()
	if !now {
		t.Error("a socket whose writer is stuck was not dropped after the close deadline")
	}
	if len(closed) != 0 {
		t.Errorf("a close frame %v went out past a write that never finished", closed)
	}
	c.out.mu.Lock()
	queued := len(c.out.q)
	c.out.mu.Unlock()
	if queued != 0 {
		t.Errorf("%d frames still queued for a page that loads again", queued)
	}
	if c.send(ctx, []byte("late"), false) {
		t.Error("a frame was accepted after the close")
	}
	close(f.hold)
}

func TestOutboxCloseFrameFollowsTheFramesAhead(t *testing.T) {
	f := &fakeSocket{hold: make(chan struct{}), inWrite: make(chan struct{}, 1)}
	c := fakeConn(f)
	ctx := context.Background()
	for i := range 5 {
		c.write(ctx, i)
	}
	<-f.inWrite
	done := make(chan struct{})
	go func() { c.closeConn(closeReload, "bye"); close(done) }()
	select {
	case <-done:
		t.Fatal("the close didn't wait for the frames ahead of it")
	case <-time.After(50 * time.Millisecond):
	}
	for range 5 {
		f.hold <- struct{}{}
	}
	<-done
	got, closed, now := f.snapshot()
	if len(got) != 5 || len(closed) != 1 || closed[0] != closeReload || now {
		t.Fatalf("written %v, closed %v, dropped %v", got, closed, now)
	}
}

func TestOutboxWriteWaitsForRoomThenGivesUp(t *testing.T) {
	setCloseTimeout(t, 100*time.Millisecond)
	old := writeTimeout
	writeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { writeTimeout = old })
	f := &fakeSocket{hold: make(chan struct{}, 1), inWrite: make(chan struct{}, 1)}
	c := fakeConn(f)
	ctx := context.Background()
	c.write(ctx, "first")
	<-f.inWrite
	for i := range liveOutFrames {
		c.write(ctx, i)
	}
	res := make(chan bool)
	go func() { res <- c.write(ctx, "waits") }()
	select {
	case <-res:
		t.Fatal("a write didn't wait for room")
	case <-time.After(100 * time.Millisecond):
	}
	f.hold <- struct{}{} // the writer takes the next frame: room
	if !<-res {
		t.Fatal("a write got no room once the writer took a frame")
	}
	start := time.Now()
	for c.write(ctx, "more") {
		if time.Since(start) > 5*time.Second {
			t.Fatal("a stuck socket never refused a frame")
		}
	}
	if _, closed, _ := f.snapshot(); len(closed) != 1 || closed[0] != closeReload {
		t.Fatalf("closed %v, want one 4001", closed)
	}
}

type lastSaid struct {
	ui.Live
	text string
}

func (l *lastSaid) LiveSchema(s *ui.LiveSchema)   { s.Mount(l.mount) }
func (l *lastSaid) mount(_ context.Context) error { l.Subscribe("last"); return nil }
func (l *lastSaid) Update(m said)                 { l.text = m.Text }
func (l *lastSaid) Render() ui.Node               { return ui.El("p", nil, ui.Text(l.text)) }

func outApp(opts ...Option) *App {
	app := liveApp(opts...)
	app.Live("/last", &lastSaid{})
	return app
}

// smallBuffers makes the sockets a listener accepts hold little unsent data,
// so a client that doesn't read backs the server's writes up soon.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		c.(*net.TCPConn).SetWriteBuffer(8 << 10)
	}
	return c, err
}

// A client that stops reading is told to reload when its socket's buffers
// back up: the close frame goes out as soon as the frame the socket is stuck
// on has, and the client finds it once it reads.
func TestLiveSlowClientIsToldToReload(t *testing.T) {
	setCloseTimeout(t, 4*time.Second)
	old := writeTimeout
	writeTimeout = 30 * time.Second
	t.Cleanup(func() { writeTimeout = old })
	app := outApp(LiveCompression(Off))
	srv := httptest.NewUnstartedServer(app.Handler())
	srv.Listener = smallBuffers{srv.Listener}
	srv.Start()
	defer srv.Close()
	drained(t, app)
	_, tok := pageToken(t, srv, "/last")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err == nil {
			c.(*net.TCPConn).SetReadBuffer(4 << 10)
		}
		return c, err
	}}}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+liveSocket+"?t="+tok, &websocket.DialOptions{HTTPClient: hc, HTTPHeader: http.Header{"Origin": {srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	c.SetReadLimit(4 << 20)
	send(t, c, map[string]any{"type": "join", "ref": 1})
	if m := must(t, c); m.Type != live.Render {
		t.Fatalf("join: %+v", m)
	}
	big := make([]byte, 64<<10)
	rand.Read(big)
	text := base64.StdEncoding.EncodeToString(big)
	closing := func() bool {
		for _, sc := range connsOf(app) {
			if sc.closing.Load() {
				return true
			}
		}
		return false
	}
	for i := 0; i < 2000 && !closing(); i++ {
		app.Broadcast(ctx, "last", said{fmt.Sprint(i, text)})
		time.Sleep(2 * time.Millisecond)
	}
	if !closing() {
		t.Fatal("the server never gave up on a client that read nothing")
	}
	for {
		_, _, err := c.Read(ctx)
		if err == nil {
			continue
		}
		if code := websocket.CloseStatus(err); code != closeReload {
			t.Fatalf("the slow client's read ended with %v (status %d), want a 4001 close", err, code)
		}
		return
	}
}

// A client that stops answering pings is closed with the reload status
// before the socket is given up.
func TestLiveUnresponsiveClientGetsCloseFrame(t *testing.T) {
	oldP, oldW := pingEvery, writeTimeout
	pingEvery, writeTimeout = 100*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { pingEvery, writeTimeout = oldP, oldW })
	app := outApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	drained(t, app)
	c, _ := joined(t, srv, "/last")
	time.Sleep(1200 * time.Millisecond) // reading nothing: no pong goes back
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			if code := websocket.CloseStatus(err); code != closeReload {
				t.Fatalf("read ended with %v (status %d), want a 4001 close", err, code)
			}
			return
		}
	}
}
