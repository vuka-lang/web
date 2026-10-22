package web

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/vuka/v2/diag"
)

// SecurityHeaders are the headers every response of an app carries unless
// its handler sets them itself. The zero value of a field is its default;
// "-" (or, for HSTS, a negative duration) leaves the header out.
type SecurityHeaders struct {
	// ContentTypeOptions: "nosniff" by default, so a browser never runs
	// an upload or a JSON answer as a script or a page.
	ContentTypeOptions string
	// ReferrerPolicy: "strict-origin-when-cross-origin" by default, so a
	// URL's path and query stay on the site.
	ReferrerPolicy string
	// FrameOptions: "DENY" by default, or "SAMEORIGIN": who may show the
	// app's pages in a frame. The CSP's frame-ancestors follows it.
	FrameOptions string
	// CSP is the Content-Security-Policy: "frame-ancestors 'none'" by
	// default ('self' with FrameOptions "SAMEORIGIN"; left out with "-").
	CSP string
	// HSTS is the Strict-Transport-Security max-age sent on HTTPS requests
	// (r.TLS, or X-Forwarded-Proto: https from a trusted proxy); 365 days
	// by default. HSTSSubdomains adds includeSubDomains.
	HSTS           time.Duration
	HSTSSubdomains bool
}

// Headers replaces the defaults of the security headers every response
// carries (see SecurityHeaders): a field left zero keeps its default.
func Headers(h SecurityHeaders) Option {
	return OptionFunc(func(a *App) { a.headers = &h; a.noHeaders = false })
}

// NoSecurityHeaders turns the default security headers off: a proxy in
// front of the app sets them, say.
func NoSecurityHeaders() Option { return OptionFunc(func(a *App) { a.noHeaders = true }) }

// headerSet is the resolved headers: those on every response, and HSTS's
// value for HTTPS ones.
type headerSet struct {
	all  [][2]string
	hsts string
}

func (a *App) securityHeaders() headerSet {
	var hs headerSet
	if a.noHeaders {
		return hs
	}
	var h SecurityHeaders
	if a.headers != nil {
		h = *a.headers
	}
	pick := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	add := func(k, v string) {
		if v != "-" {
			hs.all = append(hs.all, [2]string{k, v})
		}
	}
	add("X-Content-Type-Options", pick(h.ContentTypeOptions, "nosniff"))
	add("Referrer-Policy", pick(h.ReferrerPolicy, "strict-origin-when-cross-origin"))
	frame := pick(h.FrameOptions, "DENY")
	add("X-Frame-Options", frame)
	ancestors := "frame-ancestors 'none'"
	switch {
	case strings.EqualFold(frame, "SAMEORIGIN"):
		ancestors = "frame-ancestors 'self'"
	case frame == "-":
		ancestors = "-"
	}
	add("Content-Security-Policy", pick(h.CSP, ancestors))
	if h.HSTS >= 0 {
		age := h.HSTS
		if age == 0 {
			age = 365 * 24 * time.Hour
		}
		hs.hsts = "max-age=" + strconv.FormatInt(int64(age/time.Second), 10)
		if h.HSTSSubdomains {
			hs.hsts += "; includeSubDomains"
		}
	}
	return hs
}

// TrustedProxies names the proxies in front of the app, as CIDRs
// ("10.0.0.0/8", "fd00::/8") or single addresses: from them, and only
// from them, ClientIP reads X-Forwarded-For (else Forwarded), and an
// HTTPS request is told by X-Forwarded-Proto. An entry that doesn't parse
// fails Build.
func TrustedProxies(cidrs ...string) Option {
	return OptionFunc(func(a *App) {
		for _, c := range cidrs {
			p, err := netip.ParsePrefix(c)
			if err != nil {
				addr, aerr := netip.ParseAddr(c)
				if aerr != nil {
					a.c.errs = append(a.c.errs, diag.New("E0413", "TrustedProxies: %s is neither a CIDR nor an address", strconv.Quote(c)).WithHelp("give CIDRs (10.0.0.0/8) or addresses (10.1.2.3)"))
					continue
				}
				p = netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen())
			}
			a.proxies = append(a.proxies, p.Masked())
		}
	})
}

type proxiesKey struct{}

// ClientIP is the address r comes from: its connection's peer, or, when
// that is one of the app's TrustedProxies, the nearest address in
// X-Forwarded-For (else Forwarded) that isn't. Throttles and connection
// caps count by it.
func ClientIP(r *http.Request) string {
	peer := hostOf(r.RemoteAddr)
	ps, _ := r.Context().Value(proxiesKey{}).([]netip.Prefix)
	if len(ps) == 0 {
		return peer
	}
	addr, err := netip.ParseAddr(peer)
	if err != nil || !trusted(ps, addr) {
		return peer
	}
	hops := forwardedHops(r)
	last := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			return last // a hop that names no address: the proxy before it is all that is known
		}
		last = a.Unmap().String()
		if !trusted(ps, a) {
			return last
		}
	}
	return last
}

// fromTrustedProxy reports whether r's peer is one of the trusted proxies.
func fromTrustedProxy(r *http.Request) bool {
	ps, _ := r.Context().Value(proxiesKey{}).([]netip.Prefix)
	if len(ps) == 0 {
		return false
	}
	addr, err := netip.ParseAddr(hostOf(r.RemoteAddr))
	return err == nil && trusted(ps, addr)
}

// IsHTTPS reports whether r came over HTTPS: its own TLS, or a trusted
// proxy's X-Forwarded-Proto.
func IsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return fromTrustedProxy(r) && strings.EqualFold(strings.TrimSpace(lastValue(r.Header.Values("X-Forwarded-Proto"))), "https")
}

func lastValue(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	v := vs[len(vs)-1]
	if i := strings.LastIndexByte(v, ','); i >= 0 {
		v = v[i+1:]
	}
	return v
}

func trusted(ps []netip.Prefix, a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func hostOf(remote string) string {
	if h, _, err := net.SplitHostPort(remote); err == nil {
		return h
	}
	return remote
}

// forwardedHops lists the addresses a request went through, the client
// first: X-Forwarded-For's, else Forwarded's for= parameters.
func forwardedHops(r *http.Request) []string {
	var hops []string
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		for _, v := range xff {
			for _, h := range strings.Split(v, ",") {
				hops = append(hops, strings.TrimSpace(h))
			}
		}
		return hops
	}
	for _, v := range r.Header.Values("Forwarded") {
		for _, elem := range strings.Split(v, ",") {
			for _, pair := range strings.Split(elem, ";") {
				k, val, ok := strings.Cut(strings.TrimSpace(pair), "=")
				if !ok || !strings.EqualFold(k, "for") {
					continue
				}
				val = strings.Trim(val, `"`)
				if strings.HasPrefix(val, "[") {
					if i := strings.IndexByte(val, ']'); i > 0 {
						val = val[1:i]
					}
				} else if h, _, err := net.SplitHostPort(val); err == nil {
					val = h
				}
				hops = append(hops, val)
			}
		}
	}
	return hops
}

// MaxRequestBody bounds every request's body, in bytes: a body longer gets
// 413. 32 MiB by default; negative for none. A route sets its own with
// @web.MaxBody.
func MaxRequestBody(n int64) Option { return OptionFunc(func(a *App) { a.maxBody = n }) }

const defaultMaxBody = 32 << 20

// BodyLimit is the attribute MaxBody puts on a route.
type BodyLimit int64

// MaxBody bounds the request body of one route, in bytes (negative for
// none), in place of the app's MaxRequestBody:
//
//	@web.Post("/videos")
//	@web.MaxBody(512 << 20)
//	func Upload(r *http.Request) error { … }
//
// From Go: app.Post("/videos", h, web.Attrs(web.MaxBody(512 << 20))).
func MaxBody(n int64) vuka.Bundle { return vuka.Compose(BodyLimit(n)) }

// bodyLimit is the limit of a route's body; 0 for none.
func (a *App) bodyLimit(attrs []any) int64 {
	n := a.maxBody
	for _, x := range attrs {
		if l, ok := x.(BodyLimit); ok {
			n = int64(l)
		}
	}
	switch {
	case n == 0:
		return defaultMaxBody
	case n < 0:
		return 0
	}
	return n
}

// ErrBodyTooLarge answers a request whose body is over its limit.
var ErrBodyTooLarge = &Error{Status: http.StatusRequestEntityTooLarge, Message: "request body too large"}

// bodyError is err as a request's answer: 413 when the body was over its
// limit, else a 400 saying what.
func bodyError(err error, what string) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return ErrBodyTooLarge
	}
	return BadRequest(what + err.Error())
}

// edge is the app's outermost handler: the security headers, the trusted
// proxies ClientIP reads, and the goroutine of the request, for
// CurrentRequest.
func (a *App) edge(next http.Handler) http.Handler {
	hs := a.securityHeaders()
	proxies := a.proxies
	track := a.trackRequests
	if len(hs.all) == 0 && hs.hsts == "" && len(proxies) == 0 && !track {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(proxies) > 0 {
			r = r.WithContext(context.WithValue(r.Context(), proxiesKey{}, proxies))
		}
		h := w.Header()
		for _, kv := range hs.all {
			h.Set(kv[0], kv[1])
		}
		if hs.hsts != "" && IsHTTPS(r) {
			h.Set("Strict-Transport-Security", hs.hsts)
		}
		if track {
			s := &requestSlot{}
			r = r.WithContext(context.WithValue(r.Context(), requestSlotKey{}, s))
			defer bindRequest(s)()
		}
		next.ServeHTTP(w, r)
	})
}

// TrackRequests has the app note the goroutine serving each request, and
// the context of the route it runs, for CurrentRequest: a library checking
// work a request's goroutine does without being given the request's
// context.
func TrackRequests() Option { return OptionFunc(func(a *App) { a.trackRequests = true }) }

// requestSlot is the route context of a request being served; only its
// own goroutine reads or writes it.
type requestSlot struct{ ctx context.Context }

type requestSlotKey struct{}

var (
	requestGoroutines sync.Map // goroutine id → *requestSlot, while it serves a request
	requestsN         atomic.Int64
)

func bindRequest(s *requestSlot) (unbind func()) {
	id := goid()
	requestGoroutines.Store(id, s)
	requestsN.Add(1)
	return func() {
		requestGoroutines.Delete(id)
		requestsN.Add(-1)
	}
}

// enterRoute makes ctx the route context of the request r's goroutine
// serves, until leave.
func enterRoute(r *http.Request, ctx context.Context) (leave func()) {
	s, _ := r.Context().Value(requestSlotKey{}).(*requestSlot)
	if s == nil {
		return noop
	}
	prev := s.ctx
	s.ctx = ctx
	return func() { s.ctx = prev }
}

// CurrentRequest reports whether the calling goroutine serves an HTTP
// request of an app with TrackRequests — a live page's connection
// included — rather than being one the app's own work runs on (a cron's,
// a job's, main), and the context of the route it is running: a handler's,
// with its middleware's values; nil outside a route (in app-wide
// middleware, on a live connection between its routes' runs).
func CurrentRequest() (ctx context.Context, serving bool) {
	if requestsN.Load() == 0 {
		return nil, false
	}
	s, ok := requestGoroutines.Load(goid())
	if !ok {
		return nil, false
	}
	return s.(*requestSlot).ctx, true
}

// CronContext has every cron run with its context passed through f: a
// library marking crons as the app's own work.
func CronContext(f func(context.Context) context.Context) Option {
	return OptionFunc(func(a *App) { a.cronCtx = append(a.cronCtx, f) })
}

// staticFS serves fsys's files: never a directory's listing (a directory
// is its index.html, else 404), nor a dotfile or anything under a dot
// directory but .well-known.
func staticFS(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if hiddenPath(name) {
			http.NotFound(w, r)
			return
		}
		if name == "" {
			name = "."
		}
		if fi, err := fs.Stat(fsys, name); err == nil && fi.IsDir() {
			if _, err := fs.Stat(fsys, path.Join(name, "index.html")); err != nil {
				http.NotFound(w, r)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}

// hiddenPath reports whether a cleaned relative path names a dotfile or
// goes through a dot directory, .well-known aside.
func hiddenPath(name string) bool {
	for i, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." && !(i == 0 && seg == ".well-known") {
			return true
		}
	}
	return false
}
