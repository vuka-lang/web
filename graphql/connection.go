package graphql

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Connection is a page of a list, as GraphQL's cursor connections have
// it: the page's edges, each a node and its cursor, what lies around the
// page, and the list's length when a query asks for it. An operation or
// field returning a Connection[User] is a field of type UserConnection,
// its edges UserEdge:
//
//	@graphql.Query
//	func Users(ctx context.Context, page graphql.Page) (graphql.Connection[User], error) {
//		return graphql.Paginate(ctx, User.Objects.OrderBy("name"), page)
//	}
//
//	{ users(first: 10, after: $cursor) { edges { cursor node { name } } pageInfo { hasNextPage endCursor } totalCount } }
type Connection[T any] struct {
	Edges    []Edge[T]
	PageInfo PageInfo
	count    func(ctx context.Context) (int64, error)
}

// Edge is a node of a Connection and its cursor: where a next page starts
// (after) or ends (before).
type Edge[T any] struct {
	Cursor string
	Node   T
}

// PageInfo is what lies around a Connection's page: whether there are rows
// past it either way, and the cursors of its first and last edges (null for
// an empty page).
//
// HasPreviousPage, for a page read forwards (first), is whether it starts
// after a cursor; HasNextPage, for one read backwards (last), whether it
// ends before one — as the specification allows, neither is a query of its
// own.
type PageInfo struct {
	HasNextPage     bool
	HasPreviousPage bool
	StartCursor     *string
	EndCursor       *string
}

// TotalCount is how many rows the whole list holds: counted only when a
// query selects it.
func (c Connection[T]) TotalCount(ctx context.Context) (int, error) {
	if c.count == nil {
		return len(c.Edges), nil
	}
	n, err := c.count(ctx)
	return int(n), err
}

// Page is the arguments a connection's field takes: a parameter of type
// Page is four arguments, first and after for a page read forwards, last
// and before for one read backwards.
//
//	users(first: Int, after: String, last: Int, before: String): UserConnection!
type Page struct {
	First  *int
	After  *string
	Last   *int
	Before *string
}

// The sizes of a page: a Page giving neither first nor last reads
// DefaultPageSize rows forwards, and one asking for more than MaxPageSize
// is refused.
const (
	DefaultPageSize = 10
	MaxPageSize     = 100
)

// PageArgs is the type of a Param of type Page: the arguments first,
// after, last and before, given to the function as a Page.
var PageArgs TypeDef = pageArgs{}

type pageArgs struct{}

func (pageArgs) typeDef() {}

// Pageable is what Paginate pages: an orm query set (orm.QuerySet), or
// anything reading a page by cursors as it does — the first rows after the
// row of cursor after, or the last before the row of cursor before, both
// bounding the page; each row's cursor; whether rows go on past the page
// (after it reading forwards, before it reading backwards) — and counting
// its rows.
type Pageable[T any] interface {
	CursorPage(ctx context.Context, after, before string, first, last int) (rows []T, cursors []string, more bool, err error)
	Count(ctx context.Context) (int64, error)
}

// Paginate is the page of from that page asks for. A query set's cursors
// are keyset cursors where its ordering allows, positions otherwise (see
// the orm's CursorPage); a cursor of another list, first and last given
// together, a negative size or one above MaxPageSize are the client's
// errors (BAD_USER_INPUT).
func Paginate[T any](ctx context.Context, from Pageable[T], page Page) (Connection[T], error) {
	first, last := 0, 0
	switch {
	case page.First != nil && page.Last != nil:
		return Connection[T]{}, pageError("Give first or last, not both.")
	case page.First != nil:
		first = *page.First
	case page.Last != nil:
		last = *page.Last
	default:
		first = DefaultPageSize
	}
	for _, n := range []struct {
		name string
		v    *int
	}{{"first", page.First}, {"last", page.Last}} {
		switch {
		case n.v == nil:
		case *n.v < 0:
			return Connection[T]{}, pageError("%s is %d: a page holds 0 rows or more.", n.name, *n.v)
		case *n.v > MaxPageSize:
			return Connection[T]{}, pageError("%s is %d, more than a page holds: %d.", n.name, *n.v, MaxPageSize)
		}
	}
	after, before := deref(page.After), deref(page.Before)
	c := Connection[T]{count: from.Count, Edges: []Edge[T]{}}
	if first == 0 && last == 0 {
		c.PageInfo.HasPreviousPage = after != ""
		c.PageInfo.HasNextPage = before != ""
		return c, nil
	}
	rows, cursors, more, err := from.CursorPage(ctx, after, before, first, last)
	if err != nil {
		var sc interface{ StatusCode() int }
		var pub interface{ PublicMessage() string }
		if errors.As(err, &sc) && sc.StatusCode() == 400 && errors.As(err, &pub) {
			return Connection[T]{}, pageError("%s", pub.PublicMessage())
		}
		return Connection[T]{}, err
	}
	c.Edges = make([]Edge[T], len(rows))
	for i, r := range rows {
		c.Edges[i] = Edge[T]{Cursor: cursors[i], Node: r}
	}
	if last > 0 {
		c.PageInfo.HasPreviousPage, c.PageInfo.HasNextPage = more, before != ""
	} else {
		c.PageInfo.HasNextPage, c.PageInfo.HasPreviousPage = more, after != ""
	}
	if len(cursors) > 0 {
		c.PageInfo.StartCursor, c.PageInfo.EndCursor = &cursors[0], &cursors[len(cursors)-1]
	}
	return c, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func pageError(format string, args ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, args...), Extensions: code(CodeBadUserInput)}
}

// Slice is a Pageable over rows in memory: cursors are positions in it.
func Slice[T any](rows []T) Pageable[T] { return slicePager[T](rows) }

type slicePager[T any] []T

func (s slicePager[T]) Count(context.Context) (int64, error) { return int64(len(s)), nil }

func (s slicePager[T]) CursorPage(_ context.Context, after, before string, first, last int) ([]T, []string, bool, error) {
	start, end := 0, len(s)
	if after != "" {
		i, ok := slicePos(after, len(s))
		if !ok {
			return nil, nil, false, errSliceCursor
		}
		start = i + 1
	}
	if before != "" {
		i, ok := slicePos(before, len(s))
		if !ok {
			return nil, nil, false, errSliceCursor
		}
		end = i
	}
	if start > end {
		start = end
	}
	from, to, more := start, end, false
	if last > 0 {
		from = max(start, end-last)
		more = from > start
	} else {
		to = min(end, start+first)
		more = to < end
	}
	cursors := make([]string, to-from)
	for i := range cursors {
		cursors[i] = base64.RawURLEncoding.EncodeToString([]byte(slicePrefix + fmt.Sprint(from+i)))
	}
	return s[from:to], cursors, more, nil
}

const slicePrefix = "slice:"

var errSliceCursor = &Error{Message: "The cursor isn't one of this list's.", Extensions: map[string]any{"code": CodeBadUserInput}}

func slicePos(c string, n int) (int, bool) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	rest, ok := strings.CutPrefix(string(b), slicePrefix)
	if !ok {
		return 0, false
	}
	var i int
	if _, err := fmt.Sscan(rest, &i); err != nil || i < 0 || i >= n || fmt.Sprint(i) != rest {
		return 0, false
	}
	return i, true
}

// isConnection reports whether t is a Connection type: its list fields'
// sizes are the size of the field returning it.
func isConnection(t reflect.Type) bool {
	return t.PkgPath() == reflect.TypeFor[Page]().PkgPath() && strings.HasPrefix(t.Name(), "Connection[")
}
