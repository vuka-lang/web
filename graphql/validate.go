package graphql

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// Validate applies the specification's validation rules (section 5) to an
// executable document: every error found, each at its locations, with
// extensions.code GRAPHQL_VALIDATION_FAILED. introspection false refuses
// __schema and __type as well.
func (s *Schema) Validate(doc *language.Document, introspection bool) []*Error {
	v := &validator{s: s, doc: doc, introspection: introspection,
		frags: map[string]*language.FragmentDefinition{}, info: map[language.Definition]*defInfo{}}
	v.run()
	return v.errs
}

type validator struct {
	s             *Schema
	doc           *language.Document
	introspection bool
	errs          []*Error
	frags         map[string]*language.FragmentDefinition
	info          map[language.Definition]*defInfo
	cur           *defInfo
	overlap       *overlapCache
}

// defInfo is what an operation or fragment uses: the fragments it spreads
// and the variables it reads, each where it expects a type.
type defInfo struct {
	spreads []*language.FragmentSpread
	vars    []varUse
}

type varUse struct {
	name       string
	loc        language.Loc
	typ        TypeDef // nil where the expected type is unknown
	hasDefault bool    // the position has a default of its own
}

func (v *validator) report(msg string, locs ...language.Loc) {
	e := &Error{Message: msg, Extensions: code(CodeValidationFailed)}
	for _, l := range locs {
		e.Locations = append(e.Locations, Location{l.Line, l.Column})
	}
	v.errs = append(v.errs, e)
}

func (v *validator) run() {
	v.overlap = newOverlapCache(v)
	var ops []*language.OperationDefinition
	anonymous := 0
	opNames := map[string]*language.OperationDefinition{}
	for _, d := range v.doc.Definitions {
		switch d := d.(type) {
		case *language.OperationDefinition:
			ops = append(ops, d)
			if d.Name == "" {
				anonymous++
				continue
			}
			if prev, dup := opNames[d.Name]; dup {
				v.report(fmt.Sprintf("There can be only one operation named %q.", d.Name), prev.NameLoc, d.NameLoc)
				continue
			}
			opNames[d.Name] = d
		case *language.FragmentDefinition:
			if prev, dup := v.frags[d.Name]; dup {
				v.report(fmt.Sprintf("There can be only one fragment named %q.", d.Name), prev.NameLoc, d.NameLoc)
				continue
			}
			v.frags[d.Name] = d
		case *language.SchemaDefinition:
			v.report("The schema definition is not executable.", d.Location())
		default:
			v.report(fmt.Sprintf("The %q definition is not executable.", definitionName(d)), d.Location())
		}
	}
	if anonymous > 0 && len(ops) > 1 {
		for _, op := range ops {
			if op.Name == "" {
				v.report("This anonymous operation must be the only defined operation.", op.Location())
			}
		}
	}
	for _, d := range v.doc.Definitions {
		switch d := d.(type) {
		case *language.OperationDefinition:
			v.operation(d)
		case *language.FragmentDefinition:
			if v.frags[d.Name] != d {
				continue
			}
			v.fragment(d)
		}
	}
	v.fragmentCycles()
	v.unusedFragments(ops)
	for _, op := range ops {
		v.variables(op)
	}
}

func definitionName(d language.Definition) string {
	switch d := d.(type) {
	case *language.ScalarTypeDefinition:
		return d.Name
	case *language.ObjectTypeDefinition:
		return d.Name
	case *language.InterfaceTypeDefinition:
		return d.Name
	case *language.UnionTypeDefinition:
		return d.Name
	case *language.EnumTypeDefinition:
		return d.Name
	case *language.InputObjectTypeDefinition:
		return d.Name
	case *language.DirectiveDefinition:
		return "@" + d.Name
	}
	return "?"
}

var opLocation = map[string]string{language.OpQuery: "QUERY", language.OpMutation: "MUTATION", language.OpSubscription: "SUBSCRIPTION"}

func (v *validator) operation(op *language.OperationDefinition) {
	v.cur = &defInfo{}
	v.info[op] = v.cur
	root := v.s.root(op.Operation)
	if root == nil {
		v.report(fmt.Sprintf("Schema is not configured to execute %s operation.", op.Operation), op.Location())
	}
	seen := map[string]*language.VariableDefinition{}
	for _, d := range op.VariableDefinitions {
		if prev, dup := seen[d.Variable]; dup {
			v.report(fmt.Sprintf("There can be only one variable named \"$%s\".", d.Variable), prev.Location(), d.Location())
		} else {
			seen[d.Variable] = d
		}
		named := innerNamed(d.Type)
		t := v.s.typeFromRef(d.Type)
		if v.s.types[named.Name] == nil {
			v.unknownType(named)
		} else if t != nil && !isInputType(t) {
			v.report(fmt.Sprintf("Variable \"$%s\" cannot be non-input type %q.", d.Variable, d.Type.String()), d.Type.Location())
		}
		if d.DefaultValue != nil && t != nil && isInputType(t) {
			v.s.checkLiteral(d.DefaultValue, t, func(at language.Loc, m string) { v.report(m, at) })
		}
		v.directives(d.Directives, "VARIABLE_DEFINITION")
	}
	v.directives(op.Directives, opLocation[op.Operation])
	if op.Operation == language.OpSubscription && root != nil {
		v.singleField(op, root)
	}
	var parent NamedType
	if root != nil {
		parent = root
	}
	v.selectionSet(op.SelectionSet, parent)
}

func innerNamed(t language.TypeRef) *language.NamedType {
	for {
		switch x := t.(type) {
		case *language.NamedType:
			return x
		case *language.ListType:
			t = x.Type
		case *language.NonNullType:
			t = x.Type
		}
	}
}

func (v *validator) unknownType(n *language.NamedType) {
	var names []string
	for name := range v.s.types {
		names = append(names, name)
	}
	sort.Strings(names)
	v.report(fmt.Sprintf("Unknown type %q.%s", n.Name, didYouMean(n.Name, names)), n.Location())
}

// singleField is SingleFieldSubscriptions: one root field, not an
// introspection one.
func (v *validator) singleField(op *language.OperationDefinition, root *Object) {
	fields := map[string][]*language.Field{}
	var order []string
	seen := map[string]bool{}
	var collect func(ss *language.SelectionSet)
	collect = func(ss *language.SelectionSet) {
		for _, sel := range ss.Selections {
			switch sel := sel.(type) {
			case *language.Field:
				k := sel.ResponseKey()
				if _, ok := fields[k]; !ok {
					order = append(order, k)
				}
				fields[k] = append(fields[k], sel)
			case *language.InlineFragment:
				collect(sel.SelectionSet)
			case *language.FragmentSpread:
				if f := v.frags[sel.Name]; f != nil && !seen[sel.Name] {
					seen[sel.Name] = true
					collect(f.SelectionSet)
				}
			}
		}
	}
	collect(op.SelectionSet)
	name := "Anonymous Subscription"
	if op.Name != "" {
		name = fmt.Sprintf("Subscription %q", op.Name)
	}
	if len(order) > 1 {
		var locs []language.Loc
		for _, k := range order[1:] {
			for _, f := range fields[k] {
				locs = append(locs, f.Location())
			}
		}
		v.report(name+" must select only one top level field.", locs...)
	}
	for _, k := range order {
		for _, f := range fields[k] {
			if strings.HasPrefix(f.Name, "__") {
				v.report(name+" must not select an introspection top level field.", f.Location())
			}
		}
	}
}

func (v *validator) fragment(f *language.FragmentDefinition) {
	v.cur = &defInfo{}
	v.info[f] = v.cur
	var parent NamedType
	if t := v.s.types[f.TypeCondition.Name]; t == nil {
		v.unknownType(f.TypeCondition)
	} else if !isComposite(t) {
		v.report(fmt.Sprintf("Fragment %q cannot condition on non composite type %q.", f.Name, f.TypeCondition.Name), f.TypeCondition.Location())
	} else {
		parent = t
	}
	v.directives(f.Directives, "FRAGMENT_DEFINITION")
	v.selectionSet(f.SelectionSet, parent)
}

// fieldDef is parent's field name, the meta-fields included; nil when it
// has none.
func (s *Schema) fieldDef(parent NamedType, name string) *FieldDef {
	if name == "__typename" && isComposite(parent) {
		return typenameMetaField
	}
	if o, ok := parent.(*Object); ok && o == s.query {
		switch name {
		case "__schema":
			return schemaMetaField
		case "__type":
			return typeMetaField
		}
	}
	return s.fields[parent][name]
}

func (v *validator) selectionSet(ss *language.SelectionSet, parent NamedType) {
	if parent != nil && isComposite(parent) {
		v.overlap.check(ss, parent)
	}
	for _, sel := range ss.Selections {
		switch sel := sel.(type) {
		case *language.Field:
			v.field(sel, parent)
		case *language.InlineFragment:
			typ := parent
			if sel.TypeCondition != nil {
				t := v.s.types[sel.TypeCondition.Name]
				switch {
				case t == nil:
					v.unknownType(sel.TypeCondition)
					typ = nil
				case !isComposite(t):
					v.report(fmt.Sprintf("Fragment cannot condition on non composite type %q.", sel.TypeCondition.Name), sel.TypeCondition.Location())
					typ = nil
				default:
					typ = t
					if parent != nil && isComposite(parent) && !v.s.overlap(t, parent) {
						v.report(fmt.Sprintf("Fragment cannot be spread here as objects of type %q can never be of type %q.", parent.TypeName(), t.TypeName()), sel.Location())
					}
				}
			}
			v.directives(sel.Directives, "INLINE_FRAGMENT")
			v.selectionSet(sel.SelectionSet, typ)
		case *language.FragmentSpread:
			frag := v.frags[sel.Name]
			v.directives(sel.Directives, "FRAGMENT_SPREAD")
			if frag == nil {
				v.report(fmt.Sprintf("Unknown fragment %q.", sel.Name), sel.Location())
				continue
			}
			v.cur.spreads = append(v.cur.spreads, sel)
			if t := v.s.types[frag.TypeCondition.Name]; t != nil && isComposite(t) && parent != nil && isComposite(parent) && !v.s.overlap(t, parent) {
				v.report(fmt.Sprintf("Fragment %q cannot be spread here as objects of type %q can never be of type %q.", sel.Name, parent.TypeName(), t.TypeName()), sel.Location())
			}
		}
	}
}

func (v *validator) field(f *language.Field, parent NamedType) {
	var def *FieldDef
	if parent != nil {
		def = v.s.fieldDef(parent, f.Name)
		if def == nil && isComposite(parent) {
			v.report(fmt.Sprintf("Cannot query field %q on type %q.%s", f.Name, parent.TypeName(), v.fieldHint(parent, f.Name)), f.Location())
		}
		if def != nil && !v.introspection && (def == schemaMetaField || def == typeMetaField) {
			v.report(fmt.Sprintf("Introspection is disabled: %q can't be queried.", f.Name), f.Location())
		}
	}
	v.directives(f.Directives, "FIELD")
	if def != nil {
		v.arguments(f.Arguments, def.Args, f.Location(), func(arg string) string {
			return fmt.Sprintf("Unknown argument %q on field \"%s.%s\".", arg, parent.TypeName(), f.Name)
		}, func(a *ArgDef) string {
			return fmt.Sprintf("Field %q argument %q of type %q is required, but it was not provided.", f.Name, a.Name, TypeString(a.Type))
		})
	} else {
		v.arguments(f.Arguments, nil, f.Location(), nil, nil)
	}
	var sub NamedType
	if def != nil {
		named := NamedOf(def.Type)
		switch {
		case isLeaf(named) && f.SelectionSet != nil:
			v.report(fmt.Sprintf("Field %q must not have a selection since type %q has no subfields.", f.Name, TypeString(def.Type)), f.SelectionSet.Location())
		case !isLeaf(named) && f.SelectionSet == nil:
			v.report(fmt.Sprintf("Field %q of type %q must have a selection of subfields. Did you mean \"%s { ... }\"?", f.Name, TypeString(def.Type), f.Name), f.Location())
		}
		if isComposite(named) {
			sub = named
		}
	}
	if f.SelectionSet != nil {
		v.selectionSet(f.SelectionSet, sub)
	}
}

// fieldHint suggests, for a field parent lacks, the inline fragments on
// types having it, else parent's fields named like it.
func (v *validator) fieldHint(parent NamedType, name string) string {
	if isAbstract(parent) {
		var types []string
		for _, o := range v.s.PossibleTypes(parent) {
			if v.s.fields[o][name] != nil {
				types = append(types, o.Name)
			}
		}
		for iface, set := range v.s.possibleSet {
			if i, ok := iface.(*Interface); ok && NamedType(i) != parent && v.s.fields[i][name] != nil {
				all := true
				for o := range set {
					if !v.s.IsPossibleType(parent, o) {
						all = false
					}
				}
				if all && len(set) > 0 {
					types = append(types, i.Name)
				}
			}
		}
		if len(types) > 0 {
			sort.Strings(types)
			return strings.Replace(quotedList(types), "Did you mean", "Did you mean to use an inline fragment on", 1)
		}
	}
	var names []string
	for n := range v.s.fields[parent] {
		names = append(names, n)
	}
	sort.Strings(names)
	return didYouMean(name, names)
}

func quotedList(s []string) string {
	if len(s) > 5 {
		s = s[:5]
	}
	quoted := make([]string, len(s))
	for i, x := range s {
		quoted[i] = fmt.Sprintf("%q", x)
	}
	switch len(quoted) {
	case 1:
		return " Did you mean " + quoted[0] + "?"
	case 2:
		return " Did you mean " + quoted[0] + " or " + quoted[1] + "?"
	}
	return " Did you mean " + strings.Join(quoted[:len(quoted)-1], ", ") + ", or " + quoted[len(quoted)-1] + "?"
}

// arguments checks a field's or directive's arguments against defs (nil
// when the field is unknown: only the values' variables are recorded).
func (v *validator) arguments(args []*language.Argument, defs []*ArgDef, at language.Loc, unknown func(string) string, required func(*ArgDef) string) {
	seen := map[string]*language.Argument{}
	for _, a := range args {
		if prev, dup := seen[a.Name]; dup {
			v.report(fmt.Sprintf("There can be only one argument named %q.", a.Name), prev.Location(), a.Location())
		} else {
			seen[a.Name] = a
		}
		if unknown == nil {
			v.value(a.Value, nil, false)
			continue
		}
		var def *ArgDef
		for _, d := range defs {
			if d.Name == a.Name {
				def = d
			}
		}
		if def == nil {
			names := make([]string, len(defs))
			for i, d := range defs {
				names[i] = d.Name
			}
			v.report(unknown(a.Name)+didYouMean(a.Name, names), a.Location())
			v.value(a.Value, nil, false)
			continue
		}
		v.value(a.Value, def.Type, def.Default != nil)
		v.s.checkLiteral(a.Value, def.Type, func(l language.Loc, m string) { v.report(m, l) })
	}
	if required == nil {
		return
	}
	for _, d := range defs {
		if _, nn := d.Type.(*NonNull); nn && d.Default == nil && seen[d.Name] == nil {
			v.report(required(d), at)
		}
	}
}

// value records the variables a value uses, each with the type expected
// where it is, and checks object fields are unique.
func (v *validator) value(val language.Value, t TypeDef, hasDefault bool) {
	switch x := val.(type) {
	case *language.Variable:
		v.cur.vars = append(v.cur.vars, varUse{x.Name, x.Location(), t, hasDefault})
	case *language.ListValue:
		var of TypeDef
		if l, ok := nullable(t).(*List); ok {
			of = l.Of
		}
		for _, item := range x.Values {
			v.value(item, of, false)
		}
	case *language.ObjectValue:
		seen := map[string]*language.ObjectField{}
		io, _ := nullable(t).(*InputObject)
		for _, f := range x.Fields {
			if prev, dup := seen[f.Name]; dup {
				v.report(fmt.Sprintf("There can be only one input field named %q.", f.Name), prev.Location(), f.Location())
			} else {
				seen[f.Name] = f
			}
			var ft TypeDef
			def := false
			if io != nil {
				if fd := v.s.inputFields[io][f.Name]; fd != nil {
					ft, def = fd.Type, fd.Default != nil
				}
			}
			v.value(f.Value, ft, def)
		}
	}
}

func (v *validator) directives(ds []*language.Directive, location string) {
	seen := map[string]*language.Directive{}
	for _, d := range ds {
		def := v.s.directives[d.Name]
		if def == nil {
			v.report(fmt.Sprintf("Unknown directive \"@%s\".", d.Name), d.Location())
			v.arguments(d.Arguments, nil, d.Location(), nil, nil)
			continue
		}
		if !slices.Contains(def.Locations, location) {
			v.report(fmt.Sprintf("Directive \"@%s\" may not be used on %s.", d.Name, location), d.Location())
		}
		if prev, dup := seen[d.Name]; dup && !def.Repeatable {
			v.report(fmt.Sprintf("The directive \"@%s\" can only be used once at this location.", d.Name), prev.Location(), d.Location())
		} else if !dup {
			seen[d.Name] = d
		}
		v.arguments(d.Arguments, def.Args, d.Location(), func(arg string) string {
			return fmt.Sprintf("Unknown argument %q on directive \"@%s\".", arg, d.Name)
		}, func(a *ArgDef) string {
			return fmt.Sprintf("Directive \"@%s\" argument %q of type %q is required, but it was not provided.", d.Name, a.Name, TypeString(a.Type))
		})
	}
}

// fragmentCycles is NoFragmentCycles: a fragment spreading itself,
// directly or through others.
func (v *validator) fragmentCycles() {
	visited := map[string]bool{}
	var path []*language.FragmentSpread
	index := map[string]int{}
	var detect func(f *language.FragmentDefinition)
	detect = func(f *language.FragmentDefinition) {
		if visited[f.Name] {
			return
		}
		visited[f.Name] = true
		info := v.info[f]
		if info == nil {
			return
		}
		index[f.Name] = len(path)
		for _, sp := range info.spreads {
			at, onPath := index[sp.Name]
			path = append(path, sp)
			if !onPath {
				if next := v.frags[sp.Name]; next != nil {
					detect(next)
				}
			} else {
				cycle := path[at:]
				var via []string
				locs := make([]language.Loc, len(cycle))
				for i, s := range cycle {
					locs[i] = s.Location()
					if i < len(cycle)-1 {
						via = append(via, fmt.Sprintf("%q", s.Name))
					}
				}
				msg := fmt.Sprintf("Cannot spread fragment %q within itself", sp.Name)
				if len(via) > 0 {
					msg += " via " + strings.Join(via, ", ")
				}
				v.report(msg+".", locs...)
			}
			path = path[:len(path)-1]
		}
		delete(index, f.Name)
	}
	for _, d := range v.doc.Definitions {
		if f, ok := d.(*language.FragmentDefinition); ok && v.frags[f.Name] == f {
			detect(f)
		}
	}
}

// reachable is every fragment a definition spreads, directly or through
// others.
func (v *validator) reachable(d language.Definition) []*language.FragmentDefinition {
	var out []*language.FragmentDefinition
	seen := map[string]bool{}
	var walk func(d language.Definition)
	walk = func(d language.Definition) {
		info := v.info[d]
		if info == nil {
			return
		}
		for _, sp := range info.spreads {
			if seen[sp.Name] {
				continue
			}
			seen[sp.Name] = true
			if f := v.frags[sp.Name]; f != nil {
				out = append(out, f)
				walk(f)
			}
		}
	}
	walk(d)
	return out
}

func (v *validator) unusedFragments(ops []*language.OperationDefinition) {
	used := map[string]bool{}
	for _, op := range ops {
		for _, f := range v.reachable(op) {
			used[f.Name] = true
		}
	}
	for _, d := range v.doc.Definitions {
		if f, ok := d.(*language.FragmentDefinition); ok && !used[f.Name] {
			v.report(fmt.Sprintf("Fragment %q is never used.", f.Name), f.Location())
		}
	}
}

// variables is NoUndefinedVariables, NoUnusedVariables and
// VariablesInAllowedPosition for an operation, over the fragments it
// reaches too.
func (v *validator) variables(op *language.OperationDefinition) {
	uses := slices.Clone(v.info[op].vars)
	for _, f := range v.reachable(op) {
		uses = append(uses, v.info[f].vars...)
	}
	defs := map[string]*language.VariableDefinition{}
	for _, d := range op.VariableDefinitions {
		if _, dup := defs[d.Variable]; !dup {
			defs[d.Variable] = d
		}
	}
	used := map[string]bool{}
	reported := map[string]bool{}
	for _, u := range uses {
		used[u.name] = true
		d := defs[u.name]
		if d == nil {
			key := fmt.Sprintf("%s@%d:%d", u.name, u.loc.Line, u.loc.Column)
			if reported[key] {
				continue
			}
			reported[key] = true
			if op.Name != "" {
				v.report(fmt.Sprintf("Variable \"$%s\" is not defined by operation %q.", u.name, op.Name), u.loc, op.Location())
			} else {
				v.report(fmt.Sprintf("Variable \"$%s\" is not defined.", u.name), u.loc, op.Location())
			}
			continue
		}
		if u.typ == nil {
			continue
		}
		vt := v.s.typeFromRef(d.Type)
		if vt == nil || !isInputType(vt) {
			continue
		}
		if !v.allowed(vt, d, u) {
			key := fmt.Sprintf("pos %s@%d:%d", u.name, u.loc.Line, u.loc.Column)
			if reported[key] {
				continue
			}
			reported[key] = true
			v.report(fmt.Sprintf("Variable \"$%s\" of type %q used in position expecting type %q.", u.name, d.Type.String(), TypeString(u.typ)), d.Location(), u.loc)
		}
	}
	for _, d := range op.VariableDefinitions {
		if !used[d.Variable] {
			if op.Name != "" {
				v.report(fmt.Sprintf("Variable \"$%s\" is never used in operation %q.", d.Variable, op.Name), d.Location())
			} else {
				v.report(fmt.Sprintf("Variable \"$%s\" is never used.", d.Variable), d.Location())
			}
		}
	}
}

// allowed is whether a variable of type vt may be used where u expects
// its type: a nullable variable goes where a non-null is expected only
// with a non-null default, the variable's or the position's.
func (v *validator) allowed(vt TypeDef, d *language.VariableDefinition, u varUse) bool {
	loc := u.typ
	if lnn, ok := loc.(*NonNull); ok {
		if _, vnn := vt.(*NonNull); !vnn {
			_, null := d.DefaultValue.(*language.NullValue)
			if !(d.DefaultValue != nil && !null) && !u.hasDefault {
				return false
			}
			return v.s.isSubType(vt, lnn.Of)
		}
	}
	return v.s.isSubType(vt, loc)
}
