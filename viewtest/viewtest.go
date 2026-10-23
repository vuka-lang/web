// Package viewtest helps test pages that read template variables: it
// gives them the values a test chooses instead of what the app's context
// processors (web.ContextProcessors) compute.
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
