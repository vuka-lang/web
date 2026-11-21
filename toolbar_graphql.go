package web

import (
	"context"
	_ "embed"
	"time"
)

// What the toolbar shows of GraphQL: each request's field trace (the
// GraphQL panel: every field resolved, with its path, timing and the SQL it
// ran, and every loader batch), the field beside each statement of the SQL
// panel, and the GraphiQL panel running queries against the app's
// web.GraphQL endpoint. Package graphql notes them; development only.

// GraphQLField is a field a GraphQL request resolved, for the toolbar's
// GraphQL panel.
type GraphQLField struct {
	Path  string // "pets.0.owner"
	Field string // "Pet.owner"
	Start time.Time
	Dur   time.Duration
	Err   bool
}

// GraphQLBatch is a batch a GraphQL loader ran.
type GraphQLBatch struct {
	Loader string
	Keys   int
	Start  time.Time
	Dur    time.Duration
	Err    string
}

// Recorded reports whether the development toolbar records the request (or
// live message) of ctx: what is worth noting only for it.
func Recorded(ctx context.Context) bool { return entryOf(ctx) != nil }

// NoteGraphQLField records a resolved field for the toolbar's GraphQL panel.
func NoteGraphQLField(ctx context.Context, f GraphQLField) {
	if e := entryOf(ctx); e != nil {
		e.mu.Lock()
		if e.room() {
			e.GQLFields = append(e.GQLFields, f)
		}
		e.mu.Unlock()
	}
}

// NoteGraphQLBatch records a loader's batch for the toolbar's GraphQL panel.
func NoteGraphQLBatch(ctx context.Context, b GraphQLBatch) {
	if e := entryOf(ctx); e != nil {
		e.mu.Lock()
		if e.room() {
			e.GQLBatches = append(e.GQLBatches, b)
		}
		e.mu.Unlock()
	}
}

type originKey struct{}

// origin is what ran a statement: a GraphQL field (its path and name) or
// a loader.
type origin struct{ path, field string }

// StatementOrigin is ctx with the statements noted under it (NoteStatement)
// marked as path's and field's — a GraphQL field's, "pets.0.owner" of
// "Pet.owner", or a loader's ("" and "loader OrdersByUser"): the SQL panel
// names them, and an N+1 the field repeating it. It is ctx itself when no
// toolbar records the request.
func StatementOrigin(ctx context.Context, path, field string) context.Context {
	if entryOf(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, originKey{}, origin{path, field})
}

func originOf(ctx context.Context) origin {
	o, _ := ctx.Value(originKey{}).(origin)
	return o
}

// The GraphiQL panel's editor is a script of its own, which the toolbar
// loads when the panel first shows: pages that never open it don't fetch
// it.
const (
	graphiqlJS    = livePrefix + "toolbar/graphiql.js"
	graphiqlCheck = livePrefix + "toolbar/graphql/check"
)

func (tb *toolbar) setGraphQLCheck(c func([]byte) []byte) {
	tb.mu.Lock()
	tb.gqlCheck = c
	tb.mu.Unlock()
}
