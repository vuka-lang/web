package web

import (
	"net/http/httptest"
	"testing"
)

// TestBasePrefix covers the mount-prefix the framework prepends to the
// root-absolute URLs it writes (runtime tags, redirects).
func TestBasePrefix(t *testing.T) {
	// Mounted under /gateway via http.StripPrefix: RequestURI keeps the prefix,
	// URL.Path has it removed.
	r := httptest.NewRequest("GET", "/admin/login", nil)
	r.RequestURI = "/gateway/admin/login"
	if got := basePrefix(r); got != "/gateway" {
		t.Errorf("mounted basePrefix = %q, want /gateway", got)
	}

	// Served at the root: no prefix.
	root := httptest.NewRequest("GET", "/admin/login", nil)
	root.RequestURI = "/admin/login"
	if got := basePrefix(root); got != "" {
		t.Errorf("root basePrefix = %q, want empty", got)
	}

	// X-Forwarded-Prefix is ignored from an untrusted peer (no trusted-proxy
	// config here), so a stripped-path request without StripPrefix sees none.
	fwd := httptest.NewRequest("GET", "/admin/login", nil)
	fwd.RequestURI = "/admin/login"
	fwd.Header.Set("X-Forwarded-Prefix", "/gateway")
	if got := basePrefix(fwd); got != "" {
		t.Errorf("untrusted X-Forwarded-Prefix honoured: %q", got)
	}
}

// TestConnPrefixLoc covers the mount prefix on a live redirect location the
// server sets (web.Redirect from a live handler).
func TestConnPrefixLoc(t *testing.T) {
	mounted := httptest.NewRequest("GET", "/_vuka/live", nil)
	mounted.RequestURI = "/gateway/_vuka/live"
	c := &conn{r: mounted}
	cases := map[string]string{
		"/admin/systems": "/gateway/admin/systems", // root-absolute: prefixed
		"/":              "/gateway/",              // home, too
		"systems/x":      "systems/x",              // relative: untouched
		"//evil/x":       "//evil/x",               // scheme-relative: untouched
		"":               "",                       // no redirect
	}
	for in, want := range cases {
		if got := c.prefixLoc(in); got != want {
			t.Errorf("prefixLoc(%q) = %q, want %q", in, got, want)
		}
	}

	// At the root (no mount), locations are unchanged.
	root := httptest.NewRequest("GET", "/_vuka/live", nil)
	root.RequestURI = "/_vuka/live"
	if got := (&conn{r: root}).prefixLoc("/admin"); got != "/admin" {
		t.Errorf("root prefixLoc = %q, want /admin", got)
	}
}
