package web

import (
	"bufio"
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
)

type routeSpec struct {
	method, path string
	where        string // "main.ShowPet (pets.vuka:12)"
	fn           reflect.Value
	recv         reflect.Type
	params       []vuka.Param
	view         *declView
	mw           []Middleware
	live         *liveSpec // a Live page
	attrs        []any     // typed attributes, for WrapRoutes
	html         bool      // answers with a page; set by compile
}

type request struct {
	w   http.ResponseWriter
	r   *http.Request
	ctx context.Context
	app *App
}

func (a *App) newRequest(w http.ResponseWriter, r *http.Request) *request {
	rq := &request{w: w, r: r, app: a}
	rq.ctx = context.WithValue(r.Context(), ctxKey{}, rq)
	return rq
}

type ctxKey struct{}

// Request is the request being served, from a context.Context a handler
// received: call decorators reach it through c.Context().
func Request(ctx context.Context) *http.Request {
	rq, _ := ctx.Value(ctxKey{}).(*request)
	if rq == nil {
		return nil
	}
	return rq.r
}

// ResponseWriter is the response being written, from a handler's context.
func ResponseWriter(ctx context.Context) http.ResponseWriter {
	rq, _ := ctx.Value(ctxKey{}).(*request)
	if rq == nil {
		return nil
	}
	return rq.w
}

type binder func(*request) (reflect.Value, error)

var (
	requestType  = typeOf[*http.Request]()
	writerType   = typeOf[http.ResponseWriter]()
	contextType  = typeOf[context.Context]()
	nodeType     = typeOf[ui.Node]()
	redirectType = typeOf[Redirect]()
	textType     = typeOf[encoding.TextUnmarshaler]()
)

func (a *App) compile(s *routeSpec) (http.Handler, error) {
	if s.live != nil {
		return a.compileLive(s)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("web: %s %s (%s): %s", s.method, s.path, s.where, fmt.Sprintf(format, args...))
	}
	var binders []binder
	if s.recv != nil {
		t, deref := s.recv, false
		if !a.c.has(t) && t.Kind() != reflect.Pointer && a.c.has(reflect.PointerTo(t)) {
			t, deref = reflect.PointerTo(t), true
		}
		if !a.c.has(t) {
			return nil, fail("the receiver %s is not a @web.Controller or a provided type", s.recv)
		}
		if err := a.c.check(t, s.where); err != nil {
			return nil, err
		}
		a.needs = append(a.needs, t)
		binders = append(binders, func(*request) (reflect.Value, error) {
			v, err := a.c.get(t)
			if deref {
				v = v.Elem()
			}
			return v, err
		})
	}
	pathNames, body := pathParams(s.path), false
	shown := make([]tbParam, len(binders), len(binders)+len(s.params)) // what the toolbar shows of each argument
	for i, p := range s.params {
		b, err := a.param(s, p, pathNames, &body)
		if err != nil {
			label := p.Name
			if label == "" {
				label = fmt.Sprint("#", i+1)
			}
			return nil, fail("parameter %s %s: %v", label, p.Type, err)
		}
		binders = append(binders, b)
		shown = append(shown, a.paramSource(p, pathNames))
	}
	out, err := results(s.fn.Type())
	if err != nil {
		return nil, fail("%v", err)
	}
	htmlPage := s.view != nil || (out.data != nil && out.data.Implements(nodeType))
	s.html = htmlPage
	var v *view
	if s.view != nil {
		if out.data != nil && out.data.Implements(nodeType) {
			return nil, fail("returns a vuka.Node and has a view; drop one")
		}
		if v, err = compileView(*s.view, out.data); err != nil {
			return nil, fail("%v", err)
		}
	}
	route, handler := s.method+" "+s.path, shortName(s.where)
	limit := a.bodyLimit(s.attrs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler || tw.wrote {
					panic(p)
				}
				a.writeError(w, r, fmt.Errorf("panic: %v", p), htmlPage)
			}
		}()
		if limit > 0 && r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(tw, r.Body, limit)
		}
		rq := a.newRequest(tw, r)
		if a.tb != nil {
			noteRoute(rq.ctx, route, s.where)
		}
		args := make([]reflect.Value, len(binders))
		for i, b := range binders {
			v, err := b(rq)
			if err != nil {
				a.writeError(w, r, err, htmlPage)
				return
			}
			args[i] = v
		}
		if a.tb != nil {
			noteParams(rq.ctx, showParams(shown, args))
		}
		end := a.span(rq.ctx, "handler", handler)
		val, err := out.split(s.fn.Call(args))
		end()
		if tw.wrote && !val.IsValid() {
			if err != nil {
				log.Printf("web: %s %s: %v (after the handler wrote the response)", r.Method, r.URL.Path, err)
			}
			return
		}
		a.respond(rq, val, err, v, htmlPage)
	}), nil
}

func (a *App) respond(rq *request, val reflect.Value, err error, v *view, htmlPage bool) {
	w, r := rq.w, rq.r
	var to Redirect
	if errors.As(err, &to) {
		redirect(w, r, to)
		return
	}
	if err != nil {
		a.writeError(w, r, err, htmlPage)
		return
	}
	if !val.IsValid() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	x := val.Interface()
	switch n := x.(type) {
	case Redirect:
		redirect(w, r, n)
		return
	case ui.Node:
		a.page(rq, n)
		return
	}
	if v != nil && v.node != nil {
		n, err := v.node(val)
		if err != nil {
			a.writeError(w, r, err, true)
			return
		}
		a.page(rq, n)
		return
	}
	if v != nil {
		end := a.span(rq.ctx, "render", v.name)
		html, err := v.render(val)
		end()
		if err != nil {
			a.writeError(w, r, err, true)
			return
		}
		if wholePage(r) {
			html = a.linkCSS(r, html)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(html)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func redirect(w http.ResponseWriter, r *http.Request, to Redirect) {
	status := http.StatusSeeOther
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		status = http.StatusFound
	}
	http.Redirect(w, r, string(to), status)
}

// shape is how a handler's results become a value and an error.
type shape struct {
	data  reflect.Type // the value's type, a Result's unwrapped; nil for none
	split func([]reflect.Value) (reflect.Value, error)
}

func results(ft reflect.Type) (shape, error) {
	asErr := func(v reflect.Value) error {
		if v.IsNil() {
			return nil
		}
		return v.Interface().(error)
	}
	value := func(t reflect.Type) (reflect.Type, func(reflect.Value) (reflect.Value, error)) {
		if isResult(t) {
			get, _ := t.MethodByName("Get")
			return get.Type.Out(0), func(v reflect.Value) (reflect.Value, error) {
				out := v.Method(get.Index).Call(nil)
				if err := asErr(out[1]); err != nil {
					return reflect.Value{}, err
				}
				return out[0], nil
			}
		}
		return t, func(v reflect.Value) (reflect.Value, error) { return v, nil }
	}
	switch {
	case ft.NumOut() == 0:
		return shape{split: func([]reflect.Value) (reflect.Value, error) { return reflect.Value{}, nil }}, nil
	case ft.NumOut() == 1 && ft.Out(0) == errType:
		return shape{split: func(out []reflect.Value) (reflect.Value, error) { return reflect.Value{}, asErr(out[0]) }}, nil
	case ft.NumOut() == 1:
		t, get := value(ft.Out(0))
		return shape{t, func(out []reflect.Value) (reflect.Value, error) { return get(out[0]) }}, nil
	case ft.NumOut() == 2 && ft.Out(1) == errType && !isResult(ft.Out(0)):
		return shape{ft.Out(0), func(out []reflect.Value) (reflect.Value, error) {
			if err := asErr(out[1]); err != nil {
				return reflect.Value{}, err
			}
			return out[0], nil
		}}, nil
	}
	return shape{}, fmt.Errorf("results %s: a handler returns nothing, error, T, (T, error) or Result[T]", ft)
}

func isResult(t reflect.Type) bool {
	return t.PkgPath() == "github.com/vuka-lang/vuka" && strings.HasPrefix(t.Name(), "Result[")
}

func (a *App) param(s *routeSpec, p vuka.Param, pathNames []string, body *bool) (binder, error) {
	t := p.Type
	if f, ok := a.binders[t]; ok {
		return func(rq *request) (reflect.Value, error) { return f(rq.r.WithContext(rq.ctx)) }, nil
	}
	switch {
	case t == requestType:
		return func(rq *request) (reflect.Value, error) { return reflect.ValueOf(rq.r), nil }, nil
	case t == writerType:
		return func(rq *request) (reflect.Value, error) { return reflect.ValueOf(&rq.w).Elem(), nil }, nil
	case t == contextType:
		return func(rq *request) (reflect.Value, error) { return reflect.ValueOf(&rq.ctx).Elem(), nil }, nil
	case a.c.has(t):
		if err := a.c.check(t, s.where); err != nil {
			return nil, err
		}
		a.needs = append(a.needs, t)
		return func(*request) (reflect.Value, error) { return a.c.get(t) }, nil
	case scalar(t):
		if p.Name == "" || p.Name == "_" {
			return nil, errors.New("a scalar parameter binds by name, and this one has none (in plain Go, name it with web.Params)")
		}
		name, inPath := p.Name, slices.Contains(pathNames, p.Name)
		return func(rq *request) (reflect.Value, error) {
			var vals []string
			if inPath {
				vals = []string{a.router.Param(rq.r, name)}
			} else {
				vals = rq.r.URL.Query()[name]
			}
			v := reflect.New(t).Elem()
			if len(vals) == 0 {
				return v, nil
			}
			if err := setText(v, vals); err != nil {
				return v, BadRequest(fmt.Sprintf("%s: %v", name, err))
			}
			return v, nil
		}, nil
	case bindable(t):
		if *body {
			return nil, errors.New("a handler binds one struct from the request, and this is the second")
		}
		*body = true
		return structBinder(t, pathNames, a.router), nil
	case t.Kind() == reflect.Pointer || t.Kind() == reflect.Interface:
		return nil, fmt.Errorf("no provider for %s, needed by %s", t, s.where)
	}
	return nil, errors.New("can't be bound from a request")
}

// bindable reports whether t is a struct, or pointer to one, with exported
// fields to fill from a request: a *sql.DB is a missing dependency instead.
func bindable(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct && len(fieldsOf(t)) > 0
}

// scalar reports whether t is set from text: strings, numbers, bools, a
// TextUnmarshaler, a pointer to one of these (optional), or a slice of them.
func scalar(t reflect.Type) bool {
	if reflect.PointerTo(t).Implements(textType) {
		return true
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	case reflect.Pointer:
		return t.Elem().Kind() != reflect.Pointer && t.Elem().Kind() != reflect.Slice && scalar(t.Elem())
	case reflect.Slice:
		return t.Elem().Kind() != reflect.Slice && t.Elem().Kind() != reflect.Pointer && scalar(t.Elem())
	}
	return false
}

func setText(v reflect.Value, vals []string) error {
	t := v.Type()
	if u, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
		return u.UnmarshalText([]byte(vals[0]))
	}
	switch t.Kind() {
	case reflect.Pointer:
		p := reflect.New(t.Elem())
		if err := setText(p.Elem(), vals); err != nil {
			return err
		}
		v.Set(p)
		return nil
	case reflect.Slice:
		s := reflect.MakeSlice(t, len(vals), len(vals))
		for i, x := range vals {
			if err := setText(s.Index(i), []string{x}); err != nil {
				return err
			}
		}
		v.Set(s)
		return nil
	}
	s := vals[0]
	switch t.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("%q is not a bool", s)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, t.Bits())
		if err != nil {
			return fmt.Errorf("%q is not an integer", s)
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, t.Bits())
		if err != nil {
			return fmt.Errorf("%q is not a non-negative integer", s)
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, t.Bits())
		if err != nil {
			return fmt.Errorf("%q is not a number", s)
		}
		v.SetFloat(f)
	}
	return nil
}

type formField struct {
	key, name string
	index     []int
	required  bool
	settable  bool
}

func fieldsOf(t reflect.Type) []formField {
	var fs []formField
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() || f.Anonymous {
			continue
		}
		key := f.Name
		for _, tag := range []string{"form", "json"} {
			if n, _, _ := strings.Cut(f.Tag.Get(tag), ","); n == "-" {
				key = ""
				break
			} else if n != "" {
				key = n
				break
			}
		}
		if key == "" {
			continue
		}
		fs = append(fs, formField{key: key, name: f.Name, index: f.Index,
			required: slices.Contains(strings.Split(f.Tag.Get("validate"), ","), "required"), settable: scalar(f.Type)})
	}
	return fs
}

const maxBody = 10 << 20

func structBinder(t reflect.Type, pathNames []string, router Router) binder {
	st := t
	if t.Kind() == reflect.Pointer {
		st = t.Elem()
	}
	fields := fieldsOf(st)
	return func(rq *request) (reflect.Value, error) {
		r := rq.r
		ptr := reflect.New(st)
		v := ptr.Elem()
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if ct == "application/json" || strings.HasSuffix(ct, "+json") {
			if err := json.NewDecoder(http.MaxBytesReader(rq.w, r.Body, maxBody)).Decode(ptr.Interface()); err != nil {
				return v, bodyError(err, "invalid JSON: ")
			}
		} else {
			if ct == "multipart/form-data" {
				if err := r.ParseMultipartForm(maxBody); err != nil {
					return v, bodyError(err, "invalid form: ")
				}
			} else if err := r.ParseForm(); err != nil {
				return v, bodyError(err, "invalid form: ")
			}
			for _, f := range fields {
				if !f.settable {
					continue
				}
				for k, vals := range r.Form {
					if strings.EqualFold(k, f.key) && len(vals) > 0 {
						if err := setText(v.FieldByIndex(f.index), vals); err != nil {
							return v, BadRequest(fmt.Sprintf("%s: %v", f.key, err))
						}
						break
					}
				}
			}
		}
		for _, name := range pathNames {
			for _, f := range fields {
				if f.settable && (strings.EqualFold(f.key, name) || strings.EqualFold(f.name, name)) {
					if err := setText(v.FieldByIndex(f.index), []string{router.Param(r, name)}); err != nil {
						return v, BadRequest(fmt.Sprintf("%s: %v", name, err))
					}
				}
			}
		}
		for _, f := range fields {
			if f.required && v.FieldByIndex(f.index).IsZero() {
				return v, BadRequest(f.key + " is required")
			}
		}
		if t.Kind() == reflect.Pointer {
			return ptr, nil
		}
		return v, nil
	}
}

// trackingWriter notes whether the handler wrote the response itself, so
// that a handler returning nothing after writing gets no default status.
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *trackingWriter) WriteHeader(code int) {
	if code >= 200 || code == http.StatusSwitchingProtocols {
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *trackingWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *trackingWriter) ReadFrom(r io.Reader) (int64, error) {
	w.wrote = true
	return io.Copy(w.ResponseWriter, r)
}

func (w *trackingWriter) Flush() {
	w.wrote = true
	http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *trackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.wrote = true
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// Unwrap gives http.ResponseController the underlying writer.
func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
