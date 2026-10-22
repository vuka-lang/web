package auth_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/web"
	"github.com/vuka-lang/web/auth"
	"github.com/vuka-lang/web/auth/authtest"
)

type countingUsers struct {
	*authtest.Users
	loads atomic.Int64
}

func (u *countingUsers) Load(ctx context.Context, id string) (*auth.Identity, error) {
	u.loads.Add(1)
	return u.Users.Load(ctx, id)
}

func cachingApp(t *testing.T, ttl time.Duration) (*client, *countingUsers) {
	t.Helper()
	u := &countingUsers{Users: users()}
	app := newApp(t, u.Users, func(c *auth.Config) { c.Users, c.UserCache = u, ttl })
	app.Get("/refresh", func(ctx context.Context) error { return auth.Refresh(ctx, "1") }, web.Attrs(auth.Public()))
	c := serve(t, app)
	c.login("ada@example.com", "correct horse", "")
	return c, u
}

func TestUserCacheOff(t *testing.T) {
	c, u := cachingApp(t, 0)
	if n := reads(&u.loads, func() { c.page("/private"); c.page("/private") }); n != 2 {
		t.Errorf("without a cache two pages loaded the user %d times", n)
	}
}

func TestUserCacheKeeps(t *testing.T) {
	c, u := cachingApp(t, time.Minute)
	if n := reads(&u.loads, func() {
		for range 3 {
			if r := c.page("/private"); r.status != http.StatusOK {
				t.Fatalf("page: %d", r.status)
			}
		}
	}); n != 0 {
		t.Errorf("after a sign-in, three pages loaded the user %d times", n)
	}
	if r := c.do("GET", "/orders", "", "Accept", "application/json"); r.status != http.StatusOK || strings.TrimSpace(r.body) != `"orders of 1"` {
		t.Errorf("a cached identity's perms: %d %s", r.status, r.body)
	}
}

func TestUserCacheRefresh(t *testing.T) {
	c, u := cachingApp(t, time.Minute)
	u.Remove("1")
	if r := c.page("/private"); r.status != http.StatusOK {
		t.Fatalf("a cached user removed: %d before Refresh", r.status)
	}
	c.page("/refresh")
	if n := reads(&u.loads, func() {
		if r := c.page("/private"); r.status == http.StatusOK {
			t.Error("a removed user still signed in after Refresh")
		}
	}); n != 1 {
		t.Errorf("after Refresh the page loaded the user %d times", n)
	}
}

func TestUserCacheExpires(t *testing.T) {
	c, u := cachingApp(t, 20*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if n := reads(&u.loads, func() { c.page("/private"); c.page("/private") }); n != 1 {
		t.Errorf("after the cache expired two pages loaded the user %d times", n)
	}
}
