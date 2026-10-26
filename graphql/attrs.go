package graphql

import (
	"reflect"
	"slices"
	"sync"

	"github.com/vuka-lang/vuka/v2"
)

// The attributes a schema's declarations carry. Written in .vuka files
// (@graphql.Query above a function), they are recorded at init with the
// declaration (vuka.RegisterFunc, RegisterType, RegisterFields); Go code
// registers them the same way.

// Query makes a function an operation of the schema's Query type, named
// after the function in lower camel case (Users is users), or as given:
// @graphql.Query("usersList").
//
//	@graphql.Query
//	func Users(ctx context.Context, first int, db *DB) ([]User, error) { … }
type Query string

// Mutation makes a function an operation of the schema's Mutation type,
// named as Query names it. Mutations of one request run one after the
// other, in the order the document writes them.
type Mutation string

// Subscription is reserved for subscription operations, which a later
// version serves; the compiler refuses it for now.
type Subscription string

// Type names the GraphQL type a Go type is, in place of its Go name:
// @graphql.Type("Person") on type User. On a type an input uses, it names
// the input type too unless Input does.
type Type string

// Input names the GraphQL input type a struct is when an argument takes it,
// in place of its Go name: @graphql.Input("CreateUserInput").
type Input string

// Field names the field a method is, in place of its name in lower camel
// case: @graphql.Field("displayName") on FullName. A struct field takes a
// graphql:"name" tag instead.
type Field string

// Value names the enum value a constant is, in place of its name in
// SCREAMING_SNAKE case without its type's name: @graphql.Value("ENABLED")
// on StatusActive.
type Value string

// Deprecated marks a field, an argument's operation or an enum value
// deprecated, with the reason introspection and the schema's SDL show:
// @graphql.Deprecated("use displayName").
type Deprecated string

// ArgName names an argument: the attribute Arg gives.
type ArgName struct{ Param, Name string }

// Arg names the argument a function's or method's parameter is, in place of
// the parameter's name: @graphql.Arg("after", "cursor").
func Arg(param, name string) vuka.Bundle { return vuka.Compose(ArgName{param, name}) }

// Where each attribute goes (@vuka.Targets): anywhere else is a compile
// error at the attribute.
func (Query) vukaTargets() [vuka.OnFunc]struct{}        { return [vuka.OnFunc]struct{}{} }
func (Mutation) vukaTargets() [vuka.OnFunc]struct{}     { return [vuka.OnFunc]struct{}{} }
func (Subscription) vukaTargets() [vuka.OnFunc]struct{} { return [vuka.OnFunc]struct{}{} }
func (Type) vukaTargets() [vuka.OnType]struct{}         { return [vuka.OnType]struct{}{} }
func (Input) vukaTargets() [vuka.OnType]struct{}        { return [vuka.OnType]struct{}{} }
func (Field) vukaTargets() [vuka.OnMethod]struct{}      { return [vuka.OnMethod]struct{}{} }
func (Value) vukaTargets() [vuka.OnVar]struct{}         { return [vuka.OnVar]struct{}{} }
func (Deprecated) vukaTargets() [vuka.OnFunc | vuka.OnMethod | vuka.OnField | vuka.OnVar]struct{} {
	return [vuka.OnFunc | vuka.OnMethod | vuka.OnField | vuka.OnVar]struct{}{}
}

// enum values' attributes, by type and value (RegisterValue)
var valueAttrs sync.Map // valueKey → []any

type valueKey struct {
	t reflect.Type
	v any
}

// RegisterValue records the attributes written on an enum constant —
// @graphql.Value, @graphql.Deprecated — which Go's registry of declarations
// has no place for. The compiler generates the call for a constant carrying
// any; Go code may call it too.
func RegisterValue[T comparable](v T, attrs ...any) {
	valueMu.Lock()
	defer valueMu.Unlock()
	k := valueKey{reflect.TypeFor[T](), v}
	prev, _ := valueAttrs.Load(k)
	old, _ := prev.([]any)
	valueAttrs.Store(k, append(slices.Clip(old), attrs...))
}

var valueMu sync.Mutex

func attrsOfValue(t reflect.Type, v any) []any {
	a, _ := valueAttrs.Load(valueKey{t, v})
	attrs, _ := a.([]any)
	return attrs
}

// attr finds the attribute of type A among attrs.
func attr[A any](attrs []any) (A, bool) {
	for _, x := range attrs {
		if a, ok := x.(A); ok {
			return a, true
		}
	}
	var zero A
	return zero, false
}
