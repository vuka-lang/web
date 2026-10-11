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
	"strings"
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
	page   context.Context // the context the page's route ran with: the join's, else the latest navigation's
	topics []string        // guarded by the hub's mutex
	tb     *tbSink         // the development toolbar's record of the page, its messages under it
	wire   *wireMirror     // with tb, what the browser holds, to decode the frames sent
	frames []tbFrame       // with tb, the frames the message being recorded sent
	ev     *LiveEvent      // the event being handled, until its reply

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
	page    ui.Node
	ctx     context.Context
	updates map[ui.Stateful]any // a Live page's URLParams for its Update, on a patch
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
	if a.tb != nil {
		if c.tb = a.tb.liveSink(r.URL.Query().Get("tb"), uri); c.tb != nil {
			ctx = context.WithValue(ctx, tbKey{}, c.tb)
			c.wire = newWireMirror(c.instanceType)
		}
	}
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
	if c.tb != nil {
		f := newFrame("server", b)
		f.Report = c.wire.apply(b)
		c.frames = append(c.frames, f)
	}
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
	if c.tb != nil {
		defer c.record(m.ClientMessage, data)()
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
	case !c.check():
		c.ws.Close(closeReload, "live: the page must load again")
		return false
	case m.Type == live.Navigate:
		reply := c.navigate(ctx, m.ClientMessage)
		if _, moved := reply.(withToken); moved && c.tb != nil {
			// before the reply: the toolbar asks for the page once it lands
			c.tb.navigated(c.tb.cur.Load(), c.uri, m.Patch, c.r.Header)
		}
		c.write(ctx, reply)
	default:
		c.write(ctx, c.event(m.ClientMessage))
	}
	c.syncTopics()
	return true
}

// check runs the app's LiveChecks with the page's context.
func (c *conn) check() bool {
	for _, f := range c.app.liveChecks {
		if f(c.page) != nil {
			return false
		}
	}
	return true
}

// event runs an event's handler on this goroutine, bound as the
// CurrentLiveEvent; an event a decorator refused replies with its error.
func (c *conn) event(m live.ClientMessage) any {
	ev := &LiveEvent{ctx: c.page}
	if h, ok := c.s.(interface{ Handler(string) any }); ok {
		ev.handler = handlerName(h.Handler(m.Target))
	}
	unbind := bindLiveEvent(ev)
	c.ev = ev
	reply := c.s.Event(m)
	c.ev = nil
	unbind()
	if ev.refused != nil {
		return withError{reply, ev.refused.Error()}
	}
	return reply
}

// render is the session's render function for page: the session calls it
// first when it renders, so an event being handled learns its handler is
// done (LiveEvent.Handling).
func (c *conn) render(page ui.Node) liveproto.Render {
	return func(context.Context) ui.Node {
		if ev := c.ev; ev != nil {
			ev.rendering.Store(true)
		}
		return page
	}
}

// withError is an event's reply carrying err, unless it carries an error
// already.
type withError struct {
	msg any
	err string
}

func (w withError) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(w.msg)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return b, nil
	}
	if e, ok := m["error"]; ok && string(e) != `""` {
		return b, nil
	}
	m["error"], _ = json.Marshal(w.err)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// join rebuilds the page: the route its token names runs again, as a GET
// carrying the upgrade request's headers (cookies, auth), through the app's
// middleware; the page it renders becomes the session's, speaking protocol
// version v when the app can (else v1).
func (c *conn) join(ctx context.Context, ref, v int) (any, websocket.StatusCode) {
	j, rec, err := c.run(ctx, c.uri, "join")
	if err != nil {
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: errBadToken.Error()}, closeReload
	}
	if j.page == nil {
		if loc := rec.location(); loc != "" {
			return live.ServerMessage{Type: live.Redirect, Ref: ref, URL: loc}, websocket.StatusNormalClosure
		}
		status := cmp.Or(rec.status, http.StatusOK)
		msg := fmt.Sprintf("live: GET %s answered %d %s", c.uri, status, http.StatusText(status))
		if status == http.StatusOK {
			msg = fmt.Sprintf("live: GET %s is not a live page", c.uri)
		}
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: msg}, closeRefused
	}
	c.s, c.page = liveproto.New(j.ctx, c.render(j.page), min(v, c.app.liveMax)), j.ctx
	return c.s.Join(ref), 0
}

// navigate moves the page to m.URL over the connection: the URL's route runs
// as for a join (X-Vuka-Live: navigate), and the page it renders becomes the
// session's — a patch keeps the components it renders where the page did,
// and its Live page's Update takes the URL's params. The reply carries a
// token for the new page, which a reconnect joins. A URL whose route answers
// anything but a page (a redirect, an error, JSON) is the browser's to load:
// the reply is a redirect.
func (c *conn) navigate(ctx context.Context, m live.ClientMessage) any {
	uri, _, _ := strings.Cut(m.URL, "#")
	load := live.ServerMessage{Type: live.Redirect, Ref: m.Ref, URL: m.URL}
	if !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") || strings.HasPrefix(uri, "/\\") {
		return load
	}
	j, rec, err := c.run(ctx, uri, "navigate")
	if err != nil {
		return load
	}
	if j.page == nil {
		load.URL = cmp.Or(rec.location(), m.URL)
		return load
	}
	reply, ok := c.s.Navigate(m.Ref, uri, live.Route{Ctx: j.ctx, Render: c.render(j.page), Updates: j.updates}, m.Patch)
	if !ok {
		return reply
	}
	c.uri, c.page = uri, j.ctx
	return withToken{reply, c.app.signToken(uri)}
}

// run runs the route of uri on behalf of the connection: a GET carrying the
// upgrade request's headers, X-Vuka-Live set to why. j.page is the page it
// rendered, nil when it answered something else (rec has its status and
// headers).
func (c *conn) run(ctx context.Context, uri, why string) (*join, *recorder, error) {
	u, err := url.ParseRequestURI(uri)
	if err != nil {
		return nil, nil, err
	}
	j := &join{}
	req := c.r.Clone(context.WithValue(ctx, joinKey{}, j))
	req.Method, req.URL, req.RequestURI = http.MethodGet, u, uri
	req.Body, req.ContentLength = http.NoBody, 0
	for _, h := range upgradeHeaders {
		req.Header.Del(h)
	}
	req.Header.Set("X-Vuka-Live", why)
	rec := &recorder{h: http.Header{}}
	c.app.handler.ServeHTTP(rec, req)
	return j, rec, nil
}

// upgradeHeaders are the upgrade request's own headers, which a run of a
// route for the connection doesn't carry.
var upgradeHeaders = []string{"Upgrade", "Connection", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol"}

// withToken is a navigation's reply with the token of the page it moved to.
type withToken struct {
	msg   any
	token string
}

func (w withToken) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	if err := e.Encode(w.msg); err != nil {
		return nil, err
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(b) < 2 || b[len(b)-1] != '}' {
		return b, nil
	}
	t, _ := json.Marshal(w.token)
	return append(append(append(b[:len(b)-1:len(b)-1], `,"token":`...), t...), '}'), nil
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
		end := noop
		if c.tb != nil {
			end = c.recordInfo(d)
		}
		if m := c.s.Info(d.topic, d.msg); m != nil {
			c.write(ctx, m)
		}
		end()
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

// record starts recording a client message for the development toolbar:
// what it ran (statements, spans, logs) and its reply, ended by the
// function returned.
func (c *conn) record(m live.ClientMessage, data []byte) func() {
	typ, subject := m.Type, c.uri
	switch m.Type {
	case live.Join:
	case live.Navigate:
		subject = m.URL
		if m.Patch {
			typ = "patch"
		}
	default:
		subject = m.Target
		if h, ok := c.s.(interface{ Handler(string) any }); ok {
			subject = cmp.Or(handlerName(h.Handler(m.Target)), m.Target)
		}
		if m.Event.Type != "" {
			subject += " (" + m.Event.Type + ")"
		}
	}
	return c.recording(typ, subject, m.Ref, c.wire.maskClient(data))
}

// recordInfo starts recording a broadcast's delivery.
func (c *conn) recordInfo(d delivery) func() {
	return c.recording("broadcast", fmt.Sprintf("%s %T", cmp.Or(d.topic, `""`), d.msg), 0, nil)
}

func (c *conn) recording(typ, subject string, ref int, client []byte) func() {
	m := c.tb.message(typ, subject, ref, client)
	unbind := bindGoroutine(c.tb)
	end := m.span(typ, subject) // the message's whole handling, what it ran inside
	c.frames = nil
	return func() {
		end()
		unbind()
		frames := c.frames
		c.frames = nil
		c.tb.done(m, frames)
	}
}

// instanceType names an instance's component for the toolbar, from its
// first handler when the session says (Counter.Inc: Counter).
func (c *conn) instanceType(id string) string {
	if id == "c0" {
		return "page"
	}
	h, ok := c.s.(interface{ Handler(string) any })
	if !ok {
		return ""
	}
	t, _, _ := strings.Cut(handlerName(h.Handler(id+":0")), ".")
	return t
}

// recorder is the response of a join's run of the route: only its status and
// headers matter.
type recorder struct {
	h      http.Header
	status int
}

func (r *recorder) Header() http.Header { return r.h }

// location is where a redirect answer sends the browser; "" for another answer.
func (r *recorder) location() string {
	if r.status >= 300 && r.status < 400 {
		return r.h.Get("Location")
	}
	return ""
}
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
