package web

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/vuka-lang/ui"
)

// fakeQuery mimics github.com/vuka-lang/orm's QuerySet[T]: value receiver,
// Limit/Offset chain and return the same type, Count and All take a context.
// If Paginate works over it, it works over a real orm.QuerySet[T].
type fakeQuery struct {
	rows       []int
	limit, off int
	hasL, hasO bool
	counts     *int // how many times Count ran
	fetches    *int // how many times All ran
}

func (q fakeQuery) Count(ctx context.Context) (int64, error) {
	if q.counts != nil {
		*q.counts++
	}
	return int64(len(q.rows)), nil
}
func (q fakeQuery) Limit(n int) fakeQuery  { q.limit, q.hasL = n, true; return q }
func (q fakeQuery) Offset(n int) fakeQuery { q.off, q.hasO = n, true; return q }
func (q fakeQuery) All(ctx context.Context) ([]int, error) {
	if q.fetches != nil {
		*q.fetches++
	}
	rows := q.rows
	if q.hasO {
		if q.off < len(rows) {
			rows = rows[q.off:]
		} else {
			rows = nil
		}
	}
	if q.hasL && q.limit < len(rows) {
		rows = rows[:q.limit]
	}
	return rows, nil
}

func seq(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i + 1
	}
	return s
}

func TestPaginateQuery(t *testing.T) {
	ctx := context.Background()
	var counts, fetches int
	q := fakeQuery{rows: seq(42), counts: &counts, fetches: &fetches}

	p, err := Paginate[fakeQuery, int](ctx, q, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 42 || p.Page != 2 || p.Size != 10 || p.Pages != 5 || len(p.Items) != 10 {
		t.Fatalf("page meta: %+v", p)
	}
	if p.Items[0] != 11 || p.Items[9] != 20 {
		t.Fatalf("window: %v", p.Items)
	}
	if counts != 1 || fetches != 1 {
		t.Fatalf("one count + one fetch, got %d + %d", counts, fetches)
	}

	// Page past the end clamps to the last page.
	last, _ := Paginate[fakeQuery, int](ctx, fakeQuery{rows: seq(42)}, 99, 10)
	if last.Page != 5 || last.Items[0] != 41 || len(last.Items) != 2 {
		t.Fatalf("clamp: %+v", last)
	}

	// Empty list: count only, no fetch.
	counts, fetches = 0, 0
	empty, _ := Paginate[fakeQuery, int](ctx, fakeQuery{rows: nil, counts: &counts, fetches: &fetches}, 1, 10)
	if !empty.Empty() || counts != 1 || fetches != 0 {
		t.Fatalf("empty: %+v counts=%d fetches=%d", empty, counts, fetches)
	}
}

// intSource is a Source over the numbers 1..n.
type intSource struct {
	n              int
	counts, slices *int
}

func (s intSource) Count(ctx context.Context) (int, error) {
	if s.counts != nil {
		*s.counts++
	}
	return s.n, nil
}
func (s intSource) Slice(ctx context.Context, limit, offset int) ([]int, error) {
	if s.slices != nil {
		*s.slices++
	}
	all := seq(s.n)
	if offset > len(all) {
		offset = len(all)
	}
	all = all[offset:]
	if limit < len(all) {
		all = all[:limit]
	}
	return all, nil
}

func TestPaginateSource(t *testing.T) {
	ctx := context.Background()
	var counts, slices int
	p, err := PaginateSource[int](ctx, intSource{n: 25, counts: &counts, slices: &slices}, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if p.Page != 3 || p.Pages != 3 || len(p.Items) != 5 || p.Items[0] != 21 {
		t.Fatalf("source page: %+v", p)
	}
	if counts != 1 || slices != 1 {
		t.Fatalf("one count + one slice, got %d + %d", counts, slices)
	}
}

func TestSlice(t *testing.T) {
	p := Slice(seq(12), 2, 5)
	if p.Page != 2 || p.Total != 12 || len(p.Items) != 5 || p.Items[0] != 6 || p.Items[4] != 10 {
		t.Fatalf("slice: %+v", p)
	}
	tail := Slice(seq(12), 3, 5)
	if len(tail.Items) != 2 || tail.Items[0] != 11 {
		t.Fatalf("tail: %+v", tail)
	}
}

func TestPageParamsAndQuery(t *testing.T) {
	// Defaults and the cap.
	if page, size := (PageParams{}).Normalize(); page != 1 || size != DefaultPageSize {
		t.Fatalf("defaults: %d %d", page, size)
	}
	if _, size := (PageParams{Size: 9999}).Normalize(); size != MaxPageSize {
		t.Fatalf("cap: %d", size)
	}
	if page, size := (PageParams{Page: 4, Size: 15}).Normalize(); page != 4 || size != 15 {
		t.Fatalf("given: %d %d", page, size)
	}
	// PageQuery off the request.
	r := httptest.NewRequest("GET", "/list?page=3&size=25", nil)
	if page, size := PageQuery(r); page != 3 || size != 25 {
		t.Fatalf("query: %d %d", page, size)
	}
	r2 := httptest.NewRequest("GET", "/list", nil)
	if page, size := PageQuery(r2); page != 1 || size != DefaultPageSize {
		t.Fatalf("query defaults: %d %d", page, size)
	}
}

func TestPager(t *testing.T) {
	ctx := context.Background()
	var loads int
	src := intSource{n: 23}
	p := NewPager(5, func(ctx context.Context, page, size int) (ui.Page[int], error) {
		loads++
		return PaginateSource[int](ctx, src, page, size)
	})
	if err := p.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if p.Page() != 1 || p.Result().Items[0] != 1 || p.Result().Pages != 5 {
		t.Fatalf("first load: page %d %+v", p.Page(), p.Result())
	}
	if err := p.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if p.Page() != 2 || p.Result().Items[0] != 6 {
		t.Fatalf("next: %d %v", p.Page(), p.Result().Items)
	}
	if err := p.GoTo(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if p.Page() != 5 || len(p.Result().Items) != 3 || !p.Result().HasPrev || p.Result().HasNext {
		t.Fatalf("goto last: %d %+v", p.Page(), p.Result())
	}
	// Next past the end: the result clamps and the pager adopts it.
	if err := p.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if p.Page() != 5 {
		t.Fatalf("next past end did not clamp: %d", p.Page())
	}
	if err := p.Prev(ctx); err != nil {
		t.Fatal(err)
	}
	if p.Page() != 4 {
		t.Fatalf("prev: %d", p.Page())
	}
	if err := p.SetSize(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if p.Size() != 10 || p.Page() != 1 || p.Result().Pages != 3 {
		t.Fatalf("set size: size %d page %d pages %d", p.Size(), p.Page(), p.Result().Pages)
	}
}

// TestPaginateInference checks Paginate's type arguments infer from the query,
// so callers write Paginate(ctx, q, page, size).
func TestPaginateInference(t *testing.T) {
	p, err := Paginate(context.Background(), fakeQuery{rows: seq(8)}, 1, 5)
	if err != nil || p.Total != 8 || len(p.Items) != 5 {
		t.Fatalf("inference: %+v %v", p, err)
	}
}
