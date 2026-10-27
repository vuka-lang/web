// Package graphql serves GraphQL: a schema of Go values, the validation
// and execution of operations against it, introspection, the schema's SDL,
// and the GraphQL-over-HTTP transport. It follows the GraphQL
// specification (October 2021) and has no dependencies beyond the standard
// library.
//
// A schema is built from the types in this file — Object, FieldDef,
// Scalar, Enum, Interface, Union, InputObject, List and NonNull — and
// NewSchema checks it as the specification's type system requires.
// NewServer serves one: Execute runs a request, ServeHTTP answers one over
// HTTP.
package graphql

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// TypeDef is a type of a schema: *Scalar, *Enum, *Object, *Interface,
// *Union, *InputObject (the named types), *List or *NonNull.
type TypeDef interface{ typeDef() }

// NamedType is a type with a name: *Scalar, *Enum, *Object, *Interface,
// *Union or *InputObject.
type NamedType interface {
	TypeDef
	TypeName() string
}

// Scalar is a leaf type: Int, String, a custom scalar such as DateTime.
//
// Several Scalars may share a name. They are one type of the schema, each
// converting the values where it is used — a Go named type's codec beside
// the built-in it shows as: introspection and the SDL show the first one
// the schema meets, a built-in's own when the name is a built-in's.
type Scalar struct {
	Name, Description string
	SpecifiedByURL    string
	// Serialize turns a resolved value into the result's value: a string,
	// a bool, a number, nil. Its error is the field's.
	Serialize func(v any) (any, error)
	// ParseValue turns a variable's value — JSON, decoded with UseNumber:
	// json.Number, string, bool, nil, []any or map[string]any — into the
	// value resolvers are given.
	ParseValue func(v any) (any, error)
	// ParseLiteral turns a literal written in the document into the value
	// resolvers are given; nil has the literal read as JSON would be (an
	// enum value as its name) and given to ParseValue.
	ParseLiteral func(v language.Value) (any, error)
}

// Enum is a type of named values.
type Enum struct {
	Name, Description string
	Values            []*EnumValueDef
	// Serialize names the value a resolved Go value is; nil compares it
	// with each value's Value.
	Serialize func(v any) (string, error)
}

// EnumValueDef is one value of an Enum. Value is what resolvers get for it
// and return for it; nil stands for its name.
type EnumValueDef struct {
	Name, Description string
	Deprecated        bool
	DeprecationReason string
	Value             any
}

// Object is a type with fields: what queries select from.
type Object struct {
	Name, Description string
	Interfaces        []*Interface
	Fields            []*FieldDef
	// IsTypeOf reports whether a resolved value is of this type, for an
	// interface or union without a ResolveType; nil leaves it to the
	// abstract type.
	IsTypeOf func(v any) bool
	// Value normalizes a resolved value before its fields resolve (a T
	// into the *T the fields' resolvers take); nil keeps it. A nil result
	// is null.
	Value func(v any) any
	// Paged marks a connection (Connection[T]): in a query's complexity
	// its lists count as many items as the field returning it asks for
	// (its first or last argument).
	Paged bool
}

// FieldDef is a field of an Object or Interface.
type FieldDef struct {
	Name, Description string
	Deprecated        bool
	DeprecationReason string
	Args              []*ArgDef
	Type              TypeDef
	// Resolve gives the field's value from its parent's (p.Source); nil
	// reads key Name of a map[string]any source.
	Resolve func(ctx context.Context, p ResolveParams) (any, error)
	// Guard runs before Resolve: its error makes the field null, with
	// that error at its path.
	Guard func(ctx context.Context) error
	// Trivial marks a field that is cheap to resolve (a struct field's
	// read): it resolves inline, never on a goroutine of its own.
	Trivial bool
	// Cost is the field's weight in a query's complexity; 0 counts 1.
	Cost int
}

// ResolveParams is what a resolver is given: the parent's value, the
// field's arguments (coerced, by name; an argument not given and without a
// default is absent) and where it is.
type ResolveParams struct {
	Source any
	Args   map[string]any
	Info   *ResolveInfo
}

// ResolveInfo describes the field being resolved.
type ResolveInfo struct {
	FieldName  string
	Path       []any // response keys and list indexes from the root
	ParentType *Object
	ReturnType TypeDef
	Operation  *language.OperationDefinition
	Schema     *Schema
	Variables  map[string]any
}

// ArgDef is an argument of a field or a directive. Default is the value
// used when it isn't given; nil for none.
type ArgDef struct {
	Name, Description string
	Type              TypeDef
	Default           language.Value
	Deprecated        bool
	DeprecationReason string
}

// Interface is an abstract type with fields, which objects implement.
type Interface struct {
	Name, Description string
	Interfaces        []*Interface
	Fields            []*FieldDef
	// ResolveType names the object a resolved value is; nil asks each
	// implementing object's IsTypeOf.
	ResolveType func(v any) *Object
}

// Union is an abstract type: one of its object types.
type Union struct {
	Name, Description string
	Types             []*Object
	// ResolveType names the object a resolved value is; nil asks each
	// member's IsTypeOf.
	ResolveType func(v any) *Object
}

// InputObject is a type of input values with fields: an argument's or a
// variable's.
type InputObject struct {
	Name, Description string
	Fields            []*InputFieldDef
	// Build makes the value resolvers get from the coerced fields, by
	// name: a field not given and without a default is absent, one given
	// null is nil. Nil gives the map itself.
	Build func(fields map[string]any) (any, error)
}

// InputFieldDef is a field of an InputObject.
type InputFieldDef struct {
	Name, Description string
	Type              TypeDef
	Default           language.Value
	Deprecated        bool
	DeprecationReason string
}

// List is a list of Of.
type List struct {
	Of TypeDef
	// Items reads a resolved list: its length and its elements; ok is
	// false when v is no list. Nil takes []any.
	Items func(v any) (n int, at func(i int) any, ok bool)
	// Build makes the value resolvers get from a coerced input list; nil
	// gives the []any.
	Build func(items []any) (any, error)
}

// NonNull is Of, never null.
type NonNull struct{ Of TypeDef }

// Directive declares a directive the schema accepts. Locations are the
// specification's names: "FIELD", "FRAGMENT_SPREAD", "OBJECT", ….
type Directive struct {
	Name, Description string
	Locations         []string
	Args              []*ArgDef
	Repeatable        bool
}

func (*Scalar) typeDef()      {}
func (*Enum) typeDef()        {}
func (*Object) typeDef()      {}
func (*Interface) typeDef()   {}
func (*Union) typeDef()       {}
func (*InputObject) typeDef() {}
func (*List) typeDef()        {}
func (*NonNull) typeDef()     {}

// TypeName is the type's name.
func (t *Scalar) TypeName() string { return t.Name }

// TypeName is the type's name.
func (t *Enum) TypeName() string { return t.Name }

// TypeName is the type's name.
func (t *Object) TypeName() string { return t.Name }

// TypeName is the type's name.
func (t *Interface) TypeName() string { return t.Name }

// TypeName is the type's name.
func (t *Union) TypeName() string { return t.Name }

// TypeName is the type's name.
func (t *InputObject) TypeName() string { return t.Name }

// NamedOf is the named type t wraps (t itself when it is named); nil for
// nil.
func NamedOf(t TypeDef) NamedType {
	for {
		switch x := t.(type) {
		case *List:
			t = x.Of
		case *NonNull:
			t = x.Of
		case NamedType:
			return x
		default:
			return nil
		}
	}
}

// TypeString writes t as a document does: "[String!]!".
func TypeString(t TypeDef) string {
	switch x := t.(type) {
	case *List:
		return "[" + TypeString(x.Of) + "]"
	case *NonNull:
		return TypeString(x.Of) + "!"
	case NamedType:
		return x.TypeName()
	}
	return "<nil>"
}

func isInputType(t TypeDef) bool {
	switch NamedOf(t).(type) {
	case *Scalar, *Enum, *InputObject:
		return true
	}
	return false
}

func isOutputType(t TypeDef) bool {
	switch NamedOf(t).(type) {
	case *Scalar, *Enum, *Object, *Interface, *Union:
		return true
	}
	return false
}

func isLeaf(t TypeDef) bool {
	switch t.(type) {
	case *Scalar, *Enum:
		return true
	}
	return false
}

func isComposite(t TypeDef) bool {
	switch t.(type) {
	case *Object, *Interface, *Union:
		return true
	}
	return false
}

func isAbstract(t TypeDef) bool {
	switch t.(type) {
	case *Interface, *Union:
		return true
	}
	return false
}

func nullable(t TypeDef) TypeDef {
	if nn, ok := t.(*NonNull); ok {
		return nn.Of
	}
	return t
}

// SchemaConfig is a schema's roots and anything else it holds: types no
// root reaches, and directives beyond the built-in ones.
type SchemaConfig struct {
	Query, Mutation, Subscription *Object
	Types                         []NamedType
	Directives                    []*Directive
	Description                   string
}

// Schema is a checked schema, ready to execute operations against.
type Schema struct {
	query, mutation, subscription *Object
	description                   string
	types                         map[string]NamedType
	sorted                        []NamedType
	directives                    map[string]*Directive
	dirList                       []*Directive
	fields                        map[NamedType]map[string]*FieldDef
	inputFields                   map[*InputObject]map[string]*InputFieldDef
	possible                      map[NamedType][]*Object        // an abstract type's objects
	possibleSet                   map[NamedType]map[*Object]bool // the same, as a set
	implementers                  map[*Interface][]*Interface    // interfaces implementing one
	userTypes                     []NamedType                    // in the order met, for printing
	codecs                        map[*Scalar]*Scalar            // a scalar to the one its name shows
}

var nameRE = regexp.MustCompile(`^[_A-Za-z][_0-9A-Za-z]*$`)

// NewSchema checks c as the specification's type system requires — names,
// unique names, fields and their types in the right positions, interfaces
// implemented as declared, union members, enum values, input objects
// without cycles of non-null fields, default values of their types — and
// builds the schema. Its error lists every problem.
func NewSchema(c SchemaConfig) (*Schema, error) {
	introspectionOnce.Do(initIntrospection)
	s := &Schema{
		query: c.Query, mutation: c.Mutation, subscription: c.Subscription, description: c.Description,
		types:        map[string]NamedType{},
		directives:   map[string]*Directive{},
		fields:       map[NamedType]map[string]*FieldDef{},
		inputFields:  map[*InputObject]map[string]*InputFieldDef{},
		possible:     map[NamedType][]*Object{},
		possibleSet:  map[NamedType]map[*Object]bool{},
		implementers: map[*Interface][]*Interface{},
		codecs:       map[*Scalar]*Scalar{},
	}
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if c.Query == nil {
		fail("Query root type must be provided.")
	}
	roots := map[*Object]string{}
	for _, r := range []struct {
		name string
		t    *Object
	}{{"query", c.Query}, {"mutation", c.Mutation}, {"subscription", c.Subscription}} {
		if r.t == nil {
			continue
		}
		if other, ok := roots[r.t]; ok {
			fail("All root types must be different, %q type is used as %s and %s root types.", r.t.Name, other, r.name)
		}
		roots[r.t] = r.name
	}
	// Built-in scalars first: a codec sharing a built-in's name shows as it.
	for _, b := range []*Scalar{String, Boolean} {
		s.types[b.Name] = b
	}
	var visit func(t TypeDef, where string)
	seen := map[TypeDef]bool{}
	visit = func(t TypeDef, where string) {
		if t == nil {
			fail("%s has no type.", where)
			return
		}
		switch x := t.(type) {
		case *List:
			if x == nil {
				fail("%s has a nil list type.", where)
				return
			}
			visit(x.Of, where)
			return
		case *NonNull:
			if x == nil {
				fail("%s has a nil non-null type.", where)
				return
			}
			if _, ok := x.Of.(*NonNull); ok {
				fail("%s is non-null of non-null.", where)
			}
			visit(x.Of, where)
			return
		}
		n := t.(NamedType)
		if seen[n] {
			return
		}
		seen[n] = true
		name := n.TypeName()
		if prev, ok := s.types[name]; ok && prev != n {
			ps, pok := prev.(*Scalar)
			ns, nok := n.(*Scalar)
			if pok && nok {
				s.codecs[ns] = ps
				return
			}
			fail("Schema must contain uniquely named types but contains multiple types named %q.", name)
			return
		}
		s.types[name] = n
		if sc, ok := n.(*Scalar); ok {
			if b := builtinScalar(name); b != nil && b != sc {
				// A built-in's name: the built-in shows, the scalar converts.
				s.types[name] = b
				s.codecs[sc] = b
				seen[b] = true
			}
		}
		if !strings.HasPrefix(name, "__") {
			s.userTypes = append(s.userTypes, s.types[name])
		}
		switch x := n.(type) {
		case *Object:
			for _, i := range x.Interfaces {
				visit(i, x.Name)
			}
			for _, f := range x.Fields {
				visit(f.Type, x.Name+"."+f.Name)
				for _, a := range f.Args {
					visit(a.Type, x.Name+"."+f.Name+"("+a.Name+":)")
				}
			}
		case *Interface:
			for _, i := range x.Interfaces {
				visit(i, x.Name)
			}
			for _, f := range x.Fields {
				visit(f.Type, x.Name+"."+f.Name)
				for _, a := range f.Args {
					visit(a.Type, x.Name+"."+f.Name+"("+a.Name+":)")
				}
			}
		case *Union:
			for _, m := range x.Types {
				visit(m, x.Name)
			}
		case *InputObject:
			for _, f := range x.Fields {
				visit(f.Type, x.Name+"."+f.Name)
			}
		}
	}
	for _, r := range []*Object{c.Query, c.Mutation, c.Subscription} {
		if r != nil {
			visit(r, "root")
		}
	}
	for _, t := range c.Types {
		visit(t, "types")
	}
	for _, t := range introspectionTypes {
		visit(t, "introspection")
	}
	for _, d := range append(specifiedDirectives[:len(specifiedDirectives):len(specifiedDirectives)], c.Directives...) {
		if _, dup := s.directives[d.Name]; dup {
			fail("Directive @%s is declared twice.", d.Name)
			continue
		}
		s.directives[d.Name] = d
		s.dirList = append(s.dirList, d)
		for _, a := range d.Args {
			visit(a.Type, "@"+d.Name+"("+a.Name+":)")
		}
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	for _, t := range s.types {
		s.sorted = append(s.sorted, t)
	}
	sort.Slice(s.sorted, func(i, j int) bool { return s.sorted[i].TypeName() < s.sorted[j].TypeName() })

	// Indexes, then the type system's rules.
	for _, t := range s.sorted {
		switch x := t.(type) {
		case *Object:
			s.fields[x] = indexFields(x.Fields)
			for _, i := range x.Interfaces {
				if i != nil {
					s.possible[i] = append(s.possible[i], x)
				}
			}
		case *Interface:
			s.fields[x] = indexFields(x.Fields)
			for _, i := range x.Interfaces {
				if i != nil {
					s.implementers[i] = append(s.implementers[i], x)
				}
			}
		case *Union:
			s.possible[x] = append(s.possible[x], x.Types...)
		case *InputObject:
			m := map[string]*InputFieldDef{}
			for _, f := range x.Fields {
				if _, dup := m[f.Name]; !dup {
					m[f.Name] = f
				}
			}
			s.inputFields[x] = m
		}
	}
	for t, objs := range s.possible {
		set := map[*Object]bool{}
		for _, o := range objs {
			set[o] = true
		}
		s.possibleSet[t] = set
	}
	errs = append(errs, s.checkTypes()...)
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return s, nil
}

func indexFields(fs []*FieldDef) map[string]*FieldDef {
	m := make(map[string]*FieldDef, len(fs))
	for _, f := range fs {
		if _, dup := m[f.Name]; !dup {
			m[f.Name] = f
		}
	}
	return m
}

func builtinScalar(name string) *Scalar {
	switch name {
	case "Int":
		return Int
	case "Float":
		return Float
	case "String":
		return String
	case "Boolean":
		return Boolean
	case "ID":
		return ID
	}
	return nil
}

// checkTypes applies the type system's rules to every type.
func (s *Schema) checkTypes() []string {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	checkName := func(name, what string) {
		switch {
		case !nameRE.MatchString(name):
			fail("Names must match /^[_a-zA-Z][_a-zA-Z0-9]*$/ but %s %q does not.", what, name)
		case strings.HasPrefix(name, "__"):
			fail("Name %q must not begin with \"__\", which is reserved by GraphQL introspection.", name)
		}
	}
	checkArgs := func(owner string, args []*ArgDef) {
		names := map[string]bool{}
		for _, a := range args {
			checkName(a.Name, "argument")
			if names[a.Name] {
				fail("Argument %s(%s:) can only be defined once.", owner, a.Name)
			}
			names[a.Name] = true
			if !isInputType(a.Type) {
				fail("The type of %s(%s:) must be Input Type but got: %s.", owner, a.Name, TypeString(a.Type))
			}
			if _, req := a.Type.(*NonNull); req && a.Default == nil && a.Deprecated {
				fail("Required argument %s(%s:) cannot be deprecated.", owner, a.Name)
			}
			if a.Default != nil {
				if msgs := literalErrors(s, a.Default, a.Type); len(msgs) > 0 {
					fail("%s(%s:) has an invalid default value: %s", owner, a.Name, msgs[0])
				}
			}
		}
	}
	checkFields := func(owner string, fs []*FieldDef) {
		if len(fs) == 0 {
			fail("Type %s must define one or more fields.", owner)
		}
		names := map[string]bool{}
		for _, f := range fs {
			checkName(f.Name, "field")
			if names[f.Name] {
				fail("Field %s.%s can only be defined once.", owner, f.Name)
			}
			names[f.Name] = true
			if !isOutputType(f.Type) {
				fail("The type of %s.%s must be Output Type but got: %s.", owner, f.Name, TypeString(f.Type))
			}
			checkArgs(owner+"."+f.Name, f.Args)
		}
	}
	for _, t := range s.sorted {
		name := t.TypeName()
		if strings.HasPrefix(name, "__") {
			continue
		}
		checkName(name, "type")
		switch x := t.(type) {
		case *Object:
			checkFields(x.Name, x.Fields)
			s.checkImplements(x.Name, x.Interfaces, s.fields[x], x, fail)
		case *Interface:
			checkFields(x.Name, x.Fields)
			s.checkImplements(x.Name, x.Interfaces, s.fields[x], x, fail)
		case *Union:
			if len(x.Types) == 0 {
				fail("Union type %s must define one or more member types.", x.Name)
			}
			members := map[*Object]bool{}
			for _, m := range x.Types {
				if members[m] {
					fail("Union type %s can only include type %s once.", x.Name, m.Name)
				}
				members[m] = true
			}
		case *Enum:
			if len(x.Values) == 0 {
				fail("Enum type %s must define one or more values.", x.Name)
			}
			names := map[string]bool{}
			for _, v := range x.Values {
				checkName(v.Name, "enum value")
				if v.Name == "true" || v.Name == "false" || v.Name == "null" {
					fail("Enum type %s cannot include value: %s.", x.Name, v.Name)
				}
				if names[v.Name] {
					fail("Enum value %s.%s can only be defined once.", x.Name, v.Name)
				}
				names[v.Name] = true
			}
		case *InputObject:
			if len(x.Fields) == 0 {
				fail("Input Object type %s must define one or more fields.", x.Name)
			}
			names := map[string]bool{}
			for _, f := range x.Fields {
				checkName(f.Name, "input field")
				if names[f.Name] {
					fail("Input field %s.%s can only be defined once.", x.Name, f.Name)
				}
				names[f.Name] = true
				if !isInputType(f.Type) {
					fail("The type of %s.%s must be Input Type but got: %s.", x.Name, f.Name, TypeString(f.Type))
				}
				if _, req := f.Type.(*NonNull); req && f.Default == nil && f.Deprecated {
					fail("Required input field %s.%s cannot be deprecated.", x.Name, f.Name)
				}
				if f.Default != nil {
					if msgs := literalErrors(s, f.Default, f.Type); len(msgs) > 0 {
						fail("%s.%s has an invalid default value: %s", x.Name, f.Name, msgs[0])
					}
				}
			}
		}
	}
	for _, d := range s.dirList {
		if isSpecifiedDirective(d) {
			continue
		}
		checkName(d.Name, "directive")
		checkArgs("@"+d.Name, d.Args)
		if len(d.Locations) == 0 {
			fail("Directive @%s must include 1 or more locations.", d.Name)
		}
	}
	errs = append(errs, s.inputCycles()...)
	return errs
}

// checkImplements checks that t, with fields own, implements each of
// ifaces as the specification requires.
func (s *Schema) checkImplements(name string, ifaces []*Interface, own map[string]*FieldDef, self NamedType, fail func(string, ...any)) {
	declared := map[*Interface]bool{}
	for _, i := range ifaces {
		if i == nil {
			continue
		}
		if NamedType(i) == self {
			fail("Type %s cannot implement itself because it would create a circular reference.", name)
			continue
		}
		if declared[i] {
			fail("Type %s can only implement %s once.", name, i.Name)
			continue
		}
		declared[i] = true
	}
	for _, i := range ifaces {
		if i == nil || NamedType(i) == self {
			continue
		}
		for _, ii := range i.Interfaces {
			if !declared[ii] && NamedType(ii) != self {
				fail("Type %s must implement %s because it is implemented by %s.", name, ii.Name, i.Name)
			}
		}
		for _, f := range i.Fields {
			of, ok := own[f.Name]
			if !ok {
				fail("Interface field %s.%s expected but %s does not provide it.", i.Name, f.Name, name)
				continue
			}
			if !s.isSubType(of.Type, f.Type) {
				fail("Interface field %s.%s expects type %s but %s.%s is type %s.", i.Name, f.Name, TypeString(f.Type), name, f.Name, TypeString(of.Type))
			}
			for _, a := range f.Args {
				var oa *ArgDef
				for _, x := range of.Args {
					if x.Name == a.Name {
						oa = x
					}
				}
				if oa == nil {
					fail("Interface field argument %s.%s(%s:) expected but %s.%s does not provide it.", i.Name, f.Name, a.Name, name, f.Name)
					continue
				}
				if !sameType(a.Type, oa.Type) {
					fail("Interface field argument %s.%s(%s:) expects type %s but %s.%s(%s:) is type %s.", i.Name, f.Name, a.Name, TypeString(a.Type), name, f.Name, a.Name, TypeString(oa.Type))
				}
			}
			for _, oa := range of.Args {
				if slices.ContainsFunc(f.Args, func(a *ArgDef) bool { return a.Name == oa.Name }) {
					continue
				}
				if _, req := oa.Type.(*NonNull); req && oa.Default == nil {
					fail("Argument %s.%s(%s:) must not be required type %s if not provided by the Interface field %s.%s.", name, f.Name, oa.Name, TypeString(oa.Type), i.Name, f.Name)
				}
			}
		}
	}
}

// sameType reports whether a and b are the same type (scalars sharing a
// name count as one).
func sameType(a, b TypeDef) bool {
	switch x := a.(type) {
	case *List:
		y, ok := b.(*List)
		return ok && sameType(x.Of, y.Of)
	case *NonNull:
		y, ok := b.(*NonNull)
		return ok && sameType(x.Of, y.Of)
	case NamedType:
		y, ok := b.(NamedType)
		return ok && x.TypeName() == y.TypeName() && sameKind(x, y)
	}
	return false
}

func sameKind(a, b NamedType) bool { return typeKind(a) == typeKind(b) }

// isSubType reports whether a value of type sub is one of type super: the
// same type, a non-null of it, an object or interface implementing it, a
// list of a subtype.
func (s *Schema) isSubType(sub, super TypeDef) bool {
	if sameType(sub, super) {
		return true
	}
	if snn, ok := super.(*NonNull); ok {
		if bnn, ok := sub.(*NonNull); ok {
			return s.isSubType(bnn.Of, snn.Of)
		}
		return false
	}
	if bnn, ok := sub.(*NonNull); ok {
		return s.isSubType(bnn.Of, super)
	}
	if sl, ok := super.(*List); ok {
		if bl, ok := sub.(*List); ok {
			return s.isSubType(bl.Of, sl.Of)
		}
		return false
	}
	if _, ok := sub.(*List); ok {
		return false
	}
	sup, ok := super.(NamedType)
	if !ok || !isAbstract(sup) {
		return false
	}
	switch x := sub.(type) {
	case *Object:
		return s.IsPossibleType(sup, x)
	case *Interface:
		if si, ok := sup.(*Interface); ok {
			return slices.Contains(x.Interfaces, si)
		}
	}
	return false
}

// inputCycles reports input objects that can't be given a finite value:
// a cycle of non-null, non-list fields.
func (s *Schema) inputCycles() []string {
	var errs []string
	done := map[*InputObject]bool{}
	var path []string
	onPath := map[*InputObject]int{}
	var visit func(t *InputObject)
	visit = func(t *InputObject) {
		if done[t] {
			return
		}
		done[t] = true
		onPath[t] = len(path)
		for _, f := range t.Fields {
			nn, ok := f.Type.(*NonNull)
			if !ok {
				continue
			}
			io, ok := nn.Of.(*InputObject)
			if !ok {
				continue
			}
			path = append(path, t.Name+"."+f.Name)
			if at, ok := onPath[io]; ok {
				errs = append(errs, fmt.Sprintf("Cannot reference Input Object %q within itself through a series of non-null fields: %q.", io.Name, strings.Join(path[at:], ".")))
			} else {
				visit(io)
			}
			path = path[:len(path)-1]
		}
		delete(onPath, t)
	}
	for _, t := range s.sorted {
		if io, ok := t.(*InputObject); ok {
			visit(io)
		}
	}
	return errs
}

// Type is the schema's type of that name; nil when it has none.
func (s *Schema) Type(name string) NamedType { return s.types[name] }

// Types are the schema's types, sorted by name, built-in and introspection
// types included.
func (s *Schema) Types() []NamedType { return slices.Clone(s.sorted) }

// QueryType is the query root.
func (s *Schema) QueryType() *Object { return s.query }

// MutationType is the mutation root; nil when the schema has none.
func (s *Schema) MutationType() *Object { return s.mutation }

// SubscriptionType is the subscription root; nil when the schema has none.
func (s *Schema) SubscriptionType() *Object { return s.subscription }

// Description is the schema's description.
func (s *Schema) Description() string { return s.description }

// Directive is the directive of that name; nil when the schema has none.
func (s *Schema) Directive(name string) *Directive { return s.directives[name] }

// Directives are the schema's directives, the built-in ones first.
func (s *Schema) Directives() []*Directive { return slices.Clone(s.dirList) }

// PossibleTypes are the objects an abstract type may be: an interface's
// implementations, a union's members.
func (s *Schema) PossibleTypes(t NamedType) []*Object {
	if o, ok := t.(*Object); ok {
		return []*Object{o}
	}
	return s.possible[t]
}

// IsPossibleType reports whether obj is one of the types abstract may be.
func (s *Schema) IsPossibleType(abstract NamedType, obj *Object) bool {
	if o, ok := abstract.(*Object); ok {
		return o == obj
	}
	return s.possibleSet[abstract][obj]
}

// FieldOf is the field name of an object or interface type; nil when it has
// none.
func (s *Schema) FieldOf(t NamedType, name string) *FieldDef { return s.fields[t][name] }

// root is the root type of an operation kind.
func (s *Schema) root(op string) *Object {
	switch op {
	case language.OpMutation:
		return s.mutation
	case language.OpSubscription:
		return s.subscription
	}
	return s.query
}

// shown is the scalar the schema shows for sc (itself unless a codec).
func (s *Schema) shown(t NamedType) NamedType {
	if sc, ok := t.(*Scalar); ok {
		if c := s.codecs[sc]; c != nil {
			return c
		}
	}
	return t
}

// overlap reports whether two composite types share a possible object.
func (s *Schema) overlap(a, b NamedType) bool {
	if a == b {
		return true
	}
	if isAbstract(a) {
		if isAbstract(b) {
			for _, o := range s.PossibleTypes(a) {
				if s.IsPossibleType(b, o) {
					return true
				}
			}
			return false
		}
		if o, ok := b.(*Object); ok {
			return s.IsPossibleType(a, o)
		}
		return false
	}
	if isAbstract(b) {
		if o, ok := a.(*Object); ok {
			return s.IsPossibleType(b, o)
		}
	}
	return false
}
