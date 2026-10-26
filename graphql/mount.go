package graphql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/vuka-lang/ui/v2/forms"
	"github.com/vuka-lang/vuka/v2/diag"
	"github.com/vuka-lang/web/v2"
)

// Introspection, given to web.GraphQL, allows or refuses introspection
// (__schema, __type): allowed in development, refused otherwise, unless
// given.
type Introspection bool

// The extensions.code of errors a resolver's error becomes.
const (
	CodeInvalidInput    = "INVALID_INPUT"   // a form's checks, web.Invalid, a 422
	CodeUnauthenticated = "UNAUTHENTICATED" // web.Unauthorized, a 401
	CodeForbidden       = "FORBIDDEN"       // web.Forbidden, a 403
	CodeNotFound        = "NOT_FOUND"       // web.NotFound, a 404
	CodeConflict        = "CONFLICT"        // web.Conflict, a 409
)

// The environment variables vuka graphql schema runs the app with: the app
// writes its schema's SDL (to the file named, or stdout for "-"), or
// compares it with the committed SDL in the file named, then exits.
const (
	SchemaEnv = "VUKA_GRAPHQL_SCHEMA"
	CheckEnv  = "VUKA_GRAPHQL_CHECK"
)

func init() { web.RegisterGraphQL(build) }

// build is an app's endpoint: the schema of the registered packages, named
// and gated by the app's options and modules, served by a Server.
func build(host web.GraphQLHost) (web.GraphQLHandler, error) {
	a := &assembly{host: host, field: CamelCase}
	cfg := Config{Introspection: host.Dev}
	for _, o := range host.Options {
		switch o := o.(type) {
		case Naming:
			if o.Fields != nil {
				a.field = o.Fields
			}
		case Limits:
			cfg.Limits = o
		case Introspection:
			cfg.Introspection = bool(o)
		default:
			return nil, diag.New("E0801", "%s: %T is no GraphQL option", host.Where, o).WithHelp("pass graphql.Naming, graphql.Limits or graphql.Introspection")
		}
	}
	schema, err := a.schema()
	if err != nil {
		return nil, err
	}
	if err := schemaTool(schema); err != nil {
		return nil, err
	}
	cfg.Schema = schema
	cfg.Present = func(ctx context.Context, err error) []*Error { return present(ctx, err, host.Dev) }
	cfg.Observe = observe
	srv, err := NewServer(cfg)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r.WithContext(ctx))
	}, nil
}

// schemaTool writes or checks the schema for vuka graphql schema, and
// exits, when the app runs for it.
func schemaTool(s *Schema) error {
	if out := os.Getenv(SchemaEnv); out != "" {
		var err error
		if out == "-" {
			_, err = io.WriteString(os.Stdout, s.SDL())
		} else {
			err = os.WriteFile(out, []byte(s.SDL()), 0o644)
		}
		if err != nil {
			return err
		}
		os.Exit(0)
	}
	if path := os.Getenv(CheckEnv); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		old, err := ParseSchema(string(b))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		breaking := 0
		for _, c := range Diff(old, s) {
			fmt.Println(c)
			if c.Breaking {
				breaking++
			}
		}
		if breaking > 0 {
			os.Exit(1)
		}
		os.Exit(0)
	}
	return nil
}

// present is a resolver's error as the response shows it: a form's
// checks, web.Invalid and *web.Error as they are, with extensions.code
// from their kind or status; anything else "internal error", its text
// logged, unless the app runs in development, which shows it.
func present(ctx context.Context, err error, dev bool) []*Error {
	var inv *InvalidInput
	if errors.As(err, &inv) {
		out := make([]*Error, len(inv.Errors))
		for i, fe := range inv.Errors {
			out[i] = formError(fe.Error, fe.Message, inv.Arg)
		}
		return out
	}
	var many forms.Errors
	var one forms.Error
	switch {
	case errors.As(err, &many):
		out := make([]*Error, len(many))
		for i, fe := range many {
			out[i] = formError(fe, forms.Text(ctx, fe, ""), "")
		}
		return out
	case errors.As(err, &one):
		return []*Error{formError(one, forms.Text(ctx, one, ""), "")}
	}
	var gqlErr *Error
	if errors.As(err, &gqlErr) {
		c := *gqlErr
		return []*Error{&c}
	}
	var fields interface{ FieldErrors() map[string]string }
	if errors.As(err, &fields) && len(fields.FieldErrors()) > 0 {
		return []*Error{{Message: err.Error(), Extensions: map[string]any{"code": CodeInvalidInput, "fields": fields.FieldErrors()}, Err: err}}
	}
	var we *web.Error
	var sc interface{ StatusCode() int }
	var pub interface{ PublicMessage() string }
	status, msg := 0, ""
	switch {
	case errors.As(err, &we):
		status, msg = we.Status, we.Error()
	case errors.As(err, &sc) && errors.As(err, &pub):
		status, msg = sc.StatusCode(), pub.PublicMessage()
	}
	if status > 0 && status < 500 {
		return []*Error{{Message: msg, Extensions: map[string]any{"code": statusCode(status)}, Err: err}}
	}
	if p := (*PanicError)(nil); errors.As(err, &p) {
		web.LogError(ctx, fmt.Errorf("%w\n%s", err, p.Stack))
	} else {
		web.LogError(ctx, err)
	}
	if status >= 500 {
		return []*Error{{Message: msg, Extensions: map[string]any{"code": CodeInternal}, Err: err}}
	}
	msg = "internal error"
	if dev {
		msg = err.Error()
	}
	return []*Error{{Message: msg, Extensions: map[string]any{"code": CodeInternal}, Err: err}}
}

// formError is a form's error as a response's: its message, and what
// it is as data — the field's path in the form, the rule it broke, the
// message key, the rule's values, the argument.
func formError(e forms.Error, msg, arg string) *Error {
	key := e.Key
	if key == "" {
		key = forms.DefaultKey(e)
	}
	ext := map[string]any{"code": CodeInvalidInput, "path": e.Path, "rule": e.Rule, "key": key}
	if len(e.Params) > 0 {
		ext["params"] = e.Params
	}
	if arg != "" {
		ext["argument"] = arg
	}
	return &Error{Message: msg, Extensions: ext, Err: e}
}

func statusCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return CodeUnauthenticated
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusUnprocessableEntity:
		return CodeInvalidInput
	}
	return CodeBadRequest
}

// observe notes the operation a request ran, and its errors, for the
// development toolbar's Request panel.
func observe(ctx context.Context, op OperationInfo, res *Response, d time.Duration) {
	if op.Type != "" {
		name := op.Name
		if name == "" {
			name = "(anonymous)"
		}
		web.NoteRequest(ctx, web.RequestNote{Section: "GraphQL", Name: "Operation", Value: op.Type + " " + name + " in " + d.Round(time.Microsecond).String()})
	}
	for _, e := range res.Errors {
		where := ""
		if len(e.Path) > 0 {
			parts := make([]string, len(e.Path))
			for i, p := range e.Path {
				parts[i] = fmt.Sprint(p)
			}
			where = strings.Join(parts, ".") + ": "
		}
		code, _ := e.Extensions["code"].(string)
		web.NoteRequest(ctx, web.RequestNote{Section: "GraphQL", Name: "Error " + code, Value: where + e.Message, Refused: true})
	}
}
