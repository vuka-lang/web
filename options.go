package web

import (
	"errors"
	"log"
	"net/http"
	"reflect"

	"github.com/vuka-lang/ui/v2"
)

// Options calls (ui.JS.Options): a select's options loaded from a method of
// the page or of a component on it. The runtime asks the page's own URL — a
// GET carrying OptionsHeader, the reference the page rendered, and the
// arguments in the _vk_args query parameter — and the page's route answers
// it before its handler runs: the app's middleware and the route's own run
// (who the visitor is, its auth), then the page's guards, then the method,
// on a fresh receiver — the page's struct with its path and query fields
// bound when the method is the page's, else a component's zero value —
// with its dependencies injected. Neither the page's Load (its mount hook)
// nor its layouts' run: a method needing something takes it as an argument
// or a dependency.
//
// A GET, as a read is: the arguments are reads of the page's fields, and
// the reply is no-store. The custom header makes a cross-origin call a
// CORS preflight the app never answers, and a request a browser says is
// cross-site is refused outright. Only a reference the route rendered is
// taken (ui.ParseOptionsCall): it is signed for the route's pattern with
// the app's form key (ui.WithOptionsScope, newRequest).

// OptionsHeader carries an Options command's call: the method's reference
// as the page rendered it.
const OptionsHeader = "Vk-Options"

// optionsArgsParam is the query parameter carrying an Options call's
// arguments, a JSON array.
const optionsArgsParam = "_vk_args"

// optionsCalls answers the Options calls a page route's GET gets before h,
// its handler; any other request goes on to h.
func (a *App) optionsCalls(s *routeSpec, h http.Handler) (http.Handler, error) {
	what := s.method + " " + s.path + " (" + whereName(s.where) + ")"
	gs := *s
	gs.layouts, gs.reloads = nil, nil
	for _, ref := range s.layouts {
		if ref.guard {
			gs.layouts = append(gs.layouts, ref)
		}
	}
	guards, err := a.compileLayouts(&gs, what)
	if err != nil {
		return nil, err
	}
	pathNames := pathParams(s.path)
	page, ctor, props, bind := a.optionsPage(s)
	var template reflect.Value
	if s.live != nil {
		template = s.live.template
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref := r.Header.Get(OptionsHeader)
		if ref == "" || r.Method != http.MethodGet {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Add("Vary", OptionsHeader)
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			a.writeError(w, r, errCrossOrigin, false)
			return
		}
		args := r.URL.Query().Get(optionsArgsParam)
		q := r.URL.Query()
		q.Del(optionsArgsParam)
		r2 := r.Clone(r.Context())
		r2.URL.RawQuery = q.Encode()
		rq := a.newRequest(w, r2, s)
		call, err := ui.ParseOptionsCall(rq.ctx, ref, args)
		if errors.Is(err, ui.ErrOptionsRef) {
			err = BadRequest("the options call names no method this page loads options from: reload the page")
		}
		if err == nil {
			_, err = a.loadLayouts(rq, guards, pathNames)
		}
		var x reflect.Value
		if err == nil {
			end := a.span(rq.ctx, "handler", call.Method())
			if t := call.Receiver(); t == page {
				x, err = a.newComponent(t, template, ctor)
				if err == nil && props != nil {
					err = bind(x.Elem(), props, pathNames, r2)
				}
			} else {
				x = reflect.New(t)
			}
			var opts any
			if err == nil {
				opts, err = call.Call(rq.ctx, x)
			}
			end()
			if err == nil {
				writeJSON(w, http.StatusOK, map[string]any{"options": opts})
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
			log.Printf("web: %d GET %s options: %v", e.Status, r.URL.Path, err)
		}
		writeJSON(w, e.Status, map[string]any{"error": e})
	}), nil
}

// optionsPage is the struct a page route's handler renders, when it is a
// folder page or a live page, with how its fields bind: an Options call of
// one of its methods builds it so. Nil for any other route.
func (a *App) optionsPage(s *routeSpec) (reflect.Type, reflect.Value, []prop, func(reflect.Value, []prop, []string, *http.Request) error) {
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
