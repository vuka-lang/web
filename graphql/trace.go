package graphql

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// Trace is told what a request resolves, for development tools (the
// toolbar's GraphQL panel): each field with a resolver of its own (struct
// fields read in place aren't) and each batch a loader ran, as they end.
// Config.Trace gives one per request; nil fields are skipped.
type Trace struct {
	Field func(FieldTrace)
	Batch func(BatchTrace)
	// Context is the context a field's resolver (or a batch) runs with,
	// given its path and what it is ("Pet.owner"; a batch: "" and "loader
	// OrdersByUser"): what the resolver records under it — the SQL it runs
	// — is then the field's.
	Context func(ctx context.Context, path, field string) context.Context
}

// FieldTrace is a field a request resolved.
type FieldTrace struct {
	Path  string // "pets.0.owner"
	Field string // "Pet.owner"
	Start time.Time
	Dur   time.Duration
	Err   bool // it failed: its error is the response's
}

// BatchTrace is a batch a loader ran: how many keys, how long, its error.
type BatchTrace struct {
	Loader string // "OrdersByUser", "Post.Author"
	Keys   int
	Start  time.Time
	Dur    time.Duration
	Err    error
}

// tracer is a request's Trace.
type tracer struct{ t *Trace }

func (t *tracer) field(path []any, field string, start time.Time, d time.Duration, failed bool) {
	if t.t.Field != nil {
		t.t.Field(FieldTrace{Path: pathString(path), Field: field, Start: start, Dur: d, Err: failed})
	}
}

func (t *tracer) batch(name string, keys int, start time.Time, d time.Duration, err error) {
	if t.t.Batch != nil {
		t.t.Batch(BatchTrace{Loader: name, Keys: keys, Start: start, Dur: d, Err: err})
	}
}

func (t *tracer) context(ctx context.Context, path, field string) context.Context {
	if t.t.Context == nil {
		return ctx
	}
	return t.t.Context(ctx, path, field)
}

// pathString is a response path as the toolbar shows it: pets.0.owner.
func pathString(path []any) string {
	var b strings.Builder
	for i, p := range path {
		if i > 0 {
			b.WriteByte('.')
		}
		switch x := p.(type) {
		case string:
			b.WriteString(x)
		case int:
			b.WriteString(strconv.Itoa(x))
		}
	}
	return b.String()
}
