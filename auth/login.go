package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/vuka-lang/web"
)

// ErrInvalidLogin is a login's error whether the login is unknown or the
// password wrong: the answer never says which.
var ErrInvalidLogin = &web.Error{Status: http.StatusUnauthorized, Message: "invalid login or password"}

// ThrottledError is a login's error once too many attempts failed.
type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string {
	m := int(math.Ceil(e.RetryAfter.Minutes()))
	if m <= 1 {
		return "too many failed logins; try again in a minute"
	}
	return fmt.Sprintf("too many failed logins; try again in %d minutes", m)
}

// Unwrap makes it a 429 to web.
func (e *ThrottledError) Unwrap() error {
	return &web.Error{Status: http.StatusTooManyRequests, Message: e.Error()}
}

// Login checks a login and password and signs ctx's browser in (SignIn):
// the identity, or ErrInvalidLogin — the same error, after the same work,
// for an unknown login and a wrong password — or a *ThrottledError, or what
// Users.CheckLogin refused with. A password stored with a stale hash is
// stored again with a fresh one when Users has SetPassword.
func Login(ctx context.Context, login, password string) (*Identity, error) {
	st := stateOf(ctx)
	if st == nil || st.auth == nil {
		return nil, errNoAuth
	}
	id, err := st.auth.Verify(ctx, st.r, login, password)
	if err != nil {
		return nil, err
	}
	if err := st.auth.signIn(ctx, st, id.ID); err != nil {
		return nil, err
	}
	return st.id, nil
}

// Verify checks a login and password, as Login does, without signing in:
// the token endpoint's first step. r, when not nil, is the request the
// attempt came with, whose address throttling counts.
func (a *Auth) Verify(ctx context.Context, r *http.Request, login, password string) (*Identity, error) {
	users, err := a.usersOf()
	if err != nil {
		return nil, err
	}
	ip, now := "", time.Now()
	if r != nil {
		ip = a.cfg.ClientIP(r)
	}
	if d := a.throttle.wait(login, ip, now); d > 0 {
		return nil, &ThrottledError{RetryAfter: d}
	}
	id, encoded, err := (*Identity)(nil), "", error(nil)
	if login != "" && len(login) <= 256 {
		if id, encoded, err = users.FindLogin(ctx, login); err != nil {
			return nil, err
		}
	}
	if id == nil || encoded == "" {
		dummy, err := a.dummy()
		if err != nil {
			return nil, err
		}
		CheckPassword(dummy, password, a.cfg.Passwords) // the same work as a real check
		a.throttle.fail(login, ip, now)
		return nil, ErrInvalidLogin
	}
	ok, stale, err := CheckPassword(encoded, password, a.cfg.Passwords)
	if err != nil {
		log.Printf("auth: the stored password of user %s: %v", id.ID, err)
	}
	if !ok {
		a.throttle.fail(login, ip, now)
		return nil, ErrInvalidLogin
	}
	if c, ok := users.(loginChecker); ok {
		if err := c.CheckLogin(ctx, id); err != nil {
			var we *web.Error
			if errors.As(err, &we) {
				return nil, err
			}
			return nil, ErrInvalidLogin
		}
	}
	a.throttle.succeed(login)
	if s, ok := users.(passwordSetter); ok && stale {
		if fresh, err := a.cfg.Passwords.Hash(password); err == nil {
			if err := s.SetPassword(ctx, id.ID, fresh); err != nil {
				log.Printf("auth: storing user %s's re-hashed password: %v", id.ID, err)
			}
		}
	}
	return a.expand(id), nil
}

// SetPassword hashes password and stores it for user uid with
// Users.SetPassword. Sessions and tokens the user has stay; RevokeUser
// ends them.
func SetPassword(ctx context.Context, uid, password string) error {
	st := stateOf(ctx)
	if st == nil || st.auth == nil {
		return errNoAuth
	}
	return st.auth.SetPassword(ctx, uid, password)
}

// SetPassword hashes password and stores it for user uid.
func (a *Auth) SetPassword(ctx context.Context, uid, password string) error {
	users, err := a.usersOf()
	if err != nil {
		return err
	}
	s, ok := users.(passwordSetter)
	if !ok {
		return errors.New("auth: the app's Users has no SetPassword method")
	}
	h, err := a.cfg.Passwords.Hash(password)
	if err != nil {
		return err
	}
	return s.SetPassword(ctx, uid, h)
}
