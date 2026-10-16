package web

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/ui/live"
	"github.com/vuka-lang/web/internal/liveproto"
)

// Close codes of a live connection the runtime doesn't retry: 4001 asks it to
// reload the page (an expired or foreign token), 4004 says the page can't be
// live (its route failed or isn't a page).
// 4002 says the page sat idle (LiveIdleTimeout): the runtime connects
// again once the page is shown.
const (
	closeReload  websocket.StatusCode = 4001
	closeIdle    websocket.StatusCode = 4002
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

// The live connections' limits by default (see the options below).
const (
	defaultLiveJoinTimeout    = 10 * time.Second
	defaultLiveIdleTimeout    = 30 * time.Minute
	defaultLiveMaxConns       = 10000
	defaultLiveMaxConnsPerIP  = 100
	defaultLiveMaxConnsPerTok = 4
	defaultLiveCheckEvery     = 30 * time.Second
)

// LiveJoinTimeout is how long a live connection may stay open without
// joining its page: 10 seconds by default. A socket that sends nothing,
// or anything but its join, is closed when it runs out.
func LiveJoinTimeout(d time.Duration) Option {
	return OptionFunc(func(a *App) { a.hub.joinTimeout = d })
}

// LiveIdleTimeout is how long a joined page may send nothing before its
// connection closes and its session, with its components' state, is
// dropped: 30 minutes by default; 0 never closes one. The runtime
// connects again, rebuilding the page's session, at once when the page is
// in view and otherwise when it is shown again; broadcasts don't keep a
// page from being idle.
func LiveIdleTimeout(d time.Duration) Option {
	return OptionFunc(func(a *App) { a.hub.idleTimeout = d })
}

// LiveMaxConns caps the app's live connections: 10,000 by default. A
// socket beyond it is refused with 503 Service Unavailable before it
// opens, and the runtime tries again later; 0 sets no cap.
func LiveMaxConns(n int) Option { return OptionFunc(func(a *App) { a.hub.maxConns = n }) }

// LiveMaxConnsPerIP caps the live connections from one client address:
// 100 by default (a building behind one address has many visitors). A
// socket beyond it is refused with 429 Too Many Requests; 0 sets no cap.
func LiveMaxConnsPerIP(n int) Option { return OptionFunc(func(a *App) { a.hub.maxPerIP = n }) }

// LiveMaxConnsPerPage caps the live connections open with the same page
// token, one rendered page's: 4 by default, room for a reconnect
// overlapping its old socket. A socket beyond it is refused with 429; 0
// sets no cap.
func LiveMaxConnsPerPage(n int) Option { return OptionFunc(func(a *App) { a.hub.maxPerTok = n }) }

// LiveCheckEvery runs the app's LiveChecks on each joined page this often:
// 30 seconds by default; 0 never runs them. A page some check fails closes
// with code 4001 and reloads, so a session ended where the app doesn't
// hear of it (another process, a store edited by hand) or a revoked
// permission reaches pages that sit open; an ending the app hears of
// closes them at once (CloseLive).
func LiveCheckEvery(d time.Duration) Option {
	return OptionFunc(func(a *App) { a.hub.checkEvery = d })
}

// hub is the app's live connections, and the topics each subscribes to.
type hub struct {
	mu       sync.Mutex
	conns    map[*conn]bool
	topics   map[string]map[*conn]bool
	closed   bool
	open     int            // the sockets admitted, open or opening
	perIP    map[string]int // of them, by client address
	perToken map[string]int // by page token

	joinTimeout, idleTimeout, checkEvery time.Duration
	maxConns, maxPerIP, maxPerTok        int
}

func newHub() *hub {
	return &hub{conns: map[*conn]bool{}, topics: map[string]map[*conn]bool{}, perIP: map[string]int{}, perToken: map[string]int{},
		joinTimeout: defaultLiveJoinTimeout, idleTimeout: defaultLiveIdleTimeout, checkEvery: defaultLiveCheckEvery,
		maxConns: defaultLiveMaxConns, maxPerIP: defaultLiveMaxConnsPerIP, maxPerTok: defaultLiveMaxConnsPerTok}
}

// admit counts a socket from ip with the page token tok against the caps:
// the status refusing it, else 0 and the function releasing its place.
func (h *hub) admit(ip, tok string) (int, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.maxConns > 0 && h.open >= h.maxConns:
		return http.StatusServiceUnavailable, nil
	case h.maxPerIP > 0 && h.perIP[ip] >= h.maxPerIP, h.maxPerTok > 0 && h.perToken[tok] >= h.maxPerTok:
		return http.StatusTooManyRequests, nil
	}
	h.open++
	h.perIP[ip]++
	h.perToken[tok]++
	return 0, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.open--
		if h.perIP[ip]--; h.perIP[ip] <= 0 {
			delete(h.perIP, ip)
		}
		if h.perToken[tok]--; h.perToken[tok] <= 0 {
			delete(h.perToken, tok)
		}
	}
}

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
	tok    string        // the page's join token, the key its render parked a session under
	s      liveproto.Session
	page   context.Context                 // the context the page's route ran with: the join's, else the latest navigation's
	topics []string                        // guarded by the hub's mutex
	tb     *tbSink                         // the development toolbar's record of the page, its messages under it
	wire   *wireMirror                     // with tb, what the browser holds, to decode the frames sent
	frames []tbFrame                       // with tb, the frames the message being recorded sent
	ev     *LiveEvent                      // the event being handled, until its reply
	doing  string                          // what the session runs, for its errors' log: a handler's name, "join", …
	shared atomic.Pointer[context.Context] // page, for LiveOrigin on other goroutines

	loads context.CancelFunc // ends the deferred loads of the page joined or navigated to

	mu     sync.Mutex
	inbox  []delivery
	wake   chan struct{}
	ending atomic.Bool // CloseLive matched the page: it closes before anything else
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
	why     string      // "join" or "navigate"
	done    atomic.Bool // the route's run returned (IsLiveJoin)
	page    ui.Node
	ctx     context.Context
	updates map[ui.Stateful]any // a Live page's URLParams for its Update, on a patch
}

func (a *App) serveLive(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("t")
	uri, terr := a.verifyToken(tok)
	if terr != nil {
		tok = "" // a bad token's socket closes at once; it counts as the client's only
	}
	status, release := a.hub.admit(ClientIP(r), tok)
	if status != 0 {
		w.Header().Set("Retry-After", "10")
		http.Error(w, http.StatusText(status), status)
		return
	}
	defer release()
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
	c := &conn{app: a, ws: ws, r: r, uri: uri, tok: tok, wake: make(chan struct{}, 1)}
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
	defer bindConn(c)()
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
	// deadline: the join's, then the page's idle one
	deadline := time.NewTimer(cmp.Or(a.hub.joinTimeout, defaultLiveJoinTimeout))
	defer deadline.Stop()
	var checks <-chan time.Time
	if d := a.hub.checkEvery; d > 0 && len(a.liveChecks) > 0 {
		t := time.NewTicker(d)
		defer t.Stop()
		checks = t.C
	}
	for {
		select {
		case <-ctx.Done():
			ws.CloseNow()
			return
		case data := <-msgs:
			if c.ending.Load() {
				ws.Close(closeReload, "live: the page must load again")
				return
			}
			if !c.handle(ctx, data) {
				return
			}
			if c.s != nil {
				if !deadline.Stop() {
					select {
					case <-deadline.C:
					default:
					}
				}
				if d := a.hub.idleTimeout; d > 0 {
					deadline.Reset(d)
				}
			}
		case <-deadline.C:
			if c.s == nil {
				ws.Close(websocket.StatusPolicyViolation, "live: no join in time")
			} else {
				ws.Close(closeIdle, "live: the page sat idle")
			}
			return
		case <-checks:
			if c.s != nil && !c.check() {
				ws.Close(closeReload, "live: the page must load again")
				return
			}
		case <-c.wake:
			if c.ending.Load() {
				ws.Close(closeReload, "live: the page must load again")
				return
			}
			if !c.drain(ctx) {
				return
			}
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
		c.startDeferred()
	case c.s == nil:
		c.write(ctx, live.ServerMessage{Type: live.Error, Ref: m.Ref, Error: "live: join first"})
	case m.Type == live.Navigate:
		reply, patch := c.navigate(ctx, m.ClientMessage)
		_, moved := reply.(withToken)
		if moved && c.tb != nil {
			// before the reply: the toolbar asks for the page once it lands
			c.tb.navigated(c.tb.cur.Load(), c.uri, patch, c.r.Header)
		}
		c.write(ctx, reply)
		if moved {
			c.startDeferred()
		}
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

// setPage makes ctx the page's context: its events' and LiveOrigin's.
func (c *conn) setPage(ctx context.Context) {
	c.page = ctx
	c.shared.Store(&ctx)
}

// pageCtx is the page's context, from any goroutine.
func (c *conn) pageCtx() context.Context {
	if p := c.shared.Load(); p != nil {
		return *p
	}
	return nil
}

// event runs an event's handler on this goroutine, the CurrentLiveEvent
// while it does, once the app's GuardLiveEvents let it; an event a guard or
// a decorator refused replies with its error.
func (c *conn) event(m live.ClientMessage) any {
	ev := &LiveEvent{ctx: c.page}
	var fn any
	if h, ok := c.s.(interface{ Handler(string) any }); ok {
		fn = h.Handler(m.Target)
		ev.handler = handlerName(fn)
	}
	c.doing = cmp.Or(ev.handler, m.Target)
	if len(c.app.liveGuards) > 0 && fn != nil {
		h := c.liveHandler(m.Target, fn, ev.handler)
		for _, g := range c.app.liveGuards {
			if err := g(c.page, h); err != nil {
				return live.ServerMessage{Type: live.PatchMsg, Ref: m.Ref, Error: c.errorText(err)}
			}
		}
	}
	c.ev = ev
	reply := c.s.Event(m)
	c.ev = nil
	if ev.refused != nil {
		return withError{reply, c.errorText(ev.refused)}
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
// version v when the app can (else v1). When the GET's own session is still
// parked under the token and the same identity joins in time, it is adopted
// instead of mounting the page's components again (livepark.go).
func (c *conn) join(ctx context.Context, ref, v int) (any, websocket.StatusCode) {
	j, rec, err := c.run(ctx, c.uri, "join")
	if err != nil {
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: errBadToken.Error()}, closeReload
	}
	if j.page == nil {
		if loc := rec.location(); loc != "" {
			if !ui.LoadableURL(loc) {
				return live.ServerMessage{Type: live.Error, Ref: ref, Error: errRedirectScheme.Error()}, closeRefused
			}
			return live.ServerMessage{Type: live.Redirect, Ref: ref, URL: loc}, websocket.StatusNormalClosure
		}
		status := cmp.Or(rec.status, http.StatusOK)
		msg := fmt.Sprintf("live: GET %s answered %d %s", c.uri, status, http.StatusText(status))
		if status == http.StatusOK {
			msg = fmt.Sprintf("live: GET %s is not a live page", c.uri)
		}
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: msg}, closeRefused
	}
	if s := c.adopt(j, v); s != nil {
		c.s = s
	} else {
		c.s = liveproto.New(j.ctx, c.render(j.page), min(v, c.app.liveMax))
	}
	if e, ok := c.s.(interface{ SetErrorText(func(error) string) }); ok {
		e.SetErrorText(c.errorText)
	}
	c.setPage(j.ctx)
	c.doing = "join"
	return c.s.Join(ref), 0
}

// adopt is the session the page's HTTP render parked, when this join may
// take it (livepark.go): parked under this very token (so once, and in
// time), the same URI, the same identity as the join's own run of the
// route (j.ctx, whose gates passed already), and the native live session
// about to serve it. Anything else returns nil and the join mounts fresh —
// adoption is an optimization, never a dependency. The route still ran:
// only its render's components are kept instead of mounting again, with
// the join's context and render from here on.
func (c *conn) adopt(j *join, v int) liveproto.Session {
	a := c.app
	if a.adoptWindow <= 0 {
		return nil
	}
	p := a.parked.take(c.tok)
	if p == nil {
		return nil
	}
	ver := min(v, a.liveMax)
	if ver >= 2 && liveproto.V2 != nil { // a stand-in session can't adopt
		p.close()
		return nil
	}
	if p.uri != c.uri || p.who != a.liveWho(j.ctx) {
		p.close()
		return nil
	}
	s, err := liveproto.Adopt(p.s, j.ctx, c.render(j.page), ver)
	if err != nil {
		p.close()
		return nil
	}
	return s
}

// errRedirectScheme refuses a redirect the browser would run in the page.
var errRedirectScheme = errors.New("live: refused to redirect to a URL that isn't http or https")

// errorText is the text the browser gets for an error of the page's
// handlers, Mount or Update methods, renders or guards (liveErrorText); an
// error it hides is logged, with the page and what it was running.
func (c *conn) errorText(err error) string {
	text, public := c.app.liveErrorText(err)
	if !public {
		ctx := c.pageCtx()
		if ctx == nil {
			ctx = c.r.Context()
		}
		slog.ErrorContext(ctx, "web: live page", "page", c.uri, "handler", c.doing, "err", err)
	}
	return text
}

// liveErrorText is the text of a live page's error for the browser, and
// whether it is the error's own message: in development the error's text,
// as an HTTP error's; else a *web.Error's message, or another error the
// browser may read (live.PublicError, such as a form field that doesn't
// parse), and for any other "Internal Server Error", the error's text
// going to the log instead.
func (a *App) liveErrorText(err error) (string, bool) {
	if m, ok := live.PublicMessage(err); ok {
		return m, true
	}
	if a.dev {
		return err.Error(), false
	}
	return http.StatusText(http.StatusInternalServerError), false
}

// navigate moves the page to m.URL over the connection: the URL's route runs
// as for a join (X-Vuka-Live: navigate), and the page it renders becomes the
// session's. The server decides whether it is a patch, the client's patch
// flag only asking for one: the page's component stays mounted only when
// the URL's route is a Live page whose type has an Update taking
// web.URLParams, and that Update takes the URL's params; otherwise every
// component mounts afresh, Mount seeing the new URL, even on the same
// route. The reply carries a token for the new page, which a reconnect
// joins. A URL whose route answers anything but a page (a redirect, an
// error, JSON) is the browser's to load: the reply is a redirect, refused
// (an error, which the runtime loads the URL on) when it names a URL that
// isn't http or https. A navigation whose Mount, Update or render fails
// replies with the error and leaves nothing mounted; the runtime loads the
// URL, getting the route's own answer.
func (c *conn) navigate(ctx context.Context, m live.ClientMessage) (any, bool) {
	uri, _, _ := strings.Cut(m.URL, "#")
	load := live.ServerMessage{Type: live.Redirect, Ref: m.Ref, URL: m.URL}
	redirect := func() live.ServerMessage {
		if !ui.LoadableURL(load.URL) {
			return live.ServerMessage{Type: live.Error, Ref: m.Ref, Error: errRedirectScheme.Error()}
		}
		return load
	}
	if !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") || strings.HasPrefix(uri, "/\\") {
		return redirect(), false
	}
	j, rec, err := c.run(ctx, uri, "navigate")
	if err != nil {
		return redirect(), false
	}
	if j.page == nil {
		load.URL = cmp.Or(rec.location(), m.URL)
		return redirect(), false
	}
	patch := m.Patch && j.updates != nil
	c.doing = "navigate " + uri
	reply, ok := c.s.Navigate(m.Ref, uri, live.Route{Ctx: j.ctx, Render: c.render(j.page), Updates: j.updates}, patch)
	if !ok {
		return reply, patch
	}
	c.uri = uri
	c.setPage(j.ctx)
	return withToken{reply, c.app.signToken(uri)}, patch
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
	j := &join{why: why}
	defer j.done.Store(true)
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

// deferredSession is a session whose pages backed by their islands have
// deferred fields (live.Session.Deferred).
type deferredSession interface {
	Deferred() []*ui.DeferredLoad
	Loaded(d *ui.DeferredLoad) any
}

// loaded is a deferred load done, waiting in the inbox to be applied.
type loaded struct{ d *ui.DeferredLoad }

// startDeferred runs the deferred loads of the page just joined or
// navigated to, each on its own goroutine with the page's context, ended
// when the page is left; each, done, is applied as a broadcast is.
func (c *conn) startDeferred() {
	ds, ok := c.s.(deferredSession)
	if !ok {
		return
	}
	loads := ds.Deferred()
	if c.loads != nil {
		c.loads()
		c.loads = nil
	}
	if len(loads) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(c.page)
	c.loads = cancel
	for _, d := range loads {
		go func() {
			d.Run(ctx)
			if ctx.Err() == nil {
				c.post("", loaded{d})
			}
		}()
	}
}

// drain delivers the broadcasts waiting for the page; before a join there is
// nothing to deliver them to, and the join renders the page fresh.
func (c *conn) drain(ctx context.Context) bool {
	c.mu.Lock()
	batch := c.inbox
	c.inbox = nil
	c.mu.Unlock()
	if c.s == nil || len(batch) == 0 {
		return true
	}
	for _, d := range batch {
		if l, ok := d.msg.(loaded); ok {
			ds, _ := c.s.(deferredSession)
			if ds == nil {
				continue
			}
			c.doing = "load " + l.d.Group
			end := noop
			if c.tb != nil {
				end = c.recording("load", l.d.Group, 0, nil)
			}
			if m := ds.Loaded(l.d); m != nil {
				c.write(ctx, m)
			}
			end()
			continue
		}
		c.doing = "broadcast " + cmp.Or(d.topic, `""`)
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
	return true
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
