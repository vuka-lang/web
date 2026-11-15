// Package viewtest tests an app's pages from Go, as visitors use them.
//
// Three handles drive a page the app serves, each on a test server of its
// own, closed as the test ends:
//
//   - Open drives a page over its live connection without a browser: it
//     sends what the runtime would — clicks, filled fields, submits,
//     navigations, hovers' prefetches, loads when seen — and reads the
//     page as the server's messages leave it. Fast; needs nothing
//     installed.
//
//   - Browse drives it in a headless Chrome, the real runtime running:
//     for JS commands, islands, hooks and whatever the browser itself
//     does. EventStream runs it with no WebSocket.
//
//   - Mount drives a page's struct form (<ui.Form for>) field by field,
//     its fields named by pointers into the page's type, live or as a
//     browser without script posts it (Plain).
//
// A test of a page of todos:
//
//	p := viewtest.Open(t, app, "/todos")
//	p.Fill("title", "Buy milk").Click("Add")
//	p.Expect("#todos li").Text("Buy milk")
//
// Expect re-checks until the page shows what it should — a reply, a
// deferred group's patch, a broadcast arriving — failing with what it
// showed last once Timeout passes. To test as a signed-in user, build the
// app with auth/authtest's As.
//
// It also gives pages that read template variables the values a test
// chooses instead of what the app's context processors
// (web.ContextProcessors) compute, and sets how a page's deferred parts
// render: in a test binary a page's deferred parts (@ui.Defer, ui.Part)
// render in place, the test reading the page whole, as it is once every
// part has arrived; Pending renders the page as it first shows instead.
package viewtest

import (
	"context"
	"net/http"
	"testing"

	"github.com/vuka-lang/web/v2"
)

// With makes every request of the app — pages, live pages and their events
// — read the variables of each of vars (a context processor's result
// struct, such as users.Vars{Me: u}) from it, the processor returning its
// type not running:
//
//	app := web.New(web.ContextProcessors(users.Context), viewtest.With(users.Vars{Me: ada}))
//
// Outside a test binary it panics.
func With(vars ...any) web.Option {
	if !testing.Testing() {
		panic("viewtest: With outside a test binary")
	}
	for _, v := range vars {
		web.WithVars(context.Background(), v) // a value that isn't a struct panics here, not on a request
	}
	return web.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(Context(r.Context(), vars...)))
		})
	})
}

// Context is ctx reading the variables of each of vars from it, as
// web.WithVars does: for a live session a test builds itself
// (live.NewSession, livetest), a component rendered alone, or Go code
// calling web.Var.
func Context(ctx context.Context, vars ...any) context.Context {
	for _, v := range vars {
		ctx = web.WithVars(ctx, v)
	}
	return ctx
}

// Pending makes the app's pages render as they first show: each deferred
// part's fallback in its place, its method never run. For a test of what
// a page shows before its parts arrive:
//
//	app := web.New(pages.Routes(), viewtest.Pending())
//
// Outside a test binary it panics.
func Pending() web.Option {
	if !testing.Testing() {
		panic("viewtest: Pending outside a test binary")
	}
	return web.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(web.WithPendingParts(r.Context())))
		})
	})
}
