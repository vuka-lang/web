package web

import (
	"context"
	"net/url"

	"github.com/vuka-lang/ui/v2"
)

// Query edits the URL of a live page from one of its events: the change is
// a live patch of the page, sent once the event's reply is applied, that
// keeps the other parameters and takes the current history entry's place
// (Push adds one instead). The page then takes the URL as any patch does —
// its Update taking web.URLParams, or else it mounts again — and its
// components' @ui.URL fields fill from it.
//
//	func (t *Requests) Filter(ctx context.Context, status string) {
//		web.Query.Set(ctx, "status", status)
//		web.Query.Del(ctx, "page")
//	}
//
// Several edits in one event are one patch. Outside a live page's event an
// edit does nothing.
var Query QueryEdits

// QueryEdits is the type of Query.
type QueryEdits struct{}

// A QueryOption changes how an edit of the query moves the page.
type QueryOption interface{ queryOption() }

type pushOption struct{}

func (pushOption) queryOption() {}

// Push makes an edit of the query add a history entry, so that back
// returns to the URL before it.
var Push QueryOption = pushOption{}

func pushes(opts []QueryOption) bool {
	for _, o := range opts {
		if _, ok := o.(pushOption); ok {
			return true
		}
	}
	return false
}

// Set sets the query parameter key to value: a string as it is, a number
// or bool as strconv writes it, a type with MarshalText as it writes it,
// a slice as one value per element.
func (QueryEdits) Set(ctx context.Context, key string, value any, opts ...QueryOption) {
	vals := ui.QueryText(value)
	ui.EditURL(ctx, func(q url.Values) {
		if vals == nil {
			q.Del(key)
			return
		}
		q[key] = vals
	}, pushes(opts))
}

// Del removes the query parameter key.
func (QueryEdits) Del(ctx context.Context, key string, opts ...QueryOption) {
	ui.EditURL(ctx, func(q url.Values) { q.Del(key) }, pushes(opts))
}

// Update edits the query in place: edit gets the page's parameters, as the
// event's edits so far left them.
func (QueryEdits) Update(ctx context.Context, edit func(url.Values), opts ...QueryOption) {
	ui.EditURL(ctx, edit, pushes(opts))
}

// Get is the query parameter key of the page whose event (or request) ctx
// belongs to, with the event's edits so far.
func (QueryEdits) Get(ctx context.Context, key string) string {
	return ui.EventQuery(ctx).Get(key)
}
