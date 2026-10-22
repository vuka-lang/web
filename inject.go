package web

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2/diag"
)

// Hooks and handlers declare what they need by parameter type, and the app
// fills each parameter from the one source its type names:
//
//	context.Context                the request's, or the live session's
//	*http.Request                  the request (a live page's: the one it joined with)
//	http.ResponseWriter            the response (a route's handler)
//	a ParamFrom type (auth's)      its function's value for the request
//	a type app.Service or          the container's singleton
//	  web.Provide registered
//	url.Values                     the form's fields, as sent
//	a struct, or pointer to one    the form or JSON body (a route), the event's form (a live handler), validated
//	a string, number or bool       a route: the path segment of its name, else the query parameter;
//	                               a live handler: the event's value
//
// A parameter nothing fills is a Build error, with the file, the line and
// its type: E0407 for a pointer or interface nothing provides, E0405 for
// a type a request or an event can't carry. The same rule fills a route's
// handler, a controller's method, a live page's hooks (@ui.Mount,
// @ui.OnChange, @ui.Loads) and event handlers, a layout's or guard's Load
// and a middleware's constructor; package ui calls the hooks and event
// handlers through the app's injector, which its sessions' context carries.

const provideHelp = "register it in main with app.Service(NewX), or web.Provide(NewX)"

// injector is the app's ui.Injector: what fills a live page's hooks' and
// handlers' parameters beyond the context and the event.
type injector struct{ a *App }

func (i injector) Inject(t reflect.Type) func(context.Context) (reflect.Value, error) {
	a := i.a
	if f, ok := a.binders[t]; ok {
		return func(ctx context.Context) (reflect.Value, error) {
			r := Request(ctx)
			if r == nil {
				return reflect.Value{}, fmt.Errorf("web: a %s comes from a request, and this call has none", t)
			}
			return f(r.WithContext(ctx))
		}
	}
	switch {
	case t == requestType:
		return func(ctx context.Context) (reflect.Value, error) {
			r := Request(ctx)
			if r == nil {
				return reflect.Value{}, errors.New("web: an *http.Request comes from a request, and this call has none")
			}
			return reflect.ValueOf(r), nil
		}
	case a.c.has(t):
		return func(context.Context) (reflect.Value, error) { return a.c.get(t) }
	}
	return nil
}

// checking is the injector Build checks pages with: a provided type it
// fills is checked (its dependencies, no cycle) and built when the app
// starts.
type checking struct {
	injector
	where string
	errs  *[]error
}

func (c checking) Inject(t reflect.Type) func(context.Context) (reflect.Value, error) {
	f := c.injector.Inject(t)
	if f != nil && c.a.c.has(t) {
		if err := c.a.c.check(t, c.where); err != nil {
			*c.errs = append(*c.errs, err)
		}
		c.a.needs = append(c.a.needs, t)
	}
	return f
}

// checkParams reports every parameter of the page t's hooks and event
// handlers nothing fills, each at its method's file and line.
func (a *App) checkParams(t reflect.Type, what, where string) error {
	var errs []error
	for _, e := range ui.CheckParams(t, checking{injector{a}, where, &errs}) {
		code, help := diag.Code("E0405"), "a handler takes the context, one value from the event (its value; its form, as a struct or url.Values; or the ui.Event) and what the app provides"
		switch {
		case e.Dependency:
			code, help = "E0407", provideHelp
		case e.Hook:
			help = "a hook takes the context (OnChange: and the ui.Changed set) and what the app provides"
		}
		d := diag.New(code, "%s: %s.%s's %s parameter, %s, %s", what, e.Page.Name(), e.Method, ordinal(e.Index+1), e.Type, e.Reason).WithHelp("%s", help)
		if e.File != "" {
			d = d.At(diag.Span{File: e.File, Line: e.Line})
		} else {
			d = d.At(whereSpan(where))
		}
		errs = append(errs, d)
	}
	return errors.Join(errs...)
}

// requestParam binds a parameter only the request, the app's ParamFrom
// functions or the container fill — a layout's or guard's Load's; nil and
// an error when none does.
func (a *App) requestParam(t reflect.Type, where string) (binder, error) {
	if f, ok := a.binders[t]; ok {
		return func(rq *request) (reflect.Value, error) { return f(rq.r.WithContext(rq.ctx)) }, nil
	}
	switch {
	case t == requestType:
		return func(rq *request) (reflect.Value, error) { return reflect.ValueOf(rq.r), nil }, nil
	case t == contextType:
		return func(rq *request) (reflect.Value, error) { return reflect.ValueOf(&rq.ctx).Elem(), nil }, nil
	case a.c.has(t):
		if err := a.c.check(t, where); err != nil {
			return nil, err
		}
		a.needs = append(a.needs, t)
		return func(*request) (reflect.Value, error) { return a.c.get(t) }, nil
	case t.Kind() == reflect.Pointer || t.Kind() == reflect.Interface:
		return nil, diag.New("E0407", "not provided").WithHelp(provideHelp)
	}
	return nil, diag.New("E0405", "can't come from a request here").WithHelp("take the context, the request and what the app provides")
}

var formType = typeOf[url.Values]()

// formValues is the request's form fields, the query's and the body's.
func formValues(rq *request) (reflect.Value, error) {
	r := rq.r
	var err error
	if ct := r.Header.Get("Content-Type"); len(ct) >= 19 && ct[:19] == "multipart/form-data" {
		err = r.ParseMultipartForm(maxBody)
	} else {
		err = r.ParseForm()
	}
	if err != nil {
		return reflect.Value{}, bodyError(err, "invalid form: ")
	}
	return reflect.ValueOf(r.Form), nil
}

func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}
