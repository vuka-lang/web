package web

import (
	"context"
	"sync"
)

// liveQueue is how many client messages a connection's reader holds while
// the loop is busy; beyond it the reader waits.
const liveQueue = 64

// clientMsg is a client message as the reader passed it on: leaves when it is
// a navigate that leaves the page (conn.leaveEarly).
type clientMsg struct {
	data   []byte
	leaves bool
}

// leaveCtx is a page's context whose renders a navigate leaving the page
// ends without ending the page: end closes the current Done, and renew opens
// a new one before the next handler runs, so each handler sees a context of
// its own that is live. Contexts derived from it keep the Done they were made
// with. It ends for good with its parent.
type leaveCtx struct {
	context.Context
	mu   sync.Mutex
	done chan struct{}
	shut bool // done is closed
	gone bool // the parent ended
}

func newLeaveCtx(parent context.Context) *leaveCtx {
	l := &leaveCtx{Context: parent, done: make(chan struct{})}
	context.AfterFunc(parent, func() {
		l.mu.Lock()
		l.gone = true
		l.closeLocked()
		l.mu.Unlock()
	})
	return l
}

func (l *leaveCtx) Done() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.done
}

func (l *leaveCtx) Err() error {
	if err := l.Context.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shut {
		return context.Canceled
	}
	return nil
}

// end ends what runs with the context now.
func (l *leaveCtx) end() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.closeLocked()
	l.mu.Unlock()
}

// renew makes the context live again after end, unless its parent ended.
func (l *leaveCtx) renew() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.shut && !l.gone {
		l.done, l.shut = make(chan struct{}), false
	}
	l.mu.Unlock()
}

func (l *leaveCtx) closeLocked() {
	if !l.shut {
		close(l.done)
		l.shut = true
	}
}
