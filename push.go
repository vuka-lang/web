package web

import (
	"errors"
	"log"
	"net/http"
	"reflect"
	"strconv"

	"github.com/vuka-lang/ui/v2"
)

// Push calls (ui.JS.Push, ui.JS.Options): a method of the page or of a
// component on it, called over HTTP on the page's own URL — a page that
// isn't live, or a read (ui.Read) on any page. The runtime asks the page's
// URL with PushHeader, the reference the page rendered, and the arguments
// in the _vk_args parameter (the enclosing form's fields in _vk_form when
// a struct takes them): a GET for a read, a POST for a write, carrying the
// CSRF token. The page's route answers it before its handler runs: the
// app's middleware and the route's own run (who the visitor is, its auth,
// the CSRF check of a POST), then the page's guards, then the method, on a
// fresh receiver — the page's struct with its path and query fields bound
// when the method is the page's, else a component's zero value — with its
// dependencies injected. Neither the page's Load (its mount hook) nor its
// layouts' run: a method needing something takes it as an argument or a
// dependency.
//
// The custom header makes a cross-origin call a CORS preflight the app
// never answers, and a request a browser says is cross-site is refused
// outright. Only a reference the route rendered is taken
// (ui.ParsePushCall): it is signed for the route's pattern with the app's
// form key (ui.WithPushScope, newRequest), and says whether the call is a
// read, which only then may be a GET. A reply is no-store unless the step
// let the browser cache it (ui.Cache, in the signed reference).

// PushHeader carries a Push step's call: the method's reference as the
// page rendered it.
const PushHeader = "Vk-Push"

// OptionsHeader is PushHeader as an Options step's call sent it before
// Options became a Push; a call carrying it is answered the same.
const OptionsHeader = "Vk-Options"

// pushArgsParam and pushFormParam are the parameters carrying a call's
// arguments, a JSON array, and the enclosing form's fields, a JSON object.
const (
	pushArgsParam = "_vk_args"
	pushFormParam = "_vk_form"
)

// pushCalls answers the push calls a page route gets before h, its
// handler; any other request goes on to h. s is the route, page the page's
// GET route it calls the methods of (s itself, or the GET a POST at its
// path serves).
func (a *App) pushCalls(s, page *routeSpec, h http.Handler) (http.Handler, error) {
	what := page.method + " " + page.path + " (" + whereName(page.where) + ")"
	gs := *page
	gs.layouts, gs.reloads = nil, nil
	for _, ref := range page.layouts {
		if ref.guard {
			gs.layouts = append(gs.layouts, ref)
		}
	}
	guards, err := a.compileLayouts(&gs, what)
	if err != nil {
		return nil, err
	}
	pathNames := pathParams(page.path)
	typ, ctor, props, bind := a.pushPage(page)
	var template reflect.Value
	if page.live != nil {
		template = page.live.template
	}
	limit := a.bodyLimit(s.attrs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref := r.Header.Get(PushHeader)
		if ref == "" {
			ref = r.Header.Get(OptionsHeader)
		}
		if ref == "" || r.Method != http.MethodGet && r.Method != http.MethodPost {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Add("Vary", PushHeader+", "+OptionsHeader)
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			a.writeError(w, r, errCrossOrigin, false)
			return
		}
		r2 := r.Clone(r.Context())
		var args, form string
		if r.Method == http.MethodGet {
			q := r.URL.Query()
			args, form = q.Get(pushArgsParam), q.Get(pushFormParam)
			q.Del(pushArgsParam)
			q.Del(pushFormParam)
			r2.URL.RawQuery = q.Encode()
		} else {
			if limit > 0 && r.Body != nil && r.Body != http.NoBody {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			if err := r.ParseForm(); err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					a.writeError(w, r, errFormTooLarge, false)
					return
				}
				a.writeError(w, r, BadRequest("the call doesn't parse"), false)
				return
			}
			args, form = r.PostForm.Get(pushArgsParam), r.PostForm.Get(pushFormParam)
			r2.Form, r2.PostForm = nil, nil
		}
		rq := a.newRequest(w, r2, s)
		call, err := ui.ParsePushCall(rq.ctx, ref, args, form)
		switch {
		case errors.Is(err, ui.ErrPushRef):
			err = BadRequest("the call names no method this page calls: reload the page")
		case err == nil && r.Method == http.MethodGet && !call.Read():
			err = &Error{Status: http.StatusMethodNotAllowed, Message: "the call changes things: send it as a POST"}
		}
		if err == nil {
			_, err = a.loadLayouts(rq, guards, pathNames)
		}
		if err == nil {
			var x reflect.Value
			end := a.span(rq.ctx, "handler", call.Method())
			if t := call.Receiver(); t == typ {
				x, err = a.newComponent(t, template, ctor)
				if err == nil && props != nil {
					err = bind(x.Elem(), props, pathNames, r2)
				}
			} else {
				x = reflect.New(t)
			}
			var res any
			if err == nil {
				res, err = call.Call(rq.ctx, x)
			}
			end()
			if err == nil {
				if d := call.Cache(); d > 0 && r.Method == http.MethodGet {
					w.Header().Set("Cache-Control", "private, max-age="+strconv.FormatInt(int64(d.Seconds()), 10))
				}
				writeJSON(w, http.StatusOK, res)
				return
			}
		}
		var to Redirect
		if errors.As(err, &to) {
			writeJSON(w, http.StatusOK, map[string]any{"redirect": string(to)})
			return
		}
		e := ui.ErrorOf(err)
		if e.Status == 0 {
			e.Status = http.StatusInternalServerError
			if a.dev && e.Message == "error" {
				e.Message = err.Error()
			}
		}
		if e.Status >= 500 {
			log.Printf("web: %d %s %s push: %v", e.Status, r.Method, r.URL.Path, err)
		}
		writeJSON(w, e.Status, map[string]any{"error": e})
	}), nil
}

// pushPage is the struct a page route's handler renders, when it is a
// folder page or a live page, with how its fields bind: a push call of
// one of its methods builds it so. Nil for any other route.
func (a *App) pushPage(s *routeSpec) (reflect.Type, reflect.Value, []prop, func(reflect.Value, []prop, []string, *http.Request) error) {
	var t reflect.Type
	var ctor reflect.Value
	switch {
	case s.live != nil && !s.live.island:
		t, ctor = s.live.typ, s.live.ctor
	case s.page && s.recvCtor.IsValid():
		t, ctor = s.attrType(), s.recvCtor
	case s.page && s.recv == nil:
		t, ctor = nodePage(s.fn), s.fn
	}
	if t == nil {
		return nil, reflect.Value{}, nil, nil
	}
	var props []prop
	var err error
	bind := a.bindProps
	if s.page {
		props, err = a.pageProps(t, s.path, s.where)
		bind = a.bindPage
	} else {
		props, err = a.liveProps(t, s.where)
	}
	if err != nil {
		props = nil // the route's own compile reports it
	}
	return t, ctor, props, bind
}
