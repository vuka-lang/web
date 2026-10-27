package graphql

import (
	"context"
	"reflect"
	"sync"
)

// sched runs an execution's work: the goroutines resolving fields and
// list items, and the loaders' batches. It knows how many of them are
// running — not waiting for a loader's batch or for the work they started
// — and when none is, it dispatches every batch queued: by then each field
// that could ask for a key has asked, so a batch holds every key a level
// of the query (or more: every branch waiting) needs.
//
// slots bounds the goroutines running at once; a goroutine waiting gives
// its slot up, so a field waiting for a batch never keeps the others from
// asking for their keys.
type sched struct {
	ctx   context.Context // the request's: the batches run with it
	slots chan struct{}

	mu     sync.Mutex
	active int
	queue  []batchRun
}

// batchRun is a loader's queued batch, run when the execution idles.
type batchRun interface {
	runBatch(ctx context.Context, s *sched)
}

func newSched(ctx context.Context, n int) *sched {
	s := &sched{ctx: ctx, slots: make(chan struct{}, n), active: 1}
	s.slots <- struct{}{} // the execution's own goroutine
	return s
}

// spawn runs fn on a goroutine of its own, wg counting it.
func (s *sched) spawn(wg *sync.WaitGroup, fn func()) {
	s.mu.Lock()
	s.active++
	s.mu.Unlock()
	wg.Add(1)
	go func() {
		s.slots <- struct{}{}
		defer func() {
			<-s.slots
			wg.Done()
			s.idle(1)
		}()
		fn()
	}()
}

// wait waits for the goroutines wg counts, its slot given up meanwhile.
func (s *sched) wait(wg *sync.WaitGroup) {
	<-s.slots
	s.idle(1)
	wg.Wait()
	s.mu.Lock()
	s.active++
	s.mu.Unlock()
	s.slots <- struct{}{}
}

// enqueue queues a loader's batch, to run when the execution idles.
func (s *sched) enqueue(b batchRun) {
	s.mu.Lock()
	s.queue = append(s.queue, b)
	s.mu.Unlock()
}

// block is a goroutine starting to wait for a batch: its slot given up,
// it no longer counts as running. resume takes the slot back once the
// batch woke it (counting it running again, wake).
func (s *sched) block() {
	<-s.slots
	s.idle(1)
}

func (s *sched) resume() { s.slots <- struct{}{} }

// wake counts n goroutines a batch is about to wake as running, before
// they run, so the execution never looks idle in between.
func (s *sched) wake(n int) {
	s.mu.Lock()
	s.active += n
	s.mu.Unlock()
}

// idle counts n goroutines no longer running; when none is, the queued
// batches run, each on a goroutine of its own.
func (s *sched) idle(n int) {
	s.mu.Lock()
	s.active -= n
	var runs []batchRun
	if s.active == 0 && len(s.queue) > 0 {
		runs, s.queue = s.queue, nil
		s.active += len(runs)
	}
	s.mu.Unlock()
	for _, b := range runs {
		go func() {
			s.slots <- struct{}{}
			b.runBatch(s.ctx, s)
			<-s.slots
			s.idle(1)
		}()
	}
}

// state is a request's: its scheduler, and the loaders made for it.
type state struct {
	sched *sched
	trace *tracer // nil unless the toolbar records the request

	mu      sync.Mutex
	loaders map[loaderKey]any
}

type loaderKey struct {
	t    reflect.Type
	name string
}

type stateKey struct{}

func stateOf(ctx context.Context) *state {
	st, _ := ctx.Value(stateKey{}).(*state)
	return st
}

// loader is the request's loader of key k, made by make the first time.
func (st *state) loader(k loaderKey, make func() (any, error)) (any, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if l, ok := st.loaders[k]; ok {
		return l, nil
	}
	l, err := make()
	if err != nil {
		return nil, err
	}
	if st.loaders == nil {
		st.loaders = map[loaderKey]any{}
	}
	st.loaders[k] = l
	return l, nil
}
