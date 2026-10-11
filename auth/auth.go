// Package auth signs users in to a web app and guards its routes, live
// pages and live events.
//
// The app writes one interface, Users; auth does the rest: password
// hashing, cookie sessions for pages, bearer tokens for APIs, login
// throttling, revocation, and gates that refuse every route nobody marked
// public:
//
//	app := web.New(auth.Module(auth.Config{Users: NewUsers}))
//
//	@web.Get("/orders")
//	@auth.Requires("orders.read")
//	func Orders(me *auth.Identity, store *Store) Result[[]Order] { … }
//
//	@web.Get("/login")
//	@auth.Public
//	func Login(r *http.Request) ui.Node { return auth.LoginPage(auth.LoginPageProps{Request: r}) }
//
// See the README's Authentication section, and the docs' auth page.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vuka-lang/web"
)

// Identity is who a request comes from.
type Identity struct {
	ID string // the user's id, as Users.Load takes it
	// Perms are what the user may do: "orders.write", or a wildcard,
	// "orders.*" (everything under orders) or "*" (everything).
	Perms []string
	// Roles name sets of perms (Config.Roles), added to Perms when the
	// identity is loaded.
	Roles []string
	User  any // the app's own user record, for handlers; never sent anywhere by auth
}

// String is the identity as logs and the development toolbar show it: its
// id and roles, never its user record.
func (id Identity) String() string {
	if len(id.Roles) == 0 {
		return "Identity{ID: " + id.ID + "}"
	}
	return "Identity{ID: " + id.ID + ", Roles: " + strings.Join(id.Roles, ",") + "}"
}

// Can reports whether the identity has perm, directly or by a wildcard.
func (id *Identity) Can(perm string) bool {
	if id == nil {
		return false
	}
	for _, p := range id.Perms {
		if permMatch(p, perm) {
			return true
		}
	}
	return false
}

// permMatch reports whether the granted perm p covers perm: equal, "*", or
// "x.*" for perm under x.
func permMatch(p, perm string) bool {
	switch {
	case p == perm, p == "*":
		return true
	case strings.HasSuffix(p, ".*"):
		return strings.HasPrefix(perm, p[:len(p)-1])
	}
	return false
}

// Users is the app's side: where identities and their passwords live.
//
// FindLogin returns the identity whose login (a username, an email) is
// login, with its encoded password (HashPassword's output); nil and no error
// when there is none. Load returns the identity of a user id, as a session
// or token names it; nil and no error when the user is gone, which signs
// the session out.
//
// Two methods are optional: SetPassword(ctx, id, encoded string) error
// stores a password auth re-hashed (a login with a stale hash upgrades it)
// or SetPassword was given; CheckLogin(ctx, *Identity) error refuses a
// login whose password was right (an account disabled, unconfirmed): its
// error is shown as is when it is a *web.Error, else as the usual "invalid
// login or password". And Public(*Identity) any, when present, is what GET
// /auth/me shows of the user.
type Users interface {
	FindLogin(ctx context.Context, login string) (*Identity, string, error)
	Load(ctx context.Context, id string) (*Identity, error)
}

type passwordSetter interface {
	SetPassword(ctx context.Context, id, encoded string) error
}

type loginChecker interface {
	CheckLogin(ctx context.Context, id *Identity) error
}

type publicUser interface{ Public(id *Identity) any }

// Config configures Module. Every field is optional but Users, which may
// also come from the app's container.
type Config struct {
	// Users is the app's Users: a value, or a constructor the app's
	// container builds (func(deps…) Users). Nil: the one provided type
	// implementing Users, such as a @web.Service.
	Users any

	// PublicByDefault makes routes without a gate public, as they are
	// without auth: only those marked Required or Requires need a sign-in.
	// By default every route, live page and live event needs one unless
	// marked @auth.Public.
	PublicByDefault bool

	// LoginPath is where a page visit without a sign-in is sent, with
	// ?next=<the page>; "/login" by default. The app serves it (LoginPage
	// is ready to), and marks it @auth.Public.
	LoginPath string
	// Home is where a sign-in goes without a next; "/" by default.
	Home string
	// Endpoints mounts the login, logout, me and token endpoints under a
	// prefix ("/auth": POST /auth/login, POST /auth/logout, GET /auth/me,
	// POST /auth/token); none when empty.
	Endpoints string

	// Roles maps a role to its perms: Identity.Roles adds them.
	Roles map[string][]string

	// Sessions holds cookie sessions; Tokens, bearer tokens. Both are in
	// memory by default: lost on restart, and not shared between replicas
	// (see MemorySessions).
	Sessions SessionStore
	Tokens   TokenStore
	// SessionTTL is how long a session lasts from sign-in; 14 days by
	// default. SessionIdle, when set, ends a session not used for that
	// long. TokenTTL is how long a bearer token lasts; 30 days by default.
	SessionTTL, SessionIdle, TokenTTL time.Duration
	// RememberMe makes staying signed in the user's choice at the login
	// endpoint: a sign-in whose form has the remember field checked (or
	// whose JSON has "remember": true) keeps its session for SessionTTL in
	// a cookie that outlives the browser; any other gets a cookie that
	// ends with the browser session, and a session of a day at most.
	// LoginForm's Remember shows the checkbox. Off, every sign-in is
	// remembered.
	RememberMe bool
	// Cookie names the session cookie; "vk_session" by default.
	Cookie string
	// SecureCookie sets the cookie's Secure flag on every response, not
	// only on HTTPS requests (r.TLS, or X-Forwarded-Proto: https): set it
	// when TLS ends at a proxy that doesn't say so.
	SecureCookie bool

	// Passwords are the hashing parameters of new hashes; Argon2id's
	// defaults (Argon2Defaults) when zero.
	Passwords Argon2
	// Throttle limits failed logins; ThrottleDefaults when zero.
	Throttle Throttle
	// ClientIP is the address throttling counts a request against;
	// r.RemoteAddr's host by default. Behind a proxy, read the header it
	// sets (and only from it).
	ClientIP func(r *http.Request) string

	// JWT verifies bearer JWTs issued elsewhere; nil: none accepted.
	JWT *JWT

	// Policies decide Check for the types they name.
	Policies []Policy

	// TrustedOrigins may send cross-origin POSTs and the like (web.CSRF).
	TrustedOrigins []string

	// StrictGates refuses a gated method's call that nothing attributes to
	// an identity: no request's or live page's context, no live event
	// running it, no auth.WithIdentity — instead of running it as the app's
	// own work. Code outside requests (crons, jobs, main) then marks itself
	// with auth.System(ctx), and gated methods take a context.Context to be
	// given one; a gated method without one is let through only while a live
	// event's handler runs it. It holds for the whole process once an app
	// with it is built.
	StrictGates bool
}

// Auth is an app's authentication: Module provides it, for code without a
// request's context (jobs, the CLI) — revoking a user, issuing a token.
type Auth struct {
	cfg      Config
	users    Users
	usersErr error
	app      *web.App
	sessions SessionStore
	tokens   TokenStore
	throttle *throttler
	policies map[reflect.Type]Policy
	jwtEpoch epochs
	dummy    func() (string, error) // a hash to check unknown logins against
	problems []error                // found while wrapping routes, reported by check
}

// Module installs auth in an app: it reads each request's credentials,
// guards every route, live page and live event, binds *auth.Identity
// parameters, refuses cross-origin form posts (web.CSRF), closes the live
// connections of revoked users, and provides *auth.Auth.
func Module(cfg Config) web.Option {
	a := &Auth{cfg: cfg}
	a.defaults()
	return web.Options(
		web.Provide(a),
		web.CSRF(cfg.TrustedOrigins...),
		web.WrapRoutes(wrapperName, a.wrapRoute),
		web.ParamFrom(func(r *http.Request) (*Identity, error) { return Current(r.Context()), nil }),
		web.ParamFrom(func(r *http.Request) (Identity, error) {
			if id := Current(r.Context()); id != nil {
				return *id, nil
			}
			return Identity{}, web.Unauthorized
		}),
		web.LiveCheck(a.liveCheck),
		web.GuardLiveEvents(a.guardEvent),
		web.OptionFunc(func(app *web.App) {
			a.app = app
			if _, ok := cfg.Users.(Users); !ok && cfg.Users != nil {
				app.Provide(cfg.Users) // a constructor: check finds what it builds
			}
			app.Use(a.middleware)
			if cfg.Endpoints != "" {
				a.routeEndpoints(app, cfg.Endpoints)
			}
		}),
		web.Check(a.check),
	)
}

func (a *Auth) defaults() {
	c := &a.cfg
	if c.LoginPath == "" {
		c.LoginPath = "/login"
	}
	if c.Home == "" {
		c.Home = "/"
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = 14 * 24 * time.Hour
	}
	if c.TokenTTL <= 0 {
		c.TokenTTL = 30 * 24 * time.Hour
	}
	if c.Cookie == "" {
		c.Cookie = "vk_session"
	}
	if c.Passwords == (Argon2{}) {
		c.Passwords = Argon2Defaults
	}
	if c.Throttle == (Throttle{}) {
		c.Throttle = ThrottleDefaults
	}
	if c.ClientIP == nil {
		c.ClientIP = remoteIP
	}
	a.sessions = cmpStore(c.Sessions, SessionStore(NewMemorySessions()))
	a.tokens = cmpStore(c.Tokens, TokenStore(NewMemoryTokens()))
	a.throttle = newThrottler(c.Throttle)
	a.policies = map[reflect.Type]Policy{}
	for _, p := range c.Policies {
		a.policies[p.typ] = p
	}
	a.jwtEpoch = epochs{m: map[string]time.Time{}}
	a.dummy = sync.OnceValues(func() (string, error) { return c.Passwords.Hash("not anyone's password") })
}

func cmpStore[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// check finds Users when Build runs, and checks the configuration.
func (a *Auth) check(app *web.App) error {
	if a.cfg.StrictGates {
		strictGates.Store(true)
	}
	errs := a.problems
	if u, ok := a.cfg.Users.(Users); ok {
		a.users = u
	} else if u, err := web.Implementation[Users](app); err != nil {
		errs = append(errs, fmt.Errorf("auth: no Users: give auth.Config.Users, or declare a @web.Service implementing auth.Users (%v)", err))
	} else {
		a.users = u
	}
	if a.cfg.JWT != nil {
		if err := a.cfg.JWT.validate(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

// usersOf is the app's Users, failing the request when there is none.
func (a *Auth) usersOf() (Users, error) {
	if a.users == nil {
		return nil, errors.New("auth: the app has no Users")
	}
	return a.users, nil
}

// load is the identity of a user id, its roles expanded; nil when gone.
func (a *Auth) load(ctx context.Context, uid string) (*Identity, error) {
	u, err := a.usersOf()
	if err != nil {
		return nil, err
	}
	id, err := u.Load(ctx, uid)
	if err != nil || id == nil {
		return nil, err
	}
	return a.expand(id), nil
}

// expand adds the perms of the identity's roles.
func (a *Auth) expand(id *Identity) *Identity {
	if len(id.Roles) == 0 || len(a.cfg.Roles) == 0 {
		return id
	}
	cp := *id
	cp.Perms = slices.Clone(id.Perms)
	for _, r := range id.Roles {
		cp.Perms = append(cp.Perms, a.cfg.Roles[r]...)
	}
	return &cp
}

// credential kinds a request is signed in with.
const (
	viaSession = "session"
	viaBearer  = "bearer token"
	viaJWT     = "JWT"
	viaContext = "context" // WithIdentity: tests, background work
)

// state is auth's view of a request: who it comes from and how.
type state struct {
	auth *Auth
	id   *Identity
	via  string    // one of the via constants; "" for anonymous
	key  string    // the session's or token's store key (a hash)
	exp  time.Time // a JWT's expiry
	// brief makes a sign-in last the browser session (RememberMe, the
	// remember field unchecked).
	brief bool
	iat   time.Time // a JWT's issue time
	w     http.ResponseWriter
	r     *http.Request
}

type stateKey struct{}

func stateOf(ctx context.Context) *state {
	if ctx == nil {
		return nil
	}
	st, _ := ctx.Value(stateKey{}).(*state)
	return st
}

// Current is the identity of ctx's request — a handler's, a live page's
// Mount, a live event's — or nil for an anonymous one.
func Current(ctx context.Context) *Identity {
	if st := stateOf(ctx); st != nil {
		return st.id
	}
	return nil
}

// WithIdentity is ctx signed in as id, for tests and work outside a
// request (a job acting for a user): Current returns id, and gates check
// it. A request whose context carries an identity this way keeps it.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	var a *Auth
	if st := stateOf(ctx); st != nil {
		a = st.auth
	}
	if a != nil && id != nil {
		id = a.expand(id)
	}
	return context.WithValue(ctx, stateKey{}, &state{auth: a, id: id, via: viaContext})
}

// Can reports whether ctx's identity has perm.
func Can(ctx context.Context, perm string) bool { return Current(ctx).Can(perm) }

// errNoAuth is the error of a function needing auth on a context without it.
var errNoAuth = errors.New("auth: this context has no auth (install auth.Module, and pass the request's context)")

// middleware reads each request's credentials into its context: a bearer
// token or JWT (Authorization: Bearer), else the session cookie.
func (a *Auth) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if st := stateOf(r.Context()); st != nil && st.via == viaContext {
			if st.auth == nil {
				st2 := *st
				st2.auth, st2.w, st2.r = a, w, r
				if st2.id != nil {
					st2.id = a.expand(st2.id)
				}
				r = r.WithContext(context.WithValue(r.Context(), stateKey{}, &st2))
			}
			a.noteIdentity(r.Context())
			next.ServeHTTP(w, r)
			return
		}
		st := &state{auth: a, w: w, r: r}
		if err := a.authenticate(r.Context(), st, r); err != nil {
			if !errors.Is(err, errBadCredentials) {
				log.Printf("auth: %s %s: %v", r.Method, r.URL.Path, err)
			}
			a.app.WriteError(w, r, err, web.AcceptsHTML(r))
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), stateKey{}, st))
		a.noteIdentity(r.Context())
		next.ServeHTTP(w, r)
	})
}

// errBadCredentials answers a request whose bearer credential doesn't
// verify: a client that sent one wants to know, rather than be served as
// anonymous.
var errBadCredentials = &web.Error{Status: http.StatusUnauthorized, Message: "invalid or expired token"}

func (a *Auth) authenticate(ctx context.Context, st *state, r *http.Request) error {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		tok := strings.TrimSpace(h[7:])
		var err error
		if isJWT(tok) {
			err = a.verifyJWT(ctx, st, tok)
		} else {
			err = a.verifyBearer(ctx, st, tok)
		}
		if err != nil {
			return err
		}
		if st.id == nil {
			return errBadCredentials
		}
		return nil
	}
	return a.readSession(ctx, st, r)
}

func (a *Auth) noteIdentity(ctx context.Context) {
	st := stateOf(ctx)
	v := "anonymous"
	if st != nil && st.id != nil {
		v = st.id.ID + " (" + st.via + ")"
		if len(st.id.Roles) > 0 {
			v += ", roles " + strings.Join(st.id.Roles, ", ")
		}
	}
	web.NoteRequest(ctx, web.RequestNote{Section: "Auth", Name: "Identity", Value: v})
}

// liveCheck refuses a live connection's message once the credential its
// page joined with is gone: signed out, revoked, expired.
func (a *Auth) liveCheck(ctx context.Context) error {
	st := stateOf(ctx)
	if st == nil || st.id == nil {
		return nil
	}
	ok, err := a.stillValid(ctx, st)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("auth: the sign-in ended")
	}
	return nil
}

func (a *Auth) stillValid(ctx context.Context, st *state) (bool, error) {
	switch st.via {
	case viaSession:
		s, err := a.sessions.Get(ctx, st.key)
		if err != nil || s == nil {
			return false, err
		}
		return s.UserID == st.id.ID && a.sessionLive(s, time.Now()), nil
	case viaBearer:
		t, err := a.tokens.Get(ctx, st.key)
		if err != nil || t == nil {
			return false, err
		}
		return t.UserID == st.id.ID && time.Now().Before(t.Expires), nil
	case viaJWT:
		return time.Now().Before(st.exp) && !a.jwtEpoch.revoked(st.id.ID, st.iat), nil
	}
	return true, nil
}

// RevokeUser ends every sign-in of user id: its sessions and bearer tokens
// are deleted, JWTs issued before now are refused by this process, and its
// live pages close at their next message (and load again, signed out).
func (a *Auth) RevokeUser(ctx context.Context, uid string) error {
	a.jwtEpoch.revoke(uid, time.Now())
	return errors.Join(a.sessions.DeleteUser(ctx, uid), a.tokens.DeleteUser(ctx, uid))
}

// RevokeUser ends every sign-in of user id, as Auth.RevokeUser does, for
// the app of ctx.
func RevokeUser(ctx context.Context, uid string) error {
	st := stateOf(ctx)
	if st == nil || st.auth == nil {
		return errNoAuth
	}
	return st.auth.RevokeUser(ctx, uid)
}
