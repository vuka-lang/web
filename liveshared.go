package web

import (
	"context"
	"log/slog"
	"reflect"
	"sync"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
)

// sharedTopicPrefix marks the topic a broadcast to a shared component
// (ui.Broadcast) travels under, through the relay too: the prefix and the
// component type's stable name (ui.SharedName). No page subscribes to it.
const sharedTopicPrefix = "\x00shared:"

// sharedCtx is the context a shared component renders with: the app's
// injector and nothing of a viewer.
func (a *App) sharedCtx(string) context.Context {
	return ui.WithInjector(context.Background(), injector{a})
}

// BroadcastShared delivers msg to the shared component typ, as ui.Broadcast
// asks of the app serving its context.
func (a *App) BroadcastShared(ctx context.Context, typ reflect.Type, msg any) error {
	return a.Broadcast(ctx, sharedTopicPrefix+ui.SharedName(typ), msg)
}

// sharedRun is a shared component's side of the transport: the broadcasts
// waiting for it, which a pump goroutine renders in batches (one render, one
// diff and one encoding for each), and the connections it fans the frame to.
type sharedRun struct {
	app *App
	typ reflect.Type

	mu    sync.Mutex
	inbox []live.Delivery
	run   bool

	subs map[*conn]struct{} // guarded by the fragment's lock (live.Shared.Do, Publish)
}

func (a *App) sharedRunOf(typ reflect.Type) *sharedRun {
	if r, ok := a.sharedRuns.Load(typ); ok {
		return r.(*sharedRun)
	}
	r, _ := a.sharedRuns.LoadOrStore(typ, &sharedRun{app: a, typ: typ, subs: map[*conn]struct{}{}})
	return r.(*sharedRun)
}

// onShared is the hub's delivery of a broadcast to the shared component name.
func (a *App) onShared(name string, msg any) {
	typ, ok := ui.SharedByName(name)
	if !ok {
		slog.Warn("web: a broadcast to a shared component this process doesn't know: call ui.RegisterShared(&T{}) from an init so a replica mounts it before a page shows it", "component", name)
		return
	}
	a.sharedRunOf(typ).post(msg)
}

func (r *sharedRun) post(msg any) {
	r.mu.Lock()
	r.inbox = append(r.inbox, live.Message(msg))
	start := !r.run
	r.run = true
	r.mu.Unlock()
	if start {
		go r.pump()
	}
}

func (r *sharedRun) pump() {
	for {
		r.mu.Lock()
		batch := r.inbox
		r.inbox = nil
		if len(batch) == 0 {
			r.run = false
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		r.publish(batch)
	}
}

func (r *sharedRun) publish(batch []live.Delivery) {
	release := broadcastGate.acquire(context.Background())
	if release == nil {
		return
	}
	defer release()
	sh, err := r.app.shared.ForType(r.typ)
	if err == nil {
		err = sh.Publish(batch, r.fan)
	}
	if err != nil {
		slog.Error("web: a broadcast to a shared component failed", "component", ui.SharedName(r.typ), "err", err)
	}
}

func (r *sharedRun) fan(f live.SharedFrame) {
	for c := range r.subs {
		c.sharedDeliver(r, f)
	}
}

// sharedSt is a connection's place in a shared component's frames: the
// version its browser holds.
type sharedSt struct {
	ver    uint64
	active bool // the message that carried the snapshot is queued
	missed bool // a frame came before it was
}

// Attach registers the connection for sh's frames and returns its snapshot,
// for the message that carries the page's reference to it (live.SharedLinks).
func (c *conn) Attach(sh *live.Shared) []live.TreeUpdate {
	r := c.app.sharedRunOf(sh.Type())
	var trees []live.TreeUpdate
	sh.Do(func(snapshot func() *live.SharedSnapshot) {
		snap := snapshot()
		c.smu.Lock()
		if c.sh == nil {
			c.sh = map[*sharedRun]*sharedSt{}
		}
		c.sh[r] = &sharedSt{ver: snap.Ver}
		c.learn(snap.Uses)
		c.smu.Unlock()
		r.subs[c] = struct{}{}
		trees = snap.Trees
	})
	c.pendingShared.Store(true)
	return trees
}

// Detach stops sh's frames to the connection.
func (c *conn) Detach(sh *live.Shared) {
	r := c.app.sharedRunOf(sh.Type())
	sh.Do(func(func() *live.SharedSnapshot) {
		delete(r.subs, c)
		c.smu.Lock()
		delete(c.sh, r)
		c.smu.Unlock()
	})
}

// dropShared detaches the connection from every shared component it shows.
func (c *conn) dropShared() {
	c.smu.Lock()
	runs := make([]*sharedRun, 0, len(c.sh))
	for r := range c.sh {
		runs = append(runs, r)
	}
	c.smu.Unlock()
	for _, r := range runs {
		if sh, err := c.app.shared.ForType(r.typ); err == nil {
			c.Detach(sh)
		}
	}
}

// sharedDeliver queues a frame for the connection: as it is when the browser
// holds the version it applies to and the statics it needs, else the
// component's snapshot, after which it is at the frame's version. Called with
// the fragment locked.
func (c *conn) sharedDeliver(r *sharedRun, f live.SharedFrame) {
	c.smu.Lock()
	defer c.smu.Unlock()
	st := c.sh[r]
	if st == nil || st.ver >= f.Ver {
		return
	}
	if !st.active {
		st.missed = true
		return
	}
	if st.ver == f.Base && c.knows(f.Uses) {
		c.sendShared(f.Msg)
		st.ver = f.Ver
		return
	}
	snap := f.Snapshot()
	c.learn(snap.Uses)
	c.sendShared(snap.Msg)
	st.ver = snap.Ver
}

// activateShared is called once the message carrying a snapshot is queued:
// frames from here on are sent, and a browser that missed some in between gets
// the snapshot again.
func (c *conn) activateShared() {
	c.pendingShared.Store(false)
	c.smu.Lock()
	var late []*sharedRun
	for r, st := range c.sh {
		if !st.active {
			st.active = true
			if st.missed {
				late = append(late, r)
			}
		}
	}
	c.smu.Unlock()
	for _, r := range late {
		sh, err := c.app.shared.ForType(r.typ)
		if err != nil {
			continue
		}
		sh.Do(func(snapshot func() *live.SharedSnapshot) {
			c.smu.Lock()
			defer c.smu.Unlock()
			if st := c.sh[r]; st != nil {
				if snap := snapshot(); st.ver < snap.Ver {
					c.learn(snap.Uses)
					c.sendShared(snap.Msg)
					st.ver = snap.Ver
				}
			}
		})
	}
}

// sendShared queues a message shared by every connection; a queue that is
// full closes this browser, from a goroutine of its own: the publisher
// waits for no write.
func (c *conn) sendShared(b []byte) {
	o := &c.out
	o.mu.Lock()
	if o.dead {
		o.mu.Unlock()
		return
	}
	if len(o.q) >= liveOutFrames {
		o.mu.Unlock()
		if c.overflowing.CompareAndSwap(false, true) {
			go c.overflow("a shared frame found the browser's queue of frames full")
		}
		return
	}
	o.q = append(o.q, b)
	start := !o.run
	o.run = true
	o.mu.Unlock()
	if start {
		go c.flushOut()
	}
}

func (c *conn) knows(uses []int) bool {
	for _, id := range uses {
		i := id - live.SharedStaticsBase
		if w := i / 64; w >= len(c.known) || c.known[w]&(1<<(i%64)) == 0 {
			return false
		}
	}
	return true
}

func (c *conn) learn(uses []int) {
	for _, id := range uses {
		i := id - live.SharedStaticsBase
		for i/64 >= len(c.known) {
			c.known = append(c.known, 0)
		}
		c.known[i/64] |= 1 << (i % 64)
	}
}
