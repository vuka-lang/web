package web

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/ui/live"
	"github.com/vuka-lang/web/internal/liveproto"
)

// Close codes of a live connection the runtime doesn't retry: 4001 asks it to
// reload the page (an expired or foreign token), 4004 says the page can't be
// live (its route failed or isn't a page).
const (
	closeReload  websocket.StatusCode = 4001
	closeRefused websocket.StatusCode = 4004
)

var (
	pingEvery    = 25 * time.Second
	writeTimeout = 10 * time.Second
)

// Relay carries broadcasts between the replicas of an app: Publish sends a
// message to every replica, and each calls the function Subscribe was given
// with what arrives (its own messages included). Encoding the messages, whose
// dynamic types pick the Update methods they reach, is the relay's business.
type Relay interface {
	Publish(ctx context.Context, topic string, msg any) error
	Subscribe(deliver func(topic string, msg any))
}

// WithRelay sends the app's broadcasts through r instead of delivering them
// in-process only.
func WithRelay(r Relay) Option { return OptionFunc(func(a *App) { a.relay = r }) }

// Broadcast delivers msg to the Update methods of every live component
// subscribed to topic, in every connected page of the app serving ctx (a
// request's, or a live event's), and sends each page what changed. A topic
// of "" reaches every component.
func Broadcast(ctx context.Context, topic string, msg any) error {
	rq, _ := ctx.Value(ctxKey{}).(*request)
	if rq == nil || rq.app == nil {
		return errors.New("web: Broadcast needs the context of a request or of a live event")
	}
	return rq.app.Broadcast(ctx, topic, msg)
}

// Broadcast delivers msg to the components subscribed to topic, as the
// package's Broadcast does.
func (a *App) Broadcast(ctx context.Context, topic string, msg any) error {
	if a.relay != nil {
		return a.relay.Publish(ctx, topic, msg)
	}
	a.hub.deliver(topic, msg)
	return nil
}

// hub is the app's live connections, and the topics each subscribes to.
type hub struct {
	mu     sync.Mutex
	conns  map[*conn]bool
	topics map[string]map[*conn]bool
	closed bool
}

func newHub() *hub { return &hub{conns: map[*conn]bool{}, topics: map[string]map[*conn]bool{}} }

func (h *hub) add(c *conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.conns[c] = true
	}
	return !h.closed
}

func (h *hub) remove(c *conn) {
	h.setTopics(c, nil)
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

func (h *hub) setTopics(c *conn, ts []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range c.topics {
		if delete(h.topics[t], c); len(h.topics[t]) == 0 {
			delete(h.topics, t)
		}
	}
	for _, t := range ts {
		if h.topics[t] == nil {
			h.topics[t] = map[*conn]bool{}
		}
		h.topics[t][c] = true
	}
	c.topics = ts
}

func (h *hub) deliver(topic string, msg any) {
	h.mu.Lock()
	set := h.topics[topic]
	if topic == "" {
		set = h.conns
	}
	to := make([]*conn, 0, len(set))
	for c := range set {
		to = append(to, c)
	}
	h.mu.Unlock()
	for _, c := range to {
		c.post(topic, msg)
	}
}

// closeAll closes every connection (going away: the runtime reconnects) and
// refuses new ones.
func (h *hub) closeAll() {
	h.mu.Lock()
	h.closed = true
	conns := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Go(func() { c.ws.Close(websocket.StatusGoingAway, "the server is shutting down") })
	}
	wg.Wait()
}

// conn is one connected page: its socket, its session once joined, and the
// broadcasts waiting for it. One goroutine runs its session, so events and
// broadcasts are answered in order.
type conn struct {
	app    *App
	ws     *websocket.Conn
	r      *http.Request // the upgrade request
	uri    string        // the page, from its token
	s      liveproto.Session
	topics []string // guarded by the hub's mutex

	mu    sync.Mutex
	inbox []delivery
	wake  chan struct{}
}

type delivery struct {
	topic string
	msg   any
}

func (c *conn) post(topic string, msg any) {
	c.mu.Lock()
	c.inbox = append(c.inbox, delivery{topic, msg})
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// joinKey marks the request a join re-runs; page hands it the page instead of
// writing it.
type joinKey struct{}

type join struct {
	page ui.Node
	ctx  context.Context
}

func (a *App) serveLive(w http.ResponseWriter, r *http.Request) {
	uri, terr := a.verifyToken(r.URL.Query().Get("t"))
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: a.liveOrigins, CompressionMode: websocket.CompressionNoContextTakeover})
	if err != nil {
		return // Accept answered: 403 for another origin
	}
	if terr != nil {
		ws.Close(closeReload, terr.Error())
		return
	}
	ws.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := &conn{app: a, ws: ws, r: r, uri: uri, wake: make(chan struct{}, 1)}
	if !a.hub.add(c) {
		ws.Close(websocket.StatusGoingAway, "the server is shutting down")
		return
	}
	defer a.hub.remove(c)
	defer func() {
		if c.s != nil {
			c.s.Close()
		}
	}()
	msgs := make(chan []byte)
	go func() {
		defer cancel()
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			select {
			case msgs <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			ws.CloseNow()
			return
		case data := <-msgs:
			if !c.handle(ctx, data) {
				return
			}
		case <-c.wake:
			c.drain(ctx)
		case <-ping.C:
			go func() {
				pctx, done := context.WithTimeout(ctx, writeTimeout)
				defer done()
				if ws.Ping(pctx) != nil {
					cancel()
				}
			}()
		}
	}
}

func (c *conn) write(ctx context.Context, m any) {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false) // markup as markup, not \u003c…
	e.Encode(m)
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	wctx, done := context.WithTimeout(ctx, writeTimeout)
	defer done()
	if c.ws.Write(wctx, websocket.MessageText, b) != nil {
		c.ws.CloseNow()
	}
}

// handle answers a client message; false ends the connection.
func (c *conn) handle(ctx context.Context, data []byte) bool {
	var m struct {
		live.ClientMessage
		V int `json:"v,omitempty"` // the newest protocol version the page speaks (a join's)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		c.write(ctx, live.ServerMessage{Type: live.Error, Error: "live: bad message"})
		return true
	}
	switch {
	case m.Type == live.Join:
		if c.s != nil {
			c.s.Close()
			c.s = nil
		}
		reply, code := c.join(ctx, m.Ref, m.V)
		c.write(ctx, reply)
		if code != 0 {
			c.ws.Close(code, reply.(live.ServerMessage).Error)
			return false
		}
	case c.s == nil:
		c.write(ctx, live.ServerMessage{Type: live.Error, Ref: m.Ref, Error: "live: join first"})
	default:
		c.write(ctx, c.s.Event(m.ClientMessage))
	}
	c.syncTopics()
	return true
}

// join rebuilds the page: the route its token names runs again, as a GET
// carrying the upgrade request's headers (cookies, auth), through the app's
// middleware; the page it renders becomes the session's, speaking protocol
// version v when the app can (else v1).
func (c *conn) join(ctx context.Context, ref, v int) (any, websocket.StatusCode) {
	u, err := url.ParseRequestURI(c.uri)
	if err != nil {
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: errBadToken.Error()}, closeReload
	}
	j := &join{}
	req := c.r.Clone(context.WithValue(ctx, joinKey{}, j))
	req.Method, req.URL, req.RequestURI = http.MethodGet, u, c.uri
	req.Body, req.ContentLength = http.NoBody, 0
	for _, h := range []string{"Upgrade", "Connection", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol"} {
		req.Header.Del(h)
	}
	req.Header.Set("X-Vuka-Live", "join")
	rec := &recorder{h: http.Header{}}
	c.app.handler.ServeHTTP(rec, req)
	if j.page == nil {
		if loc := rec.h.Get("Location"); rec.status >= 300 && rec.status < 400 && loc != "" {
			return live.ServerMessage{Type: live.Redirect, Ref: ref, URL: loc}, websocket.StatusNormalClosure
		}
		status := cmp.Or(rec.status, http.StatusOK)
		msg := fmt.Sprintf("live: GET %s answered %d %s", c.uri, status, http.StatusText(status))
		if status == http.StatusOK {
			msg = fmt.Sprintf("live: GET %s is not a live page", c.uri)
		}
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: msg}, closeRefused
	}
	c.s = liveproto.New(j.ctx, func(context.Context) ui.Node { return j.page }, min(v, c.app.liveMax))
	return c.s.Join(ref), 0
}

// drain delivers the broadcasts waiting for the page; before a join there is
// nothing to deliver them to, and the join renders the page fresh.
func (c *conn) drain(ctx context.Context) {
	c.mu.Lock()
	batch := c.inbox
	c.inbox = nil
	c.mu.Unlock()
	if c.s == nil {
		return
	}
	for _, d := range batch {
		if m := c.s.Info(d.topic, d.msg); m != nil {
			c.write(ctx, m)
		}
	}
	c.syncTopics()
}

func (c *conn) syncTopics() {
	if c.s == nil {
		return
	}
	ts := c.s.Topics()
	c.app.hub.mu.Lock()
	same := slices.Equal(ts, c.topics)
	c.app.hub.mu.Unlock()
	if !same {
		c.app.hub.setTopics(c, ts)
	}
}

// recorder is the response of a join's run of the route: only its status and
// headers matter.
type recorder struct {
	h      http.Header
	status int
}

func (r *recorder) Header() http.Header { return r.h }
func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}
func (r *recorder) Write(b []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return len(b), nil
}

func serveLiveJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("ETag", `"`+liveJSTag+`"`)
	if r.URL.Query().Get("v") == liveJSTag {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, "live.js", time.Time{}, bytes.NewReader(liveJS))
}
