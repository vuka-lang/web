// Package authtest helps test apps using auth: requests and live sessions
// signed in as anyone, without a login, and Users kept in memory.
package authtest

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/auth"
)

// As signs every request of the app in as id — pages, APIs, live pages and
// their events — whatever credentials they carry:
//
//	app := web.New(auth.Module(cfg), authtest.As(&auth.Identity{ID: "7", Perms: []string{"orders.*"}}))
//
// A nil id makes every request anonymous. A handler that signs in
// (auth.Login, auth.SignIn — a sign-in form's Save) works as it does
// without As: the session cookie is set on the response, the request
// staying signed in as id. Outside a test binary it panics: an app shipped
// with it would let everyone in.
func As(id *auth.Identity) web.Option {
	if !testing.Testing() {
		panic("authtest: As outside a test binary")
	}
	return web.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, Request(r, id))
		})
	})
}

// Request is r signed in as id, for an app's handler called directly
// (app.Handler().ServeHTTP with an httptest.ResponseRecorder).
func Request(r *http.Request, id *auth.Identity) *http.Request {
	return r.WithContext(auth.WithIdentity(r.Context(), id))
}

// Context is ctx signed in as id: for a live session a test builds itself
// (live.NewSession, livetest), whose Mount and event handlers then see id
// in auth.Current, and for services called directly.
func Context(ctx context.Context, id *auth.Identity) context.Context {
	return auth.WithIdentity(ctx, id)
}

// Users is an auth.Users kept in memory, with SetPassword.
type Users struct {
	mu    sync.Mutex
	byID  map[string]*user
	order []string
	cost  auth.Argon2
}

type user struct {
	id      auth.Identity
	login   string
	encoded string
}

// Cheap is a hashing cost for tests: fast, and far too weak for anything
// else. Users hashes with it; pass it as auth.Config.Passwords so logins
// don't re-hash.
var Cheap = auth.Argon2{Memory: 64, Time: 1, Threads: 1}

// NewUsers is an empty Users.
func NewUsers() *Users { return &Users{byID: map[string]*user{}, cost: Cheap} }

// Add adds a user who signs in with login and password; id.ID names it.
func (u *Users) Add(login, password string, id auth.Identity) *Users {
	enc, err := u.cost.Hash(password)
	if err != nil {
		panic(err)
	}
	return u.AddEncoded(login, enc, id)
}

// AddEncoded adds a user whose password is already encoded (a bcrypt hash
// from an older system, say).
func (u *Users) AddEncoded(login, encoded string, id auth.Identity) *Users {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.byID[id.ID]; !ok {
		u.order = append(u.order, id.ID)
	}
	u.byID[id.ID] = &user{id: id, login: login, encoded: encoded}
	return u
}

// Remove deletes a user: its sessions stop working at their next request.
func (u *Users) Remove(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.byID, id)
	u.order = slices.DeleteFunc(u.order, func(s string) bool { return s == id })
}

// Encoded is the stored password of user id, to see a re-hash happen.
func (u *Users) Encoded(id string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if x := u.byID[id]; x != nil {
		return x.encoded
	}
	return ""
}

func (u *Users) FindLogin(_ context.Context, login string) (*auth.Identity, string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, id := range u.order {
		if x := u.byID[id]; strings.EqualFold(x.login, login) {
			cp := clone(x.id)
			return &cp, x.encoded, nil
		}
	}
	return nil, "", nil
}

func (u *Users) Load(_ context.Context, id string) (*auth.Identity, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if x := u.byID[id]; x != nil {
		cp := clone(x.id)
		return &cp, nil
	}
	return nil, nil
}

func (u *Users) SetPassword(_ context.Context, id, encoded string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if x := u.byID[id]; x != nil {
		x.encoded = encoded
	}
	return nil
}

func clone(id auth.Identity) auth.Identity {
	id.Perms, id.Roles = slices.Clone(id.Perms), slices.Clone(id.Roles)
	return id
}
