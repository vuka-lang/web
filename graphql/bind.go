package graphql

import (
	"context"
	"encoding"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// This file is what the code the compiler generates for a package's
// operations is written with — Register, Package, Operation, Struct,
// FormInput, Consts and the type constructors — readable Go that resolves
// without reflection: each resolver calls its Go function or reads its
// field directly. Go code may write the same by hand.

// Package is a package's part of the schema: its operations and the Go
// types they reach. A type several packages reach is described by each,
// alike; the first description registered is used.
type Package struct {
	Path       string // the package's import path
	Operations []Operation
	Types      []Binding // Struct, FormInput and Consts values
}

// Operation is a query or mutation: a function of the package.
type Operation struct {
	Go       string // the function's name: the operation's is CamelCase of it unless @graphql.Query names it
	Mutation bool
	Func     any    // the function, whose attributes (vuka.FuncOf) name and gate it
	Doc      string // its doc comment: the field's description
	Params   []Param
	Type     TypeDef // its result's type
	// Resolve calls the function with its parameters' values.
	Resolve func(ctx context.Context, c *Call) (any, error)
}

// Param is a parameter of an operation's function or a field's method,
// the receiver excluded: the context (Type and Dep nil), an argument (Type
// its type), or a dependency the app provides (Dep its type).
type Param struct {
	Name string // the Go parameter's name: the argument's default name
	Type TypeDef
	Dep  reflect.Type
}

// Call is what a resolver is given: the value of each parameter of its
// function, by index (the context's is nil).
type Call struct{ args []any }

// Arg is the value of parameter i: an argument's, coerced to its Go type
// (absent when the request gives none), or a dependency's.
func (c *Call) Arg(i int) any { return c.args[i] }

// Get is parameter i of c as a T: an argument's value, the zero T when the
// request gave none or null, or a dependency.
func Get[T any](c *Call, i int) T { return As[T](c.args[i]) }

// GetPtr is parameter i of c, an argument of a pointer type *E: nil when
// the request gave none or null.
func GetPtr[E any](c *Call, i int) *E { return Ptr[E](c.args[i]) }

// As is v, a coerced input value or a resolver's, as a T: the zero T for
// nil; a *T's element.
func As[T any](v any) T {
	switch x := v.(type) {
	case T:
		return x
	case *T:
		if x != nil {
			return *x
		}
	}
	var zero T
	return zero
}

// Ptr is v as a *E: nil for nil, a pointer to a copy of an E.
func Ptr[E any](v any) *E {
	switch x := v.(type) {
	case nil:
		return nil
	case *E:
		return x
	case E:
		return &x
	}
	return nil
}

// Binding describes a Go type of the schema: Struct (an object), FormInput
// (an input object) or Consts (an enum).
type Binding interface{ binding() binding }

type bindingKind int

const (
	objectBinding bindingKind = iota
	inputBinding
	enumBinding
)

// binding is a Binding without its type parameter.
type binding struct {
	kind   bindingKind
	t      reflect.Type
	doc    string
	fields []member
	inputs []inputMember
	values []enumConst
	ser    func(v any) (string, int)
	norm   func(v any) any
	build  func() any
}

type member struct {
	goName, name, doc string
	typ               TypeDef
	params            []Param
	get               func(src any) any
	resolve           func(ctx context.Context, src any, c *Call) (any, error)
}

type inputMember struct {
	goName, name, doc string
	typ               TypeDef
	set               func(dst, v any)
}

type enumConst struct {
	goName, doc string
	value       any
}

// Struct is the object type a Go struct T is: its fields, from T's
// exported fields and methods.
type Struct[T any] struct {
	Doc    string
	Fields []Member[T]
}

// Member is a field of a Struct: a struct field (Get) or a method
// (Resolve, with its parameters).
type Member[T any] struct {
	Go   string // the Go field's or method's name
	Name string // a graphql:"name" tag's name; "" for the default
	Doc  string
	Type TypeDef
	// Get reads a struct field.
	Get func(src *T) any
	// Params and Resolve call a method.
	Params  []Param
	Resolve func(ctx context.Context, src *T, c *Call) (any, error)
}

func (s Struct[T]) binding() binding {
	b := binding{kind: objectBinding, t: reflect.TypeFor[T](), doc: s.Doc, norm: normalize[T]}
	for _, f := range s.Fields {
		m := member{goName: f.Go, name: f.Name, doc: f.Doc, typ: f.Type, params: f.Params}
		if get := f.Get; get != nil {
			m.get = func(src any) any { return get(src.(*T)) }
		}
		if res := f.Resolve; res != nil {
			m.resolve = func(ctx context.Context, src any, c *Call) (any, error) { return res(ctx, src.(*T), c) }
		}
		b.fields = append(b.fields, m)
	}
	return b
}

// normalize is a resolved value of an object type T as its fields take it:
// a *T; nil for a nil pointer.
func normalize[T any](v any) any {
	switch x := v.(type) {
	case *T:
		if x == nil {
			return nil
		}
		return x
	case T:
		return &x
	}
	return v
}

// FormInput is the input object type a Go struct T is when an argument
// takes it: its fields, from T's exported fields. An argument's value is
// checked as a form is before the operation runs (ui.CheckForm): validate:
// tags, Clean<Field>, Clean.
type FormInput[T any] struct {
	Doc    string
	Fields []InputMember[T]
}

// InputMember is a field of a FormInput, and how it is set.
type InputMember[T any] struct {
	Go   string
	Name string // a graphql:"name" tag's name
	Doc  string
	Type TypeDef
	Set  func(dst *T, v any)
}

func (s FormInput[T]) binding() binding {
	b := binding{kind: inputBinding, t: reflect.TypeFor[T](), doc: s.Doc, build: func() any { return new(T) }}
	for _, f := range s.Fields {
		set := f.Set
		b.inputs = append(b.inputs, inputMember{goName: f.Go, name: f.Name, doc: f.Doc, typ: f.Type, set: func(dst, v any) { set(dst.(*T), v) }})
	}
	return b
}

// Consts is the enum type a Go type T is: its constants.
type Consts[T comparable] struct {
	Doc    string
	Values []Const[T]
}

// Const is a constant of a Consts.
type Const[T comparable] struct {
	Go    string // the constant's name
	Doc   string
	Value T
}

func (s Consts[T]) binding() binding {
	b := binding{kind: enumBinding, t: reflect.TypeFor[T](), doc: s.Doc}
	index := make(map[T]int, len(s.Values))
	for i, c := range s.Values {
		b.values = append(b.values, enumConst{goName: c.Go, doc: c.Doc, value: c.Value})
		if _, dup := index[c.Value]; !dup {
			index[c.Value] = i
		}
	}
	b.ser = func(v any) (string, int) {
		x, ok := v.(T)
		if !ok {
			if p, isPtr := v.(*T); isPtr && p != nil {
				x, ok = *p, true
			}
		}
		if i, found := index[x]; ok && found {
			return "", i
		}
		return fmt.Sprint(v), -1
	}
	return b
}

// typeRef is a named type a binding describes, resolved when the schema is
// built: ObjectOf, InputOf, EnumOf, TextScalar.
type typeRef struct {
	kind bindingKind
	t    reflect.Type
	text *Scalar // TextScalar's codec; its name is decided with the schema
}

func (*typeRef) typeDef() {}

// ObjectOf is the object type of struct T (its Struct).
func ObjectOf[T any]() TypeDef { return &typeRef{kind: objectBinding, t: reflect.TypeFor[T]()} }

// InputOf is the input object type of struct T (its FormInput).
func InputOf[T any]() TypeDef { return &typeRef{kind: inputBinding, t: reflect.TypeFor[T]()} }

// EnumOf is the enum type of T (its Consts).
func EnumOf[T comparable]() TypeDef { return &typeRef{kind: enumBinding, t: reflect.TypeFor[T]()} }

// ListOf is a list of E, a Go slice []E, its elements of type of.
func ListOf[E any](of TypeDef) *List {
	return &List{
		Of: of,
		Items: func(v any) (int, func(int) any, bool) {
			xs, ok := v.([]E)
			return len(xs), func(i int) any { return xs[i] }, ok
		},
		Build: func(items []any) (any, error) {
			xs := make([]E, len(items))
			for i, x := range items {
				xs[i] = As[E](x)
			}
			return xs, nil
		},
	}
}

// Integer, Floating, Text and Boolish are the Go types IntOf, FloatOf,
// StringOf and BooleanOf take.
type (
	Integer interface {
		~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
	}
	Floating interface{ ~float32 | ~float64 }
	Text     interface{ ~string }
	Boolish  interface{ ~bool }
)

// codec is base's scalar for the Go type a value of it is: out turns a
// resolved value into base's, in a value base gives into the Go type.
func codec(base *Scalar, out func(any) any, in func(any) (any, error)) *Scalar {
	parse := func(f func(any) (any, error)) func(any) (any, error) {
		return func(v any) (any, error) {
			x, err := f(v)
			if err != nil || x == nil {
				return x, err
			}
			return in(x)
		}
	}
	return &Scalar{
		Name: base.Name, Description: base.Description, SpecifiedByURL: base.SpecifiedByURL,
		Serialize:  func(v any) (any, error) { return base.Serialize(out(v)) },
		ParseValue: parse(base.ParseValue),
		ParseLiteral: func(v language.Value) (any, error) {
			return parse(func(any) (any, error) { return base.ParseLiteral(v) })(nil)
		},
	}
}

// IntOf is Int for the Go integer type T: resolvers return T, arguments
// arrive as T. A value out of T's range is an error.
func IntOf[T Integer]() *Scalar {
	return codec(Int,
		func(v any) any { return int64(As[T](v)) },
		func(x any) (any, error) {
			n := x.(int)
			t := T(n)
			if int(t) != n || n < 0 && t > 0 {
				return nil, fmt.Errorf("Int cannot represent %d as a %T", n, t)
			}
			return t, nil
		})
}

// FloatOf is Float for the Go type T.
func FloatOf[T Floating]() *Scalar {
	return codec(Float,
		func(v any) any { return float64(As[T](v)) },
		func(x any) (any, error) { return T(x.(float64)), nil })
}

// StringOf is String for the Go type T.
func StringOf[T Text]() *Scalar {
	return codec(String,
		func(v any) any { return string(As[T](v)) },
		func(x any) (any, error) { return T(x.(string)), nil })
}

// BooleanOf is Boolean for the Go type T.
func BooleanOf[T Boolish]() *Scalar {
	return codec(Boolean,
		func(v any) any { return bool(As[T](v)) },
		func(x any) (any, error) { return T(x.(bool)), nil })
}

// IDOf is ID for a Go string type T.
func IDOf[T Text]() *Scalar {
	return codec(ID,
		func(v any) any { return string(As[T](v)) },
		func(x any) (any, error) { return T(x.(string)), nil })
}

// IntIDOf is ID for a Go integer type T: written as a string of digits,
// read from one (or from an integer).
func IntIDOf[T Integer]() *Scalar {
	return codec(ID,
		func(v any) any { return strconv.FormatInt(int64(As[T](v)), 10) },
		func(x any) (any, error) {
			s := x.(string)
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("ID %q isn't a whole number", s)
			}
			t := T(n)
			if int64(t) != n || n < 0 && t > 0 {
				return nil, fmt.Errorf("ID %q is out of range", s)
			}
			return t, nil
		})
}

// DateTime is a time.Time, written in RFC 3339 with the fraction of a
// second it has: "2026-10-26T09:30:00Z".
var DateTime = &Scalar{
	Name:           "DateTime",
	Description:    "A date and time, in RFC 3339: 2026-10-26T09:30:00Z.",
	SpecifiedByURL: "https://scalars.graphql.org/andimarek/date-time",
	Serialize: func(v any) (any, error) {
		t, ok := v.(time.Time)
		if p, isPtr := v.(*time.Time); isPtr && p != nil {
			t, ok = *p, true
		}
		if !ok {
			return nil, fmt.Errorf("DateTime cannot represent value: %s", inspect(v))
		}
		return t.Format(time.RFC3339Nano), nil
	},
	ParseValue: func(v any) (any, error) {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("DateTime cannot represent a non string value: %s", inspect(v))
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, fmt.Errorf("DateTime cannot represent %q: write a date and time in RFC 3339, like 2026-10-26T09:30:00Z", s)
		}
		return t, nil
	},
}

// TextScalar is the custom scalar of a Go type T that reads and writes
// itself as text (encoding.TextMarshaler, encoding.TextUnmarshaler on *T):
// named after T unless @graphql.Type names it.
func TextScalar[T any, P interface {
	*T
	encoding.TextUnmarshaler
}]() TypeDef {
	s := &Scalar{
		Serialize: func(v any) (any, error) {
			var m encoding.TextMarshaler
			switch x := v.(type) {
			case encoding.TextMarshaler:
				m = x
			case T:
				if mm, ok := any(&x).(encoding.TextMarshaler); ok {
					m = mm
				}
			}
			if m == nil {
				return nil, fmt.Errorf("can't write %T as text", v)
			}
			b, err := m.MarshalText()
			if err != nil {
				return nil, err
			}
			return string(b), nil
		},
		ParseValue: func(v any) (any, error) {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("expected a string, got %s", inspect(v))
			}
			p := P(new(T))
			if err := p.UnmarshalText([]byte(s)); err != nil {
				return nil, err
			}
			return *(*T)(p), nil
		},
	}
	return &typeRef{kind: -1, t: reflect.TypeFor[T](), text: s}
}
