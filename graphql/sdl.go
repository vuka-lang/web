package graphql

import (
	"errors"
	"fmt"
	"strings"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// SDL is the schema in the schema definition language: a schema block
// only when the roots aren't named Query, Mutation and Subscription (or
// the schema has a description), the directives beyond the built-in ones,
// then the root types (query, mutation, subscription) and every other type
// sorted by name; fields, arguments and values in their declared order.
// Built-in scalars and directives and the introspection types are left
// out.
func (s *Schema) SDL() string {
	var b strings.Builder
	block := func(text string) {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(text)
		b.WriteString("\n")
	}
	if s.description != "" || s.query != nil && s.query.Name != "Query" || s.mutation != nil && s.mutation.Name != "Mutation" || s.subscription != nil && s.subscription.Name != "Subscription" {
		var sb strings.Builder
		sb.WriteString(description(s.description, ""))
		sb.WriteString("schema {\n")
		for _, r := range []struct {
			op string
			t  *Object
		}{{"query", s.query}, {"mutation", s.mutation}, {"subscription", s.subscription}} {
			if r.t != nil {
				fmt.Fprintf(&sb, "  %s: %s\n", r.op, r.t.Name)
			}
		}
		sb.WriteString("}")
		block(sb.String())
	}
	for _, d := range s.dirList {
		if !isSpecifiedDirective(d) {
			block(printDirectiveDef(d))
		}
	}
	printed := map[NamedType]bool{}
	for _, r := range []*Object{s.query, s.mutation, s.subscription} {
		if r != nil && !printed[r] {
			printed[r] = true
			block(printType(r))
		}
	}
	for _, t := range s.sorted {
		name := t.TypeName()
		if printed[t] || strings.HasPrefix(name, "__") || builtinScalar(name) == t {
			continue
		}
		printed[t] = true
		block(printType(t))
	}
	return b.String()
}

// description is a description as the SDL writes it, a block string above
// what it describes; "" for none.
func description(text, indent string) string {
	if text == "" {
		return ""
	}
	return indent + language.PrintBlockString(text, indent) + "\n"
}

func printDeprecated(deprecated bool, reason string) string {
	if !deprecated {
		return ""
	}
	if reason == "" || reason == DefaultDeprecationReason {
		return " @deprecated"
	}
	return " @deprecated(reason: " + language.PrintString(reason) + ")"
}

func printArgs(args []*ArgDef, indent string) string {
	if len(args) == 0 {
		return ""
	}
	described := false
	for _, a := range args {
		if a.Description != "" {
			described = true
		}
	}
	parts := make([]string, len(args))
	for i, a := range args {
		p := a.Name + ": " + TypeString(a.Type)
		if a.Default != nil {
			p += " = " + language.Print(a.Default)
		}
		p += printDeprecated(a.Deprecated, a.DeprecationReason)
		if described {
			p = description(a.Description, indent+"  ") + indent + "  " + p
		}
		parts[i] = p
	}
	if described {
		return "(\n" + strings.Join(parts, "\n") + "\n" + indent + ")"
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func printFields(fs []*FieldDef) string {
	var b strings.Builder
	b.WriteString(" {\n")
	for _, f := range fs {
		b.WriteString(description(f.Description, "  "))
		b.WriteString("  " + f.Name + printArgs(f.Args, "  ") + ": " + TypeString(f.Type) + printDeprecated(f.Deprecated, f.DeprecationReason) + "\n")
	}
	b.WriteString("}")
	return b.String()
}

func printImplements(is []*Interface) string {
	if len(is) == 0 {
		return ""
	}
	names := make([]string, len(is))
	for i, x := range is {
		names[i] = x.Name
	}
	return " implements " + strings.Join(names, " & ")
}

func printType(t NamedType) string {
	switch t := t.(type) {
	case *Scalar:
		s := description(t.Description, "") + "scalar " + t.Name
		if t.SpecifiedByURL != "" {
			s += " @specifiedBy(url: " + language.PrintString(t.SpecifiedByURL) + ")"
		}
		return s
	case *Object:
		return description(t.Description, "") + "type " + t.Name + printImplements(t.Interfaces) + printFields(t.Fields)
	case *Interface:
		return description(t.Description, "") + "interface " + t.Name + printImplements(t.Interfaces) + printFields(t.Fields)
	case *Union:
		names := make([]string, len(t.Types))
		for i, o := range t.Types {
			names[i] = o.Name
		}
		return description(t.Description, "") + "union " + t.Name + " = " + strings.Join(names, " | ")
	case *Enum:
		var b strings.Builder
		b.WriteString(description(t.Description, "") + "enum " + t.Name + " {\n")
		for _, v := range t.Values {
			b.WriteString(description(v.Description, "  "))
			b.WriteString("  " + v.Name + printDeprecated(v.Deprecated, v.DeprecationReason) + "\n")
		}
		b.WriteString("}")
		return b.String()
	case *InputObject:
		var b strings.Builder
		b.WriteString(description(t.Description, "") + "input " + t.Name + " {\n")
		for _, f := range t.Fields {
			b.WriteString(description(f.Description, "  "))
			p := "  " + f.Name + ": " + TypeString(f.Type)
			if f.Default != nil {
				p += " = " + language.Print(f.Default)
			}
			b.WriteString(p + printDeprecated(f.Deprecated, f.DeprecationReason) + "\n")
		}
		b.WriteString("}")
		return b.String()
	}
	return ""
}

func printDirectiveDef(d *Directive) string {
	s := description(d.Description, "") + "directive @" + d.Name + printArgs(d.Args, "")
	if d.Repeatable {
		s += " repeatable"
	}
	return s + " on " + strings.Join(d.Locations, " | ")
}

// ParseSchema builds a schema from its SDL, without resolvers: for
// comparing a schema with an earlier one (Diff), or for validating
// documents against it. Its custom scalars take and give values as they
// come. Type extensions are merged into the types they extend.
func ParseSchema(sdl string) (*Schema, error) {
	doc, err := language.Parse(sdl)
	if err != nil {
		return nil, err
	}
	b := &sdlBuilder{types: map[string]NamedType{}, defs: map[string][]language.Definition{}}
	var schemaDefs []*language.SchemaDefinition
	var dirDefs []*language.DirectiveDefinition
	var order []string
	for _, d := range doc.Definitions {
		switch d := d.(type) {
		case *language.SchemaDefinition:
			schemaDefs = append(schemaDefs, d)
		case *language.DirectiveDefinition:
			dirDefs = append(dirDefs, d)
		case *language.OperationDefinition, *language.FragmentDefinition:
			return nil, fmt.Errorf("%d:%d: a schema holds type definitions, not operations or fragments", d.Location().Line, d.Location().Column)
		default:
			name, ext := typeDefName(d)
			if !ext {
				if b.types[name] != nil {
					return nil, fmt.Errorf("%d:%d: there can be only one type named %q", d.Location().Line, d.Location().Column, name)
				}
				b.types[name] = newNamed(d)
				order = append(order, name)
			}
			b.defs[name] = append(b.defs[name], d)
		}
	}
	for name, defs := range b.defs {
		if b.types[name] == nil {
			if builtinScalar(name) == nil {
				return nil, fmt.Errorf("%d:%d: cannot extend type %q, which isn't defined", defs[0].Location().Line, defs[0].Location().Column, name)
			}
		}
	}
	for _, name := range order {
		b.fill(name)
	}
	for name := range b.defs {
		if b.types[name] == nil {
			b.fill(name)
		}
	}
	if len(b.errs) > 0 {
		return nil, errors.New(strings.Join(b.errs, "\n"))
	}
	cfg := SchemaConfig{}
	root := func(name string) *Object {
		o, _ := b.types[name].(*Object)
		if o == nil && name != "" && b.types[name] != nil {
			b.errs = append(b.errs, fmt.Sprintf("root type %q is not an object type", name))
		}
		return o
	}
	if len(schemaDefs) > 0 {
		for _, sd := range schemaDefs {
			if sd.Description != "" {
				cfg.Description = sd.Description
			}
			for _, ot := range sd.OperationTypes {
				o := root(ot.Type.Name)
				if o == nil {
					b.errs = append(b.errs, fmt.Sprintf("%d:%d: root type %q is not defined", ot.Line, ot.Column, ot.Type.Name))
				}
				switch ot.Operation {
				case language.OpQuery:
					cfg.Query = o
				case language.OpMutation:
					cfg.Mutation = o
				case language.OpSubscription:
					cfg.Subscription = o
				}
			}
		}
	} else {
		cfg.Query, cfg.Mutation, cfg.Subscription = root("Query"), root("Mutation"), root("Subscription")
	}
	for _, d := range dirDefs {
		dir := &Directive{Name: d.Name, Description: d.Description, Locations: d.Locations, Repeatable: d.Repeatable}
		dir.Args = b.args(d.Arguments)
		cfg.Directives = append(cfg.Directives, dir)
	}
	if len(b.errs) > 0 {
		return nil, errors.New(strings.Join(b.errs, "\n"))
	}
	for _, name := range order {
		cfg.Types = append(cfg.Types, b.types[name])
	}
	return NewSchema(cfg)
}

type sdlBuilder struct {
	types map[string]NamedType
	defs  map[string][]language.Definition
	errs  []string
}

func typeDefName(d language.Definition) (string, bool) {
	switch d := d.(type) {
	case *language.ScalarTypeDefinition:
		return d.Name, d.Extend
	case *language.ObjectTypeDefinition:
		return d.Name, d.Extend
	case *language.InterfaceTypeDefinition:
		return d.Name, d.Extend
	case *language.UnionTypeDefinition:
		return d.Name, d.Extend
	case *language.EnumTypeDefinition:
		return d.Name, d.Extend
	case *language.InputObjectTypeDefinition:
		return d.Name, d.Extend
	}
	return "", false
}

func newNamed(d language.Definition) NamedType {
	ident := func(v any) (any, error) { return v, nil }
	switch d := d.(type) {
	case *language.ScalarTypeDefinition:
		return &Scalar{Name: d.Name, Description: d.Description, Serialize: ident, ParseValue: ident}
	case *language.ObjectTypeDefinition:
		return &Object{Name: d.Name, Description: d.Description}
	case *language.InterfaceTypeDefinition:
		return &Interface{Name: d.Name, Description: d.Description}
	case *language.UnionTypeDefinition:
		return &Union{Name: d.Name, Description: d.Description}
	case *language.EnumTypeDefinition:
		return &Enum{Name: d.Name, Description: d.Description}
	case *language.InputObjectTypeDefinition:
		return &InputObject{Name: d.Name, Description: d.Description}
	}
	return nil
}

func (b *sdlBuilder) named(n *language.NamedType) NamedType {
	if t := b.types[n.Name]; t != nil {
		return t
	}
	if t := builtinScalar(n.Name); t != nil {
		return t
	}
	b.errs = append(b.errs, fmt.Sprintf("%d:%d: unknown type %q", n.Line, n.Column, n.Name))
	return nil
}

func (b *sdlBuilder) ref(r language.TypeRef) TypeDef {
	switch x := r.(type) {
	case *language.NamedType:
		if t := b.named(x); t != nil {
			return t
		}
		return String
	case *language.ListType:
		return &List{Of: b.ref(x.Type)}
	case *language.NonNullType:
		return &NonNull{Of: b.ref(x.Type)}
	}
	return nil
}

func deprecation(ds []*language.Directive) (bool, string) {
	for _, d := range ds {
		if d.Name != "deprecated" {
			continue
		}
		for _, a := range d.Arguments {
			if a.Name == "reason" {
				if s, ok := a.Value.(*language.StringValue); ok {
					return true, s.Value
				}
			}
		}
		return true, ""
	}
	return false, ""
}

func (b *sdlBuilder) args(defs []*language.InputValueDefinition) []*ArgDef {
	var out []*ArgDef
	for _, a := range defs {
		dep, reason := deprecation(a.Directives)
		out = append(out, &ArgDef{Name: a.Name, Description: a.Description, Type: b.ref(a.Type), Default: a.DefaultValue, Deprecated: dep, DeprecationReason: reason})
	}
	return out
}

func (b *sdlBuilder) fields(defs []*language.FieldDefinition) []*FieldDef {
	var out []*FieldDef
	for _, f := range defs {
		dep, reason := deprecation(f.Directives)
		out = append(out, &FieldDef{Name: f.Name, Description: f.Description, Type: b.ref(f.Type), Args: b.args(f.Arguments), Deprecated: dep, DeprecationReason: reason})
	}
	return out
}

func (b *sdlBuilder) interfaces(ns []*language.NamedType) []*Interface {
	var out []*Interface
	for _, n := range ns {
		t := b.named(n)
		if i, ok := t.(*Interface); ok {
			out = append(out, i)
		} else if t != nil {
			b.errs = append(b.errs, fmt.Sprintf("%d:%d: %q is not an interface", n.Line, n.Column, n.Name))
		}
	}
	return out
}

// fill gives a type what its definitions (and extensions) declare.
func (b *sdlBuilder) fill(name string) {
	for _, d := range b.defs[name] {
		switch d := d.(type) {
		case *language.ScalarTypeDefinition:
			if t, ok := b.types[name].(*Scalar); ok {
				for _, dir := range d.Directives {
					if dir.Name == "specifiedBy" {
						for _, a := range dir.Arguments {
							if s, ok := a.Value.(*language.StringValue); ok && a.Name == "url" {
								t.SpecifiedByURL = s.Value
							}
						}
					}
				}
			}
		case *language.ObjectTypeDefinition:
			if t, ok := b.types[name].(*Object); ok {
				t.Interfaces = append(t.Interfaces, b.interfaces(d.Interfaces)...)
				t.Fields = append(t.Fields, b.fields(d.Fields)...)
			}
		case *language.InterfaceTypeDefinition:
			if t, ok := b.types[name].(*Interface); ok {
				t.Interfaces = append(t.Interfaces, b.interfaces(d.Interfaces)...)
				t.Fields = append(t.Fields, b.fields(d.Fields)...)
			}
		case *language.UnionTypeDefinition:
			if t, ok := b.types[name].(*Union); ok {
				for _, n := range d.Types {
					if o, ok := b.named(n).(*Object); ok {
						t.Types = append(t.Types, o)
					} else {
						b.errs = append(b.errs, fmt.Sprintf("%d:%d: union member %q is not an object type", n.Line, n.Column, n.Name))
					}
				}
			}
		case *language.EnumTypeDefinition:
			if t, ok := b.types[name].(*Enum); ok {
				for _, v := range d.Values {
					dep, reason := deprecation(v.Directives)
					t.Values = append(t.Values, &EnumValueDef{Name: v.Name, Description: v.Description, Deprecated: dep, DeprecationReason: reason})
				}
			}
		case *language.InputObjectTypeDefinition:
			if t, ok := b.types[name].(*InputObject); ok {
				for _, f := range d.Fields {
					dep, reason := deprecation(f.Directives)
					t.Fields = append(t.Fields, &InputFieldDef{Name: f.Name, Description: f.Description, Type: b.ref(f.Type), Default: f.DefaultValue, Deprecated: dep, DeprecationReason: reason})
				}
			}
		}
	}
}
