package web

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/web/v2/internal/liveproto"
)

// livePrefetch is the client message that warms a page over the connection
// before it is followed: the route runs and its page is rendered, mounting
// its components, so the navigate a click sends applies at once instead of
// running the route and mounting the page again. The runtime sends it on
// hover, focus or touch for a ui.Link whose page moves over the connection;
// nothing is sent back.
const livePrefetch = "prefetch"

// maxAheadNav caps the pages held warm at once. The runtime caps too; this
// is the server's backstop against a client asking for more.
const maxAheadNav = 3

// aheadTTL drops a warm page that was never followed, matching the runtime's
// own window. A variable for tests.
var aheadTTL = 30 * time.Second

// aheadNav is a page made ready ahead of a click: the route's run, and when
// the page could be rendered too, the session that rendered it (s), whose
// instances the navigation takes instead of mounting them. It is valid only
// for the navigation right after the one it was made against (nav), within
// its window (at), while nothing changed the connection's context processor
// history (gen: vars is the copy the run and render read, adopted with the
// page) and for the identity that made it (who), the app's LiveChecks
// passing at the click. tb is the toolbar's record of the run and render, so
// a committed prefetch still shows them, the work having happened at hover.
type aheadNav struct {
	j     *join
	s     *live.Session
	nav   int
	at    time.Time
	tb    *tbEntry
	vars  *varHistory
	gen   uint64
	who   string
	timer *time.Timer
}

// close stops the entry's window and unmounts its rendered page, if any; a
// second close does nothing.
func (a *aheadNav) close() {
	if a.timer != nil {
		a.timer.Stop()
	}
	a.dropRender()
}

// dropRender unmounts the page rendered ahead, keeping the route's run: the
// click then mounts the page as a navigation without a prefetch does.
func (a *aheadNav) dropRender() {
	if a.s != nil {
		a.s.Close()
		a.s = nil
	}
}

// aheadExpired is the inbox message an entry's window posts when it runs
// out, so the connection's own goroutine drops it.
type aheadExpired struct {
	uri string
	a   *aheadNav
}

// aheadNavigator is a session that can take the instances a session
// rendered ahead mounted (native sessions; a stand-in can't).
type aheadNavigator interface {
	NavigateFrom(ref int, url string, rt live.Route, ahead *live.Session, patch bool) (any, bool)
}

// prefetch runs uri's route for the connection and renders the page it
// gives in a session of its own, held for a navigate to uri right after. It
// never touches the live session or the connection's current page — a hover
// must not move the page — and anything that can't be held (a redirect,
// another document, an error) is left for the navigate to handle the usual
// way. The run and the render read a copy of the connection's context
// processor history, so a hover computes nothing into the page on screen and
// shows no flash message the visitor never sees. The page's deferred loads
// don't start: they run once it is committed, so a hover costs its mount and
// render, not its loads.
func (c *conn) prefetch(ctx context.Context, m live.ClientMessage) {
	uri, _, _ := strings.Cut(m.URL, "#")
	if !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") || strings.HasPrefix(uri, "/\\") || uri == c.uri {
		return
	}
	c.sweepAhead()
	if _, held := c.ahead[uri]; held || len(c.ahead) >= maxAheadNav {
		return
	}
	a := &aheadNav{at: time.Now()}
	if c.app.vars != nil {
		if c.vars == nil {
			c.vars = newVarHistory()
		}
		a.vars, a.gen = c.vars.fork()
	}
	// Run and render as the navigation that will use the page, the next one:
	// the layouts the run loads are keyed by the navigation they are loaded
	// on, and the page below them by the slot that navigation sets, read as
	// the page renders. A navigation keeps the instances of a layout whose key
	// it renders again, and takes those of the page rendered ahead only where
	// the keys agree. Nothing else renders on this goroutine meanwhile, so the
	// connection's own count and slot are lent for the run and put back.
	nav, slot := c.navs, c.slot
	c.navs++
	c.slot = c.navs
	j, _, err := c.runIn(ctx, uri, "navigate", a.vars)
	if err == nil && j.page != nil && j.root == c.root {
		a.j, a.nav = j, nav
		a.who = c.app.liveWho(j.ctx)
		a.s = c.renderAhead(j)
	}
	c.navs, c.slot = nav, slot
	if a.j == nil {
		return // a redirect or another document: the click takes the normal path
	}
	if c.ahead == nil {
		c.ahead = make(map[string]*aheadNav, maxAheadNav)
	}
	if c.tb != nil {
		a.tb = c.tb.cur.Load() // the run's route, queries and render, for navigated at commit
	}
	a.timer = time.AfterFunc(aheadTTL, func() { c.post("", aheadExpired{uri, a}) })
	c.ahead[uri] = a
}

// renderAhead renders j's page in a session of its own for the navigation to
// take its instances (live.Session.RenderAhead); nil when it can't be used:
// the live session can't take them (a stand-in), adoption is off
// (LiveAdopt(0)), or the render failed.
func (c *conn) renderAhead(j *join) *live.Session {
	if c.app.adoptWindow <= 0 {
		return nil
	}
	if _, ok := c.s.(aheadNavigator); !ok {
		return nil
	}
	s, err := liveproto.Ahead(j.ctx, c.render(j.page))
	if err != nil {
		return nil
	}
	return s
}

// aheadTake is the page held warm for uri when this navigation may use it,
// else nil: made against the page this navigation leaves (no navigation
// since), still fresh, the context processor history untouched since (no
// message flashed or shown, no processor run), for the identity the page has,
// and the app's LiveChecks passing for its context. It is removed whether
// used or not, and every other warm page is dropped too, since a committed
// navigation moves the count they were keyed to. The caller has already
// done c.navs++, and the event's start of the history (newEvent).
func (c *conn) aheadTake(uri string) *aheadNav {
	a := c.ahead[uri]
	delete(c.ahead, uri)
	c.clearAhead()
	if a == nil {
		return nil
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	if a.nav != c.navs-1 || time.Since(a.at) >= aheadTTL || !c.aheadValid(a) {
		a.close()
		return nil
	}
	c.aheadTB = a.tb // the toolbar shows the run that happened at hover
	return a
}

// aheadValid reports whether what a's run and render saw still holds at the
// click: the history they copied, the identity, the app's LiveChecks.
func (c *conn) aheadValid(a *aheadNav) bool {
	if a.vars != nil && (c.vars == nil || c.vars.gen.Load() != a.gen) {
		return false
	}
	if who := c.app.liveWho(a.j.ctx); who != a.who || c.app.liveWho(c.page) != who {
		return false
	}
	for _, f := range c.app.liveChecks {
		if f(a.j.ctx) != nil {
			return false
		}
	}
	return true
}

// clearAhead drops every warm page, unmounting those rendered: a committed
// navigation advances the count they were mounted against, so none can be
// used; the connection's end or a join does too.
func (c *conn) clearAhead() {
	for k, a := range c.ahead {
		delete(c.ahead, k)
		a.close()
	}
}

// sweepAhead drops warm pages past their window. Each window also posts its
// own end (aheadExpired), so an idle connection that hovered and left
// doesn't hold them.
func (c *conn) sweepAhead() {
	for k, a := range c.ahead {
		if time.Since(a.at) >= aheadTTL {
			delete(c.ahead, k)
			a.close()
		}
	}
}

// expired drops the warm page e names when its window ran out, unless it
// was used or dropped already.
func (c *conn) expired(e aheadExpired) {
	if c.ahead[e.uri] == e.a {
		delete(c.ahead, e.uri)
		e.a.close()
	}
}

// aheadHeard drops the renders of warm pages that subscribed to topic (every
// one for ""), keeping their routes' runs: what a broadcast changes reaches
// the live session only, so a render made before it would show what it
// replaced.
func (c *conn) aheadHeard(topic string) {
	for _, a := range c.ahead {
		if a.s != nil && (topic == "" || slices.Contains(a.s.Topics(), topic)) {
			a.dropRender()
		}
	}
}

// aheadTopics adds to ts the topics the warm pages' renders subscribe to, so
// a broadcast to one reaches the connection and drops it (aheadHeard).
func (c *conn) aheadTopics(ts []string) []string {
	for _, a := range c.ahead {
		if a.s == nil {
			continue
		}
		for _, t := range a.s.Topics() {
			if !slices.Contains(ts, t) {
				ts = append(slices.Clip(ts), t)
			}
		}
	}
	return ts
}
