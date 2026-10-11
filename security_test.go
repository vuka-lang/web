package web

import (
	"bytes"
	"crypto/tls"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// An env file can't switch an app into development, and a binary without a
// go.mod above it reads env files from its working directory only.
func TestEnvFilesCantSetDevelopment(t *testing.T) {
	t.Setenv("VUKA_ENV", "")
	os.Unsetenv("VUKA_ENV")
	t.Setenv(DevDirEnv, "")
	os.Unsetenv(DevDirEnv)
	dir := envSandbox(t, map[string]string{".env": "VUKA_ENV=development\nVUKA_DEV_LIVE_KEY=" + strings.Repeat("k", 40) + "\nAPP_NAME=pets\n"})
	a := New(Only("none"), DotEnv(".env"))
	if a.dev || IsDev() || os.Getenv("VUKA_ENV") != "" || os.Getenv("VUKA_DEV_LIVE_KEY") != "" {
		t.Errorf("an env file set development: dev %v, IsDev %v, VUKA_ENV %q", a.dev, IsDev(), os.Getenv("VUKA_ENV"))
	}
	if os.Getenv("APP_NAME") != "pets" {
		t.Errorf("the env file's other keys: APP_NAME=%q", os.Getenv("APP_NAME"))
	}

	// No go.mod anywhere: a .env two directories up isn't read.
	os.Remove(filepath.Join(dir, "go.mod"))
	os.WriteFile(filepath.Join(dir, ".env"), []byte("DEPLOY_SECRET=1\n"), 0o600)
	os.MkdirAll(filepath.Join(dir, "srv", "app"), 0o755)
	t.Chdir(filepath.Join(dir, "srv", "app"))
	if err := LoadEnv(".env"); err != nil || os.Getenv("DEPLOY_SECRET") != "" {
		t.Errorf("a .env above a directory without go.mod was read: %v", err)
	}
}

func TestSecurityHeaders(t *testing.T) {
	a := New(Only("none"))
	a.Get("/", func() string { return "ok" })
	a.Get("/framed", func(w http.ResponseWriter) string {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		return "ok"
	})
	h := a.Handler()
	_, _, hdr := do(t, h, "GET", "/", "")
	want := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "strict-origin-when-cross-origin",
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "frame-ancestors 'none'",
	}
	for k, v := range want {
		if hdr.Get(k) != v {
			t.Errorf("%s: %q, want %q", k, hdr.Get(k), v)
		}
	}
	if hdr.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS over plain HTTP")
	}
	if _, _, hdr := do(t, h, "GET", "/framed", ""); hdr.Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Errorf("a handler's own header: %q", hdr.Get("X-Frame-Options"))
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Errorf("HSTS over TLS: %q", got)
	}

	b := New(Only("none"), Headers(SecurityHeaders{FrameOptions: "SAMEORIGIN", ReferrerPolicy: "-", HSTS: -1}))
	b.Get("/", func() string { return "ok" })
	req = httptest.NewRequest("GET", "/", nil)
	req.TLS = &tls.ConnectionState{}
	rec = httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, req)
	if hdr := rec.Header(); hdr.Get("X-Frame-Options") != "SAMEORIGIN" || hdr.Get("Content-Security-Policy") != "frame-ancestors 'self'" ||
		hdr.Get("Referrer-Policy") != "" || hdr.Get("Strict-Transport-Security") != "" || hdr.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("configured headers: %v", hdr)
	}
	c := New(Only("none"), NoSecurityHeaders())
	c.Get("/", func() string { return "ok" })
	if _, _, hdr := do(t, c.Handler(), "GET", "/", ""); hdr.Get("X-Content-Type-Options") != "" || hdr.Get("X-Frame-Options") != "" {
		t.Errorf("NoSecurityHeaders: %v", hdr)
	}
}

func TestStaticHidesListingsAndDotfiles(t *testing.T) {
	files := fstest.MapFS{
		"a.txt":                       {Data: []byte("a")},
		".env":                        {Data: []byte("SECRET=1")},
		".git/config":                 {Data: []byte("[core]")},
		"docs/guide.txt":              {Data: []byte("guide")},
		"site/index.html":             {Data: []byte("site index")},
		".well-known/security.txt":    {Data: []byte("Contact: x")},
		"assets/.hidden/x.js":         {Data: []byte("x")},
		"assets/.well-known/deep.txt": {Data: []byte("deep")},
	}
	a := New(Only("none"))
	a.Static("/static", files)
	h := a.Handler()
	spa := fstest.MapFS{"index.html": {Data: []byte("app")}}
	for k, v := range files {
		spa[k] = v
	}
	f := New(Only("none"), Frontend(spa, "."))
	fh := f.Handler()
	for _, c := range []struct {
		path string
		code int
		body string
	}{
		{"/static/a.txt", 200, "a"},
		{"/static/.env", 404, ""},
		{"/static/.git/config", 404, ""},
		{"/static/docs/", 404, ""},
		{"/static/", 404, ""},
		{"/static/site/", 200, "site index"},
		{"/static/.well-known/security.txt", 200, "Contact"},
		{"/static/assets/.hidden/x.js", 404, ""},
		{"/static/assets/.well-known/deep.txt", 404, ""},
		{"/.env", 404, ""},
		{"/.git/config", 404, ""},
		{"/.well-known/security.txt", 200, "Contact"},
	} {
		hh := h
		if !strings.HasPrefix(c.path, "/static/") {
			hh = fh
		}
		code, body, _ := do(t, hh, "GET", c.path, "")
		if code != c.code || !strings.Contains(body, c.body) || c.code == 404 && strings.Contains(body, "SECRET") {
			t.Errorf("%s: %d %.40q; want %d %q", c.path, code, body, c.code, c.body)
		}
	}
}

func TestClientIP(t *testing.T) {
	var got string
	mk := func(opts ...Option) http.Handler {
		a := New(append([]Option{Only("none")}, opts...)...)
		a.Get("/", func(r *http.Request) string { got = ClientIP(r); return "" })
		return a.Handler()
	}
	plain, proxied := mk(), mk(TrustedProxies("10.0.0.0/8", "192.168.1.1"))
	for _, c := range []struct {
		h      http.Handler
		remote string
		header []string
		want   string
	}{
		{plain, "203.0.113.5:1234", nil, "203.0.113.5"},
		{plain, "203.0.113.5:1234", []string{"X-Forwarded-For", "1.2.3.4"}, "203.0.113.5"},   // no proxy trusted: the header is anyone's
		{proxied, "203.0.113.5:1234", []string{"X-Forwarded-For", "1.2.3.4"}, "203.0.113.5"}, // not from a trusted proxy
		{proxied, "10.1.2.3:1234", []string{"X-Forwarded-For", "1.2.3.4"}, "1.2.3.4"},
		{proxied, "10.1.2.3:1234", []string{"X-Forwarded-For", "6.6.6.6, 1.2.3.4, 10.9.9.9"}, "1.2.3.4"}, // the client's own entries are forged
		{proxied, "192.168.1.1:1234", []string{"Forwarded", `for="[2001:db8::1]:443";proto=https`}, "2001:db8::1"},
		{proxied, "10.1.2.3:1234", []string{"X-Forwarded-For", "garbage, 10.2.2.2"}, "10.2.2.2"},
		{proxied, "10.1.2.3:1234", nil, "10.1.2.3"},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.remote
		for i := 0; i+1 < len(c.header); i += 2 {
			req.Header.Set(c.header[i], c.header[i+1])
		}
		c.h.ServeHTTP(httptest.NewRecorder(), req)
		if got != c.want {
			t.Errorf("%s %v: %q, want %q", c.remote, c.header, got, c.want)
		}
	}
	if err := New(Only("none"), TrustedProxies("not a cidr")).Build(); err == nil {
		t.Error("a bad CIDR built")
	}
}

type upload struct {
	Name string `form:"name"`
}

func TestRequestBodyLimit(t *testing.T) {
	a := New(Only("none"), MaxRequestBody(1<<20))
	a.Post("/form", func(u upload) string { return "got " + u.Name })
	a.Post("/big", func(u upload) string { return "big " + u.Name }, Attrs(MaxBody(4<<20)))
	a.Post("/raw", func(r *http.Request) (string, error) {
		var buf bytes.Buffer
		_, err := buf.ReadFrom(r.Body)
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return "", ErrBodyTooLarge
		}
		return "read", err
	})
	h := a.Handler()
	form := func(size int) (string, string) {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		w.WriteField("name", "pet")
		f, _ := w.CreateFormFile("photo", "photo.jpg")
		f.Write(bytes.Repeat([]byte("x"), size))
		w.Close()
		return b.String(), w.FormDataContentType()
	}
	small, ct := form(1000)
	if code, body, _ := do(t, h, "POST", "/form", small, "Content-Type", ct); code != 200 || !strings.Contains(body, "got pet") {
		t.Errorf("a small upload: %d %s", code, body)
	}
	big, ct := form(2 << 20)
	if code, _, _ := do(t, h, "POST", "/form", big, "Content-Type", ct); code != http.StatusRequestEntityTooLarge {
		t.Errorf("an upload over the app's limit: %d", code)
	}
	if code, body, _ := do(t, h, "POST", "/big", big, "Content-Type", ct); code != 200 || !strings.Contains(body, "big pet") {
		t.Errorf("an upload under the route's own limit: %d %s", code, body)
	}
	if code, _, _ := do(t, h, "POST", "/raw", strings.Repeat("x", 2<<20)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("a raw body over the limit: %d", code)
	}
}
