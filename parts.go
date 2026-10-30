package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vuka-lang/ui/v2"
)

// Parts of a page that isn't live (ui.Part, @ui.Defer on a method, a
// <ui.Suspense>): a GET page renders each part's fallback, each deferred
// method starting as the render meets it (ui.DefaultPartLimit at once), and
// is sent whole — its status and headers decided, its guards, layouts'
// Load and every method that isn't deferred run — so the document loads
// while the methods run. The methods keep the request's values but not its
// cancellation: the page's parts are parked (partsPark) under a random
// one-time token, with the page's URI and its visitor (liveWho, the session
// cookies), for PartsWindow (a prefetched page's for as long as the
// runtime keeps it). The token goes in the page (the runtime's
// script tag, data-vk-parts; PartsTokenHeader on a navigation's reply).
//
// Once the page has loaded the runtime asks for its parts: a GET of the
// page's URL with PartsHeader naming them (the ids of the page's
// <!--vk-part:id--> marks) and the token. The app's middleware runs (it
// says who the visitor is); then a token parked for this URI and visitor
// is taken — at most once — and its parts are written as each is ready, a
// group's together (ui.Parts.StreamParts), flushed through the
// compression: no guard, layout or page method runs again. The request
// going away cancels the methods still running; so does the window
// running out unclaimed, or the cap on parked pages (the oldest goes).
//
// A request whose token isn't parked here — expired, used, another
// visitor's, another replica's, a restart's, or none (HEAD,
// PartsWindow(0), whose methods never run) — renders the page again: the
// route runs as a page load does — guards, layouts' Load, the page's
// inputs from its path, query and session — and only the parts the render
// meets, of the ones asked for, run and are sent: nothing the browser
// sends but their names is used, so it can't get what the page wouldn't
// show it. A part asked for that the render no longer meets (the page
// changed since) is sent failed. The reply is never cached (Cache-Control:
// no-store), and both it and the page vary by PartsHeader.
//
// A part whose method fails shows its error fallback, else a short
// message (role="alert"); it is logged, and the toolbar shows it. The
// client going away cancels the methods still running. X-Accel-Buffering:
// no asks nginx not to buffer the reply; a proxy that buffers anyway
// delivers it whole, still correct.

// PartsHeader, on a GET of a page, asks for the page's deferred parts
// named in its value (space-separated ids) instead of the page; a reply
// carrying it is the parts.
const PartsHeader = "Vk-Parts"

// PartsTokenHeader carries the token a page's parts are parked under: on
// a navigation's reply, and on the runtime's request for the parts.
const partsTokenHeader = "Vk-Parts-Token"

// defaultPartsWindow is how long a page's parts wait for the runtime's
// request: PartsWindow.
const defaultPartsWindow = 30 * time.Second

// PartsWindow is how long a page's deferred parts, their methods started
// with the page's render, wait on the server for the runtime's request for
// them once the page has loaded: 30 seconds by default. A request after it
// (or on another replica, or after a restart) renders the page again for
// its parts. 0 (or less) parks nothing: the methods run on that request,
// after the page has loaded.
func PartsWindow(d time.Duration) Option {
	return OptionFunc(func(a *App) { a.partsWindow = d })
}

// parkedParts is a page's parts between its response and the runtime's
// request for them.
type parkedParts struct {
	parts  *ui.Parts
	cancel context.CancelFunc
	uri    string // the page's
	who    [32]byte
}

func (p *parkedParts) close() {
	p.parts.Stop()
	p.cancel()
}

// partsVisitor is who r is, for a page's parked parts: its identity
// (LiveIdentity) and the session cookies it carries.
func (a *App) partsVisitor(r *http.Request) [32]byte {
	h := sha256.New()
	io.WriteString(h, a.liveWho(r.Context()))
	for _, name := range a.sessionCookies {
		if c, err := r.Cookie(name); err == nil {
			io.WriteString(h, "\x00"+name+"="+c.Value)
		}
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum
}

// parkParts parks the parts of rq's page, their methods running, and
// returns their token.
func (a *App) parkParts(rq *request, parts *ui.Parts, cancel context.CancelFunc) string {
	var b [18]byte
	rand.Read(b[:])
	tok := base64.RawURLEncoding.EncodeToString(b[:])
	window := a.partsWindow
	if prefetching(rq.r) {
		window = max(window, prefetchWindow) // the page may open any time the browser keeps it
	}
	a.partsPark.park(tok, &parkedParts{parts: parts, cancel: cancel, uri: rq.r.URL.RequestURI(), who: a.partsVisitor(rq.r)}, window)
	return tok
}

// pickParts answers the runtime's request for a page's parts with the ones
// its page parked, when its token is parked for the request's URI and
// visitor; any other request goes on to the route.
func (a *App) pickParts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get(partsTokenHeader)
		if tok == "" || r.Method != http.MethodGet || r.Header.Get(PartsHeader) == "" {
			next.ServeHTTP(w, r)
			return
		}
		p := a.partsPark.take(tok)
		if p == nil || p.uri != r.URL.RequestURI() || p.who != a.partsVisitor(r) {
			if p != nil {
				p.close() // taken: another visitor's token is spent
			}
			next.ServeHTTP(w, r)
			return
		}
		stop := context.AfterFunc(r.Context(), p.close) // the visitor went away
		writeParts(w, p.parts)
		stop()
		p.close()
	})
}

// maxParts is how many parts one request may ask for.
const maxParts = 512

// partsWanted is the ids r asks for, or nil when r isn't a request for a
// page's parts.
func partsWanted(r *http.Request) []string {
	if r.Method != http.MethodGet {
		return nil
	}
	v := r.Header.Get(PartsHeader)
	if v == "" {
		return nil
	}
	ids := strings.Fields(v)
	if len(ids) > maxParts {
		ids = ids[:maxParts]
	}
	return ids
}

// varyParts adds PartsHeader to h's Vary, once.
func varyParts(h http.Header) {
	if !headerHasToken(h, "Vary", PartsHeader) {
		h.Add("Vary", PartsHeader)
	}
}

// In a test binary a page's parts render in place, its HTML whole: what a
// test reading it wants (viewtest). WithPendingParts leaves them pending
// instead.
var streamInTests bool // this package's tests stream, as an app does

type pendingKey struct{}

// WithPendingParts is ctx with the deferred parts of the page it renders
// left pending: the page shows their fallbacks and their methods never run.
// viewtest.Pending sets it, to test a page's first paint.
func WithPendingParts(ctx context.Context) context.Context {
	return context.WithValue(ctx, pendingKey{}, true)
}

func pendingParts(ctx context.Context) bool {
	v, _ := ctx.Value(pendingKey{}).(bool)
	return v
}

// writeParts answers a request for a page's parts with parts's.
func writeParts(w http.ResponseWriter, parts *ui.Parts) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	h.Set(PartsHeader, "1")
	varyParts(h)
	rc := http.NewResponseController(w)
	flush := func() error {
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		return nil
	}
	w.WriteHeader(http.StatusOK)
	if flush() != nil { // the headers at once: the runtime knows the parts are coming
		parts.Stop()
		return
	}
	parts.StreamParts(w, flush)
}

// partsOptions are how the deferred methods of r's page run: their
// spans in the toolbar, their errors' text and log lines.
func (a *App) partsOptions(r *http.Request, pending bool, want []string) ui.PartsOptions {
	return ui.PartsOptions{
		Pending:   pending,
		Want:      want,
		Observe:   a.observePart,
		ErrorText: a.partErrorText,
		Written: func(name string, err error) {
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("web: %s %s: part %s: %v", r.Method, r.URL.Path, name, err)
			}
		},
	}
}

// partErrorText is the message a failing part without an error fallback
// shows, as an error page would: a public message (*Error's), the error's
// text in development, else the default.
func (a *App) partErrorText(err error) string {
	var pub interface{ PublicMessage() string }
	var to Redirect
	switch {
	case errors.As(err, &to):
		return ""
	case errors.As(err, &pub):
		return pub.PublicMessage()
	case a.dev:
		return err.Error()
	}
	return ""
}

// observePart opens a deferred method's span on the toolbar, its
// statements marked as the part's.
func (a *App) observePart(ctx context.Context, name string) (context.Context, func(error)) {
	if a.tb == nil {
		return ctx, func(error) {}
	}
	e := entryOf(ctx)
	if e == nil {
		return ctx, func(error) {}
	}
	field := "part " + name
	return StatementOrigin(ctx, "", field), e.partSpan(name, field)
}
