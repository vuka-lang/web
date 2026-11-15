package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// liveTransport is what a live connection does with the channel to its
// browser: read the browser's next message, write one, ping, and close, with
// a code the runtime reads (4001 reload, 4002 idle, …) or at once. A
// *websocket.Conn is one as it is; sseConn is the other, for a network whose
// proxies refuse the socket's upgrade. Both carry the same JSON messages, so
// everything above the transport (joins, events, navigations, broadcasts,
// deferred loads, the toolbar's record) is the same code for either.
type liveTransport interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Ping(ctx context.Context) error
	Close(code websocket.StatusCode, reason string) error
	CloseNow() error
}

var _ liveTransport = (*websocket.Conn)(nil)

// The fallback transport: server-sent events down, fetch POSTs up.
//
// The runtime opens GET <live>?transport=sse&t=<token> when it can't open a
// socket (live.js has the policy). The stream counts against the same caps
// as a socket (hub.admit), checks the same origins (LiveOrigins), and its
// first event, "ready", names the connection: an id of 128 random bits, the
// capability its POSTs carry. Each server message is then one unnamed event
// whose data is the message's JSON, a comment line stands for each ping,
// and a close is a last "close" event carrying {"code","reason"} before the
// stream ends, so the runtime takes the same path it takes for a socket's
// close code.
//
// The browser's messages are POSTed to <live>?c=<id>, one per request, the
// body the message's JSON as a socket would have carried it. The handler
// hands it to the connection's read pump, which feeds the connection's loop
// as it does a socket's messages; it answers 204 once the pump has the
// message, and 409 for a connection that is gone, which the runtime treats
// as a closed connection and reconnects. The runtime sends one POST at a
// time, in order, so the messages reach the loop in the order they were
// sent.
//
// Out of scope here: compression (the socket's permessage-deflate has no
// counterpart; the stream is sent as it is), binary messages (live has
// none), resuming a stream (Last-Event-ID: a dropped stream reconnects and
// joins again as a dropped socket does), several pages sharing one stream,
// and cross-origin pages, which would need CORS on both requests.

// streamPad is a comment sent first on a stream: proxies that buffer a
// response until some bytes arrive pass the events after it on.
var streamPad = ":" + strings.Repeat(" ", 2048) + "\n\n"

// streamHeaders are the stream request's own headers, which a run of a route
// for its connection doesn't carry: the page's route answers HTML, not an
// event stream.
var streamHeaders = []string{"Accept", "Cache-Control", "Last-Event-Id"}

var errStreamClosed = errors.New("live: the stream closed")

// sseConn is a live connection over server-sent events.
type sseConn struct {
	id   string
	hub  *hub
	in   chan []byte   // the browser's messages, from its POSTs
	done chan struct{} // closed once the stream ends

	mu    sync.Mutex // writes, from the connection's loop, its pings and closeAll
	w     http.ResponseWriter
	rc    *http.ResponseController
	ended bool
}

// acceptStream answers a live connection's stream request: the stream's
// headers and its ready event, flushed. It returns the connection and the
// request a run of the page's routes clones; nil when it answered with an
// error (another origin, a response that can't stream).
func (a *App) acceptStream(w http.ResponseWriter, r *http.Request) (*sseConn, *http.Request) {
	if !a.liveOriginOK(r) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return nil, nil
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s := &sseConn{id: newStreamID(), hub: a.hub, in: make(chan []byte), done: make(chan struct{}), w: w, rc: rc}
	ready, _ := json.Marshal(map[string]string{"id": s.id})
	io.WriteString(w, streamPad)
	if err := s.event("ready", ready); err != nil {
		return nil, nil
	}
	a.hub.addStream(s)
	r = r.Clone(r.Context())
	for _, k := range streamHeaders {
		r.Header.Del(k)
	}
	return s, r
}

func newStreamID() string {
	var b [16]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// event writes one event and flushes it; the caller holds s.mu, or s isn't
// shared yet.
func (s *sseConn) event(name string, data []byte) error {
	var b bytes.Buffer
	if name != "" {
		b.WriteString("event: " + name + "\n")
	}
	// JSON has no raw newline, but a data line ends at one: split anyway
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		b.WriteString("data: ")
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return s.send(b.Bytes())
}

func (s *sseConn) send(b []byte) error {
	if _, err := s.w.Write(b); err != nil {
		return err
	}
	return s.rc.Flush()
}

// deadline bounds the write about to happen by ctx's deadline, past the
// server's own WriteTimeout, which a stream would outlive.
func (s *sseConn) deadline(ctx context.Context) {
	if d, ok := ctx.Deadline(); ok {
		s.rc.SetWriteDeadline(d)
	}
}

func (s *sseConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case b := <-s.in:
		return websocket.MessageText, b, nil
	case <-s.done:
		return 0, nil, errStreamClosed
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (s *sseConn) Write(ctx context.Context, _ websocket.MessageType, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return errStreamClosed
	}
	s.deadline(ctx)
	return s.event("", p)
}

func (s *sseConn) Ping(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return errStreamClosed
	}
	s.deadline(ctx)
	return s.send([]byte(":\n\n"))
}

// Close sends the close event and ends the stream.
func (s *sseConn) Close(code websocket.StatusCode, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return errStreamClosed
	}
	b, _ := json.Marshal(struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	}{int(code), reason})
	s.rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	err := s.event("close", b)
	s.end()
	return err
}

// CloseNow ends the stream without a close event: the runtime sees the
// stream fail, as a socket closed without a code.
func (s *sseConn) CloseNow() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ended {
		s.end()
	}
	return nil
}

// end marks the stream ended (no write reaches the response from here on,
// so none can after its handler returns) and takes it off the hub; the
// caller holds s.mu.
func (s *sseConn) end() {
	s.ended = true
	close(s.done)
	s.hub.removeStream(s)
}

// deliver hands a POSTed message to the connection's read pump; false when
// the connection ended first.
func (s *sseConn) deliver(ctx context.Context, data []byte) bool {
	select {
	case s.in <- data:
		return true
	case <-s.done:
	case <-ctx.Done():
	}
	return false
}

// The hub's streams are a sync.Map, not under its mutex: a stream ends (and
// leaves them) from code holding that mutex.
func (h *hub) addStream(s *sseConn)    { h.streams.Store(s.id, s) }
func (h *hub) removeStream(s *sseConn) { h.streams.CompareAndDelete(s.id, s) }

func (h *hub) stream(id string) *sseConn {
	s, _ := h.streams.Load(id)
	c, _ := s.(*sseConn)
	return c
}

// serveLiveSend takes a message POSTed for a live connection over a stream.
func (a *App) serveLiveSend(w http.ResponseWriter, r *http.Request) {
	if !a.liveOriginOK(r) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	s := a.hub.stream(r.URL.Query().Get("c"))
	if s == nil {
		http.Error(w, "live: no such connection", http.StatusConflict)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20)) // a socket's read limit
	if err != nil {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}
	if !s.deliver(r.Context(), data) {
		http.Error(w, "live: no such connection", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// liveOriginOK checks a stream's or a POST's Origin as the socket's upgrade
// checks it: none (a same-origin GET), the request's own host, or one of
// LiveOrigins' patterns.
func (a *App) liveOriginOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if strings.EqualFold(r.Host, u.Host) {
		return true
	}
	for _, p := range a.liveOrigins {
		target := u.Host
		if strings.Contains(p, "://") {
			target = u.Scheme + "://" + u.Host
		}
		if ok, _ := path.Match(strings.ToLower(p), strings.ToLower(target)); ok {
			return true
		}
	}
	return false
}
