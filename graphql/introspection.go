package graphql

import (
	"context"
	"sync"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// The specification's directives, which every schema has.
var (
	IncludeDirective = &Directive{
		Name:        "include",
		Description: "Directs the executor to include this field or fragment only when the `if` argument is true.",
		Locations:   []string{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"},
		Args:        []*ArgDef{{Name: "if", Description: "Included when true.", Type: &NonNull{Of: Boolean}}},
	}
	SkipDirective = &Directive{
		Name:        "skip",
		Description: "Directs the executor to skip this field or fragment when the `if` argument is true.",
		Locations:   []string{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"},
		Args:        []*ArgDef{{Name: "if", Description: "Skipped when true.", Type: &NonNull{Of: Boolean}}},
	}
	DeprecatedDirective = &Directive{
		Name:        "deprecated",
		Description: "Marks an element of a GraphQL schema as no longer supported.",
		Locations:   []string{"FIELD_DEFINITION", "ARGUMENT_DEFINITION", "INPUT_FIELD_DEFINITION", "ENUM_VALUE"},
		Args: []*ArgDef{{
			Name:        "reason",
			Description: "Explains why this element was deprecated, usually also including a suggestion for how to access supported similar data. Formatted using the Markdown syntax, as specified by [CommonMark](https://commonmark.org/).",
			Type:        String,
			Default:     &language.StringValue{Value: DefaultDeprecationReason},
		}},
	}
	SpecifiedByDirective = &Directive{
		Name:        "specifiedBy",
		Description: "Exposes a URL that specifies the behavior of this scalar.",
		Locations:   []string{"SCALAR"},
		Args:        []*ArgDef{{Name: "url", Description: "The URL that specifies the behavior of this scalar.", Type: &NonNull{Of: String}}},
	}
)

// DefaultDeprecationReason is the reason of a deprecation that gives none.
const DefaultDeprecationReason = "No longer supported"

var specifiedDirectives = []*Directive{IncludeDirective, SkipDirective, DeprecatedDirective, SpecifiedByDirective}

func isSpecifiedDirective(d *Directive) bool {
	for _, s := range specifiedDirectives {
		if s == d {
			return true
		}
	}
	return false
}

func deprecationReason(deprecated bool, reason string) any {
	if !deprecated {
		return nil
	}
	if reason == "" {
		return DefaultDeprecationReason
	}
	return reason
}

// typeKind is the __TypeKind of t.
func typeKind(t TypeDef) string {
	switch t.(type) {
	case *Scalar:
		return "SCALAR"
	case *Object:
		return "OBJECT"
	case *Interface:
		return "INTERFACE"
	case *Union:
		return "UNION"
	case *Enum:
		return "ENUM"
	case *InputObject:
		return "INPUT_OBJECT"
	case *List:
		return "LIST"
	case *NonNull:
		return "NON_NULL"
	}
	return ""
}

// inputValue is an argument or input field, as __InputValue shows it.
type inputValue struct {
	name, desc string
	typ        TypeDef
	def        language.Value
	deprecated bool
	reason     string
}

func argValues(args []*ArgDef, all bool) []any {
	out := make([]any, 0, len(args))
	for _, a := range args {
		if a.Deprecated && !all {
			continue
		}
		out = append(out, &inputValue{a.Name, a.Description, a.Type, a.Default, a.Deprecated, a.DeprecationReason})
	}
	return out
}

var (
	schemaType, typeType, fieldType, inputValueType, enumValueType, directiveType *Object
	typeKindEnum, directiveLocationEnum                                           *Enum
	introspectionTypes                                                            []NamedType

	// The meta-fields: __schema and __type on the query root, __typename
	// everywhere.
	schemaMetaField, typeMetaField, typenameMetaField *FieldDef
)

var introspectionOnce sync.Once

// initIntrospection builds the introspection types, once, before the first
// schema: package variables (a test's schema among them) may build one
// before init functions run.
func initIntrospection() {
	schemaType = &Object{Name: "__Schema", Description: "A GraphQL Schema defines the capabilities of a GraphQL server. It exposes all available types and directives on the server, as well as the entry points for query, mutation, and subscription operations."}
	typeType = &Object{Name: "__Type", Description: "The fundamental unit of any GraphQL Schema is the type. There are many kinds of types in GraphQL as represented by the `__TypeKind` enum.\n\nDepending on the kind of a type, certain fields describe information about that type. Scalar types provide no information beyond a name, description and optional `specifiedByURL`, while Enum types provide their values. Object and Interface types provide the fields they describe. Abstract types, Union and Interface, provide the Object types possible at runtime. List and NonNull types compose other types."}
	fieldType = &Object{Name: "__Field", Description: "Object and Interface types are described by a list of Fields, each of which has a name, potentially a list of arguments, and a return type."}
	inputValueType = &Object{Name: "__InputValue", Description: "Arguments provided to Fields or Directives and the input fields of an InputObject are represented as Input Values which describe their type and optionally a default value."}
	enumValueType = &Object{Name: "__EnumValue", Description: "One possible value for a given Enum. Enum values are unique values, not a placeholder for a string or numeric value. However an Enum value is returned in a JSON response as a string."}
	directiveType = &Object{Name: "__Directive", Description: "A Directive provides a way to describe alternate runtime execution and type validation behavior in a GraphQL document.\n\nIn some cases, you need to provide options to alter GraphQL's execution behavior in ways field arguments will not suffice, such as conditionally including or skipping a field. Directives provide this by describing additional information to the executor."}
	typeKindEnum = &Enum{Name: "__TypeKind", Description: "An enum describing what kind of type a given `__Type` is.", Values: []*EnumValueDef{
		{Name: "SCALAR", Description: "Indicates this type is a scalar."},
		{Name: "OBJECT", Description: "Indicates this type is an object. `fields` and `interfaces` are valid fields."},
		{Name: "INTERFACE", Description: "Indicates this type is an interface. `fields`, `interfaces`, and `possibleTypes` are valid fields."},
		{Name: "UNION", Description: "Indicates this type is a union. `possibleTypes` is a valid field."},
		{Name: "ENUM", Description: "Indicates this type is an enum. `enumValues` is a valid field."},
		{Name: "INPUT_OBJECT", Description: "Indicates this type is an input object. `inputFields` is a valid field."},
		{Name: "LIST", Description: "Indicates this type is a list. `ofType` is a valid field."},
		{Name: "NON_NULL", Description: "Indicates this type is a non-null. `ofType` is a valid field."},
	}}
	directiveLocationEnum = &Enum{Name: "__DirectiveLocation", Description: "A Directive can be adjacent to many parts of the GraphQL language, a __DirectiveLocation describes one such possible adjacencies.", Values: []*EnumValueDef{
		{Name: "QUERY", Description: "Location adjacent to a query operation."},
		{Name: "MUTATION", Description: "Location adjacent to a mutation operation."},
		{Name: "SUBSCRIPTION", Description: "Location adjacent to a subscription operation."},
		{Name: "FIELD", Description: "Location adjacent to a field."},
		{Name: "FRAGMENT_DEFINITION", Description: "Location adjacent to a fragment definition."},
		{Name: "FRAGMENT_SPREAD", Description: "Location adjacent to a fragment spread."},
		{Name: "INLINE_FRAGMENT", Description: "Location adjacent to an inline fragment."},
		{Name: "VARIABLE_DEFINITION", Description: "Location adjacent to a variable definition."},
		{Name: "SCHEMA", Description: "Location adjacent to a schema definition."},
		{Name: "SCALAR", Description: "Location adjacent to a scalar definition."},
		{Name: "OBJECT", Description: "Location adjacent to an object type definition."},
		{Name: "FIELD_DEFINITION", Description: "Location adjacent to a field definition."},
		{Name: "ARGUMENT_DEFINITION", Description: "Location adjacent to an argument definition."},
		{Name: "INTERFACE", Description: "Location adjacent to an interface definition."},
		{Name: "UNION", Description: "Location adjacent to a union definition."},
		{Name: "ENUM", Description: "Location adjacent to an enum definition."},
		{Name: "ENUM_VALUE", Description: "Location adjacent to an enum value definition."},
		{Name: "INPUT_OBJECT", Description: "Location adjacent to an input object type definition."},
		{Name: "INPUT_FIELD_DEFINITION", Description: "Location adjacent to an input object field definition."},
	}}

	nn := func(t TypeDef) TypeDef { return &NonNull{Of: t} }
	list := func(t TypeDef) TypeDef { return &List{Of: t} }
	includeDeprecated := []*ArgDef{{Name: "includeDeprecated", Type: Boolean, Default: &language.BooleanValue{Value: false}}}
	all := func(p ResolveParams) bool { b, _ := p.Args["includeDeprecated"].(bool); return b }
	shown := func(p ResolveParams, t TypeDef) any {
		if t == nil {
			return nil
		}
		if n, ok := t.(NamedType); ok {
			return p.Info.Schema.shown(n)
		}
		return t
	}
	res := func(f func(p ResolveParams) any) func(context.Context, ResolveParams) (any, error) {
		return func(_ context.Context, p ResolveParams) (any, error) { return f(p), nil }
	}
	str := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}

	schemaType.Fields = []*FieldDef{
		{Name: "description", Type: String, Resolve: res(func(p ResolveParams) any { return str(p.Source.(*Schema).description) })},
		{Name: "types", Description: "A list of all types supported by this server.", Type: nn(list(nn(typeType))), Resolve: res(func(p ResolveParams) any {
			s := p.Source.(*Schema)
			out := make([]any, len(s.sorted))
			for i, t := range s.sorted {
				out[i] = t
			}
			return out
		})},
		{Name: "queryType", Description: "The type that query operations will be rooted at.", Type: nn(typeType), Resolve: res(func(p ResolveParams) any { return p.Source.(*Schema).query })},
		{Name: "mutationType", Description: "If this server supports mutation, the type that mutation operations will be rooted at.", Type: typeType, Resolve: res(func(p ResolveParams) any {
			if m := p.Source.(*Schema).mutation; m != nil {
				return m
			}
			return nil
		})},
		{Name: "subscriptionType", Description: "If this server support subscription, the type that subscription operations will be rooted at.", Type: typeType, Resolve: res(func(p ResolveParams) any {
			if m := p.Source.(*Schema).subscription; m != nil {
				return m
			}
			return nil
		})},
		{Name: "directives", Description: "A list of all directives supported by this server.", Type: nn(list(nn(directiveType))), Resolve: res(func(p ResolveParams) any {
			s := p.Source.(*Schema)
			out := make([]any, len(s.dirList))
			for i, d := range s.dirList {
				out[i] = d
			}
			return out
		})},
	}
	typeType.Fields = []*FieldDef{
		{Name: "kind", Type: nn(typeKindEnum), Resolve: res(func(p ResolveParams) any { return typeKind(p.Source.(TypeDef)) })},
		{Name: "name", Type: String, Resolve: res(func(p ResolveParams) any {
			if n, ok := p.Source.(NamedType); ok {
				return n.TypeName()
			}
			return nil
		})},
		{Name: "description", Type: String, Resolve: res(func(p ResolveParams) any {
			switch t := p.Source.(type) {
			case *Scalar:
				return str(t.Description)
			case *Object:
				return str(t.Description)
			case *Interface:
				return str(t.Description)
			case *Union:
				return str(t.Description)
			case *Enum:
				return str(t.Description)
			case *InputObject:
				return str(t.Description)
			}
			return nil
		})},
		{Name: "specifiedByURL", Type: String, Resolve: res(func(p ResolveParams) any {
			if t, ok := p.Source.(*Scalar); ok {
				return str(t.SpecifiedByURL)
			}
			return nil
		})},
		{Name: "fields", Type: list(nn(fieldType)), Args: includeDeprecated, Resolve: res(func(p ResolveParams) any {
			var fs []*FieldDef
			switch t := p.Source.(type) {
			case *Object:
				fs = t.Fields
			case *Interface:
				fs = t.Fields
			default:
				return nil
			}
			out := make([]any, 0, len(fs))
			for _, f := range fs {
				if !f.Deprecated || all(p) {
					out = append(out, f)
				}
			}
			return out
		})},
		{Name: "interfaces", Type: list(nn(typeType)), Resolve: res(func(p ResolveParams) any {
			var is []*Interface
			switch t := p.Source.(type) {
			case *Object:
				is = t.Interfaces
			case *Interface:
				is = t.Interfaces
			default:
				return nil
			}
			out := make([]any, len(is))
			for i, x := range is {
				out[i] = x
			}
			return out
		})},
		{Name: "possibleTypes", Type: list(nn(typeType)), Resolve: res(func(p ResolveParams) any {
			t, ok := p.Source.(NamedType)
			if !ok || !isAbstract(t) {
				return nil
			}
			objs := p.Info.Schema.PossibleTypes(t)
			out := make([]any, len(objs))
			for i, o := range objs {
				out[i] = o
			}
			return out
		})},
		{Name: "enumValues", Type: list(nn(enumValueType)), Args: includeDeprecated, Resolve: res(func(p ResolveParams) any {
			t, ok := p.Source.(*Enum)
			if !ok {
				return nil
			}
			out := make([]any, 0, len(t.Values))
			for _, v := range t.Values {
				if !v.Deprecated || all(p) {
					out = append(out, v)
				}
			}
			return out
		})},
		{Name: "inputFields", Type: list(nn(inputValueType)), Args: includeDeprecated, Resolve: res(func(p ResolveParams) any {
			t, ok := p.Source.(*InputObject)
			if !ok {
				return nil
			}
			out := make([]any, 0, len(t.Fields))
			for _, f := range t.Fields {
				if !f.Deprecated || all(p) {
					out = append(out, &inputValue{f.Name, f.Description, f.Type, f.Default, f.Deprecated, f.DeprecationReason})
				}
			}
			return out
		})},
		{Name: "ofType", Type: typeType, Resolve: res(func(p ResolveParams) any {
			switch t := p.Source.(type) {
			case *List:
				return shown(p, t.Of)
			case *NonNull:
				return shown(p, t.Of)
			}
			return nil
		})},
	}
	fieldType.Fields = []*FieldDef{
		{Name: "name", Type: nn(String), Resolve: res(func(p ResolveParams) any { return p.Source.(*FieldDef).Name })},
		{Name: "description", Type: String, Resolve: res(func(p ResolveParams) any { return str(p.Source.(*FieldDef).Description) })},
		{Name: "args", Type: nn(list(nn(inputValueType))), Args: includeDeprecated, Resolve: res(func(p ResolveParams) any { return argValues(p.Source.(*FieldDef).Args, all(p)) })},
		{Name: "type", Type: nn(typeType), Resolve: res(func(p ResolveParams) any { return shown(p, p.Source.(*FieldDef).Type) })},
		{Name: "isDeprecated", Type: nn(Boolean), Resolve: res(func(p ResolveParams) any { return p.Source.(*FieldDef).Deprecated })},
		{Name: "deprecationReason", Type: String, Resolve: res(func(p ResolveParams) any {
			f := p.Source.(*FieldDef)
			return deprecationReason(f.Deprecated, f.DeprecationReason)
		})},
	}
	inputValueType.Fields = []*FieldDef{
		{Name: "name", Type: nn(String), Resolve: res(func(p ResolveParams) any { return p.Source.(*inputValue).name })},
		{Name: "description", Type: String, Resolve: res(func(p ResolveParams) any { return str(p.Source.(*inputValue).desc) })},
		{Name: "type", Type: nn(typeType), Resolve: res(func(p ResolveParams) any { return shown(p, p.Source.(*inputValue).typ) })},
		{Name: "defaultValue", Description: "A GraphQL-formatted string representing the default value for this input value.", Type: String, Resolve: res(func(p ResolveParams) any {
			if d := p.Source.(*inputValue).def; d != nil {
				return language.Print(d)
			}
			return nil
		})},
		{Name: "isDeprecated", Type: nn(Boolean), Resolve: res(func(p ResolveParams) any { return p.Source.(*inputValue).deprecated })},
		{Name: "deprecationReason", Type: String, Resolve: res(func(p ResolveParams) any {
			v := p.Source.(*inputValue)
			return deprecationReason(v.deprecated, v.reason)
		})},
	}
	enumValueType.Fields = []*FieldDef{
		{Name: "name", Type: nn(String), Resolve: res(func(p ResolveParams) any { return p.Source.(*EnumValueDef).Name })},
		{Name: "description", Type: String, Resolve: res(func(p ResolveParams) any { return str(p.Source.(*EnumValueDef).Description) })},
		{Name: "isDeprecated", Type: nn(Boolean), Resolve: res(func(p ResolveParams) any { return p.Source.(*EnumValueDef).Deprecated })},
		{Name: "deprecationReason", Type: String, Resolve: res(func(p ResolveParams) any {
			v := p.Source.(*EnumValueDef)
			return deprecationReason(v.Deprecated, v.DeprecationReason)
		})},
	}
	directiveType.Fields = []*FieldDef{
		{Name: "name", Type: nn(String), Resolve: res(func(p ResolveParams) any { return p.Source.(*Directive).Name })},
		{Name: "description", Type: String, Resolve: res(func(p ResolveParams) any { return str(p.Source.(*Directive).Description) })},
		{Name: "isRepeatable", Type: nn(Boolean), Resolve: res(func(p ResolveParams) any { return p.Source.(*Directive).Repeatable })},
		{Name: "locations", Type: nn(list(nn(directiveLocationEnum))), Resolve: res(func(p ResolveParams) any {
			ls := p.Source.(*Directive).Locations
			out := make([]any, len(ls))
			for i, l := range ls {
				out[i] = l
			}
			return out
		})},
		{Name: "args", Type: nn(list(nn(inputValueType))), Args: includeDeprecated, Resolve: res(func(p ResolveParams) any { return argValues(p.Source.(*Directive).Args, all(p)) })},
	}
	for _, o := range []*Object{schemaType, typeType, fieldType, inputValueType, enumValueType, directiveType} {
		for _, f := range o.Fields {
			f.Trivial = true
		}
	}
	introspectionTypes = []NamedType{schemaType, typeType, fieldType, inputValueType, enumValueType, directiveType, typeKindEnum, directiveLocationEnum}

	schemaMetaField = &FieldDef{Name: "__schema", Description: "Access the current type schema of this server.", Type: nn(schemaType), Trivial: true,
		Resolve: res(func(p ResolveParams) any { return p.Info.Schema })}
	typeMetaField = &FieldDef{Name: "__type", Description: "Request the type information of a single type.", Type: typeType, Trivial: true,
		Args: []*ArgDef{{Name: "name", Type: nn(String)}},
		Resolve: res(func(p ResolveParams) any {
			name, _ := p.Args["name"].(string)
			if t := p.Info.Schema.Type(name); t != nil {
				return t
			}
			return nil
		})}
	typenameMetaField = &FieldDef{Name: "__typename", Description: "The name of the current Object type at runtime.", Type: nn(String), Trivial: true}
}
