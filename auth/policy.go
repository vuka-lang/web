package auth

import (
	"context"
	"reflect"

	"github.com/vuka-lang/web"
)

// Policy decides Check for one type of object: may this identity do perm
// to this order?
type Policy struct {
	typ  reflect.Type
	rule func(ctx context.Context, id *Identity, perm string, obj any) bool
}

// PolicyFor is the policy of objects of type T: rule decides alone, so it
// grants what the perms don't (an owner editing their own order) and
// refuses what they grant, reading id.Can itself where perms matter. id is
// never nil: anonymous callers are refused before rule runs.
//
//	auth.PolicyFor(func(ctx context.Context, id *auth.Identity, perm string, o *Order) bool {
//		return id.Can(perm) || o.Owner == id.ID
//	})
func PolicyFor[T any](rule func(ctx context.Context, id *Identity, perm string, obj T) bool) Policy {
	return Policy{typ: reflect.TypeFor[T](), rule: func(ctx context.Context, id *Identity, perm string, obj any) bool {
		return rule(ctx, id, perm, obj.(T))
	}}
}

// Check is nil when ctx's identity may do perm to obj: by the policy of
// obj's type (Config.Policies), else by its perms. Otherwise it is
// web.Unauthorized for an anonymous caller and web.Forbidden for one
// signed in, which a handler returns as is.
func Check(ctx context.Context, perm string, obj any) error {
	st := stateOf(ctx)
	if st == nil || st.id == nil {
		return web.Unauthorized
	}
	if st.auth != nil && obj != nil {
		if p, ok := st.auth.policies[reflect.TypeOf(obj)]; ok {
			if p.rule(ctx, st.id, perm, obj) {
				return nil
			}
			return web.Forbidden
		}
	}
	if st.id.Can(perm) {
		return nil
	}
	return web.Forbidden
}

// Allowed reports whether Check passes.
func Allowed(ctx context.Context, perm string, obj any) bool { return Check(ctx, perm, obj) == nil }
