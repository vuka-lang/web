package web

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"html/template"
	"net/http"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vuka-lang/vuka"
)

type view struct {
	render func(w http.ResponseWriter, r *http.Request, v reflect.Value) error
}

func compileView(dv declView, data reflect.Type) (*view, error) {
	if data == nil {
		return nil, fmt.Errorf("the view %s needs a result to render", dv.file.Path())
	}
	src, err := dv.file.Bytes()
	if err != nil {
		return nil, fmt.Errorf("reading the view %s: %w", dv.file.Path(), err)
	}
	if strings.HasSuffix(dv.file.Path(), ".templ") {
		return templView(dv, src, data)
	}
	return htmlView(dv, src)
}

func htmlView(dv declView, src []byte) (*view, error) {
	t, err := template.New(path.Base(dv.file.Path())).Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("parsing the view %s: %w", dv.file.Path(), err)
	}
	if dv.name != "" {
		if t = t.Lookup(dv.name); t == nil {
			return nil, fmt.Errorf("the view %s defines no template %q", dv.file.Path(), dv.name)
		}
	}
	return &view{func(w http.ResponseWriter, r *http.Request, v reflect.Value) error {
		var buf bytes.Buffer
		if err := t.Execute(&buf, v.Interface()); err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, err := w.Write(buf.Bytes())
		return err
	}}, nil
}

func templView(dv declView, src []byte, data reflect.Type) (*view, error) {
	comps, ok := vuka.TemplComponents(dv.file)
	if !ok {
		return nil, fmt.Errorf("%s is not a .templ file Vuka compiled with this package", dv.file.Path())
	}
	name := dv.name
	if name == "" {
		var exported []string
		for n := range comps {
			if ast.IsExported(n) {
				exported = append(exported, n)
			}
		}
		if len(exported) != 1 {
			sort.Strings(exported)
			return nil, fmt.Errorf("%s has %d exported components %v; name one: @web.Template(%q, \"Name\")",
				dv.file.Path(), len(exported), exported, dv.file.Path())
		}
		name = exported[0]
	}
	comp, ok := comps[name]
	if !ok {
		return nil, fmt.Errorf("%s has no component %s", dv.file.Path(), name)
	}
	fn := reflect.ValueOf(comp)
	args, err := componentArgs(fn.Type(), templParams(src, name), data, name)
	if err != nil {
		return nil, err
	}
	return &view{func(w http.ResponseWriter, r *http.Request, v reflect.Value) error {
		in, err := args(v)
		if err != nil {
			return err
		}
		n, _ := fn.Call(in)[0].Interface().(vuka.Node)
		return vuka.Write(w, r, n)
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
		return nil, fmt.Errorf("component %s takes %s; a %s result can't fill it", comp, signature(ft, names), data)
	}
	fields := make([][]int, ft.NumIn())
	for i := range fields {
		if i >= len(names) || names[i] == "" {
			return nil, fmt.Errorf("component %s: parameter %d has no name to match a field of %s", comp, i+1, data)
		}
		f, ok := fieldNamed(st, names[i])
		if !ok {
			return nil, fmt.Errorf("component %s: %s has no field for parameter %s", comp, data, names[i])
		}
		if !f.Type.AssignableTo(ft.In(i)) {
			return nil, fmt.Errorf("component %s: field %s.%s is %s; parameter %s is %s", comp, st.Name(), f.Name, f.Type, names[i], ft.In(i))
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

var templDecl = regexp.MustCompile(`(?m)^templ\s+([A-Za-z_]\w*)\s*(?:\[[^\]]*\])?\s*\(`)

// templParams reads a component's parameter names from its .templ source;
// the compiled function keeps only their types.
func templParams(src []byte, name string) []string {
	for _, m := range templDecl.FindAllSubmatchIndex(src, -1) {
		if string(src[m[2]:m[3]]) != name {
			continue
		}
		start, depth := m[1], 1
		end := start
		for ; end < len(src) && depth > 0; end++ {
			switch src[end] {
			case '(':
				depth++
			case ')':
				depth--
			}
		}
		f, err := parser.ParseFile(token.NewFileSet(), "", "package p; func _("+string(src[start:end-1])+") {}", 0)
		if err != nil {
			return nil
		}
		var names []string
		for _, field := range f.Decls[0].(*ast.FuncDecl).Type.Params.List {
			if len(field.Names) == 0 {
				names = append(names, "")
			}
			for _, n := range field.Names {
				names = append(names, n.Name)
			}
		}
		return names
	}
	return nil
}
