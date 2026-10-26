package web

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
)

// recoverHTTP covers custom routers, middleware and framework handlers as
// well as compiled routes. It does not buffer streams or WebSocket upgrades.
func (a *App) recoverHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer a.recoverRequest(tw, r, strings.Contains(r.Header.Get("Accept"), "text/html"))
		next.ServeHTTP(tw, r)
	})
}

func logPanic(r *http.Request, p any) {
	log.Printf("web: panic %s %s: %v\n%s", r.Method, r.URL.Path, p, debug.Stack())
}

// recoverRequest must be deferred directly so recover observes the panic.
// Once a response is committed, abort it instead of appending a success-looking
// body or a second response. ErrAbortHandler retains net/http's abort semantics.
func (a *App) recoverRequest(w *trackingWriter, r *http.Request, html bool) {
	if p := recover(); p != nil {
		if p == http.ErrAbortHandler {
			panic(p)
		}
		failed, isFailure := ui.Failed(p)
		if !isFailure {
			logPanic(r, p)
		}
		if w.wrote {
			if w.hijacked != nil {
				w.hijacked.Close()
			}
			panic(http.ErrAbortHandler)
		}
		if isFailure {
			// A (T, error) call in markup run outside a render (a layout's
			// markup, a handler's): its error, as the render's would be.
			a.panicResponse(w, r, failed, html)
			return
		}
		a.panicResponse(w, r, fmt.Errorf("panic: %v", p), html)
	}
}

// render renders n into w: a (T, error) call in its markup failing
// (ui.Must) is its error, as a Mount error is.
func render(ctx context.Context, n ui.Node, w io.Writer) (err error) {
	defer ui.Catch(&err)
	return n.Render(ctx, w)
}

// Drop framing and representation metadata from an unfinished response, while
// retaining security, CORS and other middleware headers.
func resetPanicHeaders(h http.Header) {
	for _, name := range []string{"Content-Length", "Content-Encoding", "Content-Range", "ETag", "Last-Modified", "Trailer", "Location"} {
		h.Del(name)
	}
}

func (a *App) panicResponse(w *trackingWriter, r *http.Request, err error, html bool) {
	// A broken custom error handler must not turn recovery into another panic
	// response or leak its own panic to the client.
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				panic(p)
			}
			logPanic(r, p)
			if w.wrote {
				if w.hijacked != nil {
					w.hijacked.Close()
				}
				panic(http.ErrAbortHandler)
			}
			resetPanicHeaders(w.Header())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
	}()
	resetPanicHeaders(w.Header())
	a.writeError(w, r, err, html)
}

// recoverSocket runs before the connection context is canceled, so a live
// render/guard panic can close the socket cleanly. A failed session is discarded.
func (a *App) recoverSocket(ws *websocket.Conn, r *http.Request) {
	if p := recover(); p != nil {
		if p == http.ErrAbortHandler {
			panic(p)
		}
		logPanic(r, p)
		ws.Close(websocket.StatusInternalError, http.StatusText(http.StatusInternalServerError))
	}
}
