package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
)

// Forms: a live page's <ui.Form for> is served on both transports. Over the
// page's connection its submits and changes are events; without one the
// form posts to the page itself, which this file answers: the page renders
// as for a GET, the form's submit runs as its event would, and the answer
// is 303 to where the submit handler sent the visitor (the page itself when
// it sent them nowhere), 422 with the page showing the form's errors, or
// 200 with the page for a button's intent (the Update button of a field
// other fields' choices depend on).

// formHost is the app as ui's forms ask it (ui.FormHost): the CSRF token
// a form carries, the key signing what it rendered, where submits'
// outcomes are kept.
type formHost struct{ a *App }

func (h formHost) FormToken(ctx context.Context) string {
	rq, _ := ctx.Value(ctxKey{}).(*request)
	if rq == nil {
		return ""
	}
	return h.a.csrfToken(rq, true)
}

func (h formHost) FormKey() []byte { return h.a.liveKey }

func (h formHost) FormStore() ui.FormStore { return h.a.formStore }

// FormReport keeps what a struct form's render showed of its errors, for
// a check or a submit sent over HTTP to be answered with (formPost).
func (h formHost) FormReport(ctx context.Context, r ui.FormReport) {
	rq, _ := ctx.Value(ctxKey{}).(*request)
	if rq == nil {
		return
	}
	if rq.formReports == nil {
		rq.formReports = map[string]ui.FormReport{}
	}
	rq.formReports[r.Form] = r
}

// FormHeader marks a form the runtime sends over HTTP, from a page that
// isn't live: "check" for its checks as the visitor leaves a field (the
// page answers its errors, nothing saved), "submit" for its submit (its
// errors, 422, or where to go). Either answer is JSON (formAnswer), so
// the runtime puts the errors in place without touching the fields.
const FormHeader = "Vk-Form"

// formAnswer is the JSON a form sent over HTTP gets: the form's errors, or
// where the submit sent the visitor.
type formAnswer struct {
	*ui.FormReport
	Location string `json:"location,omitempty"`
}

// FormStore keeps what came of the app's form submits, by their
// idempotency keys, instead of in this process's memory: an app with
// several replicas keeps them where every replica sees them.
func FormStore(s ui.FormStore) Option {
	return OptionFunc(func(a *App) {
		a.mutable("FormStore")
		a.formStore = s
	})
}

// formTwins are the POST routes a page's forms post to without a
// connection: one per GET route of a page — a live page, or one that isn't
// (whose forms the runtime sends over HTTP) — with no POST of its own at
// its path. A twin answers a post naming a form (its _vk_form field) only
// (formOnly).
func formTwins(specs []*routeSpec) []*routeSpec {
	posts := map[string]bool{}
	for _, s := range specs {
		if s.method == http.MethodPost {
			posts[s.path] = true
		}
	}
	var out []*routeSpec
	for _, s := range specs {
		if s.method != http.MethodGet || s.live != nil && s.live.island || s.island != nil || !s.page && s.live == nil && !returnsNode(s.fn) || posts[s.path] {
			continue
		}
		t := *s
		t.method, t.where, t.formPost = http.MethodPost, s.where, true
		t.attrs, t.mw, t.mwOf = slices.Clone(s.attrs), slices.Clone(s.mw), slices.Clone(s.mwOf)
		out = append(out, &t)
	}
	return out
}

var (
	errNoForm       = &Error{Status: http.StatusMethodNotAllowed, Message: "method not allowed"}
	errStaleForm    = &Error{Status: http.StatusBadRequest, Message: "the form was sent from a page that has changed: reload it and send the form again"}
	errFormTooLarge = &Error{Status: http.StatusRequestEntityTooLarge, Message: "the form is too large"}
)

// returnsNode reports whether the handler fn returns a page (a ui.Node, or
// a pointer to a struct that is one).
func returnsNode(fn reflect.Value) bool {
	if !fn.IsValid() || fn.Kind() != reflect.Func {
		return false
	}
	for i := range fn.Type().NumOut() {
		if t := fn.Type().Out(i); t.Implements(nodeType) || t.Kind() != reflect.Interface && reflect.PointerTo(t).Implements(nodeType) {
			return true
		}
	}
	return false
}

// formOnly is a form twin's handler h, answering a post naming a form
// only: anything else is a POST the page doesn't take, refused before its
// handler runs.
func (a *App) formOnly(s *routeSpec, h http.Handler) http.Handler {
	limit := a.bodyLimit(s.attrs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limit > 0 && r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				a.writeError(w, r, errFormTooLarge, true)
				return
			}
			a.writeError(w, r, BadRequest("the form doesn't parse"), true)
			return
		}
		if r.PostForm.Get(ui.FormIDField) == "" {
			w.Header().Set("Allow", "GET, HEAD")
			a.writeError(w, r, errNoForm, true)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// formPost answers a form posted to its live page without a connection:
// the page n renders in a session, as its GET would, and the form the post
// names (its _vk_form field) is submitted with the posted values.
func (a *App) formPost(rq *request, n ui.Node) {
	w, r := rq.w, rq.r
	if err := r.ParseMultipartForm(32 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			a.writeError(w, r, errFormTooLarge, true)
			return
		}
		a.writeError(w, r, BadRequest("the form doesn't parse"), true)
		return
	}
	id := r.PostForm.Get(ui.FormIDField)
	if id == "" {
		w.Header().Set("Allow", "GET, HEAD")
		a.writeError(w, r, errNoForm, true)
		return
	}
	end := a.span(rq.ctx, "render", "page")
	s := live.NewSession(rq.ctx, func(context.Context) ui.Node { return n })
	defer s.Close()
	if _, err := s.Render(); err != nil {
		end()
		a.writeError(w, r, err, true)
		return
	}
	p, err := s.Submit(id, r.PostForm)
	end()
	var nf *live.NoFormError
	switch {
	case errors.As(err, &nf):
		a.writeError(w, r, errStaleForm, true)
		return
	case err != nil:
		a.writeError(w, r, err, true)
		return
	}
	if mode := r.Header.Get(FormHeader); mode == "check" || mode == "submit" {
		a.formAnswer(rq, id, p.Err)
		return
	}
	if to, ok := ui.RedirectOf(p.Err); ok {
		redirect(w, r, Redirect(to))
		return
	}
	if p.Err == nil || errors.Is(p.Err, ui.ErrFormChecked) {
		redirect(w, r, Redirect(r.URL.RequestURI()))
		return
	}
	if !ui.QuietError(p.Err) {
		a.writeError(w, r, p.Err, true)
		return
	}
	intent := errors.Is(p.Err, ui.ErrFormIntent)
	if err := refusedVars(rq.ctx); err != nil {
		a.WriteError(w, r, err, true)
		return
	}
	html := withTitle([]byte(s.HTML()), ui.PageTitle(rq.ctx))
	status := http.StatusOK // an intent's: the form again, its choices following its values
	if !intent {
		html, status = errorTitle(html), http.StatusUnprocessableEntity
	}
	var script string
	if bytes.Contains(html, []byte(" data-vk-js-")) || bytes.Contains(html, []byte(" data-vk-b")) || bytes.Contains(html, []byte(" data-vk-check=")) {
		script = a.scriptTag(r, "")
	}
	head := a.headTags(r, html)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	writePage(w, html, head, script)
}

// formAnswer answers a form the runtime sent over HTTP (FormHeader): 200
// with where to go once the submit succeeded — Set-Cookie and all, so a
// Save signing the visitor in works — or the form's errors, 422 for a
// submit that failed, 200 for a check. Any other error is the app's error
// page, as for a plain POST.
func (a *App) formAnswer(rq *request, id string, err error) {
	w, r := rq.w, rq.r
	w.Header().Set("Cache-Control", "no-store")
	if to, ok := ui.RedirectOf(err); ok {
		writeJSON(w, http.StatusOK, formAnswer{Location: to})
		return
	}
	if err == nil {
		writeJSON(w, http.StatusOK, formAnswer{Location: r.URL.RequestURI()})
		return
	}
	if !ui.QuietError(err) || errors.Is(err, ui.ErrFormIntent) {
		a.writeError(w, r, err, true)
		return
	}
	rep, ok := rq.formReports[id]
	if !ok {
		a.writeError(w, r, errStaleForm, true)
		return
	}
	status := http.StatusUnprocessableEntity
	if errors.Is(err, ui.ErrFormChecked) {
		status = http.StatusOK
	}
	writeJSON(w, status, formAnswer{FormReport: &rep})
}

// errorTitle starts the page's title with "Error: ", as a page showing a
// form's errors says first.
func errorTitle(html []byte) []byte {
	i := indexFold(html, "<title")
	if i < 0 {
		return html
	}
	j := bytes.IndexByte(html[i:], '>')
	if j < 0 {
		return html
	}
	at := i + j + 1
	if bytes.HasPrefix(html[at:], []byte("Error: ")) {
		return html
	}
	return slices.Concat(html[:at], []byte("Error: "), html[at:])
}

// CSRFBypass lets cross-origin requests of these patterns (net/http
// ServeMux patterns, "/webhooks/", "POST /sso/callback") through the
// app's CSRF protection: a payment provider's webhook, an SSO callback.
func CSRFBypass(patterns ...string) Option {
	return OptionFunc(func(a *App) {
		a.mutable("CSRFBypass")
		a.csrfBypass = append(a.csrfBypass, patterns...)
	})
}

// SessionCookies names cookies that carry a visitor's session, for the CSRF
// protection to know a request carrying one: an unsafe request saying
// nothing of where it comes from (no Sec-Fetch-Site, no Origin) needs the
// form's token when it carries one of them, or the app's own (vk_csrf).
// The auth module names its session cookie itself.
func SessionCookies(names ...string) Option {
	return OptionFunc(func(a *App) {
		a.mutable("SessionCookies")
		a.sessionCookies = append(a.sessionCookies, names...)
	})
}

var errCSRFToken = &Error{Status: http.StatusForbidden, Message: "the request has no valid CSRF token: reload the page and send it again"}

// csrfTokens refuses an unsafe request that says nothing of where it comes
// from — no Sec-Fetch-Site, no Origin, so an old browser's or a script's —
// when it carries a session cookie but no valid CSRF token: the form's
// _vk_csrf field, or an X-CSRF-Token header. A browser of the last years
// says where every request comes from, and CrossOriginProtection decides on
// that; this closes the gap it leaves.
func (a *App) csrfTokens(h http.Handler) http.Handler {
	bypass := http.NewServeMux()
	for _, p := range a.csrfBypass {
		func() {
			defer func() { recover() }() // a bad pattern: addBypass reports it
			bypass.Handle(p, http.NotFoundHandler())
		}()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.unvouched(r) {
			if _, p := bypass.Handler(r); p == "" && !a.validCSRF(r) {
				a.writeError(w, r, errCSRFToken, acceptsHTML(r))
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// unvouched reports whether r is an unsafe request carrying a session
// cookie and saying nothing of where it comes from.
func (a *App) unvouched(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	if r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Origin") != "" || bearer(r) {
		return false
	}
	for _, c := range r.Cookies() {
		if c.Name == csrfCookie || slices.Contains(a.sessionCookies, c.Name) {
			return true
		}
	}
	return false
}

// validCSRF reports whether r carries the visitor's CSRF token.
func (a *App) validCSRF(r *http.Request) bool {
	tok := r.Header.Get("X-CSRF-Token")
	if tok == "" {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			r.ParseMultipartForm(32 << 20)
		}
		tok = r.PostFormValue(ui.FormCSRFField)
	}
	if tok == "" {
		return false
	}
	rq := &request{r: r, app: a, ctx: r.Context()}
	if c, err := r.Cookie(csrfCookie); err != nil || len(c.Value) < 22 {
		return false
	}
	return hmac.Equal([]byte(tok), []byte(a.csrfToken(rq, false)))
}

// addBypass adds a CSRFBypass pattern, which net/http panics on when it
// doesn't parse.
func addBypass(cop *http.CrossOriginProtection, pattern string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("web: CSRFBypass(%q): %v", pattern, r)
		}
	}()
	cop.AddInsecureBypassPattern(pattern)
	return nil
}
