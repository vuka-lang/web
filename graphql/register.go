package graphql

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/vuka/v2/diag"
	"github.com/vuka-lang/web/v2"
)

var registry struct {
	sync.Mutex
	pkgs []Package
}

// Register adds a package's operations and types to the schema of every
// app serving web.GraphQL. The code the compiler generates for a package's
// @graphql.Query and @graphql.Mutation functions calls it at init.
func Register(p Package) {
	registry.Lock()
	registry.pkgs = append(registry.pkgs, p)
	registry.Unlock()
}

func registered() []Package {
	registry.Lock()
	defer registry.Unlock()
	return slices.Clone(registry.pkgs)
}

// assembly builds an app's schema from the registered packages: names
// decided (Naming, attributes), dependencies and gates resolved against
// the app.
type assembly struct {
	host     web.GraphQLHost
	field    func(string) string // Naming.Fields
	bindings map[bindKey]binding
	objects  map[reflect.Type]*Object
	inputs   map[reflect.Type]*InputObject
	enums    map[reflect.Type]*Enum
	texts    map[reflect.Type]*Scalar
	names    map[string]string // a type's name → what it names, for collisions
	loaders  map[reflect.Type]*LoaderDef
	errs     []error
}

type bindKey struct {
	kind bindingKind
	t    reflect.Type
}

func (a *assembly) fail(code diag.Code, help, format string, args ...any) {
	d := diag.New(code, format, args...)
	if help != "" {
		d = d.WithHelp("%s", help)
	}
	a.errs = append(a.errs, d)
}

// goName is a Go type as messages name it: example.com/app/graph.User.
func goName(t reflect.Type) string {
	if t.PkgPath() == "" {
		return t.String()
	}
	return t.PkgPath() + "." + t.Name()
}

// typeName is the default GraphQL name of a Go type: its name; an
// instantiation's type arguments' names before its own (Page[User] is
// UserPage).
func typeName(t reflect.Type) string {
	name := t.Name()
	base, args, ok := strings.Cut(name, "[")
	if !ok {
		return name
	}
	var b strings.Builder
	for _, arg := range strings.Split(strings.TrimSuffix(args, "]"), ",") {
		arg = strings.TrimLeft(strings.TrimSpace(arg), "*[]")
		if i := strings.LastIndexByte(arg, '.'); i >= 0 {
			arg = arg[i+1:]
		}
		b.WriteString(strings.ToUpper(arg[:1]) + arg[1:])
	}
	return b.String() + base
}

// claim takes name for what, reporting a name taken already by another.
func (a *assembly) claim(name, what string) bool {
	if strings.HasPrefix(name, "__") {
		a.fail("E0804", "rename it with @graphql.Type", "%s: the GraphQL name %q starts with __, which the specification reserves for introspection", what, name)
		return false
	}
	if prev, ok := a.names[name]; ok && prev != what {
		a.fail("E0803", "rename one with @graphql.Type(\"…\") on its type (or @graphql.Input on an input)", "two types are named %q in the GraphQL schema: %s and %s", name, prev, what)
		return false
	}
	a.names[name] = what
	return true
}

// resolve is t with the types it names built: a typeRef's object, input,
// enum or scalar, lists and non-nulls around them copied.
func (a *assembly) resolve(t TypeDef) TypeDef {
	switch x := t.(type) {
	case *NonNull:
		return &NonNull{Of: a.resolve(x.Of)}
	case *List:
		return &List{Of: a.resolve(x.Of), Items: x.Items, Build: x.Build}
	case *Scalar:
		switch x.Name {
		case "Int", "Float", "String", "Boolean", "ID":
		default:
			a.claim(x.Name, "scalar "+x.Name)
		}
		return x
	case *typeRef:
		switch {
		case x.text != nil:
			return a.text(x)
		case x.kind == objectBinding:
			return a.object(x.t)
		case x.kind == inputBinding:
			return a.input(x.t)
		case x.kind == enumBinding:
			return a.enum(x.t)
		}
	case nil:
		return nil
	}
	return t
}

func (a *assembly) bindingOf(kind bindingKind, t reflect.Type) (binding, bool) {
	b, ok := a.bindings[bindKey{kind, t}]
	if !ok {
		what := map[bindingKind]string{objectBinding: "graphql.Struct", inputBinding: "graphql.FormInput", enumBinding: "graphql.Consts"}[kind]
		a.fail("E0802", "the compiler generates it for the types an operation reaches; register the package's Package with it", "%s is used in the GraphQL schema, and no %s describes it", goName(t), what)
	}
	return b, ok
}

func (a *assembly) object(t reflect.Type) *Object {
	if o, ok := a.objects[t]; ok {
		return o
	}
	name := typeName(t)
	if n, ok := attr[Type](vuka.TypeAttrs(t)); ok && n != "" {
		name = string(n)
	}
	o := &Object{Name: name}
	a.objects[t] = o
	a.claim(name, "object "+goName(t))
	b, ok := a.bindingOf(objectBinding, t)
	if !ok {
		return o
	}
	o.Description, o.Value, o.Paged = b.doc, b.norm, isConnection(t)
	fieldAttrs := vuka.FieldAttrsOf(t)
	seen := map[string]string{}
	for _, m := range b.fields {
		what := goName(t) + "." + m.goName
		var attrs []any
		name := m.name
		if m.resolve != nil {
			attrs = vuka.MethodAttrs(t, m.goName)
			if n, ok := attr[Field](attrs); ok && n != "" {
				name = string(n)
			}
		} else {
			attrs = flatten(fieldAttrs[m.goName])
		}
		if name == "" {
			name = a.field(m.goName)
		}
		if prev, dup := seen[name]; dup {
			a.fail("E0803", "rename one with a graphql:\"name\" tag, or @graphql.Field(\"…\") on a method", "%s and %s are both field %q of %s", prev, what, name, o.Name)
			continue
		}
		seen[name] = what
		if strings.HasPrefix(name, "__") {
			a.fail("E0804", "rename it with a graphql:\"name\" tag or @graphql.Field", "%s: the GraphQL name %q starts with __, which the specification reserves for introspection", what, name)
			continue
		}
		f := &FieldDef{Name: name, Description: m.doc, Type: a.resolve(m.typ), Guard: a.guard(attrs, false, what)}
		deprecate(attrs, &f.Deprecated, &f.DeprecationReason)
		if get := m.get; get != nil {
			f.Trivial = true
			f.Resolve = func(_ context.Context, p ResolveParams) (any, error) { return get(p.Source), nil }
		} else {
			args, fill := a.params(m.params, attrs, what)
			f.Args = args
			res := m.resolve
			f.Resolve = func(ctx context.Context, p ResolveParams) (any, error) {
				c, err := fill(ctx, p.Args)
				if err != nil {
					return nil, err
				}
				return res(ctx, p.Source, c)
			}
		}
		o.Fields = append(o.Fields, f)
	}
	return o
}

func (a *assembly) input(t reflect.Type) *InputObject {
	if o, ok := a.inputs[t]; ok {
		return o
	}
	attrs := vuka.TypeAttrs(t)
	name := typeName(t)
	if n, ok := attr[Input](attrs); ok && n != "" {
		name = string(n)
	} else if n, ok := attr[Type](attrs); ok && n != "" {
		name = string(n)
	}
	o := &InputObject{Name: name}
	a.inputs[t] = o
	a.claim(name, "input "+goName(t))
	b, ok := a.bindingOf(inputBinding, t)
	if !ok {
		return o
	}
	o.Description = b.doc
	fieldAttrs := vuka.FieldAttrsOf(t)
	sets := map[string]func(dst, v any){}
	seen := map[string]string{}
	for _, m := range b.inputs {
		what := goName(t) + "." + m.goName
		name := m.name
		if name == "" {
			name = a.field(m.goName)
		}
		if prev, dup := seen[name]; dup {
			a.fail("E0803", "rename one with a graphql:\"name\" tag", "%s and %s are both field %q of input %s", prev, what, name, o.Name)
			continue
		}
		seen[name] = what
		f := &InputFieldDef{Name: name, Description: m.doc, Type: a.resolve(m.typ)}
		deprecate(flatten(fieldAttrs[m.goName]), &f.Deprecated, &f.DeprecationReason)
		o.Fields = append(o.Fields, f)
		sets[name] = m.set
	}
	build := b.build
	o.Build = func(fields map[string]any) (any, error) {
		dst := build()
		for name, v := range fields {
			if set := sets[name]; set != nil && v != nil {
				set(dst, v)
			}
		}
		return dst, nil
	}
	return o
}

func (a *assembly) enum(t reflect.Type) *Enum {
	if e, ok := a.enums[t]; ok {
		return e
	}
	name := typeName(t)
	if n, ok := attr[Type](vuka.TypeAttrs(t)); ok && n != "" {
		name = string(n)
	}
	e := &Enum{Name: name}
	a.enums[t] = e
	a.claim(name, "enum "+goName(t))
	b, ok := a.bindingOf(enumBinding, t)
	if !ok {
		return e
	}
	e.Description = b.doc
	seen := map[string]string{}
	for _, c := range b.values {
		attrs := attrsOfValue(t, c.value)
		vname := enumValueName(c.goName, t.Name())
		if n, ok := attr[Value](attrs); ok && n != "" {
			vname = string(n)
		}
		if prev, dup := seen[vname]; dup {
			a.fail("E0803", "rename one with @graphql.Value(\"…\") on its constant", "%s and %s are both value %s of enum %s", prev, c.goName, vname, name)
			continue
		}
		seen[vname] = c.goName
		v := &EnumValueDef{Name: vname, Description: c.doc, Value: c.value}
		deprecate(attrs, &v.Deprecated, &v.DeprecationReason)
		e.Values = append(e.Values, v)
	}
	ser := b.ser
	e.Serialize = func(v any) (string, error) {
		_, i := ser(v)
		if i < 0 || i >= len(e.Values) {
			return "", fmt.Errorf("Enum %q cannot represent value: %v", name, v)
		}
		return e.Values[i].Name, nil
	}
	return e
}

func (a *assembly) text(r *typeRef) *Scalar {
	if s, ok := a.texts[r.t]; ok {
		return s
	}
	name := typeName(r.t)
	if n, ok := attr[Type](vuka.TypeAttrs(r.t)); ok && n != "" {
		name = string(n)
	}
	s := *r.text
	s.Name = name
	a.texts[r.t] = &s
	a.claim(name, "scalar "+goName(r.t))
	return &s
}

// deprecate fills a declaration's deprecation from its @graphql.Deprecated.
func deprecate(attrs []any, dep *bool, reason *string) {
	if d, ok := attr[Deprecated](attrs); ok {
		*dep, *reason = true, string(d)
	}
}

// flatten is attrs with each vuka.Bundle's attributes in its place.
func flatten(attrs []any) []any {
	var out []any
	for _, x := range attrs {
		if b, ok := x.(vuka.Bundle); ok {
			out = append(out, b.Attrs()...)
			continue
		}
		out = append(out, x)
	}
	return out
}

// guard is the Guard of a declaration with attrs: the app's GuardCalls
// deciding it. A gate (a web.RouteAttr) in an app without one is a Build
// error, as a gated route's is.
func (a *assembly) guard(attrs []any, byDefault bool, what string) func(context.Context) error {
	if a.host.Guard == nil {
		for _, x := range attrs {
			if ra, ok := x.(web.RouteAttr); ok {
				a.fail("E0404", "add the module that gates it: auth.Module", "%s: its attribute %T needs %s, which the app doesn't have", what, x, ra.RouteWrapper())
			}
		}
		return nil
	}
	if !byDefault && len(attrs) == 0 {
		return nil
	}
	g := a.host.Guard
	return func(ctx context.Context) error { return g(ctx, attrs, byDefault) }
}

// params are the arguments a function's parameters give a field, and how
// its Call is filled from the field's coerced arguments: dependencies from
// the app, struct arguments checked as forms.
func (a *assembly) params(ps []Param, attrs []any, what string) ([]*ArgDef, func(ctx context.Context, args map[string]any) (*Call, error)) {
	renames := map[string]string{}
	for _, x := range attrs {
		if r, ok := x.(ArgName); ok {
			renames[r.Param] = r.Name
		}
	}
	type slot struct {
		arg  string
		form bool
		page bool
		dep  func(context.Context) (any, error)
	}
	slots := make([]slot, len(ps))
	var args []*ArgDef
	names := map[string]bool{}
	for i, p := range ps {
		switch {
		case p.Dep != nil && a.loaders[p.Dep] != nil:
			slots[i].dep = a.loader(a.loaders[p.Dep], what)
		case p.Dep != nil:
			dep, err := a.host.Dependency(p.Dep, what)
			if err != nil {
				a.errs = append(a.errs, err)
				continue
			}
			slots[i].dep = dep
		case p.Type == PageArgs:
			for _, n := range []struct {
				name string
				t    TypeDef
			}{{"first", Int}, {"after", String}, {"last", Int}, {"before", String}} {
				args = append(args, &ArgDef{Name: n.name, Type: n.t})
			}
			slots[i].page = true
		case p.Type != nil:
			name := a.field(p.Name)
			if r, ok := renames[p.Name]; ok {
				name = r
				delete(renames, p.Name)
			}
			if strings.HasPrefix(name, "__") {
				a.fail("E0804", "rename it with @graphql.Arg", "%s: the argument name %q starts with __, which the specification reserves", what, name)
			}
			typ := a.resolve(p.Type)
			_, form := NamedOf(typ).(*InputObject)
			slots[i] = slot{arg: name, form: form}
			args = append(args, &ArgDef{Name: name, Type: typ})
			names[p.Name] = true
		}
	}
	for param := range renames {
		var have []string
		for n := range names {
			have = append(have, n)
		}
		help := "name one of its parameters: " + strings.Join(slices.Sorted(slices.Values(have)), ", ")
		if s := closest(param, have); s != "" {
			help = "did you mean " + s + "?"
		}
		a.fail("E0805", help, "%s: @graphql.Arg(%q, …) names no argument of it", what, param)
	}
	fill := func(ctx context.Context, in map[string]any) (*Call, error) {
		c := &Call{args: make([]any, len(slots))}
		for i, s := range slots {
			switch {
			case s.dep != nil:
				v, err := s.dep(ctx)
				if err != nil {
					return nil, err
				}
				c.args[i] = v
			case s.page:
				c.args[i] = Page{First: Ptr[int](in["first"]), After: Ptr[string](in["after"]), Last: Ptr[int](in["last"]), Before: Ptr[string](in["before"])}
			case s.arg != "":
				v := in[s.arg]
				if s.form && v != nil {
					errs, err := ui.CheckForm(ctx, v)
					if err != nil {
						return nil, err
					}
					if len(errs) > 0 {
						return nil, &InvalidInput{Arg: s.arg, Errors: errs}
					}
				}
				c.args[i] = v
			}
		}
		return c, nil
	}
	return args, fill
}

// loader fills a parameter taking a loader type: the request's loader of
// that type, made with its dependencies the first time one is asked for.
func (a *assembly) loader(def *LoaderDef, what string) func(context.Context) (any, error) {
	_, fill := a.params(def.Params, nil, what+" (its loader "+def.Go+")")
	key := loaderKey{t: def.Type}
	return func(ctx context.Context) (any, error) {
		mk := func() (any, error) {
			c, err := fill(ctx, nil)
			if err != nil {
				return nil, err
			}
			return def.New(c), nil
		}
		if st := stateOf(ctx); st != nil {
			return st.loader(key, mk)
		}
		return mk()
	}
}

// closest is the name in names nearest to s, if near enough to suggest.
func closest(s string, names []string) string {
	best, bestD := "", len(s)/2+1
	for _, n := range names {
		if d := distance(strings.ToLower(s), strings.ToLower(n)); d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// InvalidInput is a struct argument its form checks refused: each error
// becomes one of the response's, extensions.code INVALID_INPUT.
type InvalidInput struct {
	Arg    string
	Errors []ui.FormError
}

func (e *InvalidInput) Error() string {
	msgs := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		msgs[i] = fe.Message
	}
	return e.Arg + ": " + strings.Join(msgs, "; ")
}

// schema builds the schema of the registered packages.
func (a *assembly) schema() (*Schema, error) {
	a.bindings = map[bindKey]binding{}
	a.objects, a.inputs, a.enums, a.texts = map[reflect.Type]*Object{}, map[reflect.Type]*InputObject{}, map[reflect.Type]*Enum{}, map[reflect.Type]*Scalar{}
	a.names = map[string]string{}
	pkgs := registered()
	a.loaders = map[reflect.Type]*LoaderDef{}
	for _, p := range pkgs {
		for i := range p.Loaders {
			if l := &p.Loaders[i]; a.loaders[l.Type] == nil {
				a.loaders[l.Type] = l
			}
		}
		for _, t := range p.Types {
			b := t.binding()
			if _, ok := a.bindings[bindKey{b.kind, b.t}]; !ok {
				a.bindings[bindKey{b.kind, b.t}] = b
			}
		}
	}
	query, mutation := &Object{Name: "Query"}, &Object{Name: "Mutation"}
	a.names["Query"], a.names["Mutation"] = "the Query type", "the Mutation type"
	ops := map[string]string{}
	for _, p := range pkgs {
		for _, op := range p.Operations {
			what := p.Path + "." + op.Go
			ref, _ := vuka.FuncOf(op.Func)
			attrs := flatten(ref.Attrs)
			root, kind, name := query, "query", ""
			if op.Mutation {
				root, kind = mutation, "mutation"
				if n, ok := attr[Mutation](attrs); ok {
					name = string(n)
				}
			} else if n, ok := attr[Query](attrs); ok {
				name = string(n)
			}
			if name == "" {
				name = a.field(op.Go)
			}
			key := kind + " " + name
			if prev, dup := ops[key]; dup {
				a.fail("E0803", "name one with @graphql."+map[bool]string{false: "Query", true: "Mutation"}[op.Mutation]+"(\"…\")", "%s is declared twice: by %s and by %s", key, prev, what)
				continue
			}
			ops[key] = what
			if strings.HasPrefix(name, "__") {
				a.fail("E0804", "name it with @graphql.Query(\"…\")", "%s: the GraphQL name %q starts with __, which the specification reserves", what, name)
				continue
			}
			args, fill := a.params(op.Params, attrs, what)
			res := op.Resolve
			f := &FieldDef{Name: name, Description: op.Doc, Args: args, Type: a.resolve(op.Type), Guard: a.guard(attrs, true, what)}
			deprecate(attrs, &f.Deprecated, &f.DeprecationReason)
			f.Resolve = func(ctx context.Context, p ResolveParams) (any, error) {
				c, err := fill(ctx, p.Args)
				if err != nil {
					return nil, err
				}
				return res(ctx, c)
			}
			root.Fields = append(root.Fields, f)
		}
	}
	if len(a.errs) > 0 {
		return nil, errors.Join(a.errs...)
	}
	if len(query.Fields) == 0 {
		return nil, diag.New("E0801", "%s: the schema has no query: GraphQL needs at least one", a.host.Where).WithHelp("declare one with @graphql.Query on a function")
	}
	cfg := SchemaConfig{Query: query}
	if len(mutation.Fields) > 0 {
		cfg.Mutation = mutation
	}
	s, err := NewSchema(cfg)
	if err != nil {
		return nil, diag.New("E0807", "%s: %v", a.host.Where, err)
	}
	return s, nil
}
