package web

import (
	"net/url"
	"slices"
	"strings"
)

// A patch navigation of a page that has no Update, to its own path with only
// the query changed, keeps the page when every parameter that changed is one
// its rendered components keep in the address (a @ui.URL field, a DataTable's
// page, search or sort): they read the new address as they render, which is
// all an Update would do. A pager's link or a link between two batches moves
// the page in place that way, instead of mounting it again with its loads.
// Any other parameter changing, or none changing, is a navigation as ever.

// urlOwner is a session that knows the parameters its components keep in the
// address.
type urlOwner interface {
	URLParams() map[string]bool
}

// urlSnap is the address the page shows and the parameters its components
// keep in it, as of the last message served; leaveEarly reads it from the
// socket's reader.
type urlSnap struct {
	path  string
	query url.Values
	owned map[string]bool
}

// noteURLOwned records, after a message is served, what urlKeeps asks.
func (c *conn) noteURLOwned() {
	if c.s == nil {
		c.urlSnap.Store(nil)
		return
	}
	c.urlSnap.Store(c.snapURL())
}

// snapURL is the page's address and the parameters it keeps there, nil when
// it keeps none.
func (c *conn) snapURL() *urlSnap {
	o, ok := c.s.(urlOwner)
	if !ok {
		return nil
	}
	owned := o.URLParams()
	if len(owned) == 0 {
		return nil
	}
	path, query := splitURL(c.uri)
	return &urlSnap{path: path, query: query, owned: owned}
}

func splitURL(uri string) (string, url.Values) {
	path, raw, _ := strings.Cut(uri, "?")
	q, _ := url.ParseQuery(raw)
	return path, q
}

// keeps reports whether a navigation to uri changes only the parameters the
// page keeps in the address, at least one.
func (s *urlSnap) keeps(uri string) bool {
	if s == nil {
		return false
	}
	uri, _, _ = strings.Cut(uri, "#")
	path, next := splitURL(uri)
	if path != s.path {
		return false
	}
	changed := false
	for k, vals := range next {
		if !slices.Equal(vals, s.query[k]) {
			if !s.owned[k] {
				return false
			}
			changed = true
		}
	}
	for k := range s.query {
		if _, ok := next[k]; !ok {
			if !s.owned[k] {
				return false
			}
			changed = true
		}
	}
	return changed
}
