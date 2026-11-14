package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2/live"
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

// TestMountLocation covers the mount prefix on an HTTP redirect / form-answer
// location (redirect(), formAnswer): a ui.Form's post-submit web.Redirect.
func TestMountLocation(t *testing.T) {
	mounted := httptest.NewRequest("POST", "/admin/login", nil)
	mounted.RequestURI = "/gateway/admin/login"
	cases := map[string]string{
		"/admin":          "/gateway/admin",  // the login handler's HomePath
		"/admin/users":    "/gateway/admin/users",
		"/gateway/admin":  "/gateway/admin",  // already prefixed: not doubled
		"/gateway":        "/gateway",        // the bare prefix: not doubled
		"./y":             "./y",             // relative: untouched
		"//host/z":        "//host/z",        // scheme-relative: untouched
		"https://x/z":     "https://x/z",     // absolute URL: untouched
	}
	for in, want := range cases {
		if got := mountLocation(mounted, in); got != want {
			t.Errorf("mountLocation(%q) = %q, want %q", in, got, want)
		}
	}
	// At the root (no mount), locations are unchanged.
	root := httptest.NewRequest("POST", "/admin/login", nil)
	root.RequestURI = "/admin/login"
	if got := mountLocation(root, "/admin"); got != "/admin" {
		t.Errorf("root mountLocation = %q, want /admin", got)
	}
}

// TestPrefixJS covers the mount prefix on a navigate/patch command the server
// pushes over the live socket — a live form's web.Redirect, or PushNavigate —
// which the client runs as an absolute in-app path.
func TestPrefixJS(t *testing.T) {
	mounted := httptest.NewRequest("GET", "/_vuka/live", nil)
	mounted.RequestURI = "/gateway/_vuka/live"
	c := &conn{r: mounted}
	pushes := []live.JSPush{{Ops: json.RawMessage(
		`[["navigate",{"href":"/admin/systems/records"}],` +
			`["patch",{"href":"/gateway/admin/users?q=x"}],` + // already prefixed: left alone
			`["addClass",{"name":"on"}]]`)}}
	c.prefixJS(pushes)
	got := string(pushes[0].Ops)
	for _, want := range []string{
		`["navigate",{"href":"/gateway/admin/systems/records"}]`,
		`["patch",{"href":"/gateway/admin/users?q=x"}]`, // unchanged, not doubled
		`["addClass",{"name":"on"}]`,                     // untouched
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prefixJS missing %s\n got %s", want, got)
		}
	}
	if strings.Contains(got, "/gateway/gateway") {
		t.Errorf("prefixJS doubled a prefix: %s", got)
	}

	// At the root (no mount), commands are unchanged.
	root := httptest.NewRequest("GET", "/_vuka/live", nil)
	root.RequestURI = "/_vuka/live"
	rp := []live.JSPush{{Ops: json.RawMessage(`[["navigate",{"href":"/admin"}]]`)}}
	(&conn{r: root}).prefixJS(rp)
	if got := string(rp[0].Ops); !strings.Contains(got, `"href":"/admin"`) {
		t.Errorf("root prefixJS changed the href: %s", got)
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
