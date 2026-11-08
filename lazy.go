package web

import (
	"errors"
	"net/http"

	"github.com/vuka-lang/ui/v2"
)

// Lazy regions (ui.Lazy, and a form type's form rendered hidden, waiting
// for its Init) of a page that isn't live: the page's first render has an
// empty region in each one's place, data-vk-region naming it. When the
// browser first shows one the runtime asks the page for it — a GET of the
// page's URL with LazyHeader naming the region, as it asks for the page's
// parts — and puts the reply in the region's place. The route runs as a
// page load does (guards, layouts' Load, the page's inputs) and the page
// renders again for that region only (ui.RenderRegion): its content
// rendered, the form's Init run; nothing the browser sends but the region's
// name is used. A live page's regions go over its connection instead.

// LazyHeader, on a GET of a page, asks for the lazy region its value names
// (a data-vk-region) instead of the page; a reply carrying it is the region.
const LazyHeader = "Vk-Lazy"

var errNoRegion = &Error{Status: http.StatusNotFound, Message: "the page has changed since it loaded: reload it"}

// lazyRegion answers the runtime's request for a lazy region of the page n.
func (a *App) lazyRegion(rq *request, n ui.Node, id string) {
	w, r := rq.w, rq.r
	defer a.span(rq.ctx, "render", "region")()
	html, err := ui.RenderRegion(rq.ctx, n, id)
	if errors.Is(err, ui.ErrNoRegion) {
		err = errNoRegion
	}
	if err == nil {
		err = refusedVars(rq.ctx)
	}
	if err != nil {
		a.writeError(w, r, err, true)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set(LazyHeader, "1")
	if !headerHasToken(h, "Vary", LazyHeader) {
		h.Add("Vary", LazyHeader)
	}
	w.Write([]byte(html))
}
