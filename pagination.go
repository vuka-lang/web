package web

import (
	"context"
	"net/http"
	"strconv"

	"github.com/vuka-lang/ui"
)

// DefaultPageSize is the page size Paginate and the request binders use when
// none is given or asked for.
const DefaultPageSize = 20

// MaxPageSize caps a size taken from the request, so a client can't ask for an
// unbounded page.
const MaxPageSize = 200

// Source is where a paginator reads: the total number of rows a query matches,
// and one window of them. It has no dependency on any ORM, so web does not
// import one; an orm.QuerySet[T] is adapted by Paginate (it satisfies Query),
// and any store can implement Source directly.
type Source[T any] interface {
	Count(ctx context.Context) (int, error)
	Slice(ctx context.Context, limit, offset int) ([]T, error)
}

// Pageable is the shape of an ORM query a paginator needs: count the matches,
// narrow to a window with Limit and Offset, and fetch it with All. Q is the
// query's own type, so Limit and Offset chain. github.com/vuka-lang/orm's
// QuerySet[T] satisfies Pageable[orm.QuerySet[T], T], so Paginate takes one
// directly with no import of orm here.
type Pageable[Q any, T any] interface {
	Count(ctx context.Context) (int64, error)
	Limit(n int) Q
	Offset(n int) Q
	All(ctx context.Context) ([]T, error)
}

// Paginate reads page (1-based) and size from q — an ORM query (orm.For[T](),
// a QuerySet) or anything matching Query — with one COUNT and one windowed
// fetch, and returns the page:
//
//	p, err := web.Paginate(ctx, orm.For[Book]().Where(…), page, size)
//	for _, b := range p.Items { … }
//
// page and size are clamped (size to at least 1, page to the list); it does
// not run the window fetch when the count is zero.
func Paginate[Q Pageable[Q, T], T any](ctx context.Context, q Q, page, size int) (ui.Page[T], error) {
	if size < 1 {
		size = DefaultPageSize
	}
	total64, err := q.Count(ctx)
	if err != nil {
		return ui.Page[T]{}, err
	}
	total := int(total64)
	page = clampPage(page, total, size)
	if total == 0 {
		return ui.NewPage([]T(nil), 0, page, size), nil
	}
	items, err := q.Limit(size).Offset((page - 1) * size).All(ctx)
	if err != nil {
		return ui.Page[T]{}, err
	}
	return ui.NewPage(items, total, page, size), nil
}

// PaginateSource is Paginate over a Source: a store that counts and slices
// itself, with no ORM.
func PaginateSource[T any](ctx context.Context, src Source[T], page, size int) (ui.Page[T], error) {
	if size < 1 {
		size = DefaultPageSize
	}
	total, err := src.Count(ctx)
	if err != nil {
		return ui.Page[T]{}, err
	}
	page = clampPage(page, total, size)
	if total == 0 {
		return ui.NewPage([]T(nil), 0, page, size), nil
	}
	items, err := src.Slice(ctx, size, (page-1)*size)
	if err != nil {
		return ui.Page[T]{}, err
	}
	return ui.NewPage(items, total, page, size), nil
}

// Slice paginates a slice already in memory, for small lists and tests.
func Slice[T any](rows []T, page, size int) ui.Page[T] {
	if size < 1 {
		size = DefaultPageSize
	}
	total := len(rows)
	page = clampPage(page, total, size)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	return ui.NewPage(rows[start:end], total, page, size)
}

func clampPage(page, total, size int) int {
	if page < 1 {
		page = 1
	}
	pages := (total + size - 1) / size
	if pages > 0 && page > pages {
		page = pages
	}
	return page
}

// PageParams binds ?page= and ?size= for a plain handler:
//
//	func List(p web.PageParams, …) (…) {
//		page, size := p.Normalize()
//		…
//	}
//
// Its tags let it bind as a handler's request struct; Normalize applies the
// defaults and the size cap.
type PageParams struct {
	Page int `form:"page" json:"page"`
	Size int `form:"size" json:"size"`
}

// Normalize returns the 1-based page and the size, defaulting size to
// DefaultPageSize and capping it at MaxPageSize.
func (p PageParams) Normalize() (page, size int) {
	return normPage(p.Page), normSize(p.Size)
}

// PageQuery reads page and size straight from a request's query string, for a
// handler that took the *http.Request rather than a bound struct.
func PageQuery(r *http.Request) (page, size int) {
	q := r.URL.Query()
	page, _ = strconv.Atoi(q.Get("page"))
	size, _ = strconv.Atoi(q.Get("size"))
	return normPage(page), normSize(size)
}

func normPage(p int) int {
	if p < 1 {
		return 1
	}
	return p
}

func normSize(s int) int {
	switch {
	case s < 1:
		return DefaultPageSize
	case s > MaxPageSize:
		return MaxPageSize
	default:
		return s
	}
}
