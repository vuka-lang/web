package web

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vuka-lang/vuka/v2/diag"
)

// AnyOrigin, as CORSConfig's only origin, lets every origin read the
// answers. It can't go with Credentials.
const AnyOrigin = "*"

// AnyHeader, in CORSConfig.Headers, allows whatever request headers a
// preflight asks for, Authorization included. (A browser reads a literal
// "Access-Control-Allow-Headers: *" as every header but Authorization, and
// as the header named "*" on a request with credentials: AnyHeader answers
// with the headers asked for instead.)
const AnyHeader = "*"

// CORSConfig is what CORS lets other origins do under its prefix.
type CORSConfig struct {
	// Origins are the origins allowed, exactly as a browser sends them:
	// scheme, host and port, no path ("https://shop.example",
	// "http://localhost:5173", "capacitor://localhost"). AnyOrigin alone
	// allows every one.
	Origins []string
	// Methods a request may use: GET, HEAD and POST when empty.
	Methods []string
	// Headers a request may carry besides the CORS-safelisted ones:
	// Content-Type when empty, and Authorization too under a TokenOnly
	// prefix. AnyHeader allows every one.
	Headers []string
	// ExposeHeaders are the response headers a script may read besides
	// the CORS-safelisted ones (Content-Type, Cache-Control…).
	ExposeHeaders []string
	// Credentials lets the origins send cookies: the browser sends them
	// and the app's cross-origin check lets these origins' requests
	// through under the prefix (as web.CSRF's trusted origins). It needs
	// listed origins, never AnyOrigin, and can't go with TokenOnly.
	Credentials bool
	// MaxAge is how long a browser may keep a preflight's answer: 5 minutes
	// when zero, never with a negative value. (Browsers cap it: Chrome at 2
	// hours.)
	MaxAge time.Duration
}

// CORS lets other origins call the routes under prefix ("/api/" covers
// /api and every path under it): a preflight (OPTIONS with
// Access-Control-Request-Method) is answered 204 before any routing,
// middleware or cross-origin check, and every answer under prefix says
// which origin may read it. An origin not listed gets answers without
// those headers, which its browser keeps from the page; CORS is not
// access control, so gate the routes as any others.
//
//	web.New(
//		web.TokenOnly("/api/"),
//		web.CORS("/api/", web.CORSConfig{Origins: []string{"https://shop.example"}}),
//	)
//
// The most specific prefix wins when several cover a path. A config that
// can't work (no origins, an origin with a path, Credentials with
// AnyOrigin) fails Build.
func CORS(prefix string, cfg CORSConfig) Option {
	return OptionFunc(func(a *App) {
		a.mutable("CORS")
		a.cors = append(a.cors, &corsRule{prefix: prefix, cfg: cfg})
	})
}

// TokenOnly declares the routes under prefix ("/api/" covers /api and
// every path under it) a token API: a request there signs in with its
// Authorization header alone. Its cookies are taken off before any
// middleware reads them, so no session or CSRF cookie rides along, the
// Set-Cookie headers of its answers are dropped, and, with no cookie to
// abuse, the app's cross-origin check is off there: another origin's
// script may POST, with a bearer token or none (a sign-in). The two can't
// be had apart: the check is never off for a request carrying cookies.
//
// Pages, forms and live views need cookies: keep them off a TokenOnly
// prefix. Pair it with CORS for browsers on other origins.
func TokenOnly(prefix string) Option {
	return OptionFunc(func(a *App) {
		a.mutable("TokenOnly")
		a.tokenOnly = append(a.tokenOnly, prefix)
	})
}

// corsRule is one CORS declaration, compiled by Build.
type corsRule struct {
	prefix string
	cfg    CORSConfig

	base     string // the prefix, escaped, without its trailing slash
	any      bool
	origins  []string // lower-cased
	methods  string
	headers  string // "" with anyHeader
	anyHdr   bool
	expose   string
	maxAge   string
	trusting bool // Credentials: its origins pass the cross-origin check
}

func corsErr(prefix, format string, args ...any) *diag.Diagnostic {
	return diag.New("E0430", "CORS(%q): "+format, append([]any{prefix}, args...)...)
}

// prefixBase is prefix escaped as a request's EscapedPath is, cleaned and
// without its trailing slash; ok false when it isn't a path.
func prefixBase(prefix string) (string, bool) {
	if !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "?#") {
		return "", false
	}
	b := path.Clean((&url.URL{Path: prefix}).EscapedPath())
	return strings.TrimSuffix(b, "/"), true
}

// under reports whether a request's path is base or under it, by the path
// as the router matches it: escaped, so an encoded slash stays in its
// segment, and cleaned.
func under(base string, r *http.Request) bool {
	if base == "" {
		return true
	}
	p := path.Clean(r.URL.EscapedPath())
	return p == base || strings.HasPrefix(p, base+"/")
}

func (c *corsRule) compile(tokenOnly bool) error {
	cfg := c.cfg
	base, ok := prefixBase(c.prefix)
	if !ok {
		return corsErr(c.prefix, "the prefix is not a path").WithHelp(`give the routes' path prefix: "/api/"`)
	}
	c.base = base
	if len(cfg.Origins) == 0 {
		return corsErr(c.prefix, "no origins").WithHelp(`list the origins allowed (Origins: []string{"https://shop.example"}), or web.AnyOrigin`)
	}
	for _, o := range cfg.Origins {
		if o == AnyOrigin {
			if len(cfg.Origins) > 1 {
				return corsErr(c.prefix, "AnyOrigin with other origins").WithHelp("give AnyOrigin alone, or only the origins")
			}
			c.any = true
			continue
		}
		if err := checkOrigin(o); err != "" {
			return corsErr(c.prefix, "origin %q %s", o, err).WithHelp(`an origin is scheme://host[:port], as a browser's Origin header: "https://shop.example"`)
		}
		c.origins = append(c.origins, strings.ToLower(o))
	}
	if cfg.Credentials {
		if c.any {
			return corsErr(c.prefix, "Credentials with AnyOrigin").WithHelp("browsers refuse credentials from any origin: list the origins")
		}
		if tokenOnly {
			return corsErr(c.prefix, "Credentials under TokenOnly, which takes cookies off").WithHelp("drop Credentials: a token API sends Authorization, which needs none")
		}
		c.trusting = true
	}
	methods := slices.Clone(cfg.Methods)
	if len(methods) == 0 {
		methods = []string{http.MethodGet, http.MethodHead, http.MethodPost}
	}
	for i, m := range methods {
		if m == "*" {
			return corsErr(c.prefix, "a method \"*\"").WithHelp("list the methods")
		}
		methods[i] = strings.ToUpper(m)
	}
	c.methods = strings.Join(methods, ", ")
	headers := cfg.Headers
	if len(headers) == 0 {
		headers = []string{"Content-Type"}
		if tokenOnly {
			headers = append(headers, "Authorization")
		}
	}
	var hs []string
	for _, h := range headers {
		if h == AnyHeader {
			c.anyHdr = true
			continue
		}
		hs = append(hs, http.CanonicalHeaderKey(h))
	}
	c.headers = strings.Join(hs, ", ")
	if len(cfg.ExposeHeaders) > 0 {
		var es []string
		for _, h := range cfg.ExposeHeaders {
			es = append(es, http.CanonicalHeaderKey(h))
		}
		c.expose = strings.Join(es, ", ")
	}
	switch age := cfg.MaxAge; {
	case age == 0:
		c.maxAge = "300"
	case age > 0:
		c.maxAge = strconv.FormatInt(int64(age/time.Second), 10)
	}
	return nil
}

// checkOrigin says what is wrong with an origin; "" when nothing.
func checkOrigin(o string) string {
	if o == "null" {
		return "is the opaque origin, which any sandboxed page sends"
	}
	if strings.Contains(o, "*") {
		return "has a wildcard: origins match exactly"
	}
	u, err := url.Parse(o)
	switch {
	case err != nil || u.Scheme == "" || u.Host == "":
		return "is not scheme://host"
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return "has more than scheme, host and port"
	case u.Path != "":
		return "has a path"
	}
	return ""
}

func (c *corsRule) allows(origin string) bool {
	return c.any || slices.Contains(c.origins, strings.ToLower(origin))
}

// crossOriginKey marks a request the cross-origin check lets through: a
// TokenOnly one, its cookies taken off, or one from an origin a CORS rule
// with Credentials trusts.
type crossOriginKey struct{}

func crossOriginExempt(r *http.Request) bool {
	v, _ := r.Context().Value(crossOriginKey{}).(bool)
	return v
}

// compileCrossOrigin checks the app's CORS and TokenOnly declarations.
func (a *App) compileCrossOrigin() []error {
	var errs []error
	a.tokenBases = nil
	for _, p := range a.tokenOnly {
		b, ok := prefixBase(p)
		if !ok {
			errs = append(errs, diag.New("E0431", "TokenOnly(%q): the prefix is not a path", p).WithHelp(`give the routes' path prefix: "/api/"`))
			continue
		}
		a.tokenBases = append(a.tokenBases, b)
	}
	seen := map[string]bool{}
	for _, c := range a.cors {
		tokenOnly := false
		if b, ok := prefixBase(c.prefix); ok {
			if seen[b] {
				errs = append(errs, corsErr(c.prefix, "declared twice").WithHelp("give one CORS per prefix"))
				continue
			}
			seen[b] = true
			for _, t := range a.tokenBases {
				if t == "" || b == t || strings.HasPrefix(b, t+"/") {
					tokenOnly = true
				}
			}
		}
		if err := c.compile(tokenOnly); err != nil {
			errs = append(errs, err)
		}
	}
	// The most specific prefix first.
	slices.SortStableFunc(a.cors, func(x, y *corsRule) int { return len(y.base) - len(x.base) })
	return errs
}

// crossOrigin is the app's TokenOnly and CORS handling, outside every
// other middleware: preflights are answered here, and a TokenOnly
// request's cookies are gone before anything reads them.
func (a *App) crossOrigin(next http.Handler) http.Handler {
	if len(a.cors) == 0 && len(a.tokenBases) == 0 {
		return next
	}
	rules, bases := a.cors, a.tokenBases
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rule *corsRule
		for _, c := range rules {
			if under(c.base, r) {
				rule = c
				break
			}
		}
		exempt := false
		if rule != nil {
			h := w.Header()
			h.Add("Vary", "Origin")
			origin := r.Header.Get("Origin")
			preflight := r.Method == http.MethodOptions && origin != "" && r.Header.Get("Access-Control-Request-Method") != ""
			ok := origin != "" && rule.allows(origin)
			if ok {
				if rule.any {
					h.Set("Access-Control-Allow-Origin", "*")
				} else {
					h.Set("Access-Control-Allow-Origin", origin)
				}
				if rule.cfg.Credentials {
					h.Set("Access-Control-Allow-Credentials", "true")
					exempt = true
				}
			}
			if preflight {
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				if ok {
					h.Set("Access-Control-Allow-Methods", rule.methods)
					allowed := rule.headers
					if rule.anyHdr {
						if asked := r.Header.Get("Access-Control-Request-Headers"); asked != "" {
							allowed = asked
						}
					}
					if allowed != "" {
						h.Set("Access-Control-Allow-Headers", allowed)
					}
					if rule.maxAge != "" {
						h.Set("Access-Control-Max-Age", rule.maxAge)
					}
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if ok && rule.expose != "" {
				h.Set("Access-Control-Expose-Headers", rule.expose)
			}
		}
		for _, b := range bases {
			if under(b, r) {
				r = r.Clone(r.Context())
				r.Header.Del("Cookie")
				w = &cookieless{ResponseWriter: w}
				exempt = true
				break
			}
		}
		if exempt {
			r = r.WithContext(context.WithValue(r.Context(), crossOriginKey{}, true))
		}
		next.ServeHTTP(w, r)
	})
}

// cookieless drops the Set-Cookie headers of a TokenOnly route's answer.
type cookieless struct {
	http.ResponseWriter
	wrote bool
}

func (w *cookieless) WriteHeader(code int) {
	if !w.wrote {
		w.Header().Del("Set-Cookie")
		w.wrote = code >= 200
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cookieless) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *cookieless) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *cookieless) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.Header().Del("Set-Cookie")
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *cookieless) Unwrap() http.ResponseWriter { return w.ResponseWriter }
