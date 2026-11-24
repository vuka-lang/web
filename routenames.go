package web

import (
	"context"
	"encoding"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/vuka-lang/ui/v2"
)

// Named routes: vuka's folder routes give each route a name, its folder
// path with dots (users.edit for pages/users/[user]/edit), and a page links
// to another by it: route("users.edit", user.ID) in markup is
// RouteURL(ctx, "users.edit", user.ID), checked by vuka when it is
// transpiled.

// RouteName names the route it is given with, for Route, RouteURL and
// RoutePath: what vuka's folder routes register for each route.
func RouteName(name string) RouteOption {
	return routeOpt(func(s *routeSpec) { s.name = name })
}

// RouteQuery is a link's query, the last argument of Route, RouteURL or
// RoutePath: each value written as a @ui.URL field writes it, and left out
// at its zero value. In markup it is query{"page": 2}.
type RouteQuery map[string]any

// Route is the path of the route name, its path parameters args in the
// order of the path and an optional RouteQuery last, with the request's mount
// prefix in front (as MountPath): for hand-written Go, whose names nothing
// checks before it runs. An unknown name, or arguments the path doesn't
// take, panics naming the closest route under vuka run and in tests; in
// production it is logged, and the link is "#".
//
//go:noinline
func Route(ctx context.Context, name string, args ...any) string {
	return routeLink(ctx, name, args, true, true)
}

// RouteURL is Route as vuka compiles route("name", args…): its name and
// arguments checked when the page was transpiled, a mismatch is logged and
// gives "#", never a panic.
//
//go:noinline
func RouteURL(ctx context.Context, name string, args ...any) string {
	return routeLink(ctx, name, args, true, false)
}

// RoutePath is Route without the mount prefix, as routePath("name", args…)
// compiles: for matching a request's path, or an absolute URL on another
// base.
//
//go:noinline
func RoutePath(ctx context.Context, name string, args ...any) string {
	return routeLink(ctx, name, args, false, true)
}

func routeLink(ctx context.Context, name string, args []any, mount, strict bool) string {
	r, a := requestApp(ctx)
	p, err := a.routePath(name, args)
	if err != nil {
		if strict && (testBinary || IsDev()) {
			panic(err)
		}
		slog.ErrorContext(ctx, "web: a route link", "err", err)
		return "#"
	}
	if mount && r != nil {
		return basePrefix(r) + p
	}
	return p
}

// routePath formats the path of the route name with args.
func (a *App) routePath(name string, args []any) (string, error) {
	var path string
	ok := false
	if a != nil {
		path, ok = a.names[name]
	}
	if !ok {
		return "", a.unknownRoute(name)
	}
	var q RouteQuery
	if n := len(args); n > 0 {
		if v, isQ := args[n-1].(RouteQuery); isQ {
			q, args = v, args[:n-1]
		}
	}
	segs := strings.Split(path, "/")
	var params []string
	for _, s := range segs {
		if strings.HasPrefix(s, "{") {
			params = append(params, strings.Trim(s, "{}"))
		}
	}
	if len(args) != len(params) {
		return "", fmt.Errorf("web: route %q (%s) takes %d path parameters (%s), given %d", name, path, len(params), strings.Join(params, ", "), len(args))
	}
	var b strings.Builder
	i := 0
	for j, s := range segs {
		if j > 0 {
			b.WriteByte('/')
		}
		if !strings.HasPrefix(s, "{") {
			b.WriteString(s)
			continue
		}
		seg, err := pathSegment(args[i], strings.HasSuffix(s, "...}"))
		if err != nil {
			return "", fmt.Errorf("web: route %q (%s), parameter %s: %v", name, path, params[i], err)
		}
		b.WriteString(seg)
		i++
	}
	out := b.String()
	if out == "" {
		out = "/"
	}
	if s := q.encode(); s != "" {
		out += "?" + s
	}
	return out, nil
}

func (a *App) unknownRoute(name string) error {
	if a == nil {
		return fmt.Errorf("web: no route %q: no app is built", name)
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range a.names {
			if !yield(n) {
				return
			}
		}
	})
	if c := closest(name, names); c != "" {
		return fmt.Errorf("web: no route %q; did you mean %q?", name, c)
	}
	return fmt.Errorf("web: no route %q", name)
}

// pathSegment is v written into a path: a whole number or a string as it
// is, a text marshaler's text, each escaped; a catch-all's slashes kept.
func pathSegment(v any, rest bool) (string, error) {
	var s string
	switch x := v.(type) {
	case encoding.TextMarshaler:
		t, err := x.MarshalText()
		if err != nil {
			return "", err
		}
		s = string(t)
	default:
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.String:
			s = rv.String()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return strconv.FormatInt(rv.Int(), 10), nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return strconv.FormatUint(rv.Uint(), 10), nil
		default:
			return "", fmt.Errorf("a %T can't be a path segment", v)
		}
	}
	if !rest {
		return url.PathEscape(s), nil
	}
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/"), nil
}

// encode is the query's text, its keys sorted, zero values left out.
func (q RouteQuery) encode() string {
	if len(q) == 0 {
		return ""
	}
	v := url.Values{}
	for k, x := range q {
		if x == nil || reflect.ValueOf(x).IsZero() {
			continue
		}
		if t := ui.QueryText(x); len(t) > 0 {
			v[k] = t
		}
	}
	return v.Encode()
}
