package auth_test

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/ui/live"
	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/web"
	"github.com/vuka-lang/web/auth"
	"github.com/vuka-lang/web/auth/authtest"
	"golang.org/x/crypto/bcrypt"
)

var (
	ada = auth.Identity{ID: "1", Perms: []string{"orders.*"}, Roles: []string{"admin"}}
	bob = auth.Identity{ID: "2", Perms: []string{"orders.read"}}
)

func users() *authtest.Users {
	return authtest.NewUsers().
		Add("ada@example.com", "correct horse", ada).
		Add("bob@example.com", "hunter22", bob)
}

// board is a live page any signed-in user sees; deleting needs admin.
type board struct {
	ui.Live
	n, deleted int
}

var (
	boardInc    = vuka.Func{Name: "board.Inc", ErrIndex: -1, CtxIndex: -1, Zero: func() []any { return nil }}
	boardDelete = vuka.Func{Name: "board.Delete", ErrIndex: -1, CtxIndex: -1, Zero: func() []any { return nil }}
	boardRemove = vuka.Func{Name: "board.Remove", ErrIndex: 0, CtxIndex: -1, Zero: func() []any { return []any{error(nil)} }}
	adminOnly   = auth.Requires("admin")
)

func (b *board) Inc() { b.n++ }

// Delete is what Vuka generates for
//
//	@auth.Requires("admin")
//	func (b *board) Delete() { b.deleted++ }
func (b *board) Delete() {
	vuka.NewCall(&boardDelete, adminOnly.Calls(), b, []any{}, func(*vuka.Call) []any { b.deleted++; return nil }).Run()
}

// Remove is a gated handler returning an error.
func (b *board) Remove() error {
	c := vuka.NewCall(&boardRemove, adminOnly.Calls(), b, []any{}, func(*vuka.Call) []any { b.deleted++; return []any{error(nil)} })
	c.Run()
	err, _ := c.Results[0].(error)
	return err
}

func (b *board) Render() ui.Node {
	return ui.El("div", nil, ui.El("output", nil, ui.Text(fmt.Sprintf("n=%d deleted=%d", b.n, b.deleted))),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(b.Inc)}}, ui.Text("+")),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(b.Delete)}}, ui.Text("delete")),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(b.Remove)}}, ui.Text("remove")))
}

// whoami is a live page showing who it mounted for.
type whoami struct {
	ui.Live
	who string
}

func (w *whoami) Mount(ctx context.Context) {
	if id := auth.Current(ctx); id != nil {
		w.who = id.ID
	}
}
func (w *whoami) Render() ui.Node { return ui.El("p", nil, ui.Text("who="+w.who)) }

func page(text string) func() ui.Node {
	return func() ui.Node { return ui.El("html", nil, ui.El("body", nil, ui.El("p", nil, ui.Text(text)))) }
}

type order struct{ Owner string }

func newApp(t *testing.T, u *authtest.Users, edit func(*auth.Config), opts ...web.Option) *web.App {
	t.Helper()
	cfg := auth.Config{Users: u, Passwords: authtest.Cheap, Endpoints: "/auth", Roles: map[string][]string{"admin": {"admin"}},
		Policies: []auth.Policy{auth.PolicyFor(func(_ context.Context, id *auth.Identity, perm string, o *order) bool {
			return o.Owner == id.ID || id.Can(perm)
		})}}
	if edit != nil {
		edit(&cfg)
	}
	app := web.New(append([]web.Option{web.Only("none"), web.NoDotEnv(), auth.Module(cfg)}, opts...)...)
	open := web.Attrs(auth.Public())
	app.Get("/login", page("login page"), open)
	app.Get("/open", page("open"), open)
	app.Get("/private", page("private"))
	app.Get("/api/private", func() map[string]string { return map[string]string{"ok": "private"} })
	app.Get("/orders", func(me *auth.Identity) string { return "orders of " + me.ID }, web.Attrs(auth.Requires("orders.write")), web.Params("me"))
	app.Get("/admin", func() string { return "admin" }, web.Attrs(auth.RequiresAny("admin", "billing")))
	app.Post("/orders", func(me auth.Identity) string { return "created by " + me.ID }, web.Attrs(auth.Requires("orders.write")), web.Params("me"))
	app.Get("/id", func(me auth.Identity) string { return "id " + me.ID }, open, web.Params("me"))
	app.Get("/id-or-nil", func(me *auth.Identity) string { return fmt.Sprint("id ", me != nil) }, open, web.Params("me"))
	app.Get("/check/{owner}", func(ctx context.Context, owner string) error {
		return auth.Check(ctx, "orders.delete", &order{Owner: owner})
	}, web.Params("ctx", "owner"), open)
	app.Live("/board", &board{})
	app.Live("/staff", &whoami{}, web.Attrs(auth.Requires("admin")))
	app.Live("/who", &whoami{}, web.Attrs(auth.Public()))
	return app
}

// client is a browser: cookies, and redirects shown rather than followed.
type client struct {
	t   *testing.T
	srv *httptest.Server
	c   *http.Client
}

func serve(t *testing.T, app *web.App) *client {
	t.Helper()
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &client{t, srv, &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type answer struct {
	status int
	header http.Header
	body   string
}

func (c *client) do(method, path, body string, header ...string) answer {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return answer{res.StatusCode, res.Header, string(b)}
}

const html = "text/html,application/xhtml+xml"

func (c *client) page(path string) answer { return c.do("GET", path, "", "Accept", html) }

func (c *client) login(login, password, next string) answer {
	c.t.Helper()
	f := url.Values{"login": {login}, "password": {password}}
	if next != "" {
		f.Set("next", next)
	}
	return c.do("POST", "/auth/login", f.Encode(), "Content-Type", "application/x-www-form-urlencoded", "Accept", html, "Origin", c.srv.URL)
}

func (c *client) cookie() string {
	u, _ := url.Parse(c.srv.URL)
	for _, k := range c.c.Jar.Cookies(u) {
		if k.Name == "vk_session" {
			return k.Value
		}
	}
	return ""
}

func TestDenyByDefault(t *testing.T) {
	c := serve(t, newApp(t, users(), nil))
	if r := c.page("/private"); r.status != 302 || r.header.Get("Location") != "/login?next=%2Fprivate" {
		t.Errorf("a page without a sign-in: %d %s", r.status, r.header.Get("Location"))
	}
	if r := c.page("/private?tab=2"); r.header.Get("Location") != "/login?next=%2Fprivate%3Ftab%3D2" {
		t.Errorf("next keeps the query: %s", r.header.Get("Location"))
	}
	r := c.do("GET", "/api/private", "")
	if r.status != 401 || r.header.Get("WWW-Authenticate") != "Bearer" || !strings.Contains(r.body, `"error":"unauthorized"`) {
		t.Errorf("an API without a sign-in: %d %v %s", r.status, r.header, r.body)
	}
	if r := c.page("/open"); r.status != 200 || !strings.Contains(r.body, "open") {
		t.Errorf("a public page: %d %s", r.status, r.body)
	}
	if r := c.page("/login"); r.status != 200 {
		t.Errorf("the login page: %d", r.status)
	}
	if r := c.page("/board"); r.status != 302 {
		t.Errorf("a live page without a sign-in: %d", r.status)
	}
	if r := c.page("/nowhere"); r.status != 404 {
		t.Errorf("no route: %d", r.status)
	}

	// PublicByDefault: only what asks for a sign-in needs one.
	c = serve(t, newApp(t, users(), func(c *auth.Config) { c.PublicByDefault = true }))
	if r := c.page("/private"); r.status != 200 {
		t.Errorf("PublicByDefault: %d", r.status)
	}
	if r := c.page("/orders"); r.status != 302 {
		t.Errorf("PublicByDefault, a gated route: %d", r.status)
	}
}

func TestRouteGates(t *testing.T) {
	c := serve(t, newApp(t, users(), nil))
	if r := c.login("bob@example.com", "hunter22", ""); r.status != 303 || r.header.Get("Location") != "/" {
		t.Fatalf("login: %d %v", r.status, r.header)
	}
	if r := c.page("/private"); r.status != 200 {
		t.Errorf("signed in: %d", r.status)
	}
	if r := c.do("GET", "/orders", ""); r.status != 403 || !strings.Contains(r.body, "forbidden") {
		t.Errorf("without the perm: %d %s", r.status, r.body)
	}
	if r := c.page("/orders"); r.status != 403 || !strings.Contains(r.body, "<h1>Forbidden</h1>") {
		t.Errorf("without the perm, a page: %d %s", r.status, r.body)
	}
	if r := c.do("GET", "/admin", ""); r.status != 403 {
		t.Errorf("RequiresAny without any: %d", r.status)
	}
	if r := c.page("/staff"); r.status != 403 {
		t.Errorf("a gated live page without the perm: %d", r.status)
	}

	c = serve(t, newApp(t, users(), nil))
	c.login("ada@example.com", "correct horse", "")
	if r := c.do("GET", "/orders", ""); r.status != 200 || r.body != `"orders of 1"`+"\n" {
		t.Errorf("a wildcard perm: %d %q", r.status, r.body)
	}
	if r := c.do("GET", "/admin", ""); r.status != 200 {
		t.Errorf("RequiresAny with a role's perm: %d", r.status)
	}
	if r := c.page("/staff"); r.status != 200 || !strings.Contains(r.body, "who=1") {
		t.Errorf("a gated live page with the perm: %d %s", r.status, r.body)
	}
	if r := c.do("POST", "/orders", "", "Origin", c.srv.URL); r.status != 200 || !strings.Contains(r.body, "created by 1") {
		t.Errorf("a POST: %d %s", r.status, r.body)
	}
}

func TestIdentityParams(t *testing.T) {
	c := serve(t, newApp(t, users(), nil))
	if r := c.do("GET", "/id", ""); r.status != 401 {
		t.Errorf("auth.Identity signed out: %d %s", r.status, r.body)
	}
	if r := c.do("GET", "/id-or-nil", ""); r.body != `"id false"`+"\n" {
		t.Errorf("*auth.Identity signed out: %s", r.body)
	}
	c.login("bob@example.com", "hunter22", "")
	if r := c.do("GET", "/id", ""); r.body != `"id 2"`+"\n" {
		t.Errorf("auth.Identity: %s", r.body)
	}
	if r := c.do("GET", "/auth/me", ""); r.status != 200 || r.body != `{"id":"2","perms":["orders.read"]}`+"\n" {
		t.Errorf("me: %d %s", r.status, r.body)
	}
	// Check with a policy: the owner, or orders.delete.
	if r := c.do("GET", "/check/2", ""); r.status != 204 {
		t.Errorf("the owner: %d", r.status)
	}
	if r := c.do("GET", "/check/1", ""); r.status != 403 {
		t.Errorf("not the owner: %d", r.status)
	}
}

func TestNext(t *testing.T) {
	for next, ok := range map[string]bool{
		"/orders": true, "/orders?page=2#x": true, "/": true, "/a/b%2Fc": true,
		"": false, "orders": false, "//evil.example": false, "/\\evil.example": false, "\\\\evil.example": false,
		"https://evil.example": false, "/%2F%2Fevil.example": false, "/%5Cevil.example": false,
		"/\t/evil.example": false, "/\n/evil.example": false, "javascript:alert(1)": false,
		"/%0d%0aSet-Cookie:x": false, "http:/evil.example": false, "/x\x00": false,
	} {
		if got := auth.SafeNext(next); got != ok {
			t.Errorf("SafeNext(%q) = %v", next, got)
		}
	}
	c := serve(t, newApp(t, users(), nil))
	if r := c.login("bob@example.com", "hunter22", "/orders?x=1"); r.header.Get("Location") != "/orders?x=1" {
		t.Errorf("a safe next: %s", r.header.Get("Location"))
	}
	for _, evil := range []string{"//evil.example", "https://evil.example/", "/\\evil.example", "/%2F%2Fevil.example"} {
		c := serve(t, newApp(t, users(), nil))
		if r := c.login("bob@example.com", "hunter22", evil); r.status != 303 || r.header.Get("Location") != "/" {
			t.Errorf("next %q: %d %s", evil, r.status, r.header.Get("Location"))
		}
		if r := c.page("/private?" + url.Values{"next": {evil}}.Encode()); r.status != 200 {
			t.Errorf("signed in: %d", r.status)
		}
	}
	// A failed login keeps a safe next only.
	c = serve(t, newApp(t, users(), nil))
	if r := c.login("bob@example.com", "wrong", "//evil.example"); r.header.Get("Location") != "/login?error=invalid" {
		t.Errorf("a failed login with an evil next: %s", r.header.Get("Location"))
	}
	if r := c.login("bob@example.com", "wrong", "/orders"); r.header.Get("Location") != "/login?error=invalid&next=%2Forders" {
		t.Errorf("a failed login: %s", r.header.Get("Location"))
	}
}

func TestSessionRotation(t *testing.T) {
	c := serve(t, newApp(t, users(), nil))
	u, _ := url.Parse(c.srv.URL)
	// A session id planted before the sign-in (fixation) is never the one after it.
	planted := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	c.c.Jar.SetCookies(u, []*http.Cookie{{Name: "vk_session", Value: planted, Path: "/"}})
	r := c.login("bob@example.com", "hunter22", "")
	first := c.cookie()
	if first == "" || first == planted {
		t.Fatalf("the cookie after sign-in: %q", first)
	}
	set := r.header.Get("Set-Cookie")
	for _, want := range []string{"HttpOnly", "SameSite=Lax", "Path=/", "Max-Age="} {
		if !strings.Contains(set, want) {
			t.Errorf("the cookie has no %s: %s", want, set)
		}
	}
	if strings.Contains(set, "Secure") {
		t.Errorf("Secure on plain HTTP: %s", set)
	}
	c.login("ada@example.com", "correct horse", "")
	second := c.cookie()
	if second == first {
		t.Fatal("a second sign-in kept the session id")
	}
	// The first session is gone.
	old := &client{t, c.srv, &http.Client{CheckRedirect: c.c.CheckRedirect}}
	if r := old.do("GET", "/api/private", "", "Cookie", "vk_session="+first); r.status != 401 {
		t.Errorf("the session before the second sign-in: %d", r.status)
	}
	if r := c.do("GET", "/auth/me", ""); !strings.Contains(r.body, `"id":"1"`) {
		t.Errorf("me: %s", r.body)
	}
	// Logout ends it.
	r = c.do("POST", "/auth/logout", "", "Accept", html, "Origin", c.srv.URL)
	if r.status != 303 || !strings.Contains(r.header.Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("logout: %d %v", r.status, r.header)
	}
	if r := old.do("GET", "/api/private", "", "Cookie", "vk_session="+second); r.status != 401 {
		t.Errorf("after logout: %d", r.status)
	}

	// Behind TLS the cookie is Secure.
	app := newApp(t, users(), nil)
	req := httptest.NewRequest("POST", "/auth/login", strings.NewReader("login=bob@example.com&password=hunter22"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, req)
	if !strings.Contains(w.Header().Get("Set-Cookie"), "; Secure") {
		t.Errorf("https: %s", w.Header().Get("Set-Cookie"))
	}
}

func TestSessionExpiry(t *testing.T) {
	c := serve(t, newApp(t, users(), func(c *auth.Config) { c.SessionTTL = 50 * time.Millisecond }))
	c.login("bob@example.com", "hunter22", "")
	if r := c.do("GET", "/api/private", ""); r.status != 200 {
		t.Fatalf("signed in: %d", r.status)
	}
	time.Sleep(80 * time.Millisecond)
	if r := c.do("GET", "/api/private", ""); r.status != 401 {
		t.Errorf("an expired session: %d", r.status)
	}
}

func TestUniformLoginErrors(t *testing.T) {
	c := serve(t, newApp(t, users(), nil))
	login := func(l, p string) answer {
		b, _ := json.Marshal(map[string]string{"login": l, "password": p})
		return c.do("POST", "/auth/login", string(b), "Content-Type", "application/json")
	}
	unknown, wrong := login("nobody@example.com", "x"), login("bob@example.com", "x")
	if unknown.status != 401 || unknown.status != wrong.status || unknown.body != wrong.body ||
		unknown.body != `{"error":"invalid login or password","status":401}`+"\n" {
		t.Errorf("unknown %d %s; wrong password %d %s", unknown.status, unknown.body, wrong.status, wrong.body)
	}
	if r := login("bob@example.com", "hunter22"); r.status != 200 || !strings.Contains(r.body, `"id":"2"`) {
		t.Errorf("a JSON login: %d %s", r.status, r.body)
	}
}

func TestThrottle(t *testing.T) {
	app := newApp(t, users(), func(c *auth.Config) { c.Throttle = auth.Throttle{PerLogin: 3, PerIP: 5, Window: time.Minute} })
	c := serve(t, app)
	try := func(l, p string) answer {
		b, _ := json.Marshal(map[string]string{"login": l, "password": p})
		return c.do("POST", "/auth/login", string(b), "Content-Type", "application/json")
	}
	for range 3 {
		if r := try("bob@example.com", "nope"); r.status != 401 {
			t.Fatalf("a wrong password: %d", r.status)
		}
	}
	r := try("BOB@example.com ", "hunter22") // the right password, too late; logins compare folded
	if r.status != 429 || r.header.Get("Retry-After") == "" || !strings.Contains(r.body, "too many failed logins") {
		t.Errorf("throttled: %d %v %s", r.status, r.header, r.body)
	}
	// An unknown login is throttled the same way.
	for range 2 {
		try("ghost@example.com", "nope")
	}
	// The address has failed 5 times now: everyone from it waits.
	if r := try("ada@example.com", "correct horse"); r.status != 429 {
		t.Errorf("throttled by address: %d", r.status)
	}
	// A form login says so on the login page.
	if r := c.login("ada@example.com", "correct horse", ""); r.header.Get("Location") != "/login?error=throttled" {
		t.Errorf("a throttled form: %s", r.header.Get("Location"))
	}
	if r := c.do("POST", "/auth/token", `{"login":"ada@example.com","password":"correct horse"}`, "Content-Type", "application/json"); r.status != 429 {
		t.Errorf("the token endpoint is throttled too: %d", r.status)
	}
}

func TestHashUpgrade(t *testing.T) {
	legacy, _ := bcrypt.GenerateFromPassword([]byte("old secret"), bcrypt.MinCost)
	weak, _ := auth.Argon2{Memory: 32, Time: 1, Threads: 1}.Hash("weak secret")
	u := users().AddEncoded("carol@example.com", string(legacy), auth.Identity{ID: "3"}).
		AddEncoded("dan@example.com", weak, auth.Identity{ID: "4"})
	c := serve(t, newApp(t, u, nil))
	if r := c.login("carol@example.com", "old secret", ""); r.status != 303 || r.header.Get("Location") != "/" {
		t.Fatalf("a bcrypt login: %d %s", r.status, r.header.Get("Location"))
	}
	if enc := u.Encoded("3"); !strings.HasPrefix(enc, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Errorf("a bcrypt hash wasn't upgraded: %s", enc)
	}
	c.login("dan@example.com", "weak secret", "")
	if enc := u.Encoded("4"); !strings.HasPrefix(enc, "$argon2id$v=19$m=64,") {
		t.Errorf("a cheaper argon2id hash wasn't upgraded: %s", enc)
	}
	before := u.Encoded("2")
	c.login("bob@example.com", "hunter22", "")
	if u.Encoded("2") != before {
		t.Error("a current hash was re-hashed")
	}
	// Both still check.
	for _, l := range []struct{ login, pw string }{{"carol@example.com", "old secret"}, {"dan@example.com", "weak secret"}} {
		if r := serve(t, newApp(t, u, nil)).login(l.login, l.pw, ""); r.header.Get("Location") != "/" {
			t.Errorf("%s after the upgrade: %s", l.login, r.header.Get("Location"))
		}
	}
	// A password over 72 bytes never passes a bcrypt hash on its first 72.
	long := strings.Repeat("a", 72)
	h, _ := bcrypt.GenerateFromPassword([]byte(long), bcrypt.MinCost)
	if ok, _, _ := auth.CheckPassword(string(h), long+"b", auth.Argon2Defaults); ok {
		t.Error("bcrypt's 72-byte cut")
	}
	if ok, stale, err := auth.CheckPassword(weak, "weak secret", auth.Argon2Defaults); !ok || !stale || err != nil {
		t.Errorf("CheckPassword: %v %v %v", ok, stale, err)
	}
	if _, _, err := auth.CheckPassword("$argon2id$v=19$m=99999999,t=1,p=1$AAAA$AAAAAAAAAAAAAAAAAAAAAA", "x", auth.Argon2Defaults); err == nil {
		t.Error("a hash asking for 95 GiB")
	}
}

func TestBearerTokens(t *testing.T) {
	app := newApp(t, users(), nil)
	c := serve(t, app)
	r := c.do("POST", "/auth/token", `{"login":"ada@example.com","password":"correct horse","name":"cli"}`, "Content-Type", "application/json")
	var tr auth.TokenResponse
	if json.Unmarshal([]byte(r.body), &tr); r.status != 200 || !strings.HasPrefix(tr.AccessToken, "vka_") || tr.TokenType != "Bearer" || tr.ExpiresIn < 29*24*3600 {
		t.Fatalf("token: %d %s", r.status, r.body)
	}
	if r.header.Get("Cache-Control") != "no-store" {
		t.Error("a token response may be cached")
	}
	api := func(tok string) int {
		return c.do("GET", "/orders", "", "Authorization", "Bearer "+tok).status
	}
	if s := api(tr.AccessToken); s != 200 {
		t.Errorf("with the token: %d", s)
	}
	for _, bad := range []string{"vka_nope", tr.AccessToken + "x", "vka_" + strings.Repeat("A", 43), "garbage"} {
		if s := api(bad); s != 401 {
			t.Errorf("token %q: %d", bad, s)
		}
	}
	if r := c.do("POST", "/auth/token", `{"login":"ada@example.com","password":"nope"}`, "Content-Type", "application/json"); r.status != 401 {
		t.Errorf("a token for a wrong password: %d", r.status)
	}

	a, err := web.Resolve[*auth.Auth](app)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	short, _, _ := a.Issue(ctx, "2", "short", 30*time.Millisecond)
	if s := api(short); s != 403 { // bob, without orders.write
		t.Errorf("a fresh token: %d", s)
	}
	time.Sleep(50 * time.Millisecond)
	if s := api(short); s != 401 {
		t.Errorf("an expired token: %d", s)
	}
	if err := a.Revoke(ctx, tr.AccessToken); err != nil {
		t.Fatal(err)
	}
	if s := api(tr.AccessToken); s != 401 {
		t.Errorf("a revoked token: %d", s)
	}
	t1, _, _ := a.Issue(ctx, "1", "", 0)
	t2, _, _ := a.Issue(ctx, "1", "", 0)
	a.RevokeUser(ctx, "1")
	if api(t1) != 401 || api(t2) != 401 {
		t.Error("RevokeUser left a token")
	}
	// Logout with a token revokes it.
	t3, _, _ := a.Issue(ctx, "1", "", 0)
	if r := c.do("POST", "/auth/logout", "", "Authorization", "Bearer "+t3); r.status != 204 || api(t3) != 401 {
		t.Errorf("logout with a token: %d", r.status)
	}
}

func TestJWT(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	sign := func(alg string, claims map[string]any, k any) string {
		h, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT"})
		p, _ := json.Marshal(claims)
		msg := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
		var sig []byte
		switch k := k.(type) {
		case []byte:
			m := hmac.New(sha256.New, k)
			m.Write([]byte(msg))
			sig = m.Sum(nil)
		case *rsa.PrivateKey:
			sum := sha256.Sum256([]byte(msg))
			sig, _ = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
		}
		return msg + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	exp := time.Now().Add(time.Hour).Unix()
	good := map[string]any{"sub": "1", "exp": exp, "iss": "idp", "aud": []string{"api"}, "iat": time.Now().Add(-time.Minute).Unix()}

	app := newApp(t, users(), func(c *auth.Config) { c.JWT = &auth.JWT{Key: key, Issuer: "idp", Audience: "api"} })
	c := serve(t, app)
	get := func(tok string) int { return c.do("GET", "/orders", "", "Authorization", "Bearer "+tok).status }
	if s := get(sign("HS256", good, key)); s != 200 {
		t.Errorf("a good HS256 JWT: %d", s)
	}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range good {
			m[a] = b
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for name, tok := range map[string]string{
		"another key":   sign("HS256", good, []byte(strings.Repeat("x", 32))),
		"alg none":      sign("none", good, nil),
		"RS256 claimed": sign("RS256", good, rsaKey),
		"expired":       sign("HS256", with("exp", time.Now().Add(-time.Hour).Unix()), key),
		"no exp":        sign("HS256", with("exp", nil), key),
		"not yet":       sign("HS256", with("nbf", time.Now().Add(time.Hour).Unix()), key),
		"issuer":        sign("HS256", with("iss", "other"), key),
		"audience":      sign("HS256", with("aud", "other"), key),
		"no user":       sign("HS256", with("sub", "99"), key),
	} {
		if s := get(tok); s != 401 {
			t.Errorf("%s: %d", name, s)
		}
	}
	a, _ := web.Resolve[*auth.Auth](app)
	a.RevokeUser(context.Background(), "1")
	if s := get(sign("HS256", good, key)); s != 401 {
		t.Errorf("a JWT issued before RevokeUser: %d", s)
	}

	c = serve(t, newApp(t, users(), func(c *auth.Config) { c.JWT = &auth.JWT{PublicKey: &rsaKey.PublicKey} }))
	get = func(tok string) int { return c.do("GET", "/orders", "", "Authorization", "Bearer "+tok).status }
	if s := get(sign("RS256", good, rsaKey)); s != 200 {
		t.Errorf("a good RS256 JWT: %d", s)
	}
	// The key confusion attack: an HS256 token "signed" with the public key.
	pub := []byte(fmt.Sprint(rsaKey.PublicKey))
	if s := get(sign("HS256", good, pub)); s != 401 {
		t.Errorf("HS256 against an RSA key: %d", s)
	}
}

func TestCSRF(t *testing.T) {
	c := serve(t, newApp(t, users(), nil))
	c.login("ada@example.com", "correct horse", "")
	post := func(header ...string) int { return c.do("POST", "/orders", "", header...).status }
	if s := post("Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example"); s != 403 {
		t.Errorf("a cross-site POST: %d", s)
	}
	if s := post("Origin", "https://evil.example"); s != 403 {
		t.Errorf("a cross-origin POST: %d", s)
	}
	if s := post("Sec-Fetch-Site", "same-site", "Origin", "https://sub."+strings.TrimPrefix(c.srv.URL, "http://")); s != 403 {
		t.Errorf("a same-site POST from another origin: %d", s)
	}
	if s := post("Sec-Fetch-Site", "same-origin", "Origin", c.srv.URL); s != 200 {
		t.Errorf("a same-origin POST: %d", s)
	}
	if s := post(); s != 200 {
		t.Errorf("a POST saying nothing of its origin (not a browser): %d", s)
	}
	r := c.do("POST", "/auth/login", "login=ada@example.com&password=correct+horse", "Content-Type", "application/x-www-form-urlencoded",
		"Accept", html, "Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example")
	if r.status != 403 {
		t.Errorf("a cross-site login (login CSRF): %d", r.status)
	}
	// A bearer token can't be sent by another site's form: no origin check.
	a := newApp(t, users(), nil)
	cb := serve(t, a)
	au, _ := web.Resolve[*auth.Auth](a)
	tok, _, _ := au.Issue(context.Background(), "1", "", 0)
	if r := cb.do("POST", "/orders", "", "Authorization", "Bearer "+tok, "Origin", "https://spa.example"); r.status != 200 {
		t.Errorf("a cross-origin POST with a bearer token: %d %s", r.status, r.body)
	}
	// Trusted origins may.
	c = serve(t, newApp(t, users(), func(c *auth.Config) { c.TrustedOrigins = []string{"https://admin.example"} }))
	c.login("ada@example.com", "correct horse", "")
	if s := c.do("POST", "/orders", "", "Origin", "https://admin.example", "Sec-Fetch-Site", "cross-site").status; s != 200 {
		t.Errorf("a trusted origin: %d", s)
	}
}

var tokenRe = regexp.MustCompile(`data-vk-token="([^"]+)"`)

// socket joins the live page at path as c's browser.
func (c *client) socket(path string, origin string) (*websocket.Conn, live.ServerMessage) {
	c.t.Helper()
	r := c.page(path)
	m := tokenRe.FindStringSubmatch(r.body)
	if m == nil {
		c.t.Fatalf("%s: no token: %d %s", path, r.status, r.body)
	}
	u, _ := url.Parse(c.srv.URL)
	h := http.Header{"Origin": {origin}}
	for _, k := range c.c.Jar.Cookies(u) {
		h.Add("Cookie", k.Name+"="+k.Value)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(c.srv.URL, "http")+"/_vuka/live?t="+m[1], &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { ws.CloseNow() })
	send(c.t, ws, map[string]any{"type": "join", "ref": 1, "v": 1})
	return ws, must(c.t, ws)
}

func send(t *testing.T, ws *websocket.Conn, m map[string]any) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func recv(ws *websocket.Conn) (live.ServerMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var m live.ServerMessage
	_, b, err := ws.Read(ctx)
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	return m, err
}

func must(t *testing.T, ws *websocket.Conn) live.ServerMessage {
	t.Helper()
	m, err := recv(ws)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func handler(t *testing.T, html string, i int) string {
	t.Helper()
	ms := regexp.MustCompile(`data-vk-on-click="(c\d+:\d+)"`).FindAllStringSubmatch(html, -1)
	if i >= len(ms) {
		t.Fatalf("no handler %d in %s", i, html)
	}
	return ms[i][1]
}

func click(t *testing.T, ws *websocket.Conn, target string, ref int) live.ServerMessage {
	t.Helper()
	send(t, ws, map[string]any{"type": "event", "ref": ref, "target": target, "event": "click"})
	return must(t, ws)
}

func patched(m live.ServerMessage) string {
	var b strings.Builder
	for _, u := range m.Updates {
		b.WriteString(u.HTML)
	}
	return b.String()
}

func TestLiveEventGates(t *testing.T) {
	c := serve(t, newApp(t, users(), nil))
	c.login("bob@example.com", "hunter22", "")
	ws, m := c.socket("/board", c.srv.URL)
	if m.Type != live.Render {
		t.Fatalf("join: %+v", m)
	}
	inc, del, rm := handler(t, m.HTML, 0), handler(t, m.HTML, 1), handler(t, m.HTML, 2)
	if r := click(t, ws, inc, 2); r.Error != "" || !strings.Contains(patched(r), "n=1 deleted=0") {
		t.Errorf("an ungated event: %+v", r)
	}
	if r := click(t, ws, del, 3); r.Error != "forbidden" || strings.Contains(patched(r), "deleted=1") {
		t.Errorf("a gated event without the perm: %+v", r)
	}
	if r := click(t, ws, rm, 4); r.Error != "forbidden" {
		t.Errorf("a gated event returning an error, without the perm: %+v", r)
	}
	if r := click(t, ws, inc, 5); !strings.Contains(patched(r), "n=2 deleted=0") {
		t.Errorf("after the refusals: %+v", r)
	}

	c = serve(t, newApp(t, users(), nil))
	c.login("ada@example.com", "correct horse", "")
	ws, m = c.socket("/board", c.srv.URL)
	if r := click(t, ws, handler(t, m.HTML, 1), 2); r.Error != "" || !strings.Contains(patched(r), "deleted=1") {
		t.Errorf("a gated event with the perm: %+v", r)
	}
	if r := click(t, ws, handler(t, m.HTML, 2), 3); r.Error != "" || !strings.Contains(patched(r), "deleted=2") {
		t.Errorf("a gated event returning an error, with the perm: %+v", r)
	}

	// Outside a live event the gate isn't the decorator's business: Go calls it.
	b := &board{}
	b.Delete()
	if b.deleted != 1 {
		t.Error("a direct call was refused")
	}
}

func TestLiveIdentityAndJoin(t *testing.T) {
	c := serve(t, newApp(t, users(), nil))
	// A public live page mounts anonymous; then signed in.
	_, m := c.socket("/who", c.srv.URL)
	if !strings.Contains(m.HTML, "who=") || strings.Contains(m.HTML, "who=2") {
		t.Errorf("anonymous: %s", m.HTML)
	}
	c.login("bob@example.com", "hunter22", "")
	_, m = c.socket("/who", c.srv.URL)
	if !strings.Contains(m.HTML, "who=2") {
		t.Errorf("Mount's identity: %s", m.HTML)
	}
	// The socket refuses another origin.
	r := c.page("/board")
	tok := tokenRe.FindStringSubmatch(r.body)[1]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(c.srv.URL, "http")+"/_vuka/live?t="+tok,
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}, "Cookie": {"vk_session=" + c.cookie()}}})
	if err == nil || res == nil || res.StatusCode != 403 {
		t.Errorf("a cross-origin socket: %v %v", err, res)
	}
	// A join without the cookie (another browser with the page's token) is sent to sign in.
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(c.srv.URL, "http")+"/_vuka/live?t="+tok,
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {c.srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	send(t, ws, map[string]any{"type": "join", "ref": 1, "v": 1})
	if m := must(t, ws); m.Type != live.Redirect || m.URL != "/login?next=%2Fboard" {
		t.Errorf("a join without a sign-in: %+v", m)
	}
}

func TestRevokeClosesLivePages(t *testing.T) {
	app := newApp(t, users(), nil)
	c := serve(t, app)
	c.login("bob@example.com", "hunter22", "")
	ws, m := c.socket("/board", c.srv.URL)
	inc := handler(t, m.HTML, 0)
	if r := click(t, ws, inc, 2); r.Error != "" {
		t.Fatalf("before: %+v", r)
	}
	a, _ := web.Resolve[*auth.Auth](app)
	if err := a.RevokeUser(context.Background(), "2"); err != nil {
		t.Fatal(err)
	}
	send(t, ws, map[string]any{"type": "event", "ref": 3, "target": inc, "event": "click"})
	_, err := recv(ws)
	if websocket.CloseStatus(err) != 4001 {
		t.Errorf("after RevokeUser: %v", err)
	}
	if r := c.page("/board"); r.status != 302 {
		t.Errorf("the page after RevokeUser: %d", r.status)
	}
	// Signing out in another tab closes it too.
	c.login("bob@example.com", "hunter22", "")
	ws, m = c.socket("/board", c.srv.URL)
	c.do("POST", "/auth/logout", "", "Origin", c.srv.URL)
	send(t, ws, map[string]any{"type": "event", "ref": 2, "target": handler(t, m.HTML, 0), "event": "click"})
	if _, err := recv(ws); websocket.CloseStatus(err) != 4001 {
		t.Errorf("after logout: %v", err)
	}
	// So does a user who is gone.
	u := users()
	c = serve(t, newApp(t, u, nil))
	c.login("bob@example.com", "hunter22", "")
	ws, m = c.socket("/board", c.srv.URL)
	u.Remove("2")
	if r := c.do("GET", "/api/private", ""); r.status != 401 {
		t.Errorf("a removed user: %d", r.status)
	}
	send(t, ws, map[string]any{"type": "event", "ref": 2, "target": handler(t, m.HTML, 0), "event": "click"})
	if _, err := recv(ws); websocket.CloseStatus(err) != 4001 {
		t.Errorf("a removed user's page: %v", err)
	}
}

func TestAuthtest(t *testing.T) {
	admin := &auth.Identity{ID: "9", Roles: []string{"admin"}, Perms: []string{"orders.write"}}
	for _, first := range []bool{true, false} {
		u := users()
		var app *web.App
		if first {
			app = newApp(t, u, nil, authtest.As(admin))
		} else {
			app = web.New(web.Only("none"), web.NoDotEnv(), authtest.As(admin), auth.Module(auth.Config{Users: u, Roles: map[string][]string{"admin": {"admin"}}}))
			app.Get("/orders", func(me *auth.Identity) string { return "orders of " + me.ID }, web.Attrs(auth.Requires("orders.write")), web.Params("me"))
			app.Live("/staff", &whoami{}, web.Attrs(auth.Requires("admin")))
			app.Get("/login", page("login"), web.Attrs(auth.Public()))
		}
		c := serve(t, app)
		if r := c.do("GET", "/orders", ""); r.status != 200 || !strings.Contains(r.body, "orders of 9") {
			t.Errorf("authtest.As (first %v): %d %s", first, r.status, r.body)
		}
		if r := c.page("/staff"); r.status != 200 || !strings.Contains(r.body, "who=9") {
			t.Errorf("authtest.As, a live page with a role (first %v): %d %s", first, r.status, r.body)
		}
	}
	// One request.
	app := newApp(t, users(), nil)
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, authtest.Request(httptest.NewRequest("GET", "/orders", nil), admin))
	if w.Code != 200 {
		t.Errorf("authtest.Request: %d", w.Code)
	}
	w = httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/orders", nil))
	if w.Code != 401 {
		t.Errorf("without: %d", w.Code)
	}
	// A live session built by the test.
	s := live.NewSession(authtest.Context(context.Background(), admin), func(context.Context) ui.Node { return ui.Component("w", nil, &whoami{}) })
	defer s.Close()
	if html, err := s.Render(); err != nil || !strings.Contains(html, "who=9") {
		t.Errorf("a live session: %s %v", html, err)
	}
	if !auth.Can(authtest.Context(context.Background(), admin), "orders.write") || auth.Can(context.Background(), "orders.write") {
		t.Error("Can")
	}
}

func TestBuildChecks(t *testing.T) {
	// A gated route in an app without auth.
	app := web.New(web.Only("none"), web.NoDotEnv())
	app.Get("/x", func() string { return "x" }, web.Attrs(auth.Requires("x")))
	if err := app.Build(); err == nil || !strings.Contains(err.Error(), "needs auth.Module") {
		t.Errorf("no auth: %v", err)
	}
	// A login page that needs a sign-in.
	app = web.New(web.Only("none"), web.NoDotEnv(), auth.Module(auth.Config{Users: users()}))
	app.Get("/login", page("login"))
	if err := app.Build(); err == nil || !strings.Contains(err.Error(), "is the login page") {
		t.Errorf("a gated login page: %v", err)
	}
	// No Users.
	app = web.New(web.Only("none"), web.NoDotEnv(), auth.Module(auth.Config{}))
	if err := app.Build(); err == nil || !strings.Contains(err.Error(), "auth: no Users") {
		t.Errorf("no Users: %v", err)
	}
	// Users from the container: a constructor, or a provided implementation.
	app = web.New(web.Only("none"), web.NoDotEnv(), auth.Module(auth.Config{Users: func() *authtest.Users { return users() }}))
	if err := app.Build(); err != nil {
		t.Errorf("a constructor: %v", err)
	}
	app = web.New(web.Only("none"), web.NoDotEnv(), web.Provide(users()), auth.Module(auth.Config{}))
	if err := app.Build(); err != nil {
		t.Errorf("a provided Users: %v", err)
	}
	app = web.New(web.Only("none"), web.NoDotEnv(), auth.Module(auth.Config{Users: users(), JWT: &auth.JWT{Key: []byte("short")}}))
	if err := app.Build(); err == nil || !strings.Contains(err.Error(), "shorter than 32 bytes") {
		t.Errorf("a short JWT key: %v", err)
	}
}

func TestControllerGates(t *testing.T) {
	// A controller's gate, opened on one route: attributes come type first.
	ctrl := []any{auth.Requires("admin")}
	app := newApp(t, users(), nil)
	app.Get("/ctrl/open", func() string { return "open" }, web.Attrs(append(ctrl, auth.Public())...))
	app.Get("/ctrl/closed", func() string { return "closed" }, web.Attrs(ctrl...))
	app.Get("/ctrl/more", func() string { return "more" }, web.Attrs(append(ctrl, auth.Requires("orders.read"))...))
	c := serve(t, app)
	c.login("bob@example.com", "hunter22", "")
	for path, want := range map[string]int{"/ctrl/open": 200, "/ctrl/closed": 403, "/ctrl/more": 403} {
		if r := c.do("GET", path, ""); r.status != want {
			t.Errorf("%s: %d, want %d", path, r.status, want)
		}
	}
}

func TestToolbarNotes(t *testing.T) {
	t.Setenv(web.DevDirEnv, t.TempDir())
	app := newApp(t, users(), nil, web.Dev())
	c := serve(t, app)
	c.login("bob@example.com", "hunter22", "")
	r := c.page("/orders")
	id := r.header.Get(web.ToolbarHeader)
	if id == "" {
		t.Fatal("no toolbar")
	}
	rec := c.do("GET", "/_vuka/toolbar/r/"+id+"?panels", "")
	var out struct{ HTML string }
	json.Unmarshal([]byte(rec.body), &out)
	for _, want := range []string{"<h4>Auth</h4>", "<th>Identity</th><td>2 (session)</td>", `<span class="errtext">Requires orders.write: refused, forbidden</span>`} {
		if !strings.Contains(out.HTML, want) {
			t.Errorf("the Request panel has no %q", want)
		}
	}
	login := c.do("POST", "/auth/login", "login=bob@example.com&password=hunter22", "Content-Type", "application/x-www-form-urlencoded", "Accept", html)
	rec = c.do("GET", "/_vuka/toolbar/r/"+login.header.Get(web.ToolbarHeader)+"?panels", "")
	json.Unmarshal([]byte(rec.body), &out)
	if strings.Contains(out.HTML, "hunter22") || strings.Contains(out.HTML, c.cookie()) {
		t.Error("the toolbar shows a password or a session id")
	}
}

func TestIdentityString(t *testing.T) {
	id := auth.Identity{ID: "7", Roles: []string{"a"}, User: struct{ PasswordHash string }{"secret"}}
	if s := fmt.Sprintf("%v %+v", id, &id); strings.Contains(s, "secret") {
		t.Errorf("an identity prints its user: %s", s)
	}
	if !(&auth.Identity{Perms: []string{"*"}}).Can("x.y") || (&auth.Identity{Perms: []string{"orders.*"}}).Can("ordersx") ||
		!(&auth.Identity{Perms: []string{"orders.*"}}).Can("orders.a.b") || (*auth.Identity)(nil).Can("x") {
		t.Error("Can")
	}
	if !errors.Is(auth.ErrInvalidLogin, web.Unauthorized) {
		t.Error("ErrInvalidLogin is a 401")
	}
}
