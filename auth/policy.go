package auth

import (
	"context"
	"reflect"
	"sync"

	"github.com/vuka-lang/web/v2"
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
// obj's type (Config.Policies) — a policy for Order decides for an Order
// and a *Order alike, and one for *Order for both too — else by its perms.
// Otherwise it is web.Unauthorized for an anonymous caller and
// web.Forbidden for one signed in, which a handler returns as is.
//
// A context marked auth.System passes. One without the app's auth (an
// auth.WithIdentity of a context from outside any request) can't ask the
// app's policies: Check refuses it any object of a type a policy covers,
// rather than decide by the perms alone.
func Check(ctx context.Context, perm string, obj any) error {
	if isSystem(ctx) {
		return nil
	}
	st := stateOf(ctx)
	if st == nil || st.id == nil {
		return web.Unauthorized
	}
	if obj != nil {
		if st.auth == nil {
			if policyTypes.covers(reflect.TypeOf(obj)) {
				return web.Forbidden
			}
		} else if p, v, ok := st.auth.policyOf(obj); ok {
			if p.rule(ctx, st.id, perm, v) {
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

// policyOf is the policy of obj's type, and obj as the policy takes it: a
// value for a policy of T given a *T (nil: no policy applies), a pointer to
// a copy for a policy of *T given a T.
func (a *Auth) policyOf(obj any) (Policy, any, bool) {
	t := reflect.TypeOf(obj)
	if p, ok := a.policies[t]; ok {
		return p, obj, true
	}
	v := reflect.ValueOf(obj)
	if t.Kind() == reflect.Pointer {
		if p, ok := a.policies[t.Elem()]; ok {
			if v.IsNil() {
				return p, nil, false
			}
			return p, v.Elem().Interface(), true
		}
		return Policy{}, nil, false
	}
	if p, ok := a.policies[reflect.PointerTo(t)]; ok {
		ptr := reflect.New(t)
		ptr.Elem().Set(v)
		return p, ptr.Interface(), true
	}
	return Policy{}, nil, false
}

// policyTypes are the types of every app's policies, for a Check without
// an app to ask.
var policyTypes typeSet

type typeSet struct{ m sync.Map }

func (s *typeSet) add(t reflect.Type) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	s.m.Store(t, true)
}

func (s *typeSet) covers(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	_, ok := s.m.Load(t)
	return ok
}

// Allowed reports whether Check passes.
func Allowed(ctx context.Context, perm string, obj any) bool { return Check(ctx, perm, obj) == nil }
