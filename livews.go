package web

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/web/v2/internal/liveproto"
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

// LiveCheckEvery runs the app's LiveChecks on each joined page this often: 30
// seconds by default; 0 never runs them. A connection's first check comes at a
// random point of the period, so pages opened together don't all check
// together. A page some check fails closes
// with code 4001 and reloads, so a session ended where the app doesn't
// hear of it (another process, a store edited by hand) or a revoked
// permission reaches pages that sit open; an ending the app hears of
// closes them at once (CloseLive).
func LiveCheckEvery(d time.Duration) Option {
	return OptionFunc(func(a *App) { a.hub.checkEvery = d })
}

// LiveTrimAfter makes a joined page that sends and receives nothing for d
// release what its session can rebuild, to make it again on its next event or
// broadcast. The session's render trees, and the tree the browser holds, are
// stored once for every page that shows the same markup, so a page has little
// of its own left to release and this saves little; it is kept for the rare
// page whose browser tree is not its last render. It is off by default (0).
// Replies don't change, and the page stays open; LiveIdleTimeout is what ends
// it.
func LiveTrimAfter(d time.Duration) Option {
	return OptionFunc(func(a *App) { a.hub.trimAfter = d })
}

// LiveCompressionMode is how live sockets compress their messages
// (permessage-deflate, RFC 7692), set with LiveCompression.
type LiveCompressionMode int

const (
	// NoContextTakeover, the default, compresses each message on its own:
	// a compressor is borrowed from a pool for the write and given back,
	// so an open socket holds none. The browser does the same for its
	// messages.
	NoContextTakeover LiveCompressionMode = iota
	// ContextTakeover keeps each socket's compressor and its 32 KB window
	// between messages, so a message can refer to the ones before it: a
	// few percent fewer bytes on a live page's session (2% on a 200-row
	// list), for about 800 KB more held by every open socket (8 GB at
	// LiveMaxConns' 10,000; NoContextTakeover adds about 2 KB).
	ContextTakeover
)

// Off turns a compression option off: LiveCompression(Off) sends live
// messages as they are.
const Off = -1

// liveDeflateThreshold is the smallest message live sockets compress: a
// shorter one (a small patch, an ack) gains less than the deflate
// framing costs.
const liveDeflateThreshold = 256

// LiveCompression sets how live sockets compress their messages:
// NoContextTakeover by default, ContextTakeover, or Off. Compression is
// negotiated with each client when its socket opens (every current
// browser offers it); one that doesn't offer it gets messages as they
// are. Messages under 256 bytes are sent as they are.
func LiveCompression(m LiveCompressionMode) Option {
	return OptionFunc(func(a *App) { a.liveDeflate = m })
}

func (m LiveCompressionMode) mode() websocket.CompressionMode {
	switch m {
	case NoContextTakeover:
		return websocket.CompressionNoContextTakeover
	case ContextTakeover:
		return websocket.CompressionContextTakeover
	}
	return websocket.CompressionDisabled
}

// hubShards is how many parts the hub's tables split into, a power of two.
const hubShards = 64

// hub is the app's live connections, and the topics each subscribes to. Its
// tables are split by hash into shards, each under its own lock: a topic's
// subscribers live in the shard of the topic, a connection in the shard of
// its id, the sockets of an address in the shard of the address and those of
// a page token in the shard of the token. A broadcast takes one lock.
type hub struct {
	shards   [hubShards]hubShard
	seed     maphash.Seed
	nextID   atomic.Uint64
	open     atomic.Int64 // the sockets admitted, open or opening
	closed   atomic.Bool
	streams  sync.Map                   // the event-stream connections, by id (livesse.go)
	onShared func(name string, msg any) // a broadcast to a shared component, by its type's name

	joinTimeout, idleTimeout, checkEvery, trimAfter time.Duration
	maxConns, maxPerIP, maxPerTok                   int
}

type hubShard struct {
	mu       sync.Mutex
	conns    map[*conn]bool
	topics   map[string]map[*conn]bool
	perIP    map[string]int
	perToken map[string]int
}

func newHub() *hub {
	h := &hub{seed: maphash.MakeSeed(),
		joinTimeout: defaultLiveJoinTimeout, idleTimeout: defaultLiveIdleTimeout, checkEvery: defaultLiveCheckEvery,
		maxConns: defaultLiveMaxConns, maxPerIP: defaultLiveMaxConnsPerIP, maxPerTok: defaultLiveMaxConnsPerTok}
	for i := range h.shards {
		h.shards[i] = hubShard{conns: map[*conn]bool{}, topics: map[string]map[*conn]bool{}, perIP: map[string]int{}, perToken: map[string]int{}}
	}
	return h
}

// shard is the shard of the key s (a topic, an address, a token).
func (h *hub) shard(s string) *hubShard {
	return &h.shards[maphash.String(h.seed, s)&(hubShards-1)]
}

// connShard is the shard c is registered in.
func (h *hub) connShard(c *conn) *hubShard { return &h.shards[c.id&(hubShards-1)] }

// admit counts a socket from ip with the page token tok against the caps:
// the status refusing it, else 0 and the function releasing its place.
func (h *hub) admit(ip, tok string) (int, func()) {
	if n := h.open.Add(1); h.maxConns > 0 && n > int64(h.maxConns) {
		h.open.Add(-1)
		return http.StatusServiceUnavailable, nil
	}
	ips, toks := h.shard(ip), h.shard(tok)
	ips.mu.Lock()
	if h.maxPerIP > 0 && ips.perIP[ip] >= h.maxPerIP {
		ips.mu.Unlock()
		h.open.Add(-1)
		return http.StatusTooManyRequests, nil
	}
	ips.perIP[ip]++
	ips.mu.Unlock()
	toks.mu.Lock()
	if h.maxPerTok > 0 && toks.perToken[tok] >= h.maxPerTok {
		toks.mu.Unlock()
		ips.release(ip, "")
		h.open.Add(-1)
		return http.StatusTooManyRequests, nil
	}
	toks.perToken[tok]++
	toks.mu.Unlock()
	return 0, func() {
		ips.release(ip, "")
		toks.release("", tok)
		h.open.Add(-1)
	}
}

// release gives back a place counted for ip or tok (the other "").
func (sh *hubShard) release(ip, tok string) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if ip != "" {
		if sh.perIP[ip]--; sh.perIP[ip] <= 0 {
			delete(sh.perIP, ip)
		}
	}
	if tok != "" {
		if sh.perToken[tok]--; sh.perToken[tok] <= 0 {
			delete(sh.perToken, tok)
		}
	}
}

func (h *hub) add(c *conn) bool {
	c.id = h.nextID.Add(1)
	sh := h.connShard(c)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if h.closed.Load() {
		return false
	}
	sh.conns[c] = true
	return true
}

func (h *hub) remove(c *conn) {
	h.setTopics(c, nil)
	sh := h.connShard(c)
	sh.mu.Lock()
	delete(sh.conns, c)
	sh.mu.Unlock()
}

// each calls f for every connection, one shard at a time with its lock
// released, so f may take locks of its own.
func (h *hub) each(f func(*conn)) {
	for i := range h.shards {
		sh := &h.shards[i]
		sh.mu.Lock()
		cs := make([]*conn, 0, len(sh.conns))
		for c := range sh.conns {
			cs = append(cs, c)
		}
		sh.mu.Unlock()
		for _, c := range cs {
			f(c)
		}
	}
}

// setTopics makes ts the topics c is subscribed to. Only c's own goroutine
// calls it, so c.topics needs no lock.
func (h *hub) setTopics(c *conn, ts []string) {
	for _, t := range c.topics {
		if slices.Contains(ts, t) {
			continue
		}
		sh := h.shard(t)
		sh.mu.Lock()
		if delete(sh.topics[t], c); len(sh.topics[t]) == 0 {
			delete(sh.topics, t)
		}
		sh.mu.Unlock()
	}
	for _, t := range ts {
		if slices.Contains(c.topics, t) {
			continue
		}
		sh := h.shard(t)
		sh.mu.Lock()
		if sh.topics[t] == nil {
			sh.topics[t] = map[*conn]bool{}
		}
		sh.topics[t][c] = true
		sh.mu.Unlock()
	}
	c.topics = ts
}

func (h *hub) deliver(topic string, msg any) {
	if name, ok := strings.CutPrefix(topic, sharedTopicPrefix); ok {
		if h.onShared != nil {
			h.onShared(name, msg)
		}
		return
	}
	if topic == "" {
		h.each(func(c *conn) { c.post(topic, msg) })
		return
	}
	sh := h.shard(topic)
	sh.mu.Lock()
	to := make([]*conn, 0, len(sh.topics[topic]))
	for c := range sh.topics[topic] {
		to = append(to, c)
	}
	sh.mu.Unlock()
	for _, c := range to {
		c.post(topic, msg)
	}
}

// closeAll closes every connection (going away: the runtime reconnects) and
// refuses new ones.
func (h *hub) closeAll() {
	h.closed.Store(true)
	var conns []*conn
	h.each(func(c *conn) { conns = append(conns, c) })
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Go(func() {
			c.noteClose(websocket.StatusGoingAway, "the server is shutting down", "the app is stopping")
			c.ws.Close(websocket.StatusGoingAway, "the server is shutting down")
		})
	}
	wg.Wait()
}

// conn is one connected page: its socket, its session once joined, and the
// broadcasts waiting for it. One goroutine runs its session, so events and
// broadcasts are answered in order.
type conn struct {
	app    *App
	ws     liveTransport // its socket, or its event stream (livesse.go)
	r      *http.Request // the upgrade request
	uri    string        // the page, from its token
	tok    string        // the page's join token, the key its render parked a session under
	s      liveproto.Session
	page   context.Context                 // the context the page's route ran with: the join's, else the latest navigation's
	root   reflect.Type                    // the page's root layout, nil when it has none
	topics []string                        // what the hub has it subscribed to, set by its own goroutine
	id     uint64                          // the hub's number for it
	tb     *tbSink                         // the development toolbar's record of the page, its messages under it
	wire   *wireMirror                     // with tb, what the browser holds, to decode the frames sent
	frames []tbFrame                       // with tb, the frames the message being recorded sent
	ev     *LiveEvent                      // the event being handled, until its reply
	doing  string                          // what the session runs, for its errors' log: a handler's name, "join", …
	shared atomic.Pointer[context.Context] // page, for LiveOrigin on other goroutines

	loads     []context.CancelFunc     // end the deferred loads of the page joined or navigated to
	selfURL   atomic.Pointer[string]   // the address the page's own event moved it to, the next navigate's (liveself.go)
	urlSnap   atomic.Pointer[urlSnap]  // the address and the parameters its components keep in it, for a patch that only moves those (liveurlonly.go)
	updatable atomic.Bool              // the page shown has an Update(URLParams): only then can a patch keep it
	leave     atomic.Pointer[leaveCtx] // the page's context, whose renders leaveEarly can end
	leaving   atomic.Int32             // navigates leaving the page read and not yet served
	readMu    sync.Mutex               // guards reading against leaveEarly
	reading   bool                     // the loop renders after a handler, or applies a deferred load: a read
	answering bool                     // an event's or a broadcast's handler runs; the render after it is a read
	vars      *varHistory              // the page's context processors' results, from one page to the next

	layouts []heldLayout         // the layouts the page renders in, loaded (loadLayouts)
	navs    int                  // navigations so far
	slot    int                  // the navigation the page below the layouts was rendered for (slotKey)
	ahead   map[string]*aheadNav // pages mounted ahead of a click, by URL (liveprefetch.go)
	aheadTB *tbEntry             // the toolbar record of a used prefetch's run, for navigated

	mu     sync.Mutex
	inbox  []delivery
	wake   chan struct{}
	ending atomic.Bool // CloseLive matched the page: it closes before anything else
	out    outbox      // the frames waiting for its socket

	smu           sync.Mutex // the shared components it shows: ordering of their frames in out
	sh            map[*sharedRun]*sharedSt
	known         []uint64    // the shared statics ids its browser holds, as a bitset
	pendingShared atomic.Bool // a shared component was attached by a render whose message isn't queued yet
	overflowing   atomic.Bool
	closing       atomic.Bool // closeConn ran
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
	route   string              // a Live page's route, "/orders/{id}"
	where   string              // and its page, "live page main.Order"
	root    reflect.Type        // the route's root layout (RouteInfo.RootLayout), nil when it has none
	held    []heldLayout        // on a navigation, the layouts of the page it leaves
	layouts []heldLayout        // the layouts the route loaded, for the next navigation
	slot    *int                // the connection's navigation the page is rendered for, keying the page below its layouts
	nav     int                 // on a navigation, the connection's count of them, this one's
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
	defer func() {
		if release != nil {
			release()
		}
	}()
	var ws liveTransport
	parent := r.Context()
	stream := r.URL.Query().Get("transport") == "sse"
	if stream {
		s, sr := a.acceptStream(w, r) // livesse.go
		if s == nil {
			return
		}
		ws, r = s, sr
		parent = r.Context()
	} else {
		wc, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: a.liveOrigins, CompressionMode: a.liveDeflate.mode(), CompressionThreshold: liveDeflateThreshold})
		if err != nil {
			return // Accept answered: 403 for another origin
		}
		wc.SetReadLimit(1 << 20)
		ws = wc
	}
	if terr != nil {
		if a.tb != nil {
			a.tb.refusedConn(r.URL.Query().Get("tb"), closeReload, terr.Error())
		}
		ws.Close(closeReload, terr.Error())
		return
	}
	rel := release
	release = nil
	if stream {
		a.liveLoop(parent, ws, liveRequest(r, parent), uri, tok, rel, false)
		return
	}
	// The socket is the connection's now: the handler returns, and with it
	// the upgrade request, its response and the server's state for it go, as
	// the session keeps only liveRequest's trimmed copy. Its context ends
	// with the socket, not with the handler.
	parent = context.WithoutCancel(parent)
	go a.liveLoop(parent, ws, liveRequest(r, parent), uri, tok, rel, true)
}

// liveLoop is a live connection's session: the loop that serves its socket
// until it closes. r is the request it was made with (liveRequest); parent
// the context its own derives from. When detached, it runs on a goroutine of
// its own, after the upgrade handler has returned.
func (a *App) liveLoop(parent context.Context, ws liveTransport, r *http.Request, uri, tok string, release func(), detached bool) {
	defer release()
	defer ws.CloseNow()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer a.recoverSocket(ws, r, detached)
	c := &conn{app: a, ws: ws, r: r, uri: uri, tok: tok, wake: make(chan struct{}, 1), out: outbox{space: make(chan struct{}, 1)}}
	if a.tb != nil {
		if c.tb = a.tb.liveSink(r.URL.Query().Get("tb"), uri); c.tb != nil {
			ctx = context.WithValue(ctx, tbKey{}, c.tb)
			c.wire = newWireMirror(c.instanceType)
		}
	}
	c.noteOpen()
	if !a.hub.add(c) {
		c.noteClose(websocket.StatusGoingAway, "the server is shutting down", "the app is stopping")
		ws.Close(websocket.StatusGoingAway, "the server is shutting down")
		return
	}
	defer a.hub.remove(c)
	defer bindConn(c)()
	defer func() {
		c.clearAhead()
		if c.s != nil {
			c.s.Close()
		}
		c.dropShared()
	}()
	// The reader keeps reading while the loop is busy, so a navigate that
	// leaves the page is seen behind the events queued before it.
	msgs := make(chan clientMsg, liveQueue)
	go func() {
		defer cancel()
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				c.noteDrop(err)
				return
			}
			leaves := c.leaveEarly(data)
			select {
			case msgs <- clientMsg{data, leaves}:
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
	var checkTimer *time.Timer
	if d := a.hub.checkEvery; d > 0 && len(a.liveChecks) > 0 {
		checkTimer = time.NewTimer(firstCheck(d))
		defer checkTimer.Stop()
		checks = checkTimer.C
	}
	var trim <-chan time.Time
	active := func() {}
	if d := a.hub.trimAfter; d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		trim, active = t.C, func() { t.Reset(d) }
	}
	for {
		select {
		case <-ctx.Done():
			ws.CloseNow()
			return
		case <-trim:
			if s, ok := c.s.(interface{ Trim() }); ok {
				s.Trim()
			}
		case msg := <-msgs:
			active()
			if msg.leaves {
				c.leaving.Add(-1) // served now: the renders before it are behind it
			}
			if c.ending.Load() {
				c.noteClose(closeReload, "live: the page must load again", "CloseLive closed the page")
				c.closeConn(closeReload, "live: the page must load again")
				return
			}
			if !c.handle(ctx, msg.data) || c.out.isDead() {
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
				c.noteClose(websocket.StatusPolicyViolation, "live: no join in time", "the socket sent no join within "+cmp.Or(a.hub.joinTimeout, defaultLiveJoinTimeout).String())
				c.closeConn(websocket.StatusPolicyViolation, "live: no join in time")
			} else {
				c.noteClose(closeIdle, "live: the page sat idle", "no message or broadcast for "+a.hub.idleTimeout.String()+" (LiveIdleTimeout)")
				c.closeConn(closeIdle, "live: the page sat idle")
			}
			return
		case <-checks:
			if c.s != nil && !c.check() {
				c.closeConn(closeReload, "live: the page must load again")
				return
			}
			checkTimer.Reset(a.hub.checkEvery)
		case <-c.wake:
			active()
			if c.ending.Load() {
				c.noteClose(closeReload, "live: the page must load again", "CloseLive closed the page")
				c.closeConn(closeReload, "live: the page must load again")
				return
			}
			if !c.drain(ctx) {
				return
			}
		case <-ping.C:
			go func() {
				pctx, done := context.WithTimeout(ctx, writeTimeout)
				defer done()
				if err := ws.Ping(pctx); err != nil {
					c.noteConn("drop", 0, "the browser stopped answering pings", err.Error())
					if c.closeConn(closeReload, "live: the page must load again") {
						cancel()
					}
				}
			}()
		}
	}
}

// firstCheck is how long a new connection waits for its first LiveCheck: up to
// a whole period, so sockets opened together (every page reconnecting after a
// restart) don't all check together.
func firstCheck(every time.Duration) time.Duration {
	return time.Duration(rand.Int64N(int64(every))) + 1
}

// handle answers a client message; false ends the connection.
func (c *conn) handle(ctx context.Context, data []byte) bool {
	var m struct {
		live.ClientMessage
		V   int      `json:"v,omitempty"`   // the newest protocol version the page speaks (a join's)
		Ext []string `json:"ext,omitempty"` // and the v2 extensions it speaks: "stream"
		// build: the build that rendered the page (a join's), when the runtime says
		Build string `json:"build,omitempty"`
		// loads: the deferred groups whose fallbacks the browser saw
		Groups []string `json:"groups,omitempty"`
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
		c.clearAhead() // made against the session the join replaces
		if c.s != nil {
			c.endLoads() // the page the join replaces is gone with it
			c.s.Close()
			c.s = nil
		}
		reply, code := c.join(ctx, m.Ref, m.V, m.Ext, m.Build)
		if code == 0 && c.varsRefused() {
			return false
		}
		c.syncTopics() // before the reply: a broadcast the client causes once joined reaches it
		c.write(ctx, reply)
		if code != 0 {
			c.noteClose(code, cmp.Or(reply.(live.ServerMessage).Error, "redirected to "+reply.(live.ServerMessage).URL), "the join's run of the page's route")
			c.closeConn(code, reply.(live.ServerMessage).Error)
			return false
		}
		c.startDeferred(false)
	case c.s == nil:
		c.write(ctx, live.ServerMessage{Type: live.Error, Ref: m.Ref, Error: "live: join first"})
	case m.Type == live.Navigate && c.leaving.Load() > 0:
		// a navigate leaving the page waits behind this one: the browser
		// moved on from it, so neither its route nor its render runs
		c.write(ctx, live.ServerMessage{Type: live.PatchMsg, Ref: m.Ref})
	case m.Type == live.Navigate:
		c.vars.newEvent()
		reply, patch := c.navigate(ctx, m.ClientMessage)
		if c.leaving.Load() == 0 {
			c.leave.Load().renew() // a navigation that failed keeps the page
		} else if sm, ok := reply.(live.ServerMessage); ok && sm.Type == live.Error {
			reply = live.ServerMessage{Type: live.PatchMsg, Ref: m.Ref} // ended for one the browser moved on to
		}
		if c.varsRefused() {
			return false
		}
		_, moved := reply.(withToken)
		if moved && c.tb != nil {
			// before the reply: the toolbar asks for the page once it lands.
			// A prefetch ran the route at hover, so fold its record into this
			// navigation's entry, which skipped the run but stands for the move.
			cur := c.tb.cur.Load()
			if c.aheadTB != nil && cur != nil {
				cur.adoptRun(c.aheadTB)
			}
			c.tb.navigated(cur, c.uri, patch, c.r.Header)
		}
		c.aheadTB = nil
		c.syncTopics()
		c.write(ctx, reply)
		if moved {
			c.startDeferred(patch)
		}
	case m.Type == livePrefetch:
		c.prefetch(ctx, m.ClientMessage)
	case m.Type == liveLoads:
		c.startSeen(m.Groups)
	default:
		c.vars.newEvent()
		reply := c.event(m.ClientMessage)
		if c.varsRefused() {
			return false
		}
		c.syncTopics()
		c.write(ctx, reply)
		c.startDeferred(true) // a component the event mounted
	}
	c.syncTopics()
	c.noteURLOwned()
	return true
}

// varsRefused closes the connection when a gate refused a read of the
// page's variables (Auth): the page loads again, its route answering the
// browser — a visitor signed out goes down the sign-in path.
func (c *conn) varsRefused() bool {
	if c.app.vars == nil || c.page == nil {
		return false
	}
	err := refusedVars(c.page)
	if err == nil {
		return false
	}
	c.noteClose(closeReload, "live: the page must load again", "a gate refused a read of the page's variables: "+err.Error())
	c.closeConn(closeReload, "live: the page must load again")
	return true
}

// check runs the app's LiveChecks with the page's context.
func (c *conn) check() bool {
	for _, f := range c.app.liveChecks {
		if err := f(c.page); err != nil {
			c.noteClose(closeReload, "live: the page must load again", "a LiveCheck refused the page: "+err.Error())
			return false
		}
	}
	return true
}

// setPage installs ctx as the page's context, wrapped so leaveEarly can end
// its renders, and returns the wrapped context, which the session must
// render with.
func (c *conn) setPage(ctx context.Context) context.Context {
	l := newLeaveCtx(ctx)
	c.page = l
	ctx = l
	c.shared.Store(&ctx)
	c.leave.Store(l)
	return l
}

// leaveEarly notes, from the socket's reader, a navigate that leaves the page
// (anything but a patch of a page with an Update), and reports whether data
// is one. Until the loop serves it, the page's renders are ended: the one in
// progress now, when the loop is rendering after an event's handler or
// applying a deferred load, and those after the handlers of the events queued
// before it. A render is read-only, so the slow query it runs may stop instead
// of making the navigation wait. An event's handler is never ended this way:
// it may be writing, and each runs, in order, with a context of its own.
func (c *conn) leaveEarly(data []byte) bool {
	var m struct {
		Type  string `json:"type"`
		Patch bool   `json:"patch"`
		URL   string `json:"url"`
	}
	if json.Unmarshal(data, &m) != nil || m.Type != "navigate" || m.Patch && (c.updatable.Load() || c.selfNavigates(m.URL) || c.urlSnap.Load().keeps(m.URL)) {
		return false // not a navigate, or a patch that may keep the page
	}
	c.leaving.Add(1)
	c.readMu.Lock()
	if c.reading {
		c.leave.Load().end()
	}
	c.readMu.Unlock()
	return true
}

// beginRead marks the start of a render that only reads: after a handler, or
// a deferred load's apply. With a navigate leaving the page waiting, it is
// ended at once.
func (c *conn) beginRead() {
	c.readMu.Lock()
	c.reading = true
	if c.leaving.Load() > 0 {
		c.leave.Load().end()
	}
	c.readMu.Unlock()
}

func (c *conn) endRead() {
	c.readMu.Lock()
	c.reading = false
	c.readMu.Unlock()
}

// answer runs a handler (an event's, a broadcast's) and the render after it
// with the page's context live for the handler.
func (c *conn) answer(run func() any) any {
	c.leave.Load().renew()
	c.answering = true
	defer func() {
		c.answering = false
		c.endRead()
	}()
	return run()
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
	if len(c.app.liveGuards) > 0 && len(m.Shown) > 0 {
		// the hidden forms and regions shown with it: those a guard refuses
		// stay waiting
		shown := m.Shown[:0:0]
		h, _ := c.s.(interface{ Handler(string) any })
	targets:
		for _, t := range m.Shown {
			if h == nil {
				break
			}
			sfn := h.Handler(t)
			if sfn == nil {
				continue
			}
			lh := c.liveHandler(t, sfn, handlerName(sfn))
			for _, g := range c.app.liveGuards {
				if g(c.page, lh) != nil {
					continue targets
				}
			}
			shown = append(shown, t)
		}
		m.Shown = shown
	}
	c.ev = ev
	reply := c.answer(func() any { return c.s.Event(m) })
	c.ev = nil
	defer func() { c.noteSelfURL(ev, reply) }()
	if ev.refused != nil {
		return withError{reply, c.errorText(ev.refused)}
	}
	// A handler that returns web.Redirect moves the page to a root-absolute
	// in-app path; carry the connection's mount prefix on it, as the join and
	// navigate replies do, so a /admin redirect under a /gateway mount stays
	// under it.
	// A handler that returns web.Redirect, or calls PushNavigate/PushPatch,
	// moves the page to a root-absolute in-app path; carry the connection's
	// mount prefix on it (as the join and navigate replies do) so /admin under
	// a /gateway mount stays under it. A form's redirect arrives as a navigate
	// command in the patch's js, not as a Redirect message.
	if sm, ok := reply.(live.ServerMessage); ok {
		if sm.Type == live.Redirect {
			sm.URL = c.prefixLoc(sm.URL)
		}
		c.prefixJS(sm.JS)
		reply = sm
	}
	return reply
}

// prefixJS carries the connection's mount prefix on the href of navigate and
// patch commands the server pushes (a form's redirect, PushNavigate), which the
// client runs as absolute in-app paths. Client-initiated navigations use the
// page's own rendered (already-prefixed) URLs, so they never reach here.
func (c *conn) prefixJS(pushes []live.JSPush) {
	if basePrefix(c.r) == "" {
		return
	}
	for i := range pushes {
		var ops []json.RawMessage
		if json.Unmarshal(pushes[i].Ops, &ops) != nil {
			continue
		}
		changed := false
		for j, raw := range ops {
			var pair []json.RawMessage
			if json.Unmarshal(raw, &pair) != nil || len(pair) != 2 {
				continue
			}
			var name string
			if json.Unmarshal(pair[0], &name) != nil || (name != "navigate" && name != "patch") {
				continue
			}
			var args map[string]json.RawMessage
			if json.Unmarshal(pair[1], &args) != nil {
				continue
			}
			var href string
			if json.Unmarshal(args["href"], &href) != nil {
				continue
			}
			loc := c.prefixLoc(href)
			if loc == href {
				continue
			}
			args["href"], _ = json.Marshal(loc)
			pair[1], _ = json.Marshal(args)
			ops[j], _ = json.Marshal(pair)
			changed = true
		}
		if changed {
			pushes[i].Ops, _ = json.Marshal(ops)
		}
	}
}

// render is the session's render function for page: the session calls it
// first when it renders, so an event being handled learns its handler is
// done (LiveEvent.Handling).
func (c *conn) render(page ui.Node) liveproto.Render {
	return func(context.Context) ui.Node {
		if ev := c.ev; ev != nil {
			ev.rendering.Store(true)
		}
		if c.answering {
			c.beginRead() // the handler returned
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
func (c *conn) join(ctx context.Context, ref, v int, ext []string, build string) (any, websocket.StatusCode) {
	if build != "" && build != c.app.buildID() {
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: errOtherBuild.Error()}, closeReload
	}
	j, rec, err := c.run(ctx, c.uri, "join")
	if err != nil {
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: errBadToken.Error()}, closeReload
	}
	if j.page == nil {
		if loc := c.prefixLoc(rec.location()); loc != "" {
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
	j.ctx = c.setPage(j.ctx)
	s := c.adopt(j, v, ext)
	if s != nil {
		c.s = s
	} else {
		c.s = liveproto.New(j.ctx, c.render(j.page), min(v, c.app.liveMax), ext...)
	}
	c.noteJoin(s != nil, min(v, c.app.liveMax))
	if e, ok := c.s.(interface {
		SetShared(*live.SharedSet, live.SharedLinks)
	}); ok {
		e.SetShared(c.app.shared, c)
	}
	if e, ok := c.s.(interface{ SetErrorText(func(error) string) }); ok {
		e.SetErrorText(c.errorText)
	}
	c.root = j.root
	c.layouts = j.layouts
	c.updatable.Store(j.updates != nil)
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
func (c *conn) adopt(j *join, v int, ext []string) liveproto.Session {
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
	s, err := liveproto.Adopt(p.s, j.ctx, c.render(j.page), ver, ext...)
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
// error whose text the browser doesn't get whole — hidden, or shortened to
// its public message — is logged in full, with the session, the page, what
// it was running and, for an event naming no handler, the component.
func (c *conn) errorText(err error) string {
	text, whole := c.app.liveErrorText(err)
	if !whole {
		ctx := c.pageCtx()
		if ctx == nil {
			ctx = c.r.Context()
		}
		attrs := []any{"session", c.sessionID(), "page", c.uri, "handler", c.doing}
		if nh := (*live.NoHandlerError)(nil); errors.As(err, &nh) {
			attrs = append(attrs, "component", cmp.Or(nh.Component, "none"))
		}
		slog.ErrorContext(ctx, "web: live page", append(attrs, "err", err)...)
	}
	return text
}

// sessionID names the connection's page session in the log: a digest of its
// join token, which the log never holds.
func (c *conn) sessionID() string {
	sum := sha256.Sum256([]byte(c.tok))
	return hex.EncodeToString(sum[:6])
}

// liveErrorText is the text of a live page's error for the browser, and
// whether that is the error's whole text. In development it is the error's
// text, whatever it says. Outside it, a *web.Error's message, or another
// message the browser may read (live.PublicError: a form field that doesn't
// parse, an event naming no handler as `live: no handler "c3:75"` without
// whose it was), and for any other error "Internal Server Error": Go type
// names, raw Go errors and panics' values go to the log only.
func (a *App) liveErrorText(err error) (string, bool) {
	m, public := live.PublicMessage(err)
	if a.dev {
		return err.Error(), public && m == err.Error()
	}
	if public {
		return m, m == err.Error()
	}
	return http.StatusText(http.StatusInternalServerError), false
}

// navigate moves the page to m.URL over the connection: the URL's route runs
// as for a join (X-Vuka-Live: navigate), and the page it renders becomes the
// session's. The server decides whether it is a patch, the client's patch
// flag only asking for one: the page's component stays mounted when the
// URL's route is a Live page whose type has an Update taking web.URLParams,
// and that Update takes the URL's params, or when the patch changes only
// query parameters the page's components keep in the address (their @ui.URL
// fields, their DataTables'), which they read again as they render
// (liveurlonly.go); otherwise every component mounts afresh, Mount seeing
// the new URL, even on the same route. The reply carries a token for the new page, which a reconnect
// joins. A URL whose route answers anything but a page (a redirect, an
// error, JSON) is the browser's to load: the reply is a redirect, refused
// (an error, which the runtime loads the URL on) when it names a URL that
// isn't http or https. A page under another root layout than the
// connection's (RouteInfo.RootLayout: its first LayoutOf, or none) is
// another document, the browser's to load too: the reply is a redirect to
// it. A navigation whose Mount, Update or render fails
// replies with the error and leaves nothing mounted; the runtime loads the
// URL, getting the route's own answer.
// prefixLoc carries the live connection's mount prefix on a root-absolute
// redirect location the server set (web.Redirect from a live handler), so a
// redirect to /admin under a /gateway mount stays under it. Relative,
// absolute-URL, scheme-relative and empty locations are left as they are. The
// client already resolves its own navigations against the page URL, so this
// is only for server-emitted locations.
func (c *conn) prefixLoc(loc string) string {
	if !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") {
		return loc
	}
	p := basePrefix(c.r)
	if p == "" || loc == p || strings.HasPrefix(loc, p+"/") {
		return loc
	}
	return p + loc
}

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
	if reply, ok := c.selfNavigate(m, uri); ok {
		return reply, true
	}
	c.navs++
	urlOnly := m.Patch && !c.updatable.Load() && c.snapURL().keeps(uri)
	if !m.Patch || !c.updatable.Load() && !urlOnly {
		// The page is being left: end its deferred loads now, before the next
		// page's route runs, so their queries hand the pool and the CPU to the
		// navigation instead of racing it. Only a navigation asking for a
		// patch of a page that can take one (it has an Update, or the patch
		// changes only what its components keep in the address) may keep the
		// page, so only then do its loads go on (startDeferred settles it
		// once the server has decided).
		c.endLoads()
	}
	var j *join
	var ahead *live.Session // the page rendered at hover, its instances for the taking
	if a := c.aheadTake(uri); a != nil {
		j, ahead = a.j, a.s
		a.s = nil
		if a.vars != nil {
			// the run and render read this copy of the history: it is the
			// page's from here on, what they computed and showed with it
			c.vars = a.vars
		}
	} else {
		run, rec, err := c.run(ctx, uri, "navigate")
		if err != nil {
			return redirect(), false
		}
		if run.page == nil {
			load.URL = cmp.Or(c.prefixLoc(rec.location()), m.URL)
			return redirect(), false
		}
		j = run
	}
	if j.root != c.root {
		if ahead != nil {
			ahead.Close()
		}
		return redirect(), false // another document: the browser loads it
	}
	urlOnly = urlOnly && j.updates == nil
	patch := m.Patch && (j.updates != nil || urlOnly)
	if m.Patch && !patch && !urlOnly {
		c.app.warnNoUpdate(ctx, j, c.whyRemount(uri))
	}
	if ahead != nil && patch {
		// a patch keeps the page's instances, and its slot, which the render
		// ahead keyed the page below the layouts by: nothing there to take
		ahead.Close()
		ahead = nil
	}
	c.doing = "navigate " + uri
	if !patch {
		c.slot = c.navs // the page below the layouts mounts afresh
	}
	// the components of the layouts kept stay mounted when the document's
	// is one of them
	keep := len(j.layouts) > 0 && j.layouts[0].gen < c.navs
	prevPage, prevLeave := c.page, c.leave.Load()
	j.ctx = c.setPage(j.ctx)
	rt := live.Route{Ctx: j.ctx, Render: c.render(j.page), Updates: j.updates, Keep: keep, URLOnly: urlOnly}
	var reply any
	var ok bool
	// A patch the page's Update takes is a read, as a GET is: a navigate
	// leaving the page that arrives behind it ends it, Update and render.
	read := patch
	if read {
		c.beginRead()
	}
	if nf, can := c.s.(aheadNavigator); can && ahead != nil {
		reply, ok = nf.NavigateFrom(m.Ref, uri, rt, ahead, patch) // closes ahead
	} else {
		reply, ok = c.s.Navigate(m.Ref, uri, rt, patch)
	}
	if read {
		c.endRead()
	}
	if !ok {
		c.page = prevPage
		c.shared.Store(&prevPage)
		if prevLeave != nil {
			c.leave.Store(prevLeave)
		}
		c.layouts = nil
		return reply, patch
	}
	c.uri = uri
	c.layouts = j.layouts
	c.updatable.Store(j.updates != nil)
	return withToken{reply, c.app.signToken(uri), c.docTitle(ui.PageTitle(j.ctx))}, patch
}

// warnNoUpdate says, once for each route and in development only, that a
// patch navigation asked for a live page it can't keep: the page has no
// Update taking web.URLParams, and the patch changes more of the URL than
// its components' @ui.URL fields and DataTables keep (why), so it mounts
// again, as any navigation to it does.
func (a *App) warnNoUpdate(ctx context.Context, j *join, why string) {
	if !a.dev || j.route == "" {
		return
	}
	if _, seen := a.noUpdate.LoadOrStore(j.route, true); seen {
		return
	}
	slog.WarnContext(ctx, "a patch link targets a live page without Update, so the page mounts again: "+why+
		"; to keep it mounted, declare func (p *"+pageTypeName(j.where)+") Update(web.URLParams), or take a struct embedding web.URLParams to re-bind the new URL's parameters",
		"route", j.route, "page", j.where)
}

// pageTypeName is the name of the page type a join's where names ("live
// page main.Order" → "Order"); "Page" when it names none.
func pageTypeName(where string) string {
	name := where[strings.LastIndexByte(where, ' ')+1:]
	name = name[strings.LastIndexByte(name, '.')+1:]
	name = strings.TrimLeft(name, "*")
	if name == "" {
		return "Page"
	}
	return name
}

// docTitle is the title of the document the session renders, as a load of
// the page gives it: the <title> its head has, else the page's (withTitle).
func (c *conn) docTitle(page string) string {
	h, ok := c.s.(interface{ HTML() string })
	if !ok {
		return page
	}
	return documentTitle(h.HTML(), page)
}

// run runs the route of uri on behalf of the connection: a GET carrying the
// upgrade request's headers, X-Vuka-Live set to why. j.page is the page it
// rendered, nil when it answered something else (rec has its status and
// headers).
func (c *conn) run(ctx context.Context, uri, why string) (*join, *recorder, error) {
	return c.runIn(ctx, uri, why, nil)
}

// runIn is run reading and keeping the context processors' results in hist,
// the connection's own history when nil.
func (c *conn) runIn(ctx context.Context, uri, why string, hist *varHistory) (*join, *recorder, error) {
	// The socket's runs don't pass through http.StripPrefix, so make the
	// synthetic request mirror it under a mount prefix: route on the path with
	// the prefix removed (routes are registered without it), but keep the
	// prefix on RequestURI so mountPrefix/basePrefix return it and the render
	// emits prefixed URLs. A navigate's uri arrives prefixed (the client's
	// URL); a join's arrives already a route path — both end up the same.
	route, full := uri, uri
	if p := basePrefix(c.r); p != "" {
		switch {
		case uri == p:
			route = "/"
		case strings.HasPrefix(uri, p+"/"):
			route = strings.TrimPrefix(uri, p)
		}
		full = p + route
	}
	u, err := url.ParseRequestURI(route)
	if err != nil {
		return nil, nil, err
	}
	j := &join{why: why, held: c.layouts, slot: &c.slot, nav: c.navs}
	defer j.done.Store(true)
	if c.app.vars != nil {
		if hist == nil {
			if c.vars == nil {
				c.vars = newVarHistory()
			}
			if why == "join" {
				c.vars.joined(c.app.joinMessages.take(c.tok))
			}
			hist = c.vars
		}
		ctx = context.WithValue(ctx, histKey{}, hist)
	}
	req := c.r.Clone(context.WithValue(ctx, joinKey{}, j))
	req.Method, req.URL, req.RequestURI = http.MethodGet, u, full
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
	title string // the page's title (ui.PageTitle), for the document
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
	b = append(append(b[:len(b)-1:len(b)-1], `,"token":`...), t...)
	if w.title != "" {
		t, _ = json.Marshal(w.title)
		b = append(append(b, `,"title":`...), t...)
	}
	return append(b, '}'), nil
}

// deferredSession is a session whose pages and components have deferred
// fields (live.Session.Deferred).
type deferredSession interface {
	Deferred() []*ui.DeferredLoad
	Loaded(d *ui.DeferredLoad) any
}

// seenSession is a session whose deferred groups may load once their
// fallbacks are seen (live.Session.DeferredSeen).
type seenSession interface {
	DeferredSeen(groups []string) []*ui.DeferredLoad
}

// liveLoads is the client message naming the deferred groups loading when
// seen (ui.Defer.WhenSeen) whose fallbacks scrolled into view, as
// {"type":"loads","groups":["comments"]}. The runtime sends it once per
// group per page; nothing is sent back but the groups' patches as they
// load. A group the page doesn't defer that way, or one started already,
// is ignored, so a repeat or a made-up name costs nothing.
const liveLoads = "loads"

// maxSeenGroups caps the groups one loads message may name: the server's
// backstop against a client naming more than a page could render.
const maxSeenGroups = 64

// loaded is a deferred load done, waiting in the inbox to be applied.
type loaded struct{ d *ui.DeferredLoad }

// endLoads ends the deferred loads of the page the connection shows, run
// on the connection's goroutine as every use of c.loads is.
func (c *conn) endLoads() {
	for _, end := range c.loads {
		end()
	}
	c.loads = nil
}

// startDeferred runs the deferred loads of the page just joined or
// navigated to, or of the components an event, a broadcast or a load
// mounted (kept), each on its own goroutine with the page's context; each,
// done, is applied as a broadcast is. Leaving the page ends its loads,
// except on a patch (kept): its components stay mounted, and so do the
// loads they started, which never start again.
func (c *conn) startDeferred(kept bool) {
	ds, ok := c.s.(deferredSession)
	if !ok {
		return
	}
	if !kept {
		c.endLoads()
	}
	c.runLoads(ds.Deferred())
}

// startSeen starts the loads of the groups the browser saw the fallbacks
// of, as startDeferred does the others: on the page's context, ended with
// them when the page is left.
func (c *conn) startSeen(groups []string) {
	ss, ok := c.s.(seenSession)
	if !ok || len(groups) == 0 {
		return
	}
	if len(groups) > maxSeenGroups {
		groups = groups[:maxSeenGroups]
	}
	c.runLoads(ss.DeferredSeen(groups))
}

// runLoads runs loads, each on its own goroutine with the page's context;
// each, done, waits in the inbox to be applied as a broadcast is.
func (c *conn) runLoads(loads []*ui.DeferredLoad) {
	if len(loads) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(c.page)
	c.loads = append(c.loads, cancel)
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
// nothing to deliver them to, and the join renders the page fresh. Each
// message reaches its Update in the order it arrived, but the broadcasts
// that wait side by side are one batch: the page renders once and sends one
// patch for them.
func (c *conn) drain(ctx context.Context) bool {
	c.mu.Lock()
	batch := c.inbox
	c.inbox = nil
	c.mu.Unlock()
	if c.s == nil || len(batch) == 0 {
		return true
	}
	for i := 0; i < len(batch); {
		switch d := batch[i].msg.(type) {
		case aheadExpired:
			c.expired(d)
			i++
		case loaded:
			if !c.applyLoaded(ctx, d) {
				return false
			}
			i++
		default:
			j := i + 1
			for j < len(batch) && !isLoadMsg(batch[j].msg) {
				j++
			}
			if !c.broadcasts(ctx, batch[i:j]) {
				return false
			}
			i = j
		}
	}
	c.startDeferred(true) // a component a broadcast or a load mounted
	c.syncTopics()
	return true
}

func isLoadMsg(m any) bool {
	switch m.(type) {
	case aheadExpired, loaded:
		return true
	}
	return false
}

// applyLoaded gives the page what a deferred load loaded.
func (c *conn) applyLoaded(ctx context.Context, l loaded) bool {
	ds, _ := c.s.(deferredSession)
	if ds == nil {
		return true
	}
	c.doing = "load " + l.d.Group
	end := noop
	if c.tb != nil {
		end = c.recording("load", l.d.Group, 0, nil)
	}
	c.vars.newEvent()
	c.beginRead()
	m := ds.Loaded(l.d)
	c.endRead()
	if c.varsRefused() {
		return false
	}
	if m != nil && !c.push(ctx, m) {
		return false
	}
	end()
	return true
}

// broadcasts delivers a run of broadcasts and sends the patch they made.
func (c *conn) broadcasts(ctx context.Context, run []delivery) bool {
	msgs := make([]liveproto.Message, len(run))
	for i, d := range run {
		c.aheadHeard(d.topic)
		msgs[i] = liveproto.Message{Topic: d.topic, Msg: d.msg}
	}
	c.doing = "broadcast " + cmp.Or(run[0].topic, `""`)
	end := noop
	if c.tb != nil {
		end = c.recordInfo(run)
	}
	c.vars.newEvent()
	release := broadcastGate.acquire(ctx)
	if release == nil {
		return false
	}
	m := c.answer(func() any { return c.s.InfoBatch(msgs) })
	release()
	if c.varsRefused() {
		return false
	}
	if m != nil && !c.push(ctx, m) {
		return false
	}
	end()
	return true
}

func (c *conn) syncTopics() {
	if c.s == nil {
		return
	}
	ts := c.aheadTopics(c.s.Topics())
	if !slices.Equal(ts, c.topics) {
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
	case livePrefetch:
		subject = m.URL
	case liveLoads:
		var g struct {
			Groups []string `json:"groups"`
		}
		json.Unmarshal(data, &g)
		subject = strings.Join(g.Groups, ", ")
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
func (c *conn) recordInfo(run []delivery) func() {
	d := run[0]
	subject := fmt.Sprintf("%s %T", cmp.Or(d.topic, `""`), d.msg)
	if len(run) > 1 {
		subject += fmt.Sprintf(" (and %d more, rendered together)", len(run)-1)
	}
	return c.recording("broadcast", subject, 0, nil)
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

// instanceType names an instance's component for the toolbar, when the
// session says (*views.Counter: Counter).
func (c *conn) instanceType(id string) string {
	if id == "c0" {
		return "page"
	}
	h, ok := c.s.(interface{ Component(string) ui.Stateful })
	if !ok {
		return ""
	}
	comp := h.Component(id)
	if comp == nil {
		return ""
	}
	t := reflect.TypeOf(comp)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name, _, _ := strings.Cut(t.Name(), "[")
	return name
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

func (a *App) serveLiveJS(w http.ResponseWriter, r *http.Request) {
	js, tag := a.runtime()
	serveTagged(w, r, "text/javascript; charset=utf-8", tag, js)
}
