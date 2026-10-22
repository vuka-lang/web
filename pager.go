package web

import (
	"context"

	"github.com/vuka-lang/ui/v2"
)

// Pager is a live view's paging state: the current page and size, and the
// events a ui.DataTable's controls call (GoTo, Next, Prev, SetSize). A view
// keeps one, gives it a loader that fetches a page, and renders Result — so it
// never hand-rolls the page arithmetic or the event handlers:
//
//	type Books struct {
//		web.Live        // ui.Live
//		DB    *DB       // injected
//		pager *web.Pager[Book]
//	}
//
//	@ui.Mount
//	func (b *Books) mount(ctx context.Context) error {
//		b.pager = web.NewPager(20, func(ctx context.Context, page, size int) (ui.Page[Book], error) {
//			return web.Paginate(ctx, orm.For[Book](), page, size)
//		})
//		return b.pager.Load(ctx)
//	}
//
//	func (b *Books) Render() ui.Node {
//		return ui.DataTable[Book]{
//			Columns: cols,
//			Page:    b.pager.Result(),
//			OnPage:  b.pager.GoTo,  // func(context.Context, int) error
//		}
//	}
//
// The loader is called with the page and size the pager holds; after each
// load the pager adopts the page the result settled on, so Next past the last
// page is a no-op (and the DataTable disables it anyway).
type Pager[T any] struct {
	page, size int
	load       func(ctx context.Context, page, size int) (ui.Page[T], error)
	result     ui.Page[T]
	err        error
}

// NewPager makes a pager starting on page 1 with the given size (or
// DefaultPageSize when size < 1); load fetches one page.
func NewPager[T any](size int, load func(ctx context.Context, page, size int) (ui.Page[T], error)) *Pager[T] {
	if size < 1 {
		size = DefaultPageSize
	}
	return &Pager[T]{page: 1, size: size, load: load}
}

// Load fetches the current page, remembering the result and any error, and
// clamps the current page to the page the result settled on.
func (p *Pager[T]) Load(ctx context.Context) error {
	res, err := p.load(ctx, p.page, p.size)
	p.err = err
	if err != nil {
		return err
	}
	p.result = res
	p.page = res.Page
	return nil
}

// Reload returns to page 1 and fetches it, after the data behind the pager
// changed (a new search or sort).
func (p *Pager[T]) Reload(ctx context.Context) error {
	p.page = 1
	return p.Load(ctx)
}

// GoTo moves to page n (1-based) and fetches it.
func (p *Pager[T]) GoTo(ctx context.Context, n int) error {
	if n < 1 {
		n = 1
	}
	p.page = n
	return p.Load(ctx)
}

// Next moves to the following page and fetches it.
func (p *Pager[T]) Next(ctx context.Context) error { return p.GoTo(ctx, p.page+1) }

// Prev moves to the preceding page and fetches it.
func (p *Pager[T]) Prev(ctx context.Context) error { return p.GoTo(ctx, p.page-1) }

// SetSize changes the page size, returns to page 1 and fetches it.
func (p *Pager[T]) SetSize(ctx context.Context, size int) error {
	if size < 1 {
		size = DefaultPageSize
	}
	p.size = size
	p.page = 1
	return p.Load(ctx)
}

// Result is the last loaded page (the zero Page before the first Load).
func (p *Pager[T]) Result() ui.Page[T] { return p.result }

// Err is the error of the last Load, if any.
func (p *Pager[T]) Err() error { return p.err }

// Page is the current page number.
func (p *Pager[T]) Page() int { return p.page }

// Size is the current page size.
func (p *Pager[T]) Size() int { return p.size }
