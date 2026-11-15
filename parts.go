package web

import (
	"bytes"
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

// A page's parts in its own response. To a browser loading the page as its
// document (Sec-Fetch-Dest: document or iframe; not a navigation's fetch,
// a prefetch or a request for parts) through a writer that flushes, with
// no Content-Security-Policy refusing inline scripts, the page is sent up
// to the runtime's script tag and flushed, the response kept open: each
// part, as its method returns (a group's together, in the order the page
// met them), is written as a <template data-vk-part="id"> and a one-line
// script moving it into place (partsMover, defined once at the top of the
// stream), and flushed. The runtime's tag and the rest of the page follow
// the last part, so the runtime — a deferred script — runs once every
// streamed part is in place: it finds no mark of theirs, and asks for
// none. The parts' methods run with the request's cancellation, for at
// most PartsWindow; a part still running then keeps its fallback and its
// mark, and the runtime asks for it as for a page whose parts' token has
// expired. The page's first complete paint needs no request after it.
//
// A shell shorter than streamPartsPad's padding gets the padding (a
// comment in the mover's script) so proxies that hold a response until
// some bytes arrive pass the shell on; X-Accel-Buffering: no asks nginx
// not to buffer it. Without a writer that flushes, to a HEAD, a fetch, or
// a client that doesn't say it loads a document, the page goes whole and
// its parts are parked for the runtime's request, as above.

// streamable reports whether the parts of r's page go in its response.
func streamable(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || navigation(r) || prefetching(r) {
		return false
	}
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "document" && d != "iframe" {
		return false
	}
	return canFlush(w) && inlineScripts(w.Header())
}

// canFlush reports whether w, or the writer it wraps, sends what was
// written so far when flushed.
func canFlush(w http.ResponseWriter) bool {
	for {
		if u, ok := w.(interface{ Unwrap() http.ResponseWriter }); ok {
			w = u.Unwrap()
			continue
		}
		switch w.(type) {
		case http.Flusher, interface{ FlushError() error }:
			return true
		}
		return false
	}
}

// inlineScripts reports whether a page with header h may run inline
// scripts: it has no Content-Security-Policy, or one whose script policy
// allows them.
func inlineScripts(h http.Header) bool {
	for _, policy := range h.Values("Content-Security-Policy") {
		dirs := map[string][]string{}
		for _, d := range strings.Split(policy, ";") {
			if f := strings.Fields(strings.ToLower(d)); len(f) > 0 {
				if _, ok := dirs[f[0]]; !ok {
					dirs[f[0]] = f[1:]
				}
			}
		}
		srcs, ok := dirs["script-src-elem"]
		if !ok {
			srcs, ok = dirs["script-src"]
		}
		if !ok {
			srcs, ok = dirs["default-src"]
		}
		if !ok {
			continue
		}
		inline := false
		for _, s := range srcs {
			switch {
			case s == "'unsafe-inline'":
				inline = true
			case strings.HasPrefix(s, "'nonce-"), strings.HasPrefix(s, "'sha"), s == "'strict-dynamic'":
				return false // 'unsafe-inline' is ignored beside them
			}
		}
		if !inline {
			return false
		}
	}
	return true
}

// partsMover defines the function the stream calls to move a part into the
// place its marks hold for it: the template's content replaces what lies
// between <!--vk-part:id--> and <!--/vk-part:id-->, the template and the
// calling script are removed, and vk:part fires, as the runtime's own move
// does. It needs no runtime: the runtime's script hasn't run yet.
const partsMover = `<script>window.__vkS=function(i){var d=document,c=d.currentScript,t=d.querySelector('template[data-vk-part="'+i+'"]'),w=d.createTreeWalker(d.body,128),s=null,n,x;while(n=w.nextNode())if(n.data==="vk-part:"+i){s=n;break}if(t&&s){var p=s.parentNode;for(n=s.nextSibling;n&&!(n.nodeType===8&&n.data==="/vk-part:"+i);n=x){x=n.nextSibling;p.removeChild(n)}p.insertBefore(t.content,s);p.removeChild(s);if(n)p.removeChild(n);d.dispatchEvent(new CustomEvent("vk:part",{detail:{id:i}}))}if(t)t.parentNode.removeChild(t);if(c)c.parentNode.removeChild(c)};`

// streamPartsPad is how many bytes the page's first flush has at least.
const streamPartsPad = 2048

// writeStreamed writes the page (html, with head and the runtime's script
// tag as writePage puts them) with its parts in its response: the page up
// to the script tag, then each part as it is ready, then the rest.
func writeStreamed(w http.ResponseWriter, html []byte, head, script string, parts *ui.Parts) {
	var page bytes.Buffer
	writePage(&page, html, head, script)
	b := page.Bytes()
	at := bytes.LastIndex(b, []byte(script))
	if at < 0 {
		at = len(b)
	}
	rc := http.NewResponseController(w)
	flush := func() error {
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		return nil
	}
	w.Write(b[:at])
	io.WriteString(w, partsMover)
	if at < streamPartsPad {
		io.WriteString(w, "/*"+strings.Repeat(" ", streamPartsPad)+"*/")
	}
	io.WriteString(w, `(function(s){s.parentNode.removeChild(s)})(document.currentScript)</script>`)
	if flush() != nil {
		return
	}
	mv := &moverWriter{w: w}
	if err := parts.StreamParts(mv, flush); err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return // the visitor went away
	}
	mv.end()
	w.Write(b[at:])
}

// moverWriter turns the parts StreamParts writes — each a template ended
// by <!--vk-parts:id--> for the runtime — into templates each followed by
// the call of partsMover's function moving it into place.
type moverWriter struct {
	w    io.Writer
	held []byte // after the last whole part
}

var partEnd = []byte("</template><!--vk-parts:")

func (m *moverWriter) Write(b []byte) (int, error) {
	m.held = append(m.held, b...)
	var out bytes.Buffer
	for {
		i := bytes.Index(m.held, partEnd)
		if i < 0 {
			break
		}
		rest := m.held[i+len(partEnd):]
		j := bytes.Index(rest, []byte("-->"))
		if j < 0 {
			break
		}
		id := rest[:j]
		out.Write(m.held[:i])
		out.WriteString("</template>")
		if partID(id) {
			out.WriteString(`<script>__vkS("` + string(id) + `")</script>`)
		}
		m.held = rest[j+3:]
	}
	if out.Len() > 0 {
		if _, err := m.w.Write(out.Bytes()); err != nil {
			return 0, err
		}
	}
	m.held = append([]byte(nil), m.held...)
	return len(b), nil
}

// end writes what follows the last whole part: nothing, as StreamParts
// writes whole parts.
func (m *moverWriter) end() {
	if len(m.held) > 0 {
		m.w.Write(m.held)
		m.held = nil
	}
}

// partID reports whether id is a part's id as ui names them: letters,
// digits, '.', '-' and '_'.
func partID(id []byte) bool {
	if len(id) == 0 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
