package web

import (
	"bytes"
	"fmt"
	"go/ast"
	"html/template"
	"path"
	"reflect"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/templx"

	"github.com/vuka-lang/vuka/v2/diag"
)

type view struct {
	name   string                                 // its file
	render func(v reflect.Value) ([]byte, error)  // an html/template view's page
	node   func(v reflect.Value) (ui.Node, error) // a templ view's page
}

func compileView(dv declView, data reflect.Type) (*view, error) {
	if data == nil {
		return nil, diag.New("E0417", "the view %s needs a result to render", dv.file.Path()).WithHelp("return the data the view renders")
	}
	var v *view
	var err error
	if strings.HasSuffix(dv.file.Path(), ".templ") {
		v, err = templView(dv, data)
	} else {
		v, err = htmlView(dv)
	}
	if v != nil {
		v.name = dv.file.Path()
	}
	return v, err
}

func htmlView(dv declView) (*view, error) {
	src, err := dv.file.Bytes()
	if err != nil {
		return nil, fmt.Errorf("reading the view %s: %w", dv.file.Path(), err)
	}
	t, err := template.New(path.Base(dv.file.Path())).Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("parsing the view %s: %w", dv.file.Path(), err)
	}
	if dv.name != "" {
		if t = t.Lookup(dv.name); t == nil {
			return nil, fmt.Errorf("the view %s defines no template %q", dv.file.Path(), dv.name)
		}
	}
	return &view{render: func(v reflect.Value) ([]byte, error) {
		var buf bytes.Buffer
		err := t.Execute(&buf, v.Interface())
		return buf.Bytes(), err
	}}, nil
}

func templView(dv declView, data reflect.Type) (*view, error) {
	comps, ok := templx.Components(dv.file)
	if !ok {
		return nil, diag.New("E0417", "%s is not a .templ file Vuka compiled", dv.file.Path())
	}
	var comp *templx.Component
	if dv.name == "" {
		var exported []string
		for i, c := range comps {
			if ast.IsExported(c.Name) {
				exported, comp = append(exported, c.Name), &comps[i]
			}
		}
		if len(exported) != 1 {
			return nil, diag.New("E0417", "%s has %d exported components %v", dv.file.Path(), len(exported), exported).
				WithHelp("name one: @web.Template(%q, \"Name\")", dv.file.Path())
		}
	} else if i := slices.IndexFunc(comps, func(c templx.Component) bool { return c.Name == dv.name }); i >= 0 {
		comp = &comps[i]
	} else {
		return nil, diag.New("E0417", "%s has no component %s", dv.file.Path(), dv.name)
	}
	fn := reflect.ValueOf(comp.Func)
	args, err := componentArgs(fn.Type(), comp.Params, data, comp.Name)
	if err != nil {
		return nil, err
	}
	return &view{node: func(v reflect.Value) (ui.Node, error) {
		in, err := args(v)
		if err != nil {
			return nil, err
		}
		n, _ := fn.Call(in)[0].Interface().(ui.Node)
		return n, nil
	}}, nil
}

// componentArgs binds a result to a component's parameters: the whole value
// to a lone parameter it is assignable to, else a struct's fields by the
// parameters' names (exact, or differing in the first letter's case).
func componentArgs(ft reflect.Type, names []string, data reflect.Type, comp string) (func(reflect.Value) ([]reflect.Value, error), error) {
	switch {
	case ft.NumIn() == 0:
		return func(reflect.Value) ([]reflect.Value, error) { return nil, nil }, nil
	case ft.NumIn() == 1 && data.AssignableTo(ft.In(0)):
		return func(v reflect.Value) ([]reflect.Value, error) { return []reflect.Value{v}, nil }, nil
	}
	st, ptr := data, false
	if st.Kind() == reflect.Pointer {
		st, ptr = st.Elem(), true
	}
	if st.Kind() != reflect.Struct {
		return nil, diag.New("E0417", "component %s takes %s; a %s result can't fill it", comp, signature(ft, names), data).WithHelp("return a struct whose fields are named like the parameters")
	}
	fields := make([][]int, ft.NumIn())
	for i := range fields {
		if i >= len(names) || names[i] == "" {
			return nil, diag.New("E0417", "component %s: parameter %d has no name to match a field of %s", comp, i+1, data)
		}
		f, ok := fieldNamed(st, names[i])
		if !ok {
			return nil, diag.New("E0417", "component %s: %s has no field for parameter %s", comp, data, names[i]).WithHelp("add a field %s to %s", names[i], data)
		}
		if !f.Type.AssignableTo(ft.In(i)) {
			return nil, diag.New("E0417", "component %s: field %s.%s is %s; parameter %s is %s", comp, st.Name(), f.Name, f.Type, names[i], ft.In(i))
		}
		fields[i] = f.Index
	}
	return func(v reflect.Value) ([]reflect.Value, error) {
		if ptr {
			if v.IsNil() {
				return nil, fmt.Errorf("component %s: nil %s", comp, data)
			}
			v = v.Elem()
		}
		in := make([]reflect.Value, len(fields))
		for i, idx := range fields {
			in[i] = v.FieldByIndex(idx)
		}
		return in, nil
	}, nil
}

// fieldNamed finds an exported field named name, exactly or with the first
// letter's case changed: JSX's rule for attributes.
func fieldNamed(st reflect.Type, name string) (reflect.StructField, bool) {
	if f, ok := st.FieldByName(name); ok && f.IsExported() {
		return f, true
	}
	r, n := utf8.DecodeRuneInString(name)
	alt := string(unicode.ToUpper(r)) + name[n:]
	if unicode.IsUpper(r) {
		alt = string(unicode.ToLower(r)) + name[n:]
	}
	f, ok := st.FieldByName(alt)
	return f, ok && f.IsExported()
}

func signature(ft reflect.Type, names []string) string {
	var ps []string
	for i := 0; i < ft.NumIn(); i++ {
		p := ft.In(i).String()
		if i < len(names) && names[i] != "" {
			p = names[i] + " " + p
		}
		ps = append(ps, p)
	}
	return "(" + strings.Join(ps, ", ") + ")"
}
