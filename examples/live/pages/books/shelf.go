// Package books is the shelf, /books, and in [id] a page per book,
// /books/{id}, all in a layout of their own.
package books

import "github.com/vuka-lang/web/v2"

// ShelfParams is what a patch of the shelf's URL brings to Update.
type ShelfParams struct {
	web.URLParams
	Page int // ?page=
}
