package web

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"slices"
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

type gqlFieldRow struct {
	Path, Field, At, Ms string
	Err                 bool
	SQL                 int
	Bar                 template.CSS
}

type gqlBatchRow struct {
	Loader, At, Ms, Err string
	Keys, SQL           int
	Bar                 template.CSS
}

type gqlView struct {
	Fields  []gqlFieldRow
	Batches []gqlBatchRow
	Total   string
	Slowest string
}

// graphqlOf lays a request's field trace out against its time: fields in
// the order they started, their statements counted by origin.
func graphqlOf(r tbRecord) gqlView {
	sqlBy := map[string]int{}
	for _, q := range r.Queries {
		if q.Origin != "" {
			sqlBy[q.Origin]++
		}
		if q.Field != "" && q.Origin == "" {
			sqlBy["\x00"+q.Field]++
		}
	}
	var total time.Duration
	for _, f := range r.GQLFields {
		total = max(total, f.Start.Sub(r.Start)+f.Dur)
	}
	for _, b := range r.GQLBatches {
		total = max(total, b.Start.Sub(r.Start)+b.Dur)
	}
	bar := func(start time.Time, d time.Duration) template.CSS {
		if total <= 0 {
			return ""
		}
		left := 100 * float64(start.Sub(r.Start)) / float64(total)
		return template.CSS(fmt.Sprintf("left:%.2f%%;width:%.2f%%", left, min(max(0.5, 100*float64(d)/float64(total)), 100-left)))
	}
	v := gqlView{Total: ms(total)}
	fields := slices.Clone(r.GQLFields)
	slices.SortStableFunc(fields, func(a, b GraphQLField) int { return a.Start.Compare(b.Start) })
	var slowest GraphQLField
	for _, f := range fields {
		if f.Dur > slowest.Dur {
			slowest = f
		}
		v.Fields = append(v.Fields, gqlFieldRow{Path: f.Path, Field: f.Field, At: ms(f.Start.Sub(r.Start)), Ms: ms(f.Dur), Err: f.Err, SQL: sqlBy[f.Path], Bar: bar(f.Start, f.Dur)})
	}
	if slowest.Path != "" {
		v.Slowest = slowest.Path + " (" + ms(slowest.Dur) + ")"
	}
	for _, b := range r.GQLBatches {
		v.Batches = append(v.Batches, gqlBatchRow{Loader: b.Loader, Keys: b.Keys, At: ms(b.Start.Sub(r.Start)), Ms: ms(b.Dur), Err: b.Err, SQL: sqlBy["\x00loader "+b.Loader], Bar: bar(b.Start, b.Dur)})
	}
	return v
}

// graphiqlPanel is the GraphiQL panel of an app serving web.GraphQL: the
// script fills it with its editor, which stays as records change.
func (a *App) graphiqlPanel() []panelView {
	if a.graphqlPath == "" {
		return nil
	}
	return []panelView{{Name: "GraphiQL", Body: template.HTML(`<div class="gql-slot" data-endpoint="` + template.HTMLEscapeString(a.graphqlPath) +
		`" data-script="` + graphiqlJS + `?v=` + graphiqlTag + `"></div>`)}}
}

// The GraphiQL panel's editor is a script of its own, which the toolbar
// loads when the panel first shows: pages that never open it don't fetch
// it.
const (
	graphiqlJS    = livePrefix + "toolbar/graphiql.js"
	graphiqlCheck = livePrefix + "toolbar/graphql/check"
)

//go:embed toolbar_graphiql.js
var graphiqlScript []byte

var graphiqlTag = cssTag(graphiqlScript)

func (tb *toolbar) setGraphQLCheck(c func([]byte) []byte) {
	tb.mu.Lock()
	tb.gqlCheck = c
	tb.mu.Unlock()
}

// routeGraphiQL serves the panel's script, and checks its query as it is
// typed: the errors running it would meet before execution, as the app's
// own engine finds them. Neither is recorded as a request.
func (a *App) routeGraphiQL() {
	tb := a.tb
	a.router.Handle(http.MethodGet, graphiqlJS, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveTagged(w, r, "text/javascript; charset=utf-8", graphiqlTag, graphiqlScript)
	}))
	a.router.Handle(http.MethodPost, graphiqlCheck, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// The toolbar's own request: a form on another site can't set the
		// header.
		if r.Header.Get(ToolbarHeader) == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "check a query from the toolbar"})
			return
		}
		tb.mu.Lock()
		check := tb.gqlCheck
		tb.mu.Unlock()
		if check == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "the app serves no GraphQL"})
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(check(body))
	}))
}
