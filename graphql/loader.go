package graphql

import (
	"context"
	"fmt"
	"reflect"
	"runtime/debug"
	"sync"
	"time"
)

// Loader makes a function a loader, @graphql.Loader: a function from a
// request's keys to their values,
//
//	@graphql.Loader
//	func OrdersByUser(ctx context.Context, userIDs []int64, db *DB) (map[int64][]Order, error) { … }
//
// becomes a type of that name, a *LoaderOf[int64, []Order] that field
// methods and operations take as a dependency and Load from:
//
//	func (u User) Orders(ctx context.Context, orders OrdersByUser) ([]Order, error) {
//		return orders.Load(ctx, u.ID)
//	}
//
// The function itself keeps its body under its name lowered (ordersByUser),
// and NewOrdersByUser(db) makes a loader outside GraphQL. Chunk is how many
// keys one call of the function takes at most; 0 is DefaultChunk.
type Loader struct {
	Chunk int
}

func (Loader) vukaTargets() [0]struct{} { return [0]struct{}{} }

// DefaultChunk is how many keys one call of a loader's function takes at
// most unless @graphql.Loader{Chunk: n} says otherwise: more keys are split
// into calls of that many.
const DefaultChunk = 1000

// LoaderOf loads values by key in batches: every key a request's fields ask
// for while it runs, gathered into one call of its function — once each
// field that can ask for one is waiting, so a batch holds the keys of a
// whole level of the query — the values kept for the rest of the request.
//
// A key the function's map leaves out loads as V's zero value (nil for a
// pointer or a slice). The function's error is each waiting Load's, and
// each field's at its own path.
//
// Outside a GraphQL request — a live page's Mount, a route — there is no
// level to wait for: Load runs the function for its key at once and
// LoadMany for its keys together, keeping what they load for the loader's
// life.
type LoaderOf[K comparable, V any] struct {
	name  string
	fetch func(context.Context, []K) (map[K]V, error)
	chunk int

	mu   sync.Mutex
	got  map[K]*loadEntry[V] // every key asked for: loaded or loading
	next *loadBatch[K, V]    // keys asked for since the last batch ran
}

type loadEntry[V any] struct {
	v     V
	err   error
	w     *batchWaiters // its batch's
	ready chan struct{}
}

// batchWaiters counts the goroutines waiting for a batch, for the
// scheduler to count them running again when it is done.
type batchWaiters struct {
	waiters int
	done    bool
}

type loadBatch[K comparable, V any] struct {
	l       *LoaderOf[K, V]
	keys    []K
	entries []*loadEntry[V]
	w       *batchWaiters
}

// NewLoader is a loader of fetch's values, named name in the toolbar's
// traces. Each LoaderOf keeps what it loads: make one per request (the
// framework does for a GraphQL request), or for whatever lives as long as
// what it loads may stay as loaded.
func NewLoader[K comparable, V any](name string, fetch func(ctx context.Context, keys []K) (map[K]V, error), opts ...Loader) *LoaderOf[K, V] {
	l := &LoaderOf[K, V]{name: name, fetch: fetch, chunk: DefaultChunk, got: map[K]*loadEntry[V]{}}
	for _, o := range opts {
		if o.Chunk > 0 {
			l.chunk = o.Chunk
		}
	}
	return l
}

// Load is key's value: loaded with the other keys the request's fields ask
// for meanwhile, or kept from an earlier load.
func (l *LoaderOf[K, V]) Load(ctx context.Context, key K) (V, error) {
	st := stateOf(ctx)
	l.mu.Lock()
	e, w := l.ask(key, st)
	l.mu.Unlock()
	if w != nil && st == nil {
		l.run(ctx, nil, l.take())
	}
	return l.await(st, e)
}

// LoadMany is the values of keys, in their order, loaded together (with
// the other keys the request asks for meanwhile). The first error is its
// error.
func (l *LoaderOf[K, V]) LoadMany(ctx context.Context, keys []K) ([]V, error) {
	st := stateOf(ctx)
	entries := make([]*loadEntry[V], len(keys))
	queued := false
	l.mu.Lock()
	for i, k := range keys {
		var w *batchWaiters
		entries[i], w = l.ask(k, st)
		queued = queued || w != nil
	}
	l.mu.Unlock()
	if queued && st == nil {
		l.run(ctx, nil, l.take())
	}
	out := make([]V, len(keys))
	for i, e := range entries {
		v, err := l.await(st, e)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// ask is key's entry, queued in the next batch when nothing has asked for
// it yet (then the batch is returned too). l.mu is held.
func (l *LoaderOf[K, V]) ask(key K, st *state) (*loadEntry[V], *batchWaiters) {
	if e := l.got[key]; e != nil {
		return e, nil
	}
	if l.next == nil {
		l.next = &loadBatch[K, V]{l: l, w: &batchWaiters{}}
		if st != nil {
			st.sched.enqueue(l.next)
		}
	}
	e := &loadEntry[V]{w: l.next.w, ready: make(chan struct{})}
	l.got[key] = e
	l.next.keys = append(l.next.keys, key)
	l.next.entries = append(l.next.entries, e)
	return e, l.next.w
}

// take is the next batch, no longer next.
func (l *LoaderOf[K, V]) take() *loadBatch[K, V] {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.next
	l.next = nil
	return b
}

// await is e's value once loaded: in a request, the goroutine waits as the
// scheduler counts it, so the batch runs once nothing else can ask.
func (l *LoaderOf[K, V]) await(st *state, e *loadEntry[V]) (V, error) {
	select {
	case <-e.ready:
		return e.v, e.err
	default:
	}
	if st == nil {
		<-e.ready
		return e.v, e.err
	}
	l.mu.Lock()
	if e.w.done {
		l.mu.Unlock()
		<-e.ready
		return e.v, e.err
	}
	e.w.waiters++
	l.mu.Unlock()
	st.sched.block()
	<-e.ready
	st.sched.resume()
	return e.v, e.err
}

func (b *loadBatch[K, V]) runBatch(ctx context.Context, s *sched) {
	b.l.mu.Lock()
	if b.l.next == b {
		b.l.next = nil
	}
	b.l.mu.Unlock()
	b.l.run(ctx, s, b)
}

// run calls the loader's function for b's keys, chunk by chunk, and wakes
// what waits for them.
func (l *LoaderOf[K, V]) run(ctx context.Context, s *sched, b *loadBatch[K, V]) {
	if b == nil {
		return
	}
	start := time.Now()
	st := stateOf(ctx)
	if st != nil && st.trace != nil {
		ctx = st.trace.context(ctx, "loader "+l.name)
	}
	var err error
	for i := 0; i < len(b.keys) && err == nil; i += l.chunk {
		end := min(i+l.chunk, len(b.keys))
		var got map[K]V
		got, err = l.call(ctx, b.keys[i:end])
		if err != nil {
			break
		}
		for j := i; j < end; j++ {
			b.entries[j].v = got[b.keys[j]]
		}
	}
	if err != nil {
		var zero V
		for _, e := range b.entries {
			e.v, e.err = zero, err
		}
	}
	if st != nil && st.trace != nil {
		st.trace.batch(l.name, len(b.keys), start, time.Since(start), err)
	}
	l.mu.Lock()
	n := b.w.waiters
	b.w.done = true
	l.mu.Unlock()
	if s != nil {
		s.wake(n)
	}
	for _, e := range b.entries {
		close(e.ready)
	}
}

// call is one call of the function, its panic an error.
func (l *LoaderOf[K, V]) call(ctx context.Context, keys []K) (got map[K]V, err error) {
	defer func() {
		if r := recover(); r != nil {
			got, err = nil, &PanicError{Field: l.name, Value: r, Stack: debug.Stack()}
		}
	}()
	return l.fetch(ctx, keys)
}

// LoaderDef is a loader type of a package: the type @graphql.Loader made,
// the dependencies its function takes beside the context and the keys, and
// how New makes one from their values. A GraphQL request makes one of each
// type a field or an operation takes, the first time one does.
type LoaderDef struct {
	Go     string       // the type's name
	Type   reflect.Type // the type
	Params []Param      // its function's dependencies, after the context and the keys
	New    func(c *Call) any
}

// Related is a relation's rows read into parent: load, given the parents of
// a request's level that need them, reads them into each; outside a request
// it is called with parent alone. field names the relation (the toolbar's
// traces show it); the compiler generates Related's calls for the fields of
// an orm model holding a relation.
func Related[P any](ctx context.Context, parent *P, field string, load func(ctx context.Context, parents []*P, field string) error) error {
	st := stateOf(ctx)
	if st == nil {
		return load(ctx, []*P{parent}, field)
	}
	key := loaderKey{reflect.TypeFor[P](), field}
	l, _ := st.loader(key, func() (any, error) {
		name := fmt.Sprintf("%s.%s", typeName(reflect.TypeFor[P]()), field)
		return NewLoader(name, func(ctx context.Context, parents []*P) (map[*P]struct{}, error) {
			return nil, load(ctx, parents, field)
		}), nil
	})
	_, err := l.(*LoaderOf[*P, struct{}]).Load(ctx, parent)
	return err
}
