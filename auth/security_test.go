package auth_test

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/ui/live"
	"github.com/vuka-lang/web"
	"github.com/vuka-lang/web/auth"
	"github.com/vuka-lang/web/auth/authtest"
	"golang.org/x/crypto/bcrypt"
)

// legacySHA1 reads an unsalted hex SHA-1 column, with a dummy of its own.
type legacySHA1 struct{}

func (legacySHA1) Check(encoded, password string) (bool, bool) {
	if len(encoded) != 40 {
		return false, false
	}
	h := sha1.Sum([]byte(password))
	return hex.EncodeToString(h[:]) == encoded, true
}

func (legacySHA1) Dummy() string { return strings.Repeat("0", 40) }

func verifyTime(a *auth.Auth, login, password string, n int) time.Duration {
	var total time.Duration
	for i := range n {
		r := httptest.NewRequest("POST", "/auth/login", nil)
		r.RemoteAddr = fmt.Sprintf("10.9.%d.%d:1", i, time.Now().Nanosecond()%250)
		start := time.Now()
		a.Verify(context.Background(), r, login, password)
		total += time.Since(start)
	}
	return total / time.Duration(n)
}

// Every kind of failed login takes the same time: unknown, wrong password
// against bcrypt, against a Hasher's encoding, a stored value no one reads,
// a password too long for bcrypt.
func TestFailedLoginsTakeTheSameTime(t *testing.T) {
	bc, _ := bcrypt.GenerateFromPassword([]byte("secret"), 6) // cheap: the floor is what's measured, -race included
	users := authtest.NewUsers().
		AddEncoded("bcrypt", string(bc), auth.Identity{ID: "1"}).
		AddEncoded("sha1", "5baa61e4c9b93f3f0682250b6cf8331b7ee68fd8", auth.Identity{ID: "3"}).
		AddEncoded("weird", "md5:abcdef", auth.Identity{ID: "4"})
	app := web.New(web.Only("none"), web.NoDotEnv(), auth.Module(auth.Config{Users: users, Hashers: []auth.Hasher{legacySHA1{}}, Passwords: authtest.Cheap,
		Throttle: auth.Throttle{PerLogin: 1000, PerIP: 1000, Window: time.Minute}}))
	a, err := web.Resolve[*auth.Auth](app)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, login, pw string }{
		{"unknown login", "nobody", "guess"},
		{"bcrypt, wrong", "bcrypt", "guess"},
		{"SHA-1 Hasher, wrong", "sha1", "guess"},
		{"unknown format", "weird", "guess"},
		{"bcrypt, 73 bytes", "bcrypt", strings.Repeat("x", 73)},
	}
	lo, hi := time.Hour, time.Duration(0)
	for _, c := range cases {
		d := verifyTime(a, c.login, c.pw, 2)
		t.Logf("%-20s %v", c.name, d.Round(time.Millisecond))
		lo, hi = min(lo, d), max(hi, d)
	}
	if lo < 300*time.Millisecond || hi-lo > 100*time.Millisecond {
		t.Errorf("failed logins take %v to %v; want each at least 300ms, within 100ms of each other", lo, hi)
	}
}

// Without the floor, an unknown login is checked against a hash of the
// cost the logins found lately have.
func TestUnknownLoginDummyMatchesStoredCost(t *testing.T) {
	b11, _ := bcrypt.GenerateFromPassword([]byte("secret"), 11)
	users := authtest.NewUsers().AddEncoded("old", string(b11), auth.Identity{ID: "1"})
	app := web.New(web.Only("none"), web.NoDotEnv(), auth.Module(auth.Config{Users: users, FailedLoginTime: -1,
		Throttle: auth.Throttle{PerLogin: 1000, PerIP: 1000, Window: time.Minute}}))
	a, _ := web.Resolve[*auth.Auth](app)
	known := verifyTime(a, "old", "guess", 3)
	unknown := verifyTime(a, "nobody", "guess", 3)
	t.Logf("known %v, unknown %v", known, unknown)
	if unknown < known*6/10 {
		t.Errorf("an unknown login takes %v, a known one %v", unknown, known)
	}
}

// ciUsers finds logins as a case- and accent-insensitive collation does.
type ciUsers struct{ *authtest.Users }

func (u ciUsers) FindLogin(ctx context.Context, login string) (*auth.Identity, string, error) {
	fold := strings.NewReplacer("á", "a", "à", "a", "â", "a", "ä", "a", "ã", "a", "å", "a").Replace(strings.ToLower(login))
	if fold == "root" { // a second login of the same account
		fold = "admin"
	}
	return u.Users.FindLogin(ctx, fold)
}

// Spellings a database finds as one login, and other logins of the same
// account, share its throttle.
func TestThrottleSpellingsAndAccounts(t *testing.T) {
	users := ciUsers{authtest.NewUsers().Add("admin", "correct horse", auth.Identity{ID: "1"})}
	app := web.New(web.Only("none"), web.NoDotEnv(), auth.Module(auth.Config{Users: users, Passwords: authtest.Cheap, FailedLoginTime: -1}))
	a, err := web.Resolve[*auth.Auth](app)
	if err != nil {
		t.Fatal(err)
	}
	tries := 0
	for i, login := range []string{"admin", "ádmin", "ÀDMIN", "âdmin", "ädmin", "ãdmin", "ådmin", "root", "Root"} {
		for j := range 6 {
			r := httptest.NewRequest("POST", "/auth/login", nil)
			r.RemoteAddr = fmt.Sprintf("10.0.%d.%d:1234", i, j)
			_, err := a.Verify(context.Background(), r, login, fmt.Sprintf("guess-%d-%d", i, j))
			var te *auth.ThrottledError
			if !errors.As(err, &te) {
				tries++
			}
		}
	}
	if tries > 5 {
		t.Errorf("%d password checks against one account; the limit is 5", tries)
	}
	if k := auth.ThrottleKey(" ÁDMIN "); k != auth.ThrottleKey("admin") {
		t.Errorf("folded keys: %q", k)
	}
	if k := auth.ThrottleKey(strings.Repeat("é", 10000)); len(k) > 300 {
		t.Errorf("a long login's key is %d bytes", len(k))
	}
	auth.SetThrottleMax(a, 50)
	for i := range 200 {
		r := httptest.NewRequest("POST", "/auth/login", nil)
		r.RemoteAddr = fmt.Sprintf("10.1.%d.%d:1", i/200, i%200)
		a.Verify(context.Background(), r, fmt.Sprint("ghost", i), "x")
	}
	if n := auth.ThrottleKeys(a); n > 50 {
		t.Errorf("the throttle holds %d keys; at most 50", n)
	}
}

type invoice struct{ Institution string }

// A policy decides for T and *T alike, and a context without the app's
// auth is refused an object a policy covers.
func TestPolicyLookup(t *testing.T) {
	pol := auth.PolicyFor(func(ctx context.Context, id *auth.Identity, perm string, o *invoice) bool {
		return id.Can(perm) && o.Institution == id.ID
	})
	officer := &auth.Identity{ID: "inst-A", Perms: []string{"invoices.edit"}}
	app := web.New(web.Only("none"), web.NoDotEnv(), auth.Module(auth.Config{Users: authtest.NewUsers(), Policies: []auth.Policy{pol}, PublicByDefault: true}),
		authtest.As(officer))
	other, own := invoice{Institution: "inst-B"}, invoice{Institution: "inst-A"}
	var errs []error
	app.Get("/x", func(r *http.Request) string {
		ctx := r.Context()
		errs = []error{
			auth.Check(ctx, "invoices.edit", &other),
			auth.Check(ctx, "invoices.edit", other),
			auth.Check(auth.WithIdentity(context.Background(), officer), "invoices.edit", &other),
			auth.Check(ctx, "invoices.edit", own),
			auth.Check(auth.System(context.Background()), "invoices.edit", &other),
		}
		return "ok"
	})
	app.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	want := []error{web.Forbidden, web.Forbidden, web.Forbidden, nil, nil}
	for i := range want {
		if errs[i] != want[i] {
			t.Errorf("check %d: %v, want %v", i, errs[i], want[i])
		}
	}

	// A policy of the value type decides for a pointer to it too.
	byValue := auth.PolicyFor(func(ctx context.Context, id *auth.Identity, perm string, o receipt) bool { return o.Owner == id.ID })
	app = web.New(web.Only("none"), web.NoDotEnv(), auth.Module(auth.Config{Users: authtest.NewUsers(), Policies: []auth.Policy{byValue}, PublicByDefault: true}),
		authtest.As(officer))
	var ptrErr error
	app.Get("/y", func(r *http.Request) string {
		ptrErr = auth.Check(r.Context(), "invoices.edit", &receipt{Owner: "someone else"})
		return "ok"
	})
	app.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/y", nil))
	if ptrErr != web.Forbidden {
		t.Errorf("a *T given to a policy of T: %v", ptrErr)
	}
}

type receipt struct{ Owner string }

// A gated method called on a request's goroutine without the request's
// context is checked against the request's identity — even under
// LaxGates, where such a call used to run unchecked. A cron's context is
// marked as the app's own work.
func TestGatesInRequestGoroutines(t *testing.T) {
	defer auth.SetStrictGates(true)
	app := newApp(t, users(), func(c *auth.Config) { c.LaxGates = true })
	v := &vault{}
	ran := func(f func()) bool { before := gateRuns.Load(); f(); return gateRuns.Load() > before }
	var plain, background bool
	app.Get("/plain", func() string { plain = ran(func() { v.Clear() }); return "" }, web.Attrs(auth.Public()))
	app.Get("/background", func() string {
		background = ran(func() { v.ClearCtx(context.Background()) })
		return ""
	}, web.Attrs(auth.Public()))
	app.Cron("clear", "@daily", func(ctx context.Context) error { return v.ClearCtx(ctx) })
	for _, who := range []struct {
		login, pw string
		admin     bool
	}{{"", "", false}, {"bob@example.com", "hunter22", false}, {"ada@example.com", "correct horse", true}} {
		c := serve(t, app)
		if who.login != "" {
			c.login(who.login, who.pw, "")
		}
		plain, background = !who.admin, !who.admin
		c.do("GET", "/plain", "")
		c.do("GET", "/background", "")
		if plain != who.admin || background != who.admin {
			t.Errorf("%q: gated calls on its request's goroutine ran: plain %v, background %v; want %v", who.login, plain, background, who.admin)
		}
	}
	auth.SetStrictGates(true)
	if err := app.RunCron(context.Background(), "clear"); err != nil {
		t.Errorf("a cron calling a gated method: %v", err)
	}
}

// counter is a live page whose events have no gate of their own.
type counter struct {
	ui.Live
	n int
}

func (c *counter) Inc() { c.n++ }
func (c *counter) Render() ui.Node {
	return ui.El("main", nil, ui.El("output", nil, ui.Text(fmt.Sprint("count=", c.n))),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(c.Inc)}}, ui.Text("+")),
		ui.Component("counter.notice", 0, &notice{}))
}

// On a public page, the page's own events are public; a component's
// events that no gate marks need a sign-in.
func TestPublicPageEvents(t *testing.T) {
	app := newApp(t, users(), nil)
	app.Live("/counter", &counter{}, web.Attrs(auth.Public()))
	c := serve(t, app)
	ws, m := c.socket("/counter", c.srv.URL)
	if r := click(t, ws, handler(t, m.HTML, 0), 2); r.Error != "" || !strings.Contains(patched(r), "count=1") {
		t.Errorf("the public page's own event, anonymous: %+v", r)
	}
	if r := click(t, ws, handler(t, m.HTML, 1), 3); r.Error != "unauthorized" || strings.Contains(patched(r), "bumps=1") {
		t.Errorf("an unmarked component event on a public page, anonymous: %+v", r)
	}
	c.login("bob@example.com", "hunter22", "")
	ws, m = c.socket("/counter", c.srv.URL)
	if r := click(t, ws, handler(t, m.HTML, 1), 2); r.Error != "" || !strings.Contains(patched(r), "bumps=1") {
		t.Errorf("the component's event, signed in: %+v", r)
	}
}

// feed is a live page receiving broadcasts.
type feed struct {
	ui.Live
	news int
}

func (f *feed) LiveSchema(l *ui.LiveSchema)   { l.Mount(f.mount) }
func (f *feed) mount(_ context.Context) error { f.Subscribe("news"); return nil }
func (f *feed) Update(n int)                  { f.news = n }
func (f *feed) Render() ui.Node               { return ui.El("p", nil, ui.Text(fmt.Sprint("news=", f.news))) }

// A live page whose sign-in ended closes at the next broadcast, without a
// message of its own; one whose perms changed closes once rechecked.
func TestLivePagesFollowTheSignIn(t *testing.T) {
	u := users()
	app := newApp(t, u, func(c *auth.Config) { c.LiveRecheck = time.Nanosecond })
	app.Live("/feed", &feed{})
	c := serve(t, app)
	c.login("bob@example.com", "hunter22", "")
	ws, _ := c.socket("/feed", c.srv.URL)
	app.Broadcast(context.Background(), "news", 1)
	if m := must(t, ws); !strings.Contains(patched(m), "news=1") {
		t.Fatalf("a broadcast: %+v", m)
	}
	a, _ := web.Resolve[*auth.Auth](app)
	a.RevokeUser(context.Background(), "2")
	if m, err := recv(ws); websocket.CloseStatus(err) != 4001 {
		t.Errorf("after RevokeUser: %+v %v", m, err)
	}

	// Perms changed in Users: the periodic check loads the identity again.
	u = users()
	app = newApp(t, u, func(c *auth.Config) { c.LiveRecheck = time.Nanosecond }, web.LiveCheckEvery(50*time.Millisecond))
	c = serve(t, app)
	c.login("bob@example.com", "hunter22", "")
	ws, _ = c.socket("/board", c.srv.URL)
	u.Add("bob@example.com", "hunter22", auth.Identity{ID: "2"}) // orders.read taken away
	if _, err := recv(ws); websocket.CloseStatus(err) != 4001 {
		t.Errorf("a page whose perms changed: %v", err)
	}
}

// peeker renders by calling a gated method without a context.
type peeker struct {
	ui.Live
	n int
}

func (p *peeker) Inc() { p.n++ }
func (p *peeker) Render() ui.Node {
	before := gateRuns.Load()
	(&vault{}).Clear()
	return ui.El("main", nil, ui.Text(fmt.Sprintf("n=%d peeked=%v", p.n, gateRuns.Load() > before)),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Inc)}}))
}

// A gated call in a live page's render, after an event, is checked against
// the page's identity: neither refused for everyone nor run for everyone.
func TestGatedCallsInLiveRenders(t *testing.T) {
	app := newApp(t, users(), nil)
	app.Live("/peek", &peeker{})
	for _, who := range []struct {
		login, pw string
		want      string
	}{{"ada@example.com", "correct horse", "peeked=true"}, {"bob@example.com", "hunter22", "peeked=false"}} {
		c := serve(t, app)
		c.login(who.login, who.pw, "")
		if r := c.page("/peek"); !strings.Contains(r.body, who.want) {
			t.Errorf("%s, the page: %s", who.login, r.body)
		}
		ws, m := c.socket("/peek", c.srv.URL)
		if r := click(t, ws, handler(t, m.HTML, 0), 2); !strings.Contains(patched(r), "n=1 "+who.want) {
			t.Errorf("%s, rendered after an event: %+v", who.login, r)
		}
	}
}

// A navigation over a live connection runs the new URL's route, gates and
// all: a page the visitor may not see is never mounted, patch flag or not,
// and a refused event's error is the gate's public text.
func TestLiveNavigationChecksGates(t *testing.T) {
	c := serve(t, newApp(t, users(), nil))
	c.login("bob@example.com", "hunter22", "")
	ws, m := c.socket("/who", c.srv.URL)
	if !strings.Contains(m.HTML, "who=2") {
		t.Fatalf("join: %+v", m)
	}
	for i, patch := range []bool{true, false} {
		send(t, ws, map[string]any{"type": "navigate", "ref": 2 + i, "url": "/staff", "patch": patch})
		r := must(t, ws)
		if r.Type == live.Render || r.Type == live.PatchMsg || strings.Contains(r.HTML+patched(r), "who=") {
			t.Errorf("navigating to a page bob may not see (patch %v): %+v", patch, r)
		}
	}
	send(t, ws, map[string]any{"type": "navigate", "ref": 4, "url": "/board", "patch": true})
	if r := must(t, ws); r.Error != "" || !strings.Contains(r.HTML+patched(r), "n=0") {
		t.Errorf("navigating to a page bob may see: %+v", r)
	}
}
