package auth

import (
	"context"
	"sync"
	"time"
)

// userCacheMax bounds the identities a cache holds; past it, expired ones
// are dropped, and all of them if none had expired.
const userCacheMax = 10000

// userCache keeps the identities Users.Load returned for Config.UserCache.
type userCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]cachedUser
	gen uint64 // bumped by every drop: a load begun before one isn't kept
}

type cachedUser struct {
	id  *Identity
	exp time.Time
}

func (c *userCache) get(uid string) (*Identity, uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[uid]
	if ok && time.Now().Before(e.exp) {
		return e.id, c.gen, true
	}
	return nil, c.gen, false
}

// put keeps id unless a drop happened since gen was read.
func (c *userCache) put(uid string, id *Identity, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	now := time.Now()
	if len(c.m) >= userCacheMax {
		for k, e := range c.m {
			if !now.Before(e.exp) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= userCacheMax {
			clear(c.m)
		}
	}
	c.m[uid] = cachedUser{id: id, exp: now.Add(c.ttl)}
}

func (c *userCache) drop(uid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, uid)
	c.gen++
}

// load is the identity of a user id, its roles expanded, from the cache
// when Config.UserCache keeps one; nil when gone.
func (a *Auth) load(ctx context.Context, uid string) (*Identity, error) {
	if a.cache == nil {
		return a.loadFresh(ctx, uid)
	}
	if id, _, ok := a.cache.get(uid); ok {
		cp := *id
		return &cp, nil
	}
	return a.loadFresh(ctx, uid)
}

// loadFresh calls Users.Load, and keeps what it returns when the cache is
// on: the identity as it is now.
func (a *Auth) loadFresh(ctx context.Context, uid string) (*Identity, error) {
	u, err := a.usersOf()
	if err != nil {
		return nil, err
	}
	var gen uint64
	if a.cache != nil {
		_, gen, _ = a.cache.get(uid)
	}
	id, err := u.Load(ctx, uid)
	if err != nil || id == nil {
		if a.cache != nil {
			a.cache.drop(uid)
		}
		return nil, err
	}
	id = a.expand(id)
	if a.cache != nil {
		a.cache.put(uid, id, gen)
		cp := *id
		return &cp, nil
	}
	return id, nil
}

// Refresh forgets the identity Config.UserCache keeps for user uid, so the
// next request loads it with Users.Load: call it after changing a user's
// perms, roles or record. It only reaches this process's cache.
func (a *Auth) Refresh(uid string) {
	if a.cache != nil {
		a.cache.drop(uid)
	}
}

// Refresh forgets the cached identity of user uid, as Auth.Refresh does,
// for the app of ctx.
func Refresh(ctx context.Context, uid string) error {
	st := stateOf(ctx)
	if st == nil || st.auth == nil {
		return errNoAuth
	}
	st.auth.Refresh(uid)
	return nil
}
