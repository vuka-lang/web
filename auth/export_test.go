package auth

import (
	"context"

	"github.com/vuka-lang/web"
)

// SetStrictGates sets what an app built with Config.LaxGates (on false)
// or Config.StrictGates sets, for tests that must undo it.
func SetStrictGates(on bool) { laxGates.Store(!on); pinStrict.Store(false) }

// GuardEvent is the guard Module installs (web.GuardLiveEvents).
func GuardEvent(a *Auth, ctx context.Context, h web.LiveHandler) error { return a.guardEvent(ctx, h) }

// ThrottleKey is the key a login's failures are counted under.
func ThrottleKey(login string) string { return loginKey(login) }

// ThrottleKeys is how many keys a's throttle counts; SetThrottleMax bounds them.
func ThrottleKeys(a *Auth) int {
	a.throttle.mu.Lock()
	defer a.throttle.mu.Unlock()
	return len(a.throttle.fails)
}

func SetThrottleMax(a *Auth, n int) { a.throttle.mu.Lock(); a.throttle.max = n; a.throttle.mu.Unlock() }
