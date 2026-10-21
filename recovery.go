package web

import (
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/coder/websocket"
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
		logPanic(r, p)
		if w.wrote {
			if w.hijacked != nil {
				w.hijacked.Close()
			}
			panic(http.ErrAbortHandler)
		}
		a.panicResponse(w, r, fmt.Errorf("panic: %v", p), html)
	}
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
