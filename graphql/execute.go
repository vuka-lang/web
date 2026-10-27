package graphql

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"sort"
	"sync"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// PanicError is a resolver's panic, given to Config.Present as the field's
// error: Value is what it panicked with, Stack where.
type PanicError struct {
	Field string // "Query.users"
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("%s panicked: %v", e.Field, e.Value) }

// propagated is a null raised by a non-null field whose error is recorded
// already: its parent becomes null in turn.
var propagated = &Error{Message: "null propagated"}

// execution is one operation's run.
type execution struct {
	srv   *Server
	s     *Schema
	op    *language.OperationDefinition
	frags map[string]*language.FragmentDefinition
	vars  map[string]any
	// varset is vars with what each was given, coerced again where used.
	varset *varValues
	sem    chan struct{} // goroutines a run may start beside its own; full, work runs inline

	mu   sync.Mutex
	errs []orderedErr
}

// orderedErr is an error with the position of its field in the response,
// so errors read in the order a serial run would have met them.
type orderedErr struct {
	ord []int
	seq int
	err *Error
}

// fieldGroup is the fields of one response key: one field, as the
// specification's CollectFields merges them.
type fieldGroup struct {
	key   string
	nodes []*language.Field
}

func (e *execution) record(ord []int, errs ...*Error) {
	e.mu.Lock()
	for _, err := range errs {
		e.errs = append(e.errs, orderedErr{slices.Clone(ord), len(e.errs), err})
	}
	e.mu.Unlock()
}

func (e *execution) errors() []*Error {
	sort.SliceStable(e.errs, func(i, j int) bool {
		a, b := e.errs[i], e.errs[j]
		if c := slices.Compare(a.ord, b.ord); c != 0 {
			return c < 0
		}
		return a.seq < b.seq
	})
	out := make([]*Error, len(e.errs))
	for i, x := range e.errs {
		out[i] = x.err
	}
	return out
}

func locsOf(nodes []*language.Field) []Location {
	out := make([]Location, len(nodes))
	for i, n := range nodes {
		out[i] = Location{n.Line, n.Column}
	}
	return out
}

// newErr is an error the engine raises at a field.
func newErr(msg string, path []any, nodes []*language.Field) *Error {
	return &Error{Message: msg, Path: slices.Clone(path), Locations: locsOf(nodes)}
}

// run executes the operation: its data, nil when null propagated to it.
func (e *execution) run(ctx context.Context) *OrderedMap {
	root := e.s.root(e.op.Operation)
	groups := e.collect(root, []*language.SelectionSet{e.op.SelectionSet})
	data, ok := e.fields(ctx, root, nil, nil, nil, groups, e.op.Operation == language.OpMutation)
	if !ok {
		return nil
	}
	return data
}

// collect is CollectFields over selection sets, for an object type: the
// fields by response key, in order, fragments and @skip/@include applied.
func (e *execution) collect(obj *Object, sets []*language.SelectionSet) []fieldGroup {
	var groups []fieldGroup
	index := map[string]int{}
	visited := map[string]bool{}
	var walk func(ss *language.SelectionSet)
	walk = func(ss *language.SelectionSet) {
		for _, sel := range ss.Selections {
			switch sel := sel.(type) {
			case *language.Field:
				if !e.include(sel.Directives) {
					continue
				}
				k := sel.ResponseKey()
				if i, ok := index[k]; ok {
					groups[i].nodes = append(groups[i].nodes, sel)
					continue
				}
				index[k] = len(groups)
				groups = append(groups, fieldGroup{k, []*language.Field{sel}})
			case *language.FragmentSpread:
				if visited[sel.Name] || !e.include(sel.Directives) {
					continue
				}
				visited[sel.Name] = true
				f := e.frags[sel.Name]
				if f == nil || !e.applies(obj, f.TypeCondition.Name) {
					continue
				}
				walk(f.SelectionSet)
			case *language.InlineFragment:
				if !e.include(sel.Directives) {
					continue
				}
				if sel.TypeCondition != nil && !e.applies(obj, sel.TypeCondition.Name) {
					continue
				}
				walk(sel.SelectionSet)
			}
		}
	}
	for _, ss := range sets {
		walk(ss)
	}
	return groups
}

func (e *execution) applies(obj *Object, cond string) bool {
	t := e.s.types[cond]
	if t == nil {
		return false
	}
	if o, ok := t.(*Object); ok {
		return o == obj
	}
	return e.s.IsPossibleType(t, obj)
}

// include applies @skip and @include.
func (e *execution) include(ds []*language.Directive) bool {
	for _, d := range ds {
		var def *Directive
		switch d.Name {
		case "skip":
			def = SkipDirective
		case "include":
			def = IncludeDirective
		default:
			continue
		}
		args, err := e.s.coerceArgs(def.Args, d.Arguments, e.varset)
		if err != nil {
			continue
		}
		b, _ := args["if"].(bool)
		if d.Name == "skip" && b || d.Name == "include" && !b {
			return false
		}
	}
	return true
}

// fields executes an object's fields: the object's value, false when a
// non-null field's null propagates to it. serial runs them one after the
// other (a mutation's root fields); otherwise fields that aren't Trivial
// run concurrently when there are several.
func (e *execution) fields(ctx context.Context, obj *Object, src any, path []any, ord []int, groups []fieldGroup, serial bool) (*OrderedMap, bool) {
	vals := make([]any, len(groups))
	failed := make([]bool, len(groups))
	skip := make([]bool, len(groups))
	run := func(i int) {
		g := groups[i]
		v, err, ok := e.field(ctx, obj, src, append(path[:len(path):len(path)], g.key), append(ord[:len(ord):len(ord)], i), g)
		if !ok {
			skip[i] = true
			return
		}
		vals[i], failed[i] = v, err != nil
	}
	parallel := 0
	if !serial {
		for _, g := range groups {
			if def := e.s.fieldDef(obj, g.nodes[0].Name); def != nil && !def.Trivial {
				parallel++
			}
		}
	}
	if parallel < 2 {
		for i := range groups {
			run(i)
		}
	} else {
		var wg sync.WaitGroup
		for i, g := range groups {
			def := e.s.fieldDef(obj, g.nodes[0].Name)
			if def == nil || def.Trivial {
				run(i)
				continue
			}
			select {
			case e.sem <- struct{}{}:
				wg.Add(1)
				go func() {
					defer func() { <-e.sem; wg.Done() }()
					run(i)
				}()
			default:
				run(i)
			}
		}
		wg.Wait()
	}
	m := NewOrderedMap(len(groups))
	for i, g := range groups {
		if skip[i] {
			continue
		}
		if failed[i] {
			return nil, false
		}
		m.keys = append(m.keys, g.key)
		m.values = append(m.values, vals[i])
	}
	return m, true
}

// field executes one field: its value, and propagated when it is null
// because of a non-null's error; ok false for a field the type lacks
// (left out of the response).
func (e *execution) field(ctx context.Context, obj *Object, src any, path []any, ord []int, g fieldGroup) (val any, err *Error, ok bool) {
	node := g.nodes[0]
	if node.Name == "__typename" {
		return obj.Name, nil, true
	}
	def := e.s.fieldDef(obj, node.Name)
	if def == nil {
		return nil, nil, false
	}
	info := &ResolveInfo{FieldName: node.Name, Path: path, ParentType: obj, ReturnType: def.Type, Operation: e.op, Schema: e.s, Variables: e.vars}
	_, nonNull := def.Type.(*NonNull)
	fail := func(errs ...*Error) (any, *Error, bool) {
		for _, x := range errs {
			if x.Path == nil {
				x.Path = slices.Clone(path)
			}
			if x.Locations == nil {
				x.Locations = locsOf(g.nodes)
			}
		}
		e.record(ord, errs...)
		if nonNull {
			return nil, propagated, true
		}
		return nil, nil, true
	}
	args, aerr := e.s.coerceArgs(def.Args, node.Arguments, e.varset)
	if aerr != nil {
		x := newErr(aerr.Error(), path, g.nodes)
		x.Extensions = code(CodeBadUserInput)
		return fail(x)
	}
	if cerr := ctx.Err(); cerr != nil {
		return fail(e.srv.ctxError(ctx, path, g.nodes))
	}
	if def.Guard != nil {
		if gerr := def.Guard(ctx); gerr != nil {
			return fail(e.srv.present(ctx, gerr)...)
		}
	}
	res, rerr := e.resolve(ctx, def, ResolveParams{Source: src, Args: args, Info: info})
	if rerr != nil {
		if errors.Is(rerr, context.DeadlineExceeded) && ctx.Err() != nil || errors.Is(rerr, context.Canceled) && ctx.Err() != nil {
			return fail(e.srv.ctxError(ctx, path, g.nodes))
		}
		return fail(e.srv.present(ctx, rerr)...)
	}
	v, cerr := e.complete(ctx, def.Type, g.nodes, info, path, ord, res)
	if cerr != nil {
		if cerr != propagated {
			e.record(ord, cerr)
		}
		if nonNull {
			return nil, propagated, true
		}
		return nil, nil, true
	}
	return v, nil, true
}

func (e *execution) resolve(ctx context.Context, def *FieldDef, p ResolveParams) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			v, err = nil, &PanicError{Field: p.Info.ParentType.Name + "." + def.Name, Value: r, Stack: debug.Stack()}
		}
	}()
	if def.Resolve != nil {
		return def.Resolve(ctx, p)
	}
	switch src := p.Source.(type) {
	case map[string]any:
		return src[def.Name], nil
	case *OrderedMap:
		v, _ := src.Get(def.Name)
		return v, nil
	}
	return nil, nil
}

// complete is CompleteValue: the resolved value v as type t gives it, or
// the error it raises (propagated when recorded already).
func (e *execution) complete(ctx context.Context, t TypeDef, nodes []*language.Field, info *ResolveInfo, path []any, ord []int, v any) (any, *Error) {
	if nn, ok := t.(*NonNull); ok {
		val, err := e.complete(ctx, nn.Of, nodes, info, path, ord, v)
		if err != nil {
			return nil, err
		}
		if val == nil {
			return nil, newErr(fmt.Sprintf("Cannot return null for non-nullable field %s.%s.", info.ParentType.Name, info.FieldName), path, nodes)
		}
		return val, nil
	}
	if v == nil {
		return nil, nil
	}
	switch t := t.(type) {
	case *List:
		return e.completeList(ctx, t, nodes, info, path, ord, v)
	case *Scalar:
		out, err := t.Serialize(v)
		if err != nil {
			return nil, newErr(err.Error(), path, nodes)
		}
		return out, nil
	case *Enum:
		if t.Serialize != nil {
			name, err := t.Serialize(v)
			if err != nil {
				return nil, newErr(err.Error(), path, nodes)
			}
			return name, nil
		}
		for _, d := range t.Values {
			if equal(enumValue(d), v) {
				return d.Name, nil
			}
		}
		return nil, newErr(fmt.Sprintf("Enum %q cannot represent value: %s", t.Name, inspect(v)), path, nodes)
	case *Object:
		return e.completeObject(ctx, t, nodes, path, ord, v)
	case *Interface, *Union:
		obj, err := e.resolveType(t.(NamedType), v, info)
		if err != "" {
			return nil, newErr(err, path, nodes)
		}
		return e.completeObject(ctx, obj, nodes, path, ord, v)
	}
	return nil, newErr(fmt.Sprintf("Cannot complete value of unexpected type %q.", TypeString(t)), path, nodes)
}

// equal compares values that may not be comparable.
func equal(a, b any) (eq bool) {
	defer func() {
		if recover() != nil {
			eq = false
		}
	}()
	return a == b
}

func (e *execution) completeList(ctx context.Context, t *List, nodes []*language.Field, info *ResolveInfo, path []any, ord []int, v any) (any, *Error) {
	var n int
	var at func(int) any
	if t.Items != nil {
		var ok bool
		if n, at, ok = t.Items(v); !ok {
			return nil, newErr(fmt.Sprintf("Expected Iterable, but did not find one for field \"%s.%s\".", info.ParentType.Name, info.FieldName), path, nodes)
		}
	} else {
		items, ok := v.([]any)
		if !ok {
			return nil, newErr(fmt.Sprintf("Expected Iterable, but did not find one for field \"%s.%s\".", info.ParentType.Name, info.FieldName), path, nodes)
		}
		n, at = len(items), func(i int) any { return items[i] }
	}
	out := make([]any, n)
	errs := make([]*Error, n)
	_, itemNonNull := t.Of.(*NonNull)
	one := func(i int) {
		val, err := e.complete(ctx, t.Of, nodes, info, append(path[:len(path):len(path)], i), append(ord[:len(ord):len(ord)], i), at(i))
		out[i], errs[i] = val, err
	}
	if n > 1 && isComposite(NamedOf(t.Of)) {
		var wg sync.WaitGroup
		for i := range n {
			select {
			case e.sem <- struct{}{}:
				wg.Add(1)
				go func() {
					defer func() { <-e.sem; wg.Done() }()
					one(i)
				}()
			default:
				one(i)
			}
		}
		wg.Wait()
	} else {
		for i := range n {
			one(i)
		}
	}
	for i, err := range errs {
		if err == nil {
			continue
		}
		if itemNonNull {
			// The first item's error decides; the others' are recorded.
			if err != propagated {
				for _, other := range errs[i+1:] {
					if other != nil && other != propagated {
						e.record(append(ord[:len(ord):len(ord)], n), other)
					}
				}
			}
			return nil, err
		}
		if err != propagated {
			e.record(append(ord[:len(ord):len(ord)], i), err)
		}
		out[i] = nil
	}
	return out, nil
}

func (e *execution) completeObject(ctx context.Context, t *Object, nodes []*language.Field, path []any, ord []int, v any) (any, *Error) {
	if t.Value != nil {
		if v = t.Value(v); v == nil {
			return nil, nil
		}
	}
	if t.IsTypeOf != nil && !t.IsTypeOf(v) {
		return nil, newErr(fmt.Sprintf("Expected value of type %q but got: %s.", t.Name, describeValue(v)), path, nodes)
	}
	sets := make([]*language.SelectionSet, 0, len(nodes))
	for _, n := range nodes {
		if n.SelectionSet != nil {
			sets = append(sets, n.SelectionSet)
		}
	}
	m, ok := e.fields(ctx, t, v, path, ord, e.collect(t, sets), false)
	if !ok {
		return nil, propagated
	}
	return m, nil
}

func describeValue(v any) string {
	switch v.(type) {
	case map[string]any, []any, string, bool, float64, int:
		return inspect(v)
	}
	return fmt.Sprintf("%T", v)
}

// resolveType is the object an abstract type's value is; a message when
// there is none.
func (e *execution) resolveType(t NamedType, v any, info *ResolveInfo) (*Object, string) {
	var rt func(any) *Object
	switch x := t.(type) {
	case *Interface:
		rt = x.ResolveType
	case *Union:
		rt = x.ResolveType
	}
	if rt != nil {
		obj := rt(v)
		if obj == nil {
			return nil, fmt.Sprintf("Abstract type %q must resolve to an Object type at runtime for field \"%s.%s\". Either the %q type should provide a ResolveType function or each possible type should provide an IsTypeOf function.", t.TypeName(), info.ParentType.Name, info.FieldName, t.TypeName())
		}
		if !e.s.IsPossibleType(t, obj) {
			return nil, fmt.Sprintf("Runtime Object type %q is not a possible type for %q.", obj.Name, t.TypeName())
		}
		return obj, ""
	}
	for _, o := range e.s.PossibleTypes(t) {
		if o.IsTypeOf != nil && o.IsTypeOf(v) {
			return o, ""
		}
	}
	return nil, fmt.Sprintf("Abstract type %q must resolve to an Object type at runtime for field \"%s.%s\". Either the %q type should provide a ResolveType function or each possible type should provide an IsTypeOf function.", t.TypeName(), info.ParentType.Name, info.FieldName, t.TypeName())
}
