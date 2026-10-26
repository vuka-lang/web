package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"sync"

	"github.com/vuka-lang/vuka/v2/diag"
)

// GraphQL serves the app's GraphQL schema at path, by GET and POST: the
// operations its packages declare with @graphql.Query and @graphql.Mutation
// (package github.com/vuka-lang/web/v2/graphql, which the code the compiler
// generates for them imports). opts are that package's options:
// graphql.Naming, graphql.Limits, graphql.Introspection.
//
//	app := web.New(web.GraphQL("/graphql"))
//
// The route runs the app's middleware, CSRF protection and toolbar as any
// other; a library gating routes (web/auth) leaves the route itself open
// and gates each operation and field by its attributes instead (GuardCalls).
// Build fails when no operation is linked into the program.
func GraphQL(path string, opts ...any) Option {
	return OptionFunc(func(a *App) {
		a.mutable("GraphQL")
		var (
			mu      sync.Mutex
			serve   GraphQLHandler
			handler = func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				h := serve
				mu.Unlock()
				if h == nil {
					a.writeError(w, r, errors.New("web: GraphQL isn't built"), false)
					return
				}
				h(ctx, w, r)
			}
		)
		where := fmt.Sprintf("web.GraphQL(%q)", path)
		a.Handle(http.MethodGet, path, handler, routeOpt(func(s *routeSpec) { s.calls, s.where = true, where }))
		a.Handle(http.MethodPost, path, handler, routeOpt(func(s *routeSpec) { s.calls, s.where = true, where }))
		a.checks = append(a.checks, func(a *App) error {
			graphqlMu.Lock()
			build := graphqlBuild
			graphqlMu.Unlock()
			if build == nil {
				return diag.New("E0801", "%s: no GraphQL operation is linked into the program", where).
					WithHelp("declare one with @graphql.Query or @graphql.Mutation on a function in a .vuka file, in a package the program imports")
			}
			h, err := build(GraphQLHost{Path: path, Options: opts, Dev: a.dev, Where: where, Dependency: a.graphqlDep, Guard: a.callGuard()})
			if err != nil {
				return err
			}
			mu.Lock()
			serve = h
			mu.Unlock()
			return nil
		})
	})
}

// GraphQLHandler answers a GraphQL request, given the request's context:
// the one the app's handlers get, with its injector, identity and toolbar.
type GraphQLHandler func(ctx context.Context, w http.ResponseWriter, r *http.Request)

// GraphQLHost is what web gives package graphql to build an app's
// endpoint (RegisterGraphQL).
type GraphQLHost struct {
	Path    string
	Options []any // GraphQL's options, as given
	Dev     bool  // the app runs in development
	Where   string
	// Dependency fills a resolver's parameter of type t, as a handler's
	// is filled: a provided service, the request, a ParamFrom type. Its
	// error is a Build error naming where.
	Dependency func(t reflect.Type, where string) (func(context.Context) (any, error), error)
	// Guard decides a call by its declaration's attributes (GuardCalls);
	// nil when no library gates calls. byDefault applies the library's
	// rule for a declaration carrying no gate (an operation's), rather than
	// letting it through (a field's).
	Guard func(ctx context.Context, attrs []any, byDefault bool) error
}

var (
	graphqlMu    sync.Mutex
	graphqlBuild func(GraphQLHost) (GraphQLHandler, error)
)

// RegisterGraphQL is how package graphql serves web.GraphQL: build makes
// an app's endpoint when the app is built. Package graphql calls it at
// init; nothing else needs to.
func RegisterGraphQL(build func(GraphQLHost) (GraphQLHandler, error)) {
	graphqlMu.Lock()
	graphqlBuild = build
	graphqlMu.Unlock()
}

// graphqlDep fills a resolver's dependency of type t.
func (a *App) graphqlDep(t reflect.Type, where string) (func(context.Context) (any, error), error) {
	if t == contextType {
		return func(ctx context.Context) (any, error) { return ctx, nil }, nil
	}
	if t == writerType {
		return func(ctx context.Context) (any, error) {
			if w := ResponseWriter(ctx); w != nil {
				return w, nil
			}
			return nil, errors.New("web: an http.ResponseWriter comes from a request, and this call has none")
		}, nil
	}
	f := injector{a}.Inject(t)
	if f == nil {
		code, help := diag.Code("E0405"), "a resolver takes the context, its arguments and what the app provides"
		if t.Kind() == reflect.Pointer || t.Kind() == reflect.Interface {
			code, help = "E0407", provideHelp
		}
		return nil, diag.New(code, "%s: a parameter of type %s is not provided", where, t).WithHelp("%s", help)
	}
	if a.c.has(t) {
		if err := a.c.check(t, where); err != nil {
			return nil, err
		}
		a.needs = append(a.needs, t)
	}
	return func(ctx context.Context) (any, error) {
		v, err := f(ctx)
		if err != nil {
			return nil, err
		}
		return v.Interface(), nil
	}, nil
}

// GuardCalls has f decide calls the app serves by their declarations'
// attributes: GraphQL's operations and fields (web.GraphQL). An error
// refuses the call — web.Unauthorized, web.Forbidden. byDefault asks for
// the library's rule for a declaration carrying none of its attributes (an
// operation: web/auth needs a sign-in unless the app is public by
// default); without it such a declaration passes (a field, gated by its
// operation already). A library gating routes registers one.
func GuardCalls(f func(ctx context.Context, attrs []any, byDefault bool) error) Option {
	return OptionFunc(func(a *App) {
		a.mutable("GuardCalls")
		a.callGuards = append(a.callGuards, f)
	})
}

// callGuard is the app's GuardCalls functions as one; nil when it has none.
func (a *App) callGuard() func(ctx context.Context, attrs []any, byDefault bool) error {
	gs := a.callGuards
	if len(gs) == 0 {
		return nil
	}
	return func(ctx context.Context, attrs []any, byDefault bool) error {
		for _, g := range gs {
			if err := g(ctx, attrs, byDefault); err != nil {
				return err
			}
		}
		return nil
	}
}

// LogError logs err for the request ctx belongs to, as a handler's server
// error is logged: one line with the method and path.
func LogError(ctx context.Context, err error) {
	if r := Request(ctx); r != nil {
		log.Printf("web: %s %s: %v", r.Method, r.URL.Path, err)
		return
	}
	log.Printf("web: %v", err)
}
