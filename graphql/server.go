package graphql

import (
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// Request is a GraphQL request: the document, which of its operations to
// run (needed when it has several), the variables' values (JSON, decoded
// with UseNumber) and extensions.
type Request struct {
	Query         string
	OperationName string
	Variables     map[string]any
	Extensions    map[string]any
}

// OperationInfo names the operation a request ran.
type OperationInfo struct {
	Name string // "" for an anonymous one
	Type string // "query", "mutation" or "subscription"
}

// Config configures a Server.
type Config struct {
	Schema *Schema
	Limits Limits
	// Introspection allows __schema and __type; off, a document selecting
	// them fails validation. __typename is always allowed.
	Introspection bool
	// Present turns a resolver's error (or a Guard's, or a *PanicError)
	// into the response's errors: their messages and extensions. The
	// engine fills each one's Path and Locations when it leaves them
	// empty. Nil shows err.Error(), except for a panic, shown as "internal
	// error".
	Present func(ctx context.Context, err error) []*Error
	// CacheSize is how many parsed and validated documents are kept, by
	// the SHA-256 of their text. Default 256; negative keeps none.
	CacheSize int
	// Observe, when set, is called after each request with the operation
	// run (zero when none was), the response and how long it took.
	Observe func(ctx context.Context, op OperationInfo, res *Response, d time.Duration)
	// Trace, when set, is asked for each request's Trace: nil traces
	// nothing.
	Trace func(ctx context.Context) *Trace
}

// Server executes requests against a schema.
type Server struct {
	cfg    Config
	limits Limits

	mu    sync.Mutex
	cache map[[32]byte]*list.Element
	lru   *list.List
	// parses counts documents parsed and validated (cache misses).
	parses atomic.Int64
}

type cached struct {
	key   [32]byte
	doc   *language.Document
	errs  []*Error
	frags map[string]*language.FragmentDefinition
}

// NewServer is a server for c.Schema.
func NewServer(c Config) (*Server, error) {
	if c.Schema == nil {
		return nil, errors.New("graphql: Config.Schema is nil")
	}
	if c.CacheSize == 0 {
		c.CacheSize = defaultCacheSize
	}
	return &Server{cfg: c, limits: c.Limits.withDefaults(), cache: map[[32]byte]*list.Element{}, lru: list.New()}, nil
}

// Schema is the schema the server executes against.
func (s *Server) Schema() *Schema { return s.cfg.Schema }

// Limits are the limits the server applies, defaults filled in.
func (s *Server) Limits() Limits { return s.limits }

// document is query parsed and validated: from the cache, else parsed,
// validated and cached.
func (s *Server) document(query string) *cached {
	key := sha256.Sum256([]byte(query))
	if s.cfg.CacheSize > 0 {
		s.mu.Lock()
		if el, ok := s.cache[key]; ok {
			s.lru.MoveToFront(el)
			s.mu.Unlock()
			return el.Value.(*cached)
		}
		s.mu.Unlock()
	}
	s.parses.Add(1)
	c := &cached{key: key}
	doc, err := language.Parse(query)
	if err != nil {
		var se *language.SyntaxError
		if errors.As(err, &se) {
			c.errs = []*Error{{Message: "Syntax Error: " + se.Message, Locations: []Location{{se.Line, se.Column}}, Extensions: code(CodeParseFailed)}}
		} else {
			c.errs = []*Error{{Message: err.Error(), Extensions: code(CodeParseFailed)}}
		}
	} else {
		c.doc = doc
		c.errs = s.cfg.Schema.Validate(doc, s.cfg.Introspection)
		c.frags = doc.Fragments()
	}
	if s.cfg.CacheSize > 0 {
		s.mu.Lock()
		if _, ok := s.cache[key]; !ok {
			s.cache[key] = s.lru.PushFront(c)
			for s.lru.Len() > s.cfg.CacheSize {
				old := s.lru.Remove(s.lru.Back()).(*cached)
				delete(s.cache, old.key)
			}
		}
		s.mu.Unlock()
	}
	return c
}

func requestError(c, format string, args ...any) *Response {
	return &Response{Errors: []*Error{{Message: fmt.Sprintf(format, args...), Extensions: code(c)}}}
}

// operationOf is the operation a request names: the only one when it
// names none.
func operationOf(doc *language.Document, name string) (*language.OperationDefinition, *Response) {
	ops := doc.Operations()
	if name == "" {
		switch len(ops) {
		case 0:
			return nil, requestError(CodeBadRequest, "Must provide an operation.")
		case 1:
			return ops[0], nil
		}
		return nil, requestError(CodeBadRequest, "Must provide operation name if query contains multiple operations.")
	}
	for _, op := range ops {
		if op.Name == name {
			return op, nil
		}
	}
	return nil, requestError(CodeBadRequest, "Unknown operation named %q.", name)
}

// Execute runs a request: parse (or the cache), validation, the limits,
// the variables' coercion, then the operation. A request refused before
// execution has errors only (Response.Executed false).
func (s *Server) Execute(ctx context.Context, req Request) *Response {
	start := time.Now()
	res, info := s.execute(ctx, req)
	if s.cfg.Observe != nil {
		s.cfg.Observe(ctx, info, res, time.Since(start))
	}
	return res
}

func (s *Server) execute(ctx context.Context, req Request) (*Response, OperationInfo) {
	if s.limits.Bytes > 0 && len(req.Query) > s.limits.Bytes {
		return requestError(CodeSizeLimit, "The query is %d bytes, more than the limit of %d.", len(req.Query), s.limits.Bytes), OperationInfo{}
	}
	if req.Query == "" {
		return requestError(CodeBadRequest, "Must provide query string."), OperationInfo{}
	}
	c := s.document(req.Query)
	if len(c.errs) > 0 {
		return &Response{Errors: c.errs}, OperationInfo{}
	}
	op, bad := operationOf(c.doc, req.OperationName)
	if bad != nil {
		return bad, OperationInfo{}
	}
	info := OperationInfo{Name: op.Name, Type: op.Operation}
	schema := s.cfg.Schema
	vars, verrs := schema.coerceVariables(op, req.Variables)
	if len(verrs) > 0 {
		return &Response{Errors: verrs}, info
	}
	if errs := s.limits.check(schema, op, c.frags, vars); len(errs) > 0 {
		return &Response{Errors: errs}, info
	}
	if op.Operation == language.OpSubscription {
		return requestError(CodeBadRequest, "Subscriptions are not supported over this transport."), info
	}
	if s.limits.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.limits.Timeout)
		defer cancel()
	}
	st := &state{}
	if s.cfg.Trace != nil {
		if t := s.cfg.Trace(ctx); t != nil {
			st.trace = &tracer{t}
		}
	}
	ctx = context.WithValue(ctx, stateKey{}, st)
	st.sched = newSched(ctx, maxConcurrentResolvers)
	e := &execution{srv: s, s: schema, op: op, frags: c.frags, vars: vars, varset: newVarValues(op, vars, req.Variables), sched: st.sched, trace: st.trace}
	data := e.run(ctx)
	return &Response{Data: data, Errors: e.errors(), Executed: true}, info
}

// present is a resolver's error as the response shows it.
func (s *Server) present(ctx context.Context, err error) []*Error {
	if s.cfg.Present != nil {
		if errs := s.cfg.Present(ctx, err); len(errs) > 0 {
			out := make([]*Error, len(errs))
			for i, e := range errs {
				c := *e
				c.Path, c.Locations = slices.Clone(e.Path), slices.Clone(e.Locations)
				out[i] = &c
			}
			return out
		}
	}
	var gqlErr *Error
	if errors.As(err, &gqlErr) {
		c := *gqlErr
		return []*Error{&c}
	}
	var p *PanicError
	if errors.As(err, &p) {
		return []*Error{{Message: "internal error", Extensions: code(CodeInternal), Err: err}}
	}
	return []*Error{{Message: err.Error(), Err: err}}
}

// ctxError is the error of a field the request's context ended before.
func (s *Server) ctxError(ctx context.Context, path []any, nodes []*language.Field) *Error {
	msg := "The request was canceled."
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		msg = fmt.Sprintf("The request took longer than the limit of %s.", s.limits.Timeout)
	}
	e := newErr(msg, path, nodes)
	e.Extensions, e.Err = code(CodeTimeout), ctx.Err()
	return e
}

// Check is what Execute would refuse req for, without running it: its
// syntax, the schema's rules, then — for the operation it names, or its
// only one — its variables' values and the limits. Nothing is cached. The
// development toolbar's GraphiQL panel checks its query as it is typed.
func (s *Server) Check(req Request) []*Error {
	if s.limits.Bytes > 0 && len(req.Query) > s.limits.Bytes {
		return requestError(CodeSizeLimit, "The query is %d bytes, more than the limit of %d.", len(req.Query), s.limits.Bytes).Errors
	}
	doc, err := language.Parse(req.Query)
	if err != nil {
		var se *language.SyntaxError
		if errors.As(err, &se) {
			return []*Error{{Message: "Syntax Error: " + se.Message, Locations: []Location{{se.Line, se.Column}}, Extensions: code(CodeParseFailed)}}
		}
		return []*Error{{Message: err.Error(), Extensions: code(CodeParseFailed)}}
	}
	if errs := s.cfg.Schema.Validate(doc, s.cfg.Introspection); len(errs) > 0 {
		return errs
	}
	op, bad := operationOf(doc, req.OperationName)
	if bad != nil {
		return nil // which operation runs is the run's to say
	}
	given := req.Variables
	if given == nil {
		given = map[string]any{}
	}
	vars, errs := s.cfg.Schema.coerceVariables(op, given)
	if req.Variables == nil {
		errs = slices.DeleteFunc(errs, func(e *Error) bool { return strings.Contains(e.Message, "was not provided") })
	}
	if len(errs) > 0 {
		return errs
	}
	return s.limits.check(s.cfg.Schema, op, doc.Fragments(), vars)
}
