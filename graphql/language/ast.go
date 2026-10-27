// Package language reads and writes GraphQL documents: a lexer and a parser
// for executable documents (operations and fragments) and type-system
// documents (schema definitions, the SDL), the syntax tree both produce, and
// a printer. It follows the GraphQL specification (October 2021): block
// strings, \u{…} escapes and descriptions included.
//
// Every node records where it starts (Loc): a 1-based line and a 1-based
// column counted in characters.
package language

// Loc is where a node starts in its source: 1-based line and column, the
// column counted in characters (runes).
type Loc struct{ Line, Column int }

// Location is the node's position.
func (l Loc) Location() Loc { return l }

// Node is any node of the syntax tree.
type Node interface{ Location() Loc }

// Document is a parsed GraphQL document.
type Document struct {
	Loc
	Definitions []Definition
}

// Definition is a top-level definition of a document: an
// *OperationDefinition or *FragmentDefinition (executable), or a
// type-system definition.
type Definition interface {
	Node
	isDefinition()
}

// Operations are the document's operation definitions, in order.
func (d *Document) Operations() []*OperationDefinition {
	var out []*OperationDefinition
	for _, def := range d.Definitions {
		if op, ok := def.(*OperationDefinition); ok {
			out = append(out, op)
		}
	}
	return out
}

// Fragments are the document's fragment definitions by name; the first of
// a name when several share it.
func (d *Document) Fragments() map[string]*FragmentDefinition {
	out := map[string]*FragmentDefinition{}
	for _, def := range d.Definitions {
		if f, ok := def.(*FragmentDefinition); ok {
			if _, dup := out[f.Name]; !dup {
				out[f.Name] = f
			}
		}
	}
	return out
}

// Operation kinds, as OperationDefinition.Operation and
// OperationTypeDefinition.Operation spell them.
const (
	OpQuery        = "query"
	OpMutation     = "mutation"
	OpSubscription = "subscription"
)

// OperationDefinition is a query, mutation or subscription. Shorthand is a
// query written as a bare selection set ("{ a }").
type OperationDefinition struct {
	Loc
	Operation           string // "query", "mutation" or "subscription"
	Name                string
	NameLoc             Loc
	VariableDefinitions []*VariableDefinition
	Directives          []*Directive
	SelectionSet        *SelectionSet
	Shorthand           bool
}

// FragmentDefinition is a named fragment: fragment Name on Type { … }.
type FragmentDefinition struct {
	Loc
	Name          string
	NameLoc       Loc
	TypeCondition *NamedType
	Directives    []*Directive
	SelectionSet  *SelectionSet
}

func (*OperationDefinition) isDefinition() {}
func (*FragmentDefinition) isDefinition()  {}

// VariableDefinition declares an operation's variable: $name: Type = default.
type VariableDefinition struct {
	Loc
	Variable     string // its name, without $
	Type         TypeRef
	DefaultValue Value // nil when it has none
	Directives   []*Directive
}

// SelectionSet is { … }: the fields and fragments selected.
type SelectionSet struct {
	Loc
	Selections []Selection
}

// Selection is a *Field, *FragmentSpread or *InlineFragment.
type Selection interface {
	Node
	isSelection()
}

// Field selects a field: alias: name(args) @directives { … }.
type Field struct {
	Loc
	Alias        string // "" when it has none
	Name         string
	Arguments    []*Argument
	Directives   []*Directive
	SelectionSet *SelectionSet // nil for a leaf
}

// ResponseKey is the key the field's value takes in the response: its
// alias, else its name.
func (f *Field) ResponseKey() string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
}

// FragmentSpread is ...Name @directives.
type FragmentSpread struct {
	Loc
	Name       string
	Directives []*Directive
}

// InlineFragment is ... on Type @directives { … }; TypeCondition is nil
// when the fragment names no type.
type InlineFragment struct {
	Loc
	TypeCondition *NamedType
	Directives    []*Directive
	SelectionSet  *SelectionSet
}

func (*Field) isSelection()          {}
func (*FragmentSpread) isSelection() {}
func (*InlineFragment) isSelection() {}

// Argument is name: value, of a field or a directive.
type Argument struct {
	Loc
	Name  string
	Value Value
}

// Directive is @name(args).
type Directive struct {
	Loc
	Name      string
	Arguments []*Argument
}

// Value is a literal or a variable: *Variable, *IntValue, *FloatValue,
// *StringValue, *BooleanValue, *NullValue, *EnumValue, *ListValue or
// *ObjectValue.
type Value interface {
	Node
	isValue()
}

// Variable is $name in a value's position.
type Variable struct {
	Loc
	Name string
}

// IntValue is an integer literal, as written.
type IntValue struct {
	Loc
	Value string
}

// FloatValue is a float literal, as written.
type FloatValue struct {
	Loc
	Value string
}

// StringValue is a string literal, its escapes resolved (a block string's
// indentation removed); Block says it was written """…""".
type StringValue struct {
	Loc
	Value string
	Block bool
}

// BooleanValue is true or false.
type BooleanValue struct {
	Loc
	Value bool
}

// NullValue is null.
type NullValue struct{ Loc }

// EnumValue is a bare name in a value's position: an enum value.
type EnumValue struct {
	Loc
	Value string
}

// ListValue is [ … ].
type ListValue struct {
	Loc
	Values []Value
}

// ObjectValue is { name: value … }.
type ObjectValue struct {
	Loc
	Fields []*ObjectField
}

// ObjectField is one name: value of an ObjectValue.
type ObjectField struct {
	Loc
	Name  string
	Value Value
}

func (*Variable) isValue()     {}
func (*IntValue) isValue()     {}
func (*FloatValue) isValue()   {}
func (*StringValue) isValue()  {}
func (*BooleanValue) isValue() {}
func (*NullValue) isValue()    {}
func (*EnumValue) isValue()    {}
func (*ListValue) isValue()    {}
func (*ObjectValue) isValue()  {}

// TypeRef is a type as a document writes it: *NamedType, *ListType or
// *NonNullType.
type TypeRef interface {
	Node
	isTypeRef()
	String() string
}

// NamedType is a type's name.
type NamedType struct {
	Loc
	Name string
}

// ListType is [Type].
type ListType struct {
	Loc
	Type TypeRef
}

// NonNullType is Type!.
type NonNullType struct {
	Loc
	Type TypeRef
}

func (*NamedType) isTypeRef()   {}
func (*ListType) isTypeRef()    {}
func (*NonNullType) isTypeRef() {}

func (t *NamedType) String() string   { return t.Name }
func (t *ListType) String() string    { return "[" + t.Type.String() + "]" }
func (t *NonNullType) String() string { return t.Type.String() + "!" }

// Type-system definitions. Extend is set on a definition written with
// extend (extend type User { … }): its description is empty, and its
// lists hold what it adds.

// SchemaDefinition is schema { query: Query … }.
type SchemaDefinition struct {
	Loc
	Extend         bool
	Description    string
	Directives     []*Directive
	OperationTypes []*OperationTypeDefinition
}

// OperationTypeDefinition is query: Query inside a schema definition.
type OperationTypeDefinition struct {
	Loc
	Operation string
	Type      *NamedType
}

// ScalarTypeDefinition is scalar Name.
type ScalarTypeDefinition struct {
	Loc
	Extend      bool
	Description string
	Name        string
	Directives  []*Directive
}

// ObjectTypeDefinition is type Name implements A & B { fields }.
type ObjectTypeDefinition struct {
	Loc
	Extend      bool
	Description string
	Name        string
	Interfaces  []*NamedType
	Directives  []*Directive
	Fields      []*FieldDefinition
}

// InterfaceTypeDefinition is interface Name implements A { fields }.
type InterfaceTypeDefinition struct {
	Loc
	Extend      bool
	Description string
	Name        string
	Interfaces  []*NamedType
	Directives  []*Directive
	Fields      []*FieldDefinition
}

// FieldDefinition is a field of an object or interface type.
type FieldDefinition struct {
	Loc
	Description string
	Name        string
	Arguments   []*InputValueDefinition
	Type        TypeRef
	Directives  []*Directive
}

// InputValueDefinition is an argument or an input object's field.
type InputValueDefinition struct {
	Loc
	Description  string
	Name         string
	Type         TypeRef
	DefaultValue Value // nil when it has none
	Directives   []*Directive
}

// UnionTypeDefinition is union Name = A | B.
type UnionTypeDefinition struct {
	Loc
	Extend      bool
	Description string
	Name        string
	Directives  []*Directive
	Types       []*NamedType
}

// EnumTypeDefinition is enum Name { VALUES }.
type EnumTypeDefinition struct {
	Loc
	Extend      bool
	Description string
	Name        string
	Directives  []*Directive
	Values      []*EnumValueDefinition
}

// EnumValueDefinition is one value of an enum.
type EnumValueDefinition struct {
	Loc
	Description string
	Name        string
	Directives  []*Directive
}

// InputObjectTypeDefinition is input Name { fields }.
type InputObjectTypeDefinition struct {
	Loc
	Extend      bool
	Description string
	Name        string
	Directives  []*Directive
	Fields      []*InputValueDefinition
}

// DirectiveDefinition is directive @name(args) repeatable on LOCATIONS.
type DirectiveDefinition struct {
	Loc
	Description string
	Name        string
	Arguments   []*InputValueDefinition
	Repeatable  bool
	Locations   []string
}

func (*SchemaDefinition) isDefinition()          {}
func (*ScalarTypeDefinition) isDefinition()      {}
func (*ObjectTypeDefinition) isDefinition()      {}
func (*InterfaceTypeDefinition) isDefinition()   {}
func (*UnionTypeDefinition) isDefinition()       {}
func (*EnumTypeDefinition) isDefinition()        {}
func (*InputObjectTypeDefinition) isDefinition() {}
func (*DirectiveDefinition) isDefinition()       {}
