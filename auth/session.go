package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// Session is a signed-in browser, as a SessionStore keeps it. The cookie
// holds a random id; the store holds the SHA-256 of it, so reading the store
// gives nobody a cookie that works.
type Session struct {
	UserID  string
	Created time.Time
	Seen    time.Time // last used, within a minute
	Expires time.Time
}

// SessionStore keeps sessions by key, the hash of their cookie's id. Get
// returns nil and no error for an unknown key. DeleteUser deletes every
// session of a user.
type SessionStore interface {
	Get(ctx context.Context, key string) (*Session, error)
	Save(ctx context.Context, key string, s Session) error
	Delete(ctx context.Context, key string) error
	DeleteUser(ctx context.Context, uid string) error
}

// MemorySessions is the default SessionStore: a map in the process. Its
// sessions end when the process does — every user signs in again after a
// restart or a deploy — and replicas don't share them, so behind a load
// balancer a user is signed in on one replica only. Use it for development,
// tests and single-process apps that accept that; otherwise implement
// SessionStore over the app's database or cache.
type MemorySessions struct {
	mu    sync.Mutex
	m     map[string]Session
	saves int
}

// NewMemorySessions is an empty MemorySessions.
func NewMemorySessions() *MemorySessions { return &MemorySessions{m: map[string]Session{}} }

func (s *MemorySessions) Get(_ context.Context, key string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	if !ok {
		return nil, nil
	}
	return &v, nil
}

func (s *MemorySessions) Save(_ context.Context, key string, v Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = v
	if s.saves++; s.saves%1000 == 0 { // drop the expired now and then
		now := time.Now()
		for k, v := range s.m {
			if now.After(v.Expires) {
				delete(s.m, k)
			}
		}
	}
	return nil
}

func (s *MemorySessions) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

func (s *MemorySessions) DeleteUser(_ context.Context, uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if v.UserID == uid {
			delete(s.m, k)
		}
	}
	return nil
}

// newSecret is 32 random bytes, base64url: a session id, a token.
func newSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("auth: no randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// storeKey is what a store keeps for a secret: its SHA-256, hex.
func storeKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// wellFormed reports whether a cookie's or token's secret part could be
// newSecret's, before anything is looked up.
func wellFormed(s string) bool {
	if len(s) != 43 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (a *Auth) sessionLive(s *Session, now time.Time) bool {
	if now.After(s.Expires) {
		return false
	}
	return a.cfg.SessionIdle <= 0 || now.Sub(s.Seen) <= a.cfg.SessionIdle
}

// readSession signs the request in with its session cookie, when it has a
// live one.
func (a *Auth) readSession(ctx context.Context, st *state, r *http.Request) error {
	c, err := r.Cookie(a.cfg.Cookie)
	if err != nil || !wellFormed(c.Value) {
		return nil
	}
	key := storeKey(c.Value)
	s, err := a.sessions.Get(ctx, key)
	if err != nil {
		return err
	}
	now := time.Now()
	if s == nil || !a.sessionLive(s, now) {
		if s != nil {
			a.sessions.Delete(ctx, key)
		}
		return nil
	}
	id, err := a.load(ctx, s.UserID)
	if err != nil {
		return err
	}
	if id == nil { // the user is gone
		a.sessions.Delete(ctx, key)
		return nil
	}
	if now.Sub(s.Seen) > time.Minute {
		s.Seen = now
		if err := a.sessions.Save(ctx, key, *s); err != nil {
			return err
		}
	}
	st.id, st.via, st.key = id, viaSession, key
	return nil
}

// errNoResponse is SignIn's and SignOut's error over a live connection,
// whose response is long gone.
var errNoResponse = errors.New("auth: signing in or out sets a cookie, which needs an HTTP request's response: a form post, not a live event")

// SignIn signs ctx's browser in as the identity whose id is uid: a new
// session, under a new cookie id — any session the browser had is ended
// first, so an id planted before the sign-in is worthless after it. Call it
// from a handler of an HTTP request (a form post), before writing the
// response. Login checks a password first; SignIn is for flows that proved
// who the user is some other way.
func SignIn(ctx context.Context, uid string) error {
	st := stateOf(ctx)
	if st == nil || st.auth == nil {
		return errNoAuth
	}
	return st.auth.signIn(ctx, st, uid)
}

func (a *Auth) signIn(ctx context.Context, st *state, uid string) error {
	if st.w == nil || st.r == nil || isLive(st.r) {
		return errNoResponse
	}
	id, err := a.load(ctx, uid)
	if err != nil {
		return err
	}
	if id == nil {
		return errors.New("auth: SignIn: no user " + uid)
	}
	if c, err := st.r.Cookie(a.cfg.Cookie); err == nil && wellFormed(c.Value) {
		a.sessions.Delete(ctx, storeKey(c.Value))
	}
	if st.via == viaSession && st.key != "" {
		a.sessions.Delete(ctx, st.key)
	}
	secret, now := newSecret(), time.Now()
	key := storeKey(secret)
	if err := a.sessions.Save(ctx, key, Session{UserID: uid, Created: now, Seen: now, Expires: now.Add(a.cfg.SessionTTL)}); err != nil {
		return err
	}
	a.setCookie(st, secret, a.cfg.SessionTTL)
	st.id, st.via, st.key = id, viaSession, key
	return nil
}

// SignOut ends ctx's sign-in: the session is deleted and its cookie
// expired, or the bearer token the request carries is revoked.
func SignOut(ctx context.Context) error {
	st := stateOf(ctx)
	if st == nil || st.auth == nil {
		return errNoAuth
	}
	a := st.auth
	var err error
	switch st.via {
	case viaSession:
		if isLive(st.r) {
			return errNoResponse
		}
		err = a.sessions.Delete(ctx, st.key)
		a.setCookie(st, "", -1)
	case viaBearer:
		err = a.tokens.Delete(ctx, st.key)
	}
	st.id, st.via, st.key = nil, "", ""
	return err
}

// isLive reports whether r is a live connection's run of a route, which
// has no response to set a cookie on.
func isLive(r *http.Request) bool { return r != nil && r.Header.Get("X-Vuka-Live") != "" }

func (a *Auth) setCookie(st *state, value string, ttl time.Duration) {
	c := &http.Cookie{
		Name:     a.cfg.Cookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.cfg.SecureCookie || https(st.r),
	}
	if ttl < 0 {
		c.MaxAge, c.Expires = -1, time.Unix(1, 0)
	} else {
		c.MaxAge = int(ttl / time.Second)
	}
	st.w.Header().Add("Set-Cookie", c.String())
	st.w.Header().Add("Cache-Control", `no-store`)
}

func https(r *http.Request) bool {
	return r != nil && (r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https")
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
