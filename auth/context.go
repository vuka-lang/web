package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vuka-lang/web/v2"
)

// Perms are what an identity may do, as templates read them: perms.Has.
// They never reach the browser (web.NotForBrowser).
type Perms []string

// Has reports whether the perms grant perm, directly or by a wildcard:
// "orders.*" grants everything under orders, "*" everything.
func (p Perms) Has(perm string) bool {
	for _, g := range p {
		if permMatch(g, perm) {
			return true
		}
	}
	return false
}

// NotForBrowser keeps the perms off the browser (web.Browser).
func (Perms) NotForBrowser() {}

// Vars is Context's result: the template variables user, perms and actor.
type Vars struct {
	// User is who the request is signed in as, nil for an anonymous
	// visitor: while impersonating, the user being viewed as.
	User *Identity
	// Perms are User's: perms.Has("orders.write").
	Perms Perms
	// Actor is who is really signed in while impersonating
	// (Impersonate), for a "viewing as" banner; nil otherwise.
	Actor *Identity
}

// Context is a context processor (web.ContextProcessors) giving templates
// the variables user, perms and actor. Its inputs are the signed-in
// identity and its permission version, which auth moves when the user's
// perms or roles change (Refresh), on sign-out, impersonation and
// RevokeUser: a live page's components showing them render again at its
// next event, a layout that stays among them.
func Context(ctx context.Context) (Vars, error) {
	id := Current(ctx)
	v := Vars{User: id, Actor: Actor(ctx)}
	if id != nil {
		v.Perms = Perms(id.Perms)
	}
	return v, nil
}

// versions are the users' permission versions.
type versions struct{ m sync.Map } // uid → *atomic.Uint64

func (v *versions) of(uid string) uint64 {
	if c, ok := v.m.Load(uid); ok {
		return c.(*atomic.Uint64).Load()
	}
	return 0
}

func (v *versions) bump(uid string) {
	c, _ := v.m.LoadOrStore(uid, new(atomic.Uint64))
	c.(*atomic.Uint64).Add(1)
}

// Version is user uid's permission version: it moves when auth hears that
// what the user may do, or who they are signed in as, changed — Refresh
// (call it after changing their perms, roles or record), SignOut,
// Impersonate, StopImpersonating, RevokeUser. A live page's identity is
// loaded again (Users.Load) the first time it is read after it moved.
func (a *Auth) Version(uid string) uint64 { return a.versions.of(uid) }

// fresh is an identity loaded again because its user's version moved.
type fresh struct {
	ver uint64
	id  *Identity
}

// current is the identity of the state, loaded again (once per version)
// when its user's permission version moved since it was.
func (st *state) current(ctx context.Context) *Identity {
	id := st.id
	if id == nil || st.auth == nil || st.via == viaContext {
		return id
	}
	a := st.auth
	ver := a.versions.of(id.ID)
	if ver == st.ver {
		return id
	}
	if f := st.fresh.Load(); f != nil && f.ver == ver {
		return f.id
	}
	got, err := a.loadFresh(context.WithoutCancel(ctx), id.ID)
	if err != nil {
		return id // the next read tries again
	}
	st.fresh.Store(&fresh{ver, got})
	return got
}

// authInput is the input auth gives a context processor reading the
// identity (web.RecordInput): who is signed in, how, and their version.
func authInput(ctx context.Context) string {
	st := stateOf(ctx)
	if st == nil || st.id == nil {
		return ""
	}
	key := signedIn(ctx)
	if st.auth != nil {
		key += "\x00" + strconv.FormatUint(st.auth.versions.of(st.id.ID), 10)
	}
	if st.actor != nil {
		key += "\x00as " + st.actor.ID
	}
	return key
}

func recordAuth(ctx context.Context) {
	web.RecordInput(ctx, web.Input{Key: "auth", Now: authInput, Personal: true})
}

// guardVars decides the reads of a processor wrapped in web.Auth: its
// gates, as a route's, against the reading context's identity. Refused for
// a visitor who isn't signed in, a page is sent to the login page.
func (a *Auth) guardVars(ctx context.Context, attrs []any) error {
	gs, _ := gatesOf(attrs)
	id := Current(ctx)
	for _, g := range gs {
		err := g.allow(id)
		if err == nil {
			continue
		}
		if errors.Is(err, web.Unauthorized) {
			if r := web.Request(ctx); r != nil && r.Header.Get("X-Vuka-Live") == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				return web.Redirect(a.loginURL(r.URL.RequestURI()))
			}
		}
		return err
	}
	return nil
}

// Actor is who is really signed in while ctx's request views the app as
// another user (Impersonate); nil otherwise.
func Actor(ctx context.Context) *Identity {
	if st := stateOf(ctx); st != nil {
		recordAuth(ctx)
		return st.actor
	}
	return nil
}

// Impersonate signs ctx's browser in as user uid, viewing the app as they
// would, while remembering who is really signed in: Actor returns them, and
// StopImpersonating signs back in as them. The signed-in identity needs
// Config.ImpersonatePerm ("auth.impersonate" by default), and isn't
// impersonating already. Like SignIn it needs an HTTP request's response
// (a form post), and makes a new session.
func Impersonate(ctx context.Context, uid string) error {
	st := stateOf(ctx)
	if st == nil || st.auth == nil {
		return errNoAuth
	}
	a := st.auth
	me := st.current(ctx)
	switch {
	case me == nil:
		return web.Unauthorized
	case st.actor != nil:
		return &web.Error{Status: http.StatusConflict, Message: "auth: already impersonating; stop first"}
	case !me.Can(a.cfg.ImpersonatePerm):
		return web.Forbidden
	case st.via != viaSession:
		return errors.New("auth: Impersonate needs a cookie session")
	}
	target, err := a.loadFresh(ctx, uid)
	if err != nil {
		return err
	}
	if target == nil {
		return errors.New("auth: Impersonate: no user " + uid)
	}
	if err := a.startSession(ctx, st, Session{UserID: uid, Actor: me.ID}); err != nil {
		return err
	}
	st.id, st.actor, st.ver = target, me, a.versions.of(uid)
	a.versions.bump(me.ID)
	a.versions.bump(uid)
	return nil
}

// StopImpersonating signs ctx's browser back in as who Impersonate found
// signed in. It does nothing when not impersonating.
func StopImpersonating(ctx context.Context) error {
	st := stateOf(ctx)
	if st == nil || st.auth == nil {
		return errNoAuth
	}
	if st.actor == nil {
		return nil
	}
	a := st.auth
	me, viewed := st.actor, st.id
	id, err := a.loadFresh(ctx, me.ID)
	if err != nil {
		return err
	}
	if id == nil {
		return SignOut(ctx)
	}
	if err := a.startSession(ctx, st, Session{UserID: me.ID}); err != nil {
		return err
	}
	st.id, st.actor, st.ver = id, nil, a.versions.of(me.ID)
	a.versions.bump(me.ID)
	if viewed != nil {
		a.versions.bump(viewed.ID)
	}
	return nil
}

// startSession replaces the browser's session with a new one for s: the
// old one ends, and its live pages close.
func (a *Auth) startSession(ctx context.Context, st *state, s Session) error {
	if st.w == nil || st.r == nil || isLive(st.r) {
		return errNoResponse
	}
	if st.key != "" {
		a.sessions.Delete(ctx, st.key)
		a.closeKey(viaSession, st.key)
	}
	secret, now := newSecret(), time.Now()
	key := storeKey(secret)
	ttl, cookieTTL := a.cfg.SessionTTL, a.cfg.SessionTTL
	if st.brief {
		ttl, cookieTTL = min(ttl, briefSession), 0
	}
	s.Created, s.Seen, s.Expires = now, now, now.Add(ttl)
	if err := a.sessions.Save(ctx, key, s); err != nil {
		return err
	}
	a.setCookie(st, secret, cookieTTL)
	st.via, st.key = viaSession, key
	return nil
}
