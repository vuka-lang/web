package graphql

import (
	"fmt"
	"strings"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// Change is a difference between two versions of a schema. Breaking says
// a client written against the old one may fail against the new: a type,
// field, argument or enum value removed or renamed, a type changed in a
// way its values don't fit (an output made nullable, an input made
// required), a required argument or input field added, a union member or
// an implemented interface dropped. Path names the element: "User",
// "User.email", "Query.users(first:)", "Status.ACTIVE".
type Change struct {
	Breaking bool
	Path     string
	Message  string
}

func (c Change) String() string {
	if c.Breaking {
		return "BREAKING " + c.Message
	}
	return c.Message
}

// Diff lists what changed from old to new, in a stable order: the old
// schema's types by name, each one's fields, arguments and values in
// order, then what new adds.
func Diff(old, new *Schema) []Change {
	var out []Change
	add := func(breaking bool, path, format string, args ...any) {
		out = append(out, Change{breaking, path, fmt.Sprintf(format, args...)})
	}
	user := func(s *Schema) []NamedType {
		var ts []NamedType
		for _, t := range s.sorted {
			if !strings.HasPrefix(t.TypeName(), "__") && builtinScalar(t.TypeName()) != t {
				ts = append(ts, t)
			}
		}
		return ts
	}
	for _, ot := range user(old) {
		name := ot.TypeName()
		nt := new.types[name]
		if nt == nil {
			add(true, name, "Type %s was removed.", name)
			continue
		}
		if typeKind(ot) != typeKind(nt) {
			add(true, name, "Type %s changed from %s to %s.", name, kindWords(ot), kindWords(nt))
			continue
		}
		switch o := ot.(type) {
		case *Object:
			n := nt.(*Object)
			diffFields(name, o.Fields, n.Fields, add)
			diffInterfaces(name, o.Interfaces, n.Interfaces, add)
		case *Interface:
			n := nt.(*Interface)
			diffFields(name, o.Fields, n.Fields, add)
			diffInterfaces(name, o.Interfaces, n.Interfaces, add)
		case *Union:
			n := nt.(*Union)
			for _, m := range o.Types {
				if !containsName(n.Types, m.Name) {
					add(true, name, "Member %s was removed from union type %s.", m.Name, name)
				}
			}
			for _, m := range n.Types {
				if !containsName(o.Types, m.Name) {
					add(false, name, "Member %s was added to union type %s.", m.Name, name)
				}
			}
		case *Enum:
			n := nt.(*Enum)
			for _, v := range o.Values {
				nv := enumValueNamed(n, v.Name)
				if nv == nil {
					add(true, name+"."+v.Name, "Enum value %s.%s was removed.", name, v.Name)
					continue
				}
				if nv.Deprecated && !v.Deprecated {
					add(false, name+"."+v.Name, "Enum value %s.%s was deprecated.", name, v.Name)
				}
			}
			for _, v := range n.Values {
				if enumValueNamed(o, v.Name) == nil {
					add(false, name+"."+v.Name, "Enum value %s.%s was added.", name, v.Name)
				}
			}
		case *InputObject:
			n := nt.(*InputObject)
			for _, f := range o.Fields {
				path := name + "." + f.Name
				var nf *InputFieldDef
				for _, x := range n.Fields {
					if x.Name == f.Name {
						nf = x
					}
				}
				if nf == nil {
					add(true, path, "Input field %s was removed.", path)
					continue
				}
				if !safeInputChange(f.Type, nf.Type) {
					add(true, path, "Input field %s changed type from %s to %s.", path, TypeString(f.Type), TypeString(nf.Type))
				} else if TypeString(f.Type) != TypeString(nf.Type) {
					add(false, path, "Input field %s changed type from %s to %s.", path, TypeString(f.Type), TypeString(nf.Type))
				}
				if printDefault(f.Default) != printDefault(nf.Default) {
					add(false, path, "Input field %s changed its default value from %s to %s.", path, printDefault(f.Default), printDefault(nf.Default))
				}
			}
			for _, f := range n.Fields {
				found := false
				for _, x := range o.Fields {
					found = found || x.Name == f.Name
				}
				if found {
					continue
				}
				path := name + "." + f.Name
				if _, req := f.Type.(*NonNull); req && f.Default == nil {
					add(true, path, "A required input field %s was added.", path)
				} else {
					add(false, path, "An optional input field %s was added.", path)
				}
			}
		}
	}
	for _, nt := range user(new) {
		if old.types[nt.TypeName()] == nil {
			add(false, nt.TypeName(), "Type %s was added.", nt.TypeName())
		}
	}
	for _, d := range old.dirList {
		if !isSpecifiedDirective(d) && new.directives[d.Name] == nil {
			add(true, "@"+d.Name, "Directive @%s was removed.", d.Name)
		}
	}
	for _, d := range new.dirList {
		if !isSpecifiedDirective(d) && old.directives[d.Name] == nil {
			add(false, "@"+d.Name, "Directive @%s was added.", d.Name)
		}
	}
	return out
}

func kindWords(t TypeDef) string {
	switch t.(type) {
	case *Scalar:
		return "a scalar type"
	case *Object:
		return "an object type"
	case *Interface:
		return "an interface type"
	case *Union:
		return "a union type"
	case *Enum:
		return "an enum type"
	case *InputObject:
		return "an input object type"
	}
	return "a type"
}

func containsName[T NamedType](ts []T, name string) bool {
	for _, t := range ts {
		if t.TypeName() == name {
			return true
		}
	}
	return false
}

func enumValueNamed(e *Enum, name string) *EnumValueDef {
	for _, v := range e.Values {
		if v.Name == name {
			return v
		}
	}
	return nil
}

func printDefault(v language.Value) string {
	if v == nil {
		return "none"
	}
	return language.Print(v)
}

func diffInterfaces(name string, old, new []*Interface, add func(bool, string, string, ...any)) {
	for _, i := range old {
		if !containsName(new, i.Name) {
			add(true, name, "%s no longer implements interface %s.", name, i.Name)
		}
	}
	for _, i := range new {
		if !containsName(old, i.Name) {
			add(false, name, "%s now implements interface %s.", name, i.Name)
		}
	}
}

func diffFields(owner string, old, new []*FieldDef, add func(bool, string, string, ...any)) {
	for _, f := range old {
		path := owner + "." + f.Name
		var nf *FieldDef
		for _, x := range new {
			if x.Name == f.Name {
				nf = x
			}
		}
		if nf == nil {
			add(true, path, "Field %s was removed.", path)
			continue
		}
		if !safeOutputChange(f.Type, nf.Type) {
			add(true, path, "Field %s changed type from %s to %s.", path, TypeString(f.Type), TypeString(nf.Type))
		} else if TypeString(f.Type) != TypeString(nf.Type) {
			add(false, path, "Field %s changed type from %s to %s.", path, TypeString(f.Type), TypeString(nf.Type))
		}
		if nf.Deprecated && !f.Deprecated {
			add(false, path, "Field %s was deprecated.", path)
		}
		for _, a := range f.Args {
			apath := path + "(" + a.Name + ":)"
			var na *ArgDef
			for _, x := range nf.Args {
				if x.Name == a.Name {
					na = x
				}
			}
			if na == nil {
				add(true, apath, "Argument %s was removed.", apath)
				continue
			}
			if !safeInputChange(a.Type, na.Type) {
				add(true, apath, "Argument %s changed type from %s to %s.", apath, TypeString(a.Type), TypeString(na.Type))
			} else if TypeString(a.Type) != TypeString(na.Type) {
				add(false, apath, "Argument %s changed type from %s to %s.", apath, TypeString(a.Type), TypeString(na.Type))
			}
			if printDefault(a.Default) != printDefault(na.Default) {
				add(false, apath, "Argument %s changed its default value from %s to %s.", apath, printDefault(a.Default), printDefault(na.Default))
			}
		}
		for _, a := range nf.Args {
			found := false
			for _, x := range f.Args {
				found = found || x.Name == a.Name
			}
			if found {
				continue
			}
			apath := path + "(" + a.Name + ":)"
			if _, req := a.Type.(*NonNull); req && a.Default == nil {
				add(true, apath, "A required argument %s was added.", apath)
			} else {
				add(false, apath, "An optional argument %s was added.", apath)
			}
		}
	}
	for _, f := range new {
		found := false
		for _, x := range old {
			found = found || x.Name == f.Name
		}
		if !found {
			add(false, owner+"."+f.Name, "Field %s.%s was added.", owner, f.Name)
		}
	}
}

// safeOutputChange reports whether a field typed old may become typed new
// with no client failing: the same type, or one never null where it could
// be.
func safeOutputChange(old, new TypeDef) bool {
	switch o := old.(type) {
	case *List:
		switch n := new.(type) {
		case *List:
			return safeOutputChange(o.Of, n.Of)
		case *NonNull:
			return safeOutputChange(old, n.Of)
		}
		return false
	case *NonNull:
		n, ok := new.(*NonNull)
		return ok && safeOutputChange(o.Of, n.Of)
	case NamedType:
		switch n := new.(type) {
		case NamedType:
			return o.TypeName() == n.TypeName()
		case *NonNull:
			return safeOutputChange(old, n.Of)
		}
	}
	return false
}

// safeInputChange reports whether an argument or input field typed old
// may become typed new with every value clients send still valid: the
// same type, or one that is nullable where it was required.
func safeInputChange(old, new TypeDef) bool {
	switch o := old.(type) {
	case *List:
		n, ok := new.(*List)
		return ok && safeInputChange(o.Of, n.Of)
	case *NonNull:
		if n, ok := new.(*NonNull); ok {
			return safeInputChange(o.Of, n.Of)
		}
		return safeInputChange(o.Of, new)
	case NamedType:
		n, ok := new.(NamedType)
		return ok && o.TypeName() == n.TypeName()
	}
	return false
}
