package web

import (
	"context"
	"runtime"
	"sync"
)

// broadcastGate bounds the renders broadcasts run at once, process-wide, so a
// storm on one topic queues behind its own subscribers' turns and leaves the
// processors to the events of other pages.
var broadcastGate gate

// gate is a counting semaphore of 2 × GOMAXPROCS permits, unless reset.
type gate struct {
	mu sync.Mutex
	ch chan struct{}
}

// reset makes the gate hold n permits (the default when n is 0).
func (g *gate) reset(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ch = nil
	if n > 0 {
		g.ch = make(chan struct{}, n)
	}
}

// acquire takes a permit: the function giving it back, nil when ctx ended first.
func (g *gate) acquire(ctx context.Context) func() {
	g.mu.Lock()
	if g.ch == nil {
		g.ch = make(chan struct{}, 2*runtime.GOMAXPROCS(0))
	}
	ch := g.ch
	g.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }
	case <-ctx.Done():
		return nil
	}
}
