// Package accounts is the app's users: who can sign in, and as whom.
package accounts

import (
	"context"
	"strings"
	"sync"

	"github.com/vuka-lang/web/v2/auth"
)

// Account is a user as the app keeps it. A real app reads its database.
type Account struct {
	ID       string
	Name     string
	Email    string
	Password string // encoded: auth.HashPassword's output, never the password
	Roles    []string
	Perms    []string
}

// Users is the app's side of auth: who can sign in, and as whom. Being a
// service implementing auth.Users, auth.Module finds it by itself.
type Users struct {
	mu       sync.Mutex
	accounts []*Account
}

func NewUsers() *Users {
	u := &Users{}
	u.add(Account{ID: "1", Name: "Ada", Email: "ada@example.com", Roles: []string{"admin"}}, "correct horse battery")
	u.add(Account{ID: "2", Name: "Bob", Email: "bob@example.com", Perms: []string{"notices.read", "notices.write"}}, "hunter2hunter2")
	return u
}

func (u *Users) add(a Account, password string) {
	enc, err := auth.HashPassword(password)
	if err != nil {
		panic(err)
	}
	a.Password = enc
	u.accounts = append(u.accounts, &a)
}

func (a *Account) identity() *auth.Identity {
	return &auth.Identity{ID: a.ID, Roles: a.Roles, Perms: a.Perms, User: a}
}

func (u *Users) FindLogin(ctx context.Context, login string) (*auth.Identity, string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, a := range u.accounts {
		if strings.EqualFold(a.Email, login) {
			return a.identity(), a.Password, nil
		}
	}
	return nil, "", nil
}

func (u *Users) Load(ctx context.Context, id string) (*auth.Identity, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, a := range u.accounts {
		if a.ID == id {
			return a.identity(), nil
		}
	}
	return nil, nil
}

// SetPassword stores a password auth hashed again (a stale hash, upgraded
// at login).
func (u *Users) SetPassword(ctx context.Context, id, encoded string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, a := range u.accounts {
		if a.ID == id {
			a.Password = encoded
		}
	}
	return nil
}

// Public is what GET /auth/me shows of a user: never the password.
func (u *Users) Public(id *auth.Identity) any {
	a := id.User.(*Account)
	return map[string]string{"name": a.Name, "email": a.Email}
}

// Name is who a page greets.
func Name(id *auth.Identity) string {
	if a, ok := id.User.(*Account); ok {
		return a.Name
	}
	return id.ID
}
