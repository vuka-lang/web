package web

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/vuka-lang/ui/v2/live"
)

// A live page's GET mounts its components and renders them; the join of its
// connection, moments later, runs the route again and would mount them a
// second time. Instead the render's session is parked for a short window,
// keyed by the page's one-time join token, and the join adopts it when it
// may: Mount (and its queries) runs once per page view. Adoption is an
// optimization, never something to rely on — any doubt (the window ran out,
// the token was used, another identity, another process after a restart)
// mounts fresh, exactly as every join did before.

const (
	defaultLiveAdoptWindow = 10 * time.Second
	// maxParkedPages caps the sessions parked at once; beyond it the oldest
	// is dropped, its join mounting fresh.
	maxParkedPages = 1024
)

// LiveAdopt is how long after a live page's GET its join may adopt the
// session the render mounted, instead of mounting the page's components
// again: 10 seconds by default, enough for the page to load and connect.
// 0 (or less) turns adoption off: every join mounts fresh, and renders
// park nothing. The join still runs the page's route — middleware, gates
// and the handler, with the connection's own cookies — whether or not it
// adopts; only the mounting of components it skips.
func LiveAdopt(window time.Duration) Option {
	return OptionFunc(func(a *App) { a.adoptWindow = window })
}

// LiveIdentity tells live adoption and stateless pages' signed props who a
// context is signed in as: a stable key for the identity and its sign-in
// ("" for no one). A join adopts a parked session only when every
// registered function returns for the join's context what it returned for
// the render's — the same sign-in, or anonymous both times — and a
// stateless page's call verifies only for the visitor its props were
// signed for. A library that signs visitors in registers one, so one
// visitor's page is never adopted, or its props replayed, by another.
func LiveIdentity(f func(ctx context.Context) string) Option {
	return OptionFunc(func(a *App) {
		a.mutable("LiveIdentity")
		a.liveIDs = append(a.liveIDs, f)
	})
}

// liveWho is the identity key of ctx: every LiveIdentity function's,
// joined. "" when none is registered, or no one is signed in.
func (a *App) liveWho(ctx context.Context) string {
	if len(a.liveIDs) == 0 {
		return ""
	}
	keys := make([]string, len(a.liveIDs))
	for i, f := range a.liveIDs {
		keys[i] = f(ctx)
	}
	return strings.Join(keys, "\n")
}

// parkedPage is a live page's session between its GET and its join: the
// mounted instances, and what must match for a join to adopt them.
type parkedPage struct {
	s   *live.Session
	uri string // the page the render served, as its token names it
	who string // the identity that rendered it (liveWho)
}

func (p *parkedPage) close() { p.s.Close() }

// parkStore holds each parked value (a live page's session, a page's
// parts) under its token until a request takes it (at most once), the
// window runs out, or the app stops.
type parkStore[V interface{ close() }] struct {
	mu     sync.Mutex
	m      map[string]*parkEntry[V]
	order  []string // tokens oldest first, for the cap
	max    int      // the cap: maxParkedPages when 0
	closed bool
}

type parkEntry[V any] struct {
	v     V
	timer *time.Timer
}

func (e *parkEntry[V]) stop() {
	if e.timer != nil {
		e.timer.Stop()
	}
}

// park keeps p for window under tok, evicting the oldest page beyond the cap.
func (ps *parkStore[V]) park(tok string, p V, window time.Duration) {
	ps.mu.Lock()
	if ps.closed {
		ps.mu.Unlock()
		p.close()
		return
	}
	if ps.m == nil {
		ps.m = map[string]*parkEntry[V]{}
	}
	limit := ps.max
	if limit <= 0 {
		limit = maxParkedPages
	}
	var evicted []*parkEntry[V]
	for len(ps.order) >= limit {
		old := ps.order[0]
		ps.order = ps.order[1:]
		if e := ps.m[old]; e != nil {
			delete(ps.m, old)
			evicted = append(evicted, e)
		}
	}
	if e := ps.m[tok]; e != nil { // the same second rendered the page twice: keep the newest
		evicted = append(evicted, e)
		ps.remove(tok)
	}
	e := &parkEntry[V]{v: p}
	ps.m[tok] = e
	ps.order = append(ps.order, tok)
	e.timer = time.AfterFunc(window, func() { ps.drop(tok) })
	ps.mu.Unlock()
	for _, e := range evicted {
		e.stop()
		e.v.close()
	}
}

// take removes and returns the page parked under tok, nil when none is: a
// token adopts at most once.
func (ps *parkStore[V]) take(tok string) (v V) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	e := ps.m[tok]
	if e == nil {
		return v
	}
	ps.remove(tok)
	e.stop()
	return e.v
}

// remove drops tok from the map and the order; the caller holds the mutex.
func (ps *parkStore[V]) remove(tok string) {
	delete(ps.m, tok)
	for i, t := range ps.order {
		if t == tok {
			ps.order = append(ps.order[:i:i], ps.order[i+1:]...)
			break
		}
	}
}

// drop closes the page parked under tok: its window ran out unadopted.
func (ps *parkStore[V]) drop(tok string) {
	ps.mu.Lock()
	e := ps.m[tok]
	if e != nil {
		ps.remove(tok)
	}
	ps.mu.Unlock()
	if e != nil {
		e.v.close()
	}
}

// closeAll closes every parked session and parks no more: the app stops.
func (ps *parkStore[V]) closeAll() {
	ps.mu.Lock()
	ps.closed = true
	pages := make([]*parkEntry[V], 0, len(ps.m))
	for _, p := range ps.m {
		pages = append(pages, p)
	}
	ps.m, ps.order = nil, nil
	ps.mu.Unlock()
	for _, p := range pages {
		p.stop()
		p.v.close()
	}
}
