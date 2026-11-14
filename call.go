package web

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/vuka-lang/ui/v2"
)

// Calls (ui.JS.Call(component)) on a page that isn't live: the runtime
// opens the component with a POST of the page's URL carrying CallHeader,
// the step's token, the CSRF token, and in the form the key it names the
// component by (_vk_key) and the props it read (_vk_reads). The token must
// be one this page rendered for this visitor — sealed with the app's form
// key, bound to the visitor's CSRF token (their session cookie and who is
// signed in), unexpired, naming a component a Call names
// (ui.ParseCall) — else the call is refused. The route runs as for a
// push: the app's middleware and the route's own, then the page's guards;
// then the component renders with its props, in its presentation
// (ui.RenderCall), and the reply is its markup, {"html"}, which the runtime
// puts in the overlay host. The token proves only what the page rendered:
// the component checks that the visitor may see what it shows. A live
// page's Calls go over its connection instead.

// CallHeader carries a Call step's open: its token, as the page rendered it.
const CallHeader = "Vk-Call"

// overlayHost is the element the document shell holds the opened
// components in, after the runtime's script: what follows the script is the
// runtime's, so a live page's morph leaves it.
const overlayHost = `<div id="vk-overlays" data-vk-overlays></div>`

// callOpen answers a Call's open for the page route s.
func (a *App) callOpen(w http.ResponseWriter, r *http.Request, s *routeSpec, guards []*layoutSpec, pathNames []string, token string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", CallHeader)
	if r.Method != http.MethodPost {
		a.writeError(w, r, &Error{Status: http.StatusMethodNotAllowed, Message: "a call opens with a POST"}, false)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		a.writeError(w, r, errCrossOrigin, false)
		return
	}
	if limit := a.bodyLimit(s.attrs); limit > 0 && r.Body != nil && r.Body != http.NoBody {
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
	key, reads := r.PostForm.Get("_vk_key"), r.PostForm.Get("_vk_reads")
	r2 := r.Clone(r.Context())
	r2.Form, r2.PostForm = nil, nil
	rq := a.newRequest(w, r2, s)
	c, err := ui.ParseCall(rq.ctx, a.csrfToken(rq, false), token, json.RawMessage(reads))
	switch {
	case errors.Is(err, ui.ErrCallToken):
		err = &Error{Status: http.StatusForbidden, Message: "the call isn't one this page rendered for you: reload the page"}
	case err == nil && !validCallKey(key), err == nil && c.Key() != "" && c.Key() != key:
		err = BadRequest("the call has no key, or another than its token's")
	case err == nil && c.Stateful():
		err = BadRequest(c.Component() + " is a live component: it opens on a live page")
	}
	if err == nil {
		_, err = a.loadLayouts(rq, guards, pathNames)
	}
	var comp any
	if err == nil {
		comp, err = c.Value(nil)
	}
	if err != nil {
		a.callError(w, r2, err)
		return
	}
	defer a.span(rq.ctx, "render", c.Component())()
	html, failed := ui.RenderCall(rq.ctx, key, comp, c.Options()...)
	if failed == nil {
		writeJSON(w, http.StatusOK, map[string]any{"html": html})
		return
	}
	reply := map[string]any{"html": html, "error": a.publicError(failed)}
	if ch, ok := ui.OnErrorOf(comp).(ui.JSChain); ok {
		if j, err := ui.EncodeJS(rq.ctx, ch); err == nil {
			reply["run"] = json.RawMessage(j)
		}
	}
	writeJSON(w, http.StatusOK, reply)
}

// validCallKey reports whether key is one the runtime names a component
// by: letters, digits, - and _, at most 40.
func validCallKey(key string) bool {
	if key == "" || len(key) > 40 {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// publicError is err as a call's reply shows it.
func (a *App) publicError(err error) *ui.IslandError {
	e := ui.ErrorOf(err)
	if e.Status == 0 {
		e.Status = http.StatusInternalServerError
		if a.dev && e.Message == "error" {
			e.Message = err.Error()
		}
	}
	return e
}

// callError answers an open that failed before its component rendered.
func (a *App) callError(w http.ResponseWriter, r *http.Request, err error) {
	var to Redirect
	if errors.As(err, &to) {
		writeJSON(w, http.StatusOK, map[string]any{"redirect": string(to)})
		return
	}
	e := a.publicError(err)
	if e.Status >= 500 {
		log.Printf("web: %d %s %s call%s: %v", e.Status, r.Method, r.URL.Path, a.whereTag(r), err)
	}
	writeJSON(w, e.Status, map[string]any{"error": e})
}
