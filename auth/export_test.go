package auth

import (
	"context"

	"github.com/vuka-lang/web"
)

// SetStrictGates sets what an app built with Config.StrictGates sets, for
// tests that must undo it.
func SetStrictGates(on bool) { strictGates.Store(on) }

// GuardEvent is the guard Module installs (web.GuardLiveEvents).
func GuardEvent(a *Auth, ctx context.Context, h web.LiveHandler) error { return a.guardEvent(ctx, h) }
