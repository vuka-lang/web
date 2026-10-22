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
	"github.com/vuka-lang/vuka/diag"
)

type routeSpec struct {
	method, path string
	where        string // "main.ShowPet (pets.vuka:12)"
	fn           reflect.Value
	recv         reflect.Type
	params       []vuka.Param
	view         *declView
	mw           []Middleware
	live         *liveSpec   // a Live page
	island       *islandPage // a stateless page backed by its island: its GET, or its calls' POST
	attrs        []any       // typed attributes, for WrapRoutes
	html         bool        // answers with a page; set by compile
	page         bool        // a page as a pages/ folder declares it (PageRoute, LayoutOf, GuardOf, Receiver)
	layouts      []layoutRef
	recvCtor     reflect.Value // the receiver's constructor (Receiver)
	mwOf         []useOf       // UseOf's middleware, resolved when the app is built
}

type request struct {
	w   http.ResponseWriter
	r   *http.Request
	ctx context.Context
	app *App
}

func (a *App) newRequest(w http.ResponseWriter, r *http.Request) *request {
	rq := &request{w: w, r: r, app: a}
	rq.ctx = ui.WithInjector(context.WithValue(r.Context(), ctxKey{}, rq), injector{a})
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
	if s.island != nil {
		if s.method == http.MethodPost {
			return a.compileIslandCall(s)
		}
		return a.compileIslandPage(s)
	}
	if s.live != nil {
		return a.compileLive(s)
	}
	what := s.method + " " + s.path + " (" + whereName(s.where) + ")"
	fail := func(format string, args ...any) error {
		return failf("E0405", what, s.where, format, args...)
	}
	if s.page && s.recv == nil && s.method == http.MethodGet {
		if t := nodePage(s.fn); t != nil {
			return a.compileNodePage(s, t)
		}
	}
	var layouts []*layoutSpec
	if len(s.layouts) > 0 {
		var err error
		if layouts, err = a.compileLayouts(s, what); err != nil {
			return nil, err
		}
	}
	var binders []binder
	if s.recv != nil && s.recvCtor.IsValid() {
		b, err := a.pageReceiver(s, what)
		if err != nil {
			return nil, err
		}
		binders = append(binders, b)
	} else if s.recv != nil {
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
	a.nameByPath(s.params, pathNames)
	shown := make([]tbParam, len(binders), len(binders)+len(s.params)) // what the toolbar shows of each argument
	for i, p := range s.params {
		b, err := a.param(s, p, pathNames, &body)
		if err != nil {
			label := p.Name
			if label == "" {
				label = fmt.Sprint("#", i+1)
			}
			return nil, failure("E0405", fmt.Sprintf("%s: parameter %s %s", what, label, p.Type), s.where, err)
		}
		binders = append(binders, b)
		shown = append(shown, a.paramSource(p, pathNames))
	}
	out, err := results(s.fn.Type())
	if err != nil {
		return nil, failure("E0406", what, s.where, err)
	}
	htmlPage := s.view != nil || (out.data != nil && out.data.Implements(nodeType))
	s.html = htmlPage
	var v *view
	if s.view != nil {
		if out.data != nil && out.data.Implements(nodeType) {
			return nil, failf("E0417", what, s.where, "returns a vuka.Node and has a view").WithHelp("drop one")
		}
		if v, err = compileView(*s.view, out.data); err != nil {
			return nil, failure("E0417", what, s.where, err)
		}
	}
	route, handler := s.method+" "+s.path, shortName(s.where)
	limit := a.bodyLimit(s.attrs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer a.recoverRequest(tw, r, htmlPage)
		if limit > 0 && r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(tw, r.Body, limit)
		}
		rq := a.newRequest(tw, r)
		if a.tb != nil {
			noteRoute(rq.ctx, route, s.where)
		}
		var loaded []reflect.Value
		if len(layouts) > 0 {
			var err error
			if loaded, err = a.loadLayouts(rq, layouts, pathNames); err != nil {
				a.writeError(w, r, err, htmlPage)
				return
			}
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
		if n, ok := pageResult(val); ok && err == nil && len(layouts) > 0 && wholePage(r) {
			rq.ctx = titled(rq.ctx, val)
			wrapped := nestLayouts(layouts, loaded, n)
			val = reflect.ValueOf(&wrapped).Elem()
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
	return shape{}, diag.New("E0406", "results %s", ft).WithHelp("a handler returns nothing, error, T, (T, error) or Result[T]")
}

func isResult(t reflect.Type) bool {
	return t.PkgPath() == "github.com/vuka-lang/vuka" && strings.HasPrefix(t.Name(), "Result[")
}

// param binds a route handler's parameter by its type (see inject.go).
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
	case formType.ConvertibleTo(t) && t.Kind() == reflect.Map:
		if *body {
			return nil, diag.New("E0405", "a handler takes the request's form once, and this is the second").WithHelp("take one struct or url.Values")
		}
		*body = true
		return func(rq *request) (reflect.Value, error) {
			v, err := formValues(rq)
			if err != nil {
				return v, err
			}
			return v.Convert(t), nil
		}, nil
	case scalar(t):
		if p.Name == "" || p.Name == "_" {
			return nil, diag.New("E0405", "a scalar binds the path segment its position gives, and the path has none left for it").
				WithHelp("take the query in a struct, func(q Query), or add the segment to the path")
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
				if inPath && s.page {
					return v, NotFound // a folder route's segment that doesn't parse names nothing
				}
				return v, BadRequest(fmt.Sprintf("%s: %v", name, err))
			}
			return v, nil
		}, nil
	case bindable(t):
		if *body {
			return nil, diag.New("E0405", "a handler binds one struct from the request, and this is the second").WithHelp("join them in one struct, or register one of them with app.Service")
		}
		*body = true
		if err := ui.CheckValidate(t); err != nil {
			return nil, err
		}
		return structBinder(t, pathNames, a.router), nil
	case t.Kind() == reflect.Pointer || t.Kind() == reflect.Interface:
		return nil, diag.New("E0407", "not provided; needed by %s", whereName(s.where)).WithHelp(provideHelp)
	}
	return nil, diag.New("E0405", "can't come from a request").WithHelp("take the context, the request, its form (a struct or url.Values), scalars from the path and query, and what the app provides")
}

// nameByPath names the scalar parameters the compiler didn't (a handler
// registered from Go): they bind the path's segments in order, the last
// of them taking the last segment, as a nested route's handler takes the
// segments its own path adds.
func (a *App) nameByPath(params []vuka.Param, pathNames []string) {
	var unnamed []int
	for i, p := range params {
		if p.Name == "" && scalar(p.Type) && a.binders[p.Type] == nil && !a.c.has(p.Type) {
			unnamed = append(unnamed, i)
		}
	}
	off := max(len(pathNames)-len(unnamed), 0) // more scalars than segments: the first take them
	for j, i := range unnamed {
		if off+j < len(pathNames) {
			params[i].Name = pathNames[off+j]
		}
	}
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
		fs = append(fs, formField{key: key, name: f.Name, index: f.Index, settable: scalar(f.Type)})
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
		isJSON := ct == "application/json" || strings.HasSuffix(ct, "+json")
		if isJSON {
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
		if err := validateBound(ptr.Interface(), isJSON); err != nil {
			return v, err
		}
		if t.Kind() == reflect.Pointer {
			return ptr, nil
		}
		return v, nil
	}
}

// validateBound checks a bound struct's validate: tags, naming each field
// as the request did: by its json names for a JSON body, else as a form
// binds it.
func validateBound(v any, isJSON bool) error {
	var err error
	if isJSON {
		err = ui.Validate(v)
	} else {
		err = ui.ValidateTags(v, "form", "json")
	}
	if bad, ok := err.(ui.Invalid); ok {
		return Invalid(bad)
	}
	return err
}

// trackingWriter notes whether the handler wrote the response itself, so
// that a handler returning nothing after writing gets no default status.
type trackingWriter struct {
	http.ResponseWriter
	wrote    bool
	hijacked net.Conn
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
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.wrote, w.hijacked = true, conn
	}
	return conn, rw, err
}

// Push preserves HTTP/2 server push through request tracking and recovery.
func (w *trackingWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Unwrap gives http.ResponseController the underlying writer.
func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
