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
// Any other parameter changing, the path changing, or nothing changing is a
// navigation as ever: the page mounts again unless it has an Update taking
// web.URLParams, and in development warnNoUpdate says which applied.

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
	d := diffURL(s.path, s.query, s.owned, uri)
	return !d.path && d.changed && len(d.unkept) == 0
}

// urlDiff is what a navigation changes of the page's address.
type urlDiff struct {
	path    bool     // the path changes
	changed bool     // a parameter changes
	unkept  []string // the parameters that change which no component keeps, sorted
}

// diffURL compares uri with the address path?query, owned naming the
// parameters the page's components keep in it.
func diffURL(path string, query url.Values, owned map[string]bool, uri string) urlDiff {
	uri, _, _ = strings.Cut(uri, "#")
	npath, next := splitURL(uri)
	d := urlDiff{path: npath != path}
	note := func(k string) {
		d.changed = true
		if !owned[k] && !slices.Contains(d.unkept, k) {
			d.unkept = append(d.unkept, k)
		}
	}
	for k, vals := range next {
		if !slices.Equal(vals, query[k]) {
			note(k)
		}
	}
	for k := range query {
		if _, ok := next[k]; !ok {
			note(k)
		}
	}
	slices.Sort(d.unkept)
	return d
}

// whyRemount says why a patch to uri can't keep the page without an Update,
// for warnNoUpdate.
func (c *conn) whyRemount(uri string) string {
	var owned map[string]bool
	if o, ok := c.s.(urlOwner); ok {
		owned = o.URLParams()
	}
	path, query := splitURL(c.uri)
	d := diffURL(path, query, owned, uri)
	switch {
	case d.path:
		return "the patch moves it to another path"
	case len(d.unkept) > 0:
		names := make([]string, len(d.unkept))
		for i, k := range d.unkept {
			names[i] = "?" + k + "="
		}
		return "the patch changes " + strings.Join(names, ", ") + ", which no @ui.URL field or DataTable it renders keeps"
	default:
		return "the patch changes no parameter"
	}
}
