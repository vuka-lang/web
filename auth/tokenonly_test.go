package auth_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/auth"
)

// A token API beside the app's pages: a session never signs a request in
// under it, a bearer token does, and another origin's script may call it.
func TestTokenOnlyAPI(t *testing.T) {
	app := newApp(t, users(), nil,
		web.TokenOnly("/api/"), web.TokenOnly("/auth/token"),
		web.CORS("/api/", web.CORSConfig{Origins: []string{"https://shop.example"}, Methods: []string{"GET", "POST"}}),
		web.CORS("/auth/token", web.CORSConfig{Origins: []string{"https://shop.example"}}))
	app.Post("/api/orders", func(me auth.Identity) string { return "created by " + me.ID }, web.Attrs(auth.Requires("orders.write")))
	c := serve(t, app)
	if r := c.login("ada@example.com", "correct horse", ""); r.status != 303 || c.cookie() == "" {
		t.Fatalf("sign-in: %d", r.status)
	}
	// The session signs pages in, never the API.
	if r := c.do("GET", "/orders", ""); r.status != 200 {
		t.Errorf("a page with the session: %d", r.status)
	}
	if r := c.do("GET", "/api/private", ""); r.status != 401 {
		t.Errorf("the API with the session cookie: %d %s", r.status, r.body)
	}
	if r := c.do("POST", "/api/orders", "", "Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"); r.status != 401 {
		t.Errorf("a cross-site POST riding the session: %d %s", r.status, r.body)
	}
	// Elsewhere the cross-origin check stands.
	if r := c.do("POST", "/orders", "", "Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"); r.status != 403 {
		t.Errorf("a cross-site POST to a page's route: %d", r.status)
	}

	// The shop's script: a preflight, a token from another origin, the call.
	r := c.do("OPTIONS", "/auth/token", "", "Origin", "https://shop.example", "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "content-type")
	if r.status != 204 || r.header.Get("Access-Control-Allow-Origin") != "https://shop.example" || r.header.Get("Access-Control-Allow-Headers") != "Content-Type, Authorization" {
		t.Errorf("preflight: %d %v", r.status, r.header)
	}
	r = c.do("POST", "/auth/token", `{"login":"ada@example.com","password":"correct horse"}`, "Content-Type", "application/json",
		"Origin", "https://shop.example", "Sec-Fetch-Site", "cross-site")
	var tr auth.TokenResponse
	if json.Unmarshal([]byte(r.body), &tr); r.status != 200 || tr.AccessToken == "" || r.header.Get("Set-Cookie") != "" {
		t.Fatalf("a token for another origin: %d %s", r.status, r.body)
	}
	r = c.do("POST", "/api/orders", "", "Authorization", "Bearer "+tr.AccessToken, "Origin", "https://shop.example", "Sec-Fetch-Site", "cross-site")
	if r.status != 200 || !strings.Contains(r.body, "created by 1") || r.header.Get("Access-Control-Allow-Origin") != "https://shop.example" {
		t.Errorf("the API with the token: %d %s %v", r.status, r.body, r.header)
	}
	// The browser's session cookie went along and changed nothing.
	if c.cookie() == "" {
		t.Error("the session cookie was dropped from the browser")
	}
}
