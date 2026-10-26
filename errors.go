package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
)

// Error is an error with an HTTP status. Returned by a handler (wrapped or
// not), it answers with that status and message: an HTML page for an HTML
// route, JSON otherwise.
type Error struct {
	Status  int
	Message string
	Err     error // the cause, never shown
}

func (e *Error) Error() string {
	if e.Message == "" {
		return http.StatusText(e.Status)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// PublicMessage is the message the browser reads (live.PublicError): a live
// page's handler returning an *Error shows its message, as an HTTP route's
// answer does, where any other error's text stays in the log.
func (e *Error) PublicMessage() string { return e.Error() }

// StatusCode is the error's status.
func (e *Error) StatusCode() int { return e.Status }

// Invalid is an error naming fields and what is wrong with each: a
// component's call answering with it shows each field's message
// (live.error.fields), a route answers 422 with them.
//
//	return web.Invalid{"q": "type at least 2 letters"}
type Invalid map[string]string

func (e Invalid) Error() string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("invalid")
	for i, k := range keys {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(k + ": " + e[k])
	}
	return b.String()
}

func (e Invalid) PublicMessage() string          { return e.Error() }
func (Invalid) StatusCode() int                  { return http.StatusUnprocessableEntity }
func (e Invalid) FieldErrors() map[string]string { return e }
func (e Invalid) Unwrap() error                  { return nil }

// Is matches another *Error of the same status: errors.Is(err, web.NotFound).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Status == e.Status
}

var (
	NotFound     = &Error{Status: http.StatusNotFound, Message: "not found"}
	Unauthorized = &Error{Status: http.StatusUnauthorized, Message: "unauthorized"}
	Forbidden    = &Error{Status: http.StatusForbidden, Message: "forbidden"}
	Conflict     = &Error{Status: http.StatusConflict, Message: "conflict"}
)

// Status is an error answering with status and the formatted message.
func Status(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// BadRequest is a 400 error with msg.
func BadRequest(msg string) *Error { return &Error{Status: http.StatusBadRequest, Message: msg} }

// Redirect, returned by a handler as its result or as its error, redirects to
// the URL: 302 for GET and HEAD, 303 See Other otherwise. It is also a
// vuka.Node, so a handler returning a page can redirect instead.
type Redirect string

func (r Redirect) Error() string                           { return "redirect to " + string(r) }
func (r Redirect) Render(context.Context, io.Writer) error { return nil }

// RedirectLocation is the URL: a live page's handler returning a Redirect
// moves the page there (ui.RedirectOf).
func (r Redirect) RedirectLocation() string { return string(r) }

func (a *App) writeError(w http.ResponseWriter, r *http.Request, err error, htmlPage bool) {
	// A redirect returned anywhere — a page handler, a live view's Mount or
	// render — redirects, rather than becoming a 500. The request path handles
	// this before erroring; doing it here covers the live paths too.
	var to Redirect
	if errors.As(err, &to) {
		redirect(w, r, to)
		return
	}
	var e *Error
	var sc interface{ StatusCode() int }
	var pub interface{ PublicMessage() string }
	if !errors.As(err, &e) && errors.As(err, &sc) && errors.As(err, &pub) {
		e = &Error{Status: sc.StatusCode(), Message: pub.PublicMessage(), Err: err}
	}
	if e == nil {
		e = &Error{Status: http.StatusInternalServerError, Err: err}
		if a.dev {
			e.Message = err.Error()
		}
	}
	if e.Status >= 500 {
		// Server errors go to the console, not only the response: one
		// clean line with the method, path and error.
		log.Printf("web: %d %s %s: %v", e.Status, r.Method, r.URL.Path, err)
	}
	if a.onError != nil {
		a.onError(w, r, e, htmlPage)
		return
	}
	if htmlPage {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(e.Status)
		fmt.Fprintf(w, "<!doctype html>\n<title>%d %s</title>\n<h1>%s</h1>\n<p>%s</p>\n",
			e.Status, http.StatusText(e.Status), http.StatusText(e.Status), html.EscapeString(e.Error()))
		return
	}
	body := map[string]any{"status": e.Status, "error": e.Error()}
	var fe interface{ FieldErrors() map[string]string }
	if errors.As(err, &fe) && len(fe.FieldErrors()) > 0 {
		body["fields"] = fe.FieldErrors()
	}
	writeJSON(w, e.Status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
