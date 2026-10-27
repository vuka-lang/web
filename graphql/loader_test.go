package graphql

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The loader tests' domain: users with orders, orders with items, each
// level read through a loader that counts its calls.

type lUser struct{ id int }
type lOrder struct{ id, user int }
type lItem struct{ name string }

type batchLog struct {
	mu    sync.Mutex
	calls map[string][]int // loader → each call's key count
}

func (b *batchLog) add(name string, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls == nil {
		b.calls = map[string][]int{}
	}
	b.calls[name] = append(b.calls[name], n)
}

func (b *batchLog) get(name string) []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls[name])
}

// requestLoader is the request's loader of name, as the framework makes
// one for a field taking a loader type.
func requestLoader[K comparable, V any](ctx context.Context, name string, mk func() *LoaderOf[K, V]) *LoaderOf[K, V] {
	st := stateOf(ctx)
	if st == nil {
		return mk()
	}
	l, _ := st.loader(loaderKey{reflect.TypeFor[*LoaderOf[K, V]](), name}, func() (any, error) { return mk(), nil })
	return l.(*LoaderOf[K, V])
}

type loaderWorld struct {
	log        batchLog
	firstKeys  []int // OrdersByUser's first call's
	users      int
	failOrders error
	slow       bool
	chunk      int
}

func (w *loaderWorld) orders(ctx context.Context) *LoaderOf[int, []lOrder] {
	return requestLoader(ctx, "OrdersByUser", func() *LoaderOf[int, []lOrder] {
		return NewLoader("OrdersByUser", func(ctx context.Context, users []int) (map[int][]lOrder, error) {
			w.log.add("OrdersByUser", len(users))
			w.log.mu.Lock()
			if w.firstKeys == nil {
				w.firstKeys = slices.Clone(users)
			}
			w.log.mu.Unlock()
			if w.slow {
				time.Sleep(5 * time.Millisecond)
			}
			if w.failOrders != nil {
				return nil, w.failOrders
			}
			out := map[int][]lOrder{}
			for _, u := range users {
				if u%10 == 0 {
					continue // no orders: missing from the map
				}
				out[u] = []lOrder{{u*100 + 1, u}, {u*100 + 2, u}}
			}
			return out, nil
		}, Loader{Chunk: w.chunk})
	})
}

func (w *loaderWorld) items(ctx context.Context) *LoaderOf[int, []lItem] {
	return requestLoader(ctx, "ItemsByOrder", func() *LoaderOf[int, []lItem] {
		return NewLoader("ItemsByOrder", func(ctx context.Context, orders []int) (map[int][]lItem, error) {
			w.log.add("ItemsByOrder", len(orders))
			out := map[int][]lItem{}
			for _, o := range orders {
				out[o] = []lItem{{fmt.Sprintf("item of %d", o)}}
			}
			return out, nil
		})
	})
}

func (w *loaderWorld) schema(t *testing.T) *Schema {
	item := &Object{Name: "Item", Fields: []*FieldDef{{Name: "name", Type: String, Trivial: true,
		Resolve: func(_ context.Context, p ResolveParams) (any, error) { return p.Source.(lItem).name, nil }}}}
	order := &Object{Name: "Order"}
	order.Fields = []*FieldDef{
		{Name: "id", Type: Int, Trivial: true, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return p.Source.(lOrder).id, nil }},
		{Name: "items", Type: &List{Of: item, Items: listOf[lItem]}, Resolve: func(ctx context.Context, p ResolveParams) (any, error) {
			return w.items(ctx).Load(ctx, p.Source.(lOrder).id)
		}},
	}
	user := &Object{Name: "User"}
	user.Fields = []*FieldDef{
		{Name: "id", Type: Int, Trivial: true, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return p.Source.(lUser).id, nil }},
		{Name: "orders", Type: &List{Of: &NonNull{Of: order}, Items: listOf[lOrder]}, Resolve: func(ctx context.Context, p ResolveParams) (any, error) {
			return w.orders(ctx).Load(ctx, p.Source.(lUser).id)
		}},
		{Name: "firstOrder", Type: order, Resolve: func(ctx context.Context, p ResolveParams) (any, error) {
			os, err := w.orders(ctx).Load(ctx, p.Source.(lUser).id)
			if err != nil || len(os) == 0 {
				return nil, err
			}
			return os[0], nil
		}},
		{Name: "requiredOrders", Type: &NonNull{Of: &List{Of: order, Items: listOf[lOrder]}}, Resolve: func(ctx context.Context, p ResolveParams) (any, error) {
			return w.orders(ctx).Load(ctx, p.Source.(lUser).id)
		}},
	}
	query := &Object{Name: "Query", Fields: []*FieldDef{
		{Name: "users", Type: &List{Of: user, Items: listOf[lUser]}, Resolve: func(context.Context, ResolveParams) (any, error) {
			out := make([]lUser, w.users)
			for i := range out {
				out[i] = lUser{i + 1}
			}
			return out, nil
		}},
		{Name: "someOrders", Type: &List{Of: order, Items: listOf[lOrder]}, Resolve: func(ctx context.Context, p ResolveParams) (any, error) {
			lists, err := w.orders(ctx).LoadMany(ctx, []int{1, 2, 3, 1})
			var out []lOrder
			for _, l := range lists {
				out = append(out, l...)
			}
			return out, err
		}},
	}}
	s, err := NewSchema(SchemaConfig{Query: query})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func listOf[E any](v any) (int, func(int) any, bool) {
	xs, ok := v.([]E)
	return len(xs), func(i int) any { return xs[i] }, ok
}

func TestLoaderBatchesALevel(t *testing.T) {
	w := &loaderWorld{users: 100}
	res := runWith(t, Config{Schema: w.schema(t), Limits: Limits{Complexity: -1}}, `{ users { id orders { id items { name } } firstOrder { id } } }`)
	if len(res.Errors) > 0 {
		t.Fatal(res.Errors)
	}
	// Every user's orders in one call (orders and firstOrder share the
	// keys, kept), then every order's items in one.
	if got := w.log.get("OrdersByUser"); !slices.Equal(got, []int{100}) {
		t.Errorf("OrdersByUser calls = %v, want [100]", got)
	}
	if got := w.log.get("ItemsByOrder"); !slices.Equal(got, []int{180}) {
		t.Errorf("ItemsByOrder calls = %v, want [180]", got)
	}
	// The keys come in the order of the fields asking for them in the
	// response, whatever order their goroutines ran in.
	if !slices.IsSorted(w.firstKeys) || len(w.firstKeys) != 100 {
		t.Errorf("OrdersByUser's keys aren't in the response's order: %v", w.firstKeys)
	}
	users := res.Data.values[0].([]any)
	u10 := users[9].(*OrderedMap)
	if v, _ := u10.Get("orders"); v == nil || len(v.([]any)) != 0 {
		t.Errorf("a key the loader's map leaves out loads as the zero value, an empty list: got %v", v)
	}
	if v, _ := u10.Get("firstOrder"); v != nil {
		t.Errorf("firstOrder of a user without orders = %v, want null", v)
	}
	u1 := users[0].(*OrderedMap)
	got := jsonOf(t, u1)
	want := `{"id":1,"orders":[{"id":101,"items":[{"name":"item of 101"}]},{"id":102,"items":[{"name":"item of 102"}]}],"firstOrder":{"id":101}}`
	if got != want {
		t.Errorf("user 1 = %s\nwant %s", got, want)
	}
}

func TestLoaderChunks(t *testing.T) {
	w := &loaderWorld{users: 25, chunk: 10}
	res := run(t, w.schema(t), `{ users { orders { id } } }`)
	if len(res.Errors) > 0 {
		t.Fatal(res.Errors)
	}
	if got := w.log.get("OrdersByUser"); !slices.Equal(got, []int{10, 10, 5}) {
		t.Errorf("OrdersByUser calls = %v, want [10 10 5]", got)
	}
}

func TestLoaderErrorAtEachPath(t *testing.T) {
	w := &loaderWorld{users: 3, failOrders: errors.New("orders are down")}
	res := run(t, w.schema(t), `{ users { id firstOrder { id } } }`)
	if len(res.Errors) != 3 {
		t.Fatalf("errors = %v, want one per user", res.Errors)
	}
	for i, e := range res.Errors {
		want := fmt.Sprintf("users.%d.firstOrder", i)
		if pathString(e.Path) != want || e.Message != "orders are down" {
			t.Errorf("error %d = %s at %s, want orders are down at %s", i, e.Message, pathString(e.Path), want)
		}
	}
	if got := w.log.get("OrdersByUser"); !slices.Equal(got, []int{3}) {
		t.Errorf("calls = %v, want one of 3 keys", got)
	}
	if got := jsonOf(t, run(t, w.schema(t), `{ users { id requiredOrders { id } } }`).Data); got != `{"users":[null,null,null]}` {
		t.Errorf("a non-null field failing nulls its user: %s", got)
	}
}

func TestLoaderLoadMany(t *testing.T) {
	w := &loaderWorld{}
	res := run(t, w.schema(t), `{ someOrders { id } }`)
	expectJSON(t, res, `{"data":{"someOrders":[{"id":101},{"id":102},{"id":201},{"id":202},{"id":301},{"id":302},{"id":101},{"id":102}]}}`)
	if got := w.log.get("OrdersByUser"); !slices.Equal(got, []int{3}) {
		t.Errorf("calls = %v, want one of the 3 distinct keys", got)
	}
}

// TestLoaderConcurrent runs many requests at once, a slow loader keeping
// many goroutines waiting, under the race detector in CI: each request's
// batches are its own and stay one per level.
func TestLoaderConcurrent(t *testing.T) {
	w := &loaderWorld{users: 150, slow: true}
	s := w.schema(t)
	srv, err := NewServer(Config{Schema: s, Limits: Limits{Complexity: -1}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var failed atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := srv.Execute(context.Background(), Request{Query: `{ users { orders { items { name } } firstOrder { id } } }`})
			if len(res.Errors) > 0 {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if failed.Load() > 0 {
		t.Fatal("a request failed")
	}
	for _, n := range w.log.get("OrdersByUser") {
		if n != 150 {
			t.Errorf("OrdersByUser calls = %v, want 150 keys each", w.log.get("OrdersByUser"))
			break
		}
	}
	if got := len(w.log.get("ItemsByOrder")); got != 8 {
		t.Errorf("ItemsByOrder ran %d times for 8 requests", got)
	}
}

func TestLoaderOutsideARequest(t *testing.T) {
	var calls [][]int
	l := NewLoader("Squares", func(ctx context.Context, keys []int) (map[int]int, error) {
		calls = append(calls, slices.Clone(keys))
		out := map[int]int{}
		for _, k := range keys {
			out[k] = k * k
		}
		return out, nil
	})
	ctx := context.Background()
	if v, err := l.Load(ctx, 3); v != 9 || err != nil {
		t.Fatalf("Load(3) = %d, %v", v, err)
	}
	vs, err := l.LoadMany(ctx, []int{1, 2, 3})
	if err != nil || !slices.Equal(vs, []int{1, 4, 9}) {
		t.Fatalf("LoadMany = %v, %v", vs, err)
	}
	if fmt.Sprint(calls) != "[[3] [1 2]]" {
		t.Errorf("calls = %v: Load runs at once, LoadMany its new keys together, 3 kept", calls)
	}
}

func TestLoaderPanic(t *testing.T) {
	l := NewLoader("Boom", func(ctx context.Context, keys []int) (map[int]int, error) { panic("no") })
	_, err := l.Load(context.Background(), 1)
	var p *PanicError
	if !errors.As(err, &p) || !strings.Contains(err.Error(), "Boom panicked") {
		t.Fatalf("err = %v, want a PanicError", err)
	}
}

type relPost struct {
	id     int
	author *string
}

func TestRelated(t *testing.T) {
	var calls []int
	load := func(ctx context.Context, posts []*relPost, field string) error {
		calls = append(calls, len(posts))
		for _, p := range posts {
			a := fmt.Sprintf("author of %d (%s)", p.id, field)
			p.author = &a
		}
		return nil
	}
	post := &Object{Name: "Post", Value: func(v any) any {
		if p, ok := v.(relPost); ok {
			return &p
		}
		return v
	}}
	post.Fields = []*FieldDef{{Name: "author", Type: String, Resolve: func(ctx context.Context, p ResolveParams) (any, error) {
		rp := p.Source.(*relPost)
		if rp.author == nil {
			if err := Related(ctx, rp, "Author", load); err != nil {
				return nil, err
			}
		}
		return *rp.author, nil
	}}}
	query := &Object{Name: "Query", Fields: []*FieldDef{{Name: "posts", Type: &List{Of: post, Items: listOf[relPost]},
		Resolve: constant([]relPost{{id: 1}, {id: 2}, {id: 3}})}}}
	s, err := NewSchema(SchemaConfig{Query: query})
	if err != nil {
		t.Fatal(err)
	}
	expectJSON(t, run(t, s, `{ posts { author } }`), `{"data":{"posts":[{"author":"author of 1 (Author)"},{"author":"author of 2 (Author)"},{"author":"author of 3 (Author)"}]}}`)
	if !slices.Equal(calls, []int{3}) {
		t.Errorf("load calls = %v, want one for the 3 posts", calls)
	}
	p := &relPost{id: 9}
	if err := Related(context.Background(), p, "Author", load); err != nil || *p.author != "author of 9 (Author)" {
		t.Errorf("outside a request: %v, %v", err, p.author)
	}
}

func TestTraceFieldsAndBatches(t *testing.T) {
	w := &loaderWorld{users: 4}
	var mu sync.Mutex
	var fields []string
	var batches []string
	tr := &Trace{
		Field: func(f FieldTrace) { mu.Lock(); fields = append(fields, f.Path); mu.Unlock() },
		Batch: func(b BatchTrace) {
			mu.Lock()
			batches = append(batches, fmt.Sprintf("%s:%d", b.Loader, b.Keys))
			mu.Unlock()
		},
	}
	res := runWith(t, Config{Schema: w.schema(t), Trace: func(context.Context) *Trace { return tr }}, `{ users { orders { id } } }`)
	if len(res.Errors) > 0 {
		t.Fatal(res.Errors)
	}
	slices.Sort(fields)
	if want := "users users.0.orders users.1.orders users.2.orders users.3.orders"; strings.Join(fields, " ") != want {
		t.Errorf("fields traced = %v, want %s (trivial fields aren't)", fields, want)
	}
	if !slices.Equal(batches, []string{"OrdersByUser:4"}) {
		t.Errorf("batches = %v", batches)
	}
}
