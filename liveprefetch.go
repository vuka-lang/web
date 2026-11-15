package web

import (
	"context"
	"strings"
	"time"

	"github.com/vuka-lang/ui/v2/live"
)

// livePrefetch is the client message that warms a page over the connection
// before it is followed: the route runs and its mounted page is held, so the
// navigate a click sends applies at once instead of mounting the page again.
// The runtime sends it on hover, focus or touch for a ui.Link whose page moves
// over the connection; nothing is sent back.
const livePrefetch = "prefetch"

const (
	// maxAheadNav caps the pages held warm at once. The runtime caps too; this
	// is the server's backstop against a client asking for more.
	maxAheadNav = 3
	// aheadTTL drops a warm page that was never followed, matching the
	// runtime's own window.
	aheadTTL = 30 * time.Second
)

// aheadNav is a page mounted ahead of a click: the route's run, the navigation
// count it was rendered against (it is valid only until the next navigation
// moves that count), when it was made so a stale one is dropped, and the
// toolbar's record of the run (its route, handler and queries) so a committed
// prefetch still shows them, the run having happened at hover.
type aheadNav struct {
	j   *join
	nav int
	at  time.Time
	tb  *tbEntry
}

// prefetch runs uri's route for the connection and holds its mounted page, so a
// navigate to uri right after applies without mounting again. It never touches
// the live session or the connection's current page — a hover must not move the
// page — and anything that can't be held (a redirect, another document, an
// error) is left for the navigate to handle the usual way. The mounted page is
// dropped unused if no click follows: deferred loads run only once it is
// committed, so holding it costs a mount, not its queries.
func (c *conn) prefetch(ctx context.Context, m live.ClientMessage) {
	uri, _, _ := strings.Cut(m.URL, "#")
	if !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") || strings.HasPrefix(uri, "/\\") || uri == c.uri {
		return
	}
	c.sweepAhead()
	if _, held := c.ahead[uri]; held || len(c.ahead) >= maxAheadNav {
		return
	}
	j, _, err := c.run(ctx, uri, "navigate")
	if err != nil || j.page == nil || j.root != c.root {
		return // a redirect or another document: the click takes the normal path
	}
	if c.ahead == nil {
		c.ahead = make(map[string]*aheadNav, maxAheadNav)
	}
	a := &aheadNav{j: j, nav: c.navs, at: time.Now()}
	if c.tb != nil {
		a.tb = c.tb.cur.Load() // the run's route and queries, for navigated at commit
	}
	c.ahead[uri] = a
}

// aheadTake is the page held warm for uri when this navigation may use it:
// mounted against the page this navigation leaves (no navigation since, still
// fresh). It is removed whether used or not, and every other warm page is
// dropped too, since a committed navigation moves the count they were keyed to.
// The caller has already done c.navs++, so the page is valid when it was held
// one count ago.
func (c *conn) aheadTake(uri string) *join {
	a := c.ahead[uri]
	c.clearAhead()
	if a == nil || a.nav != c.navs-1 || time.Since(a.at) >= aheadTTL {
		return nil
	}
	a.j.nav = c.navs // key the page to this navigation, as a fresh run would
	c.aheadTB = a.tb // the toolbar shows the run that happened at hover
	return a.j
}

// clearAhead drops every warm page: a committed navigation advances the count
// they were mounted against, so none can be used.
func (c *conn) clearAhead() {
	for k := range c.ahead {
		delete(c.ahead, k)
	}
}

// sweepAhead drops warm pages past their window, so an idle connection that
// hovered and left doesn't hold them.
func (c *conn) sweepAhead() {
	for k, a := range c.ahead {
		if time.Since(a.at) >= aheadTTL {
			delete(c.ahead, k)
		}
	}
}
