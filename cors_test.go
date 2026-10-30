package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCORSConfigErrors(t *testing.T) {
	shop := []string{"https://shop.example"}
	for _, c := range []struct {
		name string
		opts []Option
		want string
	}{
		{"no origins", []Option{CORS("/api/", CORSConfig{})}, "E0430"},
		{"credentials with any origin", []Option{CORS("/api/", CORSConfig{Origins: []string{AnyOrigin}, Credentials: true})}, "Credentials with AnyOrigin"},
		{"any origin with others", []Option{CORS("/api/", CORSConfig{Origins: []string{AnyOrigin, "https://shop.example"}})}, "AnyOrigin with other origins"},
		{"an origin with a path", []Option{CORS("/api/", CORSConfig{Origins: []string{"https://shop.example/"}})}, "has a path"},
		{"the null origin", []Option{CORS("/api/", CORSConfig{Origins: []string{"null"}})}, "opaque origin"},
		{"a wildcard origin", []Option{CORS("/api/", CORSConfig{Origins: []string{"https://*.shop.example"}})}, "wildcard"},
		{"no scheme", []Option{CORS("/api/", CORSConfig{Origins: []string{"shop.example"}})}, "not scheme://host"},
		{"a method *", []Option{CORS("/api/", CORSConfig{Origins: shop, Methods: []string{"*"}})}, `method "*"`},
		{"a prefix that is no path", []Option{CORS("api", CORSConfig{Origins: shop})}, "not a path"},
		{"twice", []Option{CORS("/api/", CORSConfig{Origins: shop}), CORS("/api", CORSConfig{Origins: shop})}, "declared twice"},
		{"credentials under TokenOnly", []Option{TokenOnly("/api/"), CORS("/api/orders/", CORSConfig{Origins: shop, Credentials: true})}, "Credentials under TokenOnly"},
		{"a TokenOnly prefix that is no path", []Option{TokenOnly("api/")}, "E0431"},
	} {
		a := New(append([]Option{Only("none")}, c.opts...)...)
		if err := a.Build(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want %q", c.name, err, c.want)
		}
	}
	// Credentials on a prefix only part of which is TokenOnly is fine.
	a := New(Only("none"), TokenOnly("/api/tokens/"), CORS("/api/", CORSConfig{Origins: shop, Credentials: true}))
	if err := a.Build(); err != nil {
		t.Error(err)
	}
}

// denyAll stands for an auth gate: everything without a token is 401.
func denyAll(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !bearer(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestCORSPreflight(t *testing.T) {
	methods := []string{"GET", "POST", "delete"}
	a := New(Only("none"), Use(denyAll),
		CORS("/api/", CORSConfig{Origins: []string{"https://shop.example", "capacitor://localhost"}, Methods: methods, MaxAge: time.Hour}),
		CORS("/api/open/", CORSConfig{Origins: []string{AnyOrigin}, Headers: []string{AnyHeader}, MaxAge: -1}),
	)
	a.Post("/api/orders", func() string { return "ordered" })
	h := a.Handler()
	pre := func(path, origin, headers string) (int, http.Header) {
		code, _, hd := do(t, h, "OPTIONS", path, "", "Origin", origin, "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", headers)
		return code, hd
	}

	// Answered before the app's middleware (a gate would 401) and routing.
	code, hd := pre("/api/orders", "https://shop.example", "content-type")
	if code != 204 || hd.Get("Access-Control-Allow-Origin") != "https://shop.example" || hd.Get("Access-Control-Allow-Methods") != "GET, POST, DELETE" ||
		hd.Get("Access-Control-Allow-Headers") != "Content-Type" || hd.Get("Access-Control-Max-Age") != "3600" || hd.Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("a listed origin's preflight: %d %v", code, hd)
	}
	if v := strings.Join(hd.Values("Vary"), ","); !strings.Contains(v, "Origin") || !strings.Contains(v, "Access-Control-Request-Headers") {
		t.Errorf("Vary: %q", v)
	}
	if hd.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("a preflight without the security headers")
	}
	if methods[2] != "delete" {
		t.Error("CORS changed the config's slice")
	}
	if code, hd := pre("/api/orders", "CAPACITOR://localhost", ""); code != 204 || hd.Get("Access-Control-Allow-Origin") != "CAPACITOR://localhost" {
		t.Errorf("origins match without case: %d %v", code, hd)
	}
	if code, hd := pre("/api/no-such-route", "https://shop.example", ""); code != 204 || hd.Get("Access-Control-Allow-Origin") == "" {
		t.Errorf("a path no route serves: %d %v", code, hd)
	}
	if code, hd := pre("/api/orders", "https://evil.example", ""); code != 204 || hd.Get("Access-Control-Allow-Origin") != "" || hd.Get("Access-Control-Allow-Methods") != "" {
		t.Errorf("an origin not listed: %d %v", code, hd)
	}
	if code, hd := pre("/api/orders", "https://shop.example.evil.example", ""); code != 204 || hd.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("an origin starting like a listed one: %d %v", code, hd)
	}
	// The most specific prefix: any origin, the headers asked for
	// (Authorization included, which a literal * wouldn't cover), no cache.
	code, hd = pre("/api/open/stats", "https://anyone.example", "authorization, x-trace")
	if code != 204 || hd.Get("Access-Control-Allow-Origin") != "*" || hd.Get("Access-Control-Allow-Headers") != "authorization, x-trace" || hd.Get("Access-Control-Max-Age") != "" {
		t.Errorf("AnyOrigin, AnyHeader: %d %v", code, hd)
	}
	// Outside the prefixes, OPTIONS is the app's.
	if code, hd := pre("/apis", "https://shop.example", ""); code == 204 || hd.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("outside the prefix: %d %v", code, hd)
	}
	// Defaults: GET, HEAD, POST; Content-Type; 5 minutes.
	b := New(Only("none"), CORS("/", CORSConfig{Origins: []string{"https://shop.example"}}))
	_, _, hd = do(t, b.Handler(), "OPTIONS", "/x", "", "Origin", "https://shop.example", "Access-Control-Request-Method", "GET")
	if hd.Get("Access-Control-Allow-Methods") != "GET, HEAD, POST" || hd.Get("Access-Control-Allow-Headers") != "Content-Type" || hd.Get("Access-Control-Max-Age") != "300" {
		t.Errorf("defaults: %v", hd)
	}
}

func TestCORSResponses(t *testing.T) {
	a := New(Only("none"), CORS("/api/", CORSConfig{Origins: []string{"https://shop.example"}, ExposeHeaders: []string{"x-total"}}))
	a.Get("/api/products", func() string { return "products" })
	a.Post("/api/orders", func() string { return "ordered" })
	h := a.Handler()
	_, body, hd := do(t, h, "GET", "/api/products", "", "Origin", "https://shop.example", "Sec-Fetch-Site", "cross-site")
	if strings.TrimSpace(body) != `"products"` || hd.Get("Access-Control-Allow-Origin") != "https://shop.example" || hd.Get("Access-Control-Expose-Headers") != "X-Total" || hd.Get("Vary") != "Origin" {
		t.Errorf("a listed origin: %s %v", body, hd)
	}
	if _, _, hd := do(t, h, "GET", "/api/products", "", "Origin", "https://evil.example"); hd.Get("Access-Control-Allow-Origin") != "" || hd.Get("Vary") != "Origin" {
		t.Errorf("another origin: %v", hd)
	}
	// CORS alone doesn't open the cross-origin check: a cookie could ride
	// along on a POST. TokenOnly or Credentials does.
	if code, _, hd := do(t, h, "POST", "/api/orders", "", "Origin", "https://shop.example", "Sec-Fetch-Site", "cross-site"); code != 403 || hd.Get("Access-Control-Allow-Origin") != "https://shop.example" {
		t.Errorf("a cross-site POST without TokenOnly: %d %v", code, hd)
	}
}

func TestCORSCredentialsTrustTheOrigins(t *testing.T) {
	a := New(Only("none"), CORS("/api/", CORSConfig{Origins: []string{"https://admin.example"}, Credentials: true}))
	a.Post("/api/orders", func(r *http.Request) string { return "cookie " + r.Header.Get("Cookie") })
	a.Post("/orders", func() string { return "ordered" })
	h := a.Handler()
	code, body, hd := do(t, h, "POST", "/api/orders", "", "Origin", "https://admin.example", "Sec-Fetch-Site", "cross-site", "Cookie", "sid=1")
	if code != 200 || strings.TrimSpace(body) != `"cookie sid=1"` || hd.Get("Access-Control-Allow-Credentials") != "true" || hd.Get("Access-Control-Allow-Origin") != "https://admin.example" {
		t.Errorf("a listed origin with credentials: %d %s %v", code, body, hd)
	}
	for _, c := range []struct{ path, origin string }{
		{"/api/orders", "https://evil.example"},
		{"/orders", "https://admin.example"}, // trusted under the prefix only
	} {
		if code, _, _ := do(t, h, "POST", c.path, "", "Origin", c.origin, "Sec-Fetch-Site", "cross-site", "Cookie", "sid=1"); code != 403 {
			t.Errorf("%s from %s: %d", c.path, c.origin, code)
		}
	}
}

func TestTokenOnly(t *testing.T) {
	var sawCookie []string
	a := New(Only("none"), TokenOnly("/api/"), SessionCookies("sid"),
		CORS("/api/", CORSConfig{Origins: []string{"https://shop.example"}}),
		Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c, err := r.Cookie("sid"); err == nil {
					sawCookie = append(sawCookie, r.URL.Path+"="+c.Value)
				}
				next.ServeHTTP(w, r)
			})
		}))
	who := func(r *http.Request) string {
		if bearer(r) {
			return "token " + strings.TrimSpace(r.Header.Get("Authorization")[7:])
		}
		if c, err := r.Cookie("sid"); err == nil {
			return "session " + c.Value
		}
		return "anonymous"
	}
	a.Post("/api/orders", who)
	a.Get("/api/orders", who)
	a.Post("/api/login", func(ctx context.Context) string {
		http.SetCookie(ResponseWriter(ctx), &http.Cookie{Name: "sid", Value: "new"})
		return "token issued"
	})
	a.Post("/orders", who)
	h := a.Handler()

	// A cookie never reaches a TokenOnly route, nor the app's middleware.
	if code, body, _ := do(t, h, "GET", "/api/orders", "", "Cookie", "sid=victim"); code != 200 || strings.TrimSpace(body) != `"anonymous"` {
		t.Errorf("a cookie-only request: %d %s", code, body)
	}
	if code, body, _ := do(t, h, "POST", "/api/orders", "", "Origin", "https://shop.example", "Sec-Fetch-Site", "cross-site", "Cookie", "sid=victim"); code != 200 || strings.TrimSpace(body) != `"anonymous"` {
		t.Errorf("a cross-site POST with a cookie: %d %s", code, body)
	}
	if len(sawCookie) > 0 {
		t.Errorf("middleware saw cookies: %v", sawCookie)
	}
	// The token does.
	code, body, hd := do(t, h, "POST", "/api/orders", "", "Origin", "https://shop.example", "Sec-Fetch-Site", "cross-site", "Authorization", "Bearer abc", "Cookie", "sid=victim")
	if code != 200 || strings.TrimSpace(body) != `"token abc"` || hd.Get("Access-Control-Allow-Origin") != "https://shop.example" {
		t.Errorf("a cross-site POST with a token: %d %s %v", code, body, hd)
	}
	// A sign-in from another origin carries no token: the check is off.
	code, body, hd = do(t, h, "POST", "/api/login", "", "Origin", "https://shop.example", "Sec-Fetch-Site", "cross-site")
	if code != 200 || strings.TrimSpace(body) != `"token issued"` || hd.Get("Set-Cookie") != "" {
		t.Errorf("a cross-site sign-in: %d %s, Set-Cookie %q", code, body, hd.Get("Set-Cookie"))
	}
	// A cookie and no origin: no CSRF token needed, the cookie is gone.
	if code, body, _ := do(t, h, "POST", "/api/orders", "", "Cookie", "sid=victim"); code != 200 || strings.TrimSpace(body) != `"anonymous"` {
		t.Errorf("a POST with a cookie saying nothing of its origin: %d %s", code, body)
	}

	// Everywhere else the check stands.
	for _, c := range []struct {
		path   string
		header []string
		want   int
	}{
		{"/orders", []string{"Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site", "Cookie", "sid=victim"}, 403},
		{"/orders", []string{"Cookie", "sid=victim"}, 403}, // no origin: wants the CSRF token
		{"/orders", []string{"Sec-Fetch-Site", "same-origin", "Cookie", "sid=me"}, 200},
		{"/api/../orders", []string{"Sec-Fetch-Site", "cross-site", "Cookie", "sid=victim"}, 403},
		{"/api%2F..%2Forders", []string{"Sec-Fetch-Site", "cross-site", "Cookie", "sid=victim"}, 403},
		{"/apiorders", []string{"Sec-Fetch-Site", "cross-site", "Cookie", "sid=victim"}, 403},
	} {
		if code, body, _ := do(t, h, "POST", c.path, "", c.header...); code != c.want {
			t.Errorf("POST %s %v: %d %s; want %d", c.path, c.header, code, body, c.want)
		}
	}
	if _, body, _ := do(t, h, "POST", "/orders", "", "Sec-Fetch-Site", "same-origin", "Cookie", "sid=me"); strings.TrimSpace(body) != `"session me"` {
		t.Errorf("a same-origin POST lost its cookie: %s", body)
	}
}
