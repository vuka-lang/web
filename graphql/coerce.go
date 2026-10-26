package graphql

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// errMissingVar is a literal naming a variable the request didn't give: the
// value is absent, not null.
var errMissingVar = errors.New("variable not provided")

// enumValue is the Go value of an enum value: its Value, else its name.
func enumValue(v *EnumValueDef) any {
	if v.Value != nil {
		return v.Value
	}
	return v.Name
}

// coerceLiteral is the value literal v gives an input of type t, its
// variables read from vars (already coerced). A variable vars lacks gives
// errMissingVar, for the caller to treat as absent.
func (s *Schema) coerceLiteral(v language.Value, t TypeDef, vars map[string]any) (any, error) {
	if vr, ok := v.(*language.Variable); ok {
		val, ok := vars[vr.Name]
		if !ok {
			return nil, errMissingVar
		}
		if _, nn := t.(*NonNull); nn && val == nil {
			return nil, fmt.Errorf("Expected value of type %q, found null.", TypeString(t))
		}
		return val, nil
	}
	if nn, ok := t.(*NonNull); ok {
		if _, null := v.(*language.NullValue); null {
			return nil, fmt.Errorf("Expected value of type %q, found null.", TypeString(t))
		}
		return s.coerceLiteral(v, nn.Of, vars)
	}
	if _, null := v.(*language.NullValue); null {
		return nil, nil
	}
	switch t := t.(type) {
	case *List:
		lv, ok := v.(*language.ListValue)
		if !ok {
			item, err := s.coerceLiteral(v, t.Of, vars)
			if err == errMissingVar {
				item, err = nil, nil
			}
			if err != nil {
				return nil, err
			}
			return buildList(t, []any{item})
		}
		items := make([]any, len(lv.Values))
		for i, x := range lv.Values {
			item, err := s.coerceLiteral(x, t.Of, vars)
			if err == errMissingVar {
				if _, nn := t.Of.(*NonNull); nn {
					return nil, fmt.Errorf("Expected value of type %q, found null.", TypeString(t.Of))
				}
				item, err = nil, nil
			}
			if err != nil {
				return nil, err
			}
			items[i] = item
		}
		return buildList(t, items)
	case *InputObject:
		ov, ok := v.(*language.ObjectValue)
		if !ok {
			return nil, fmt.Errorf("Expected value of type %q, found %s.", t.Name, language.Print(v))
		}
		given := map[string]*language.ObjectField{}
		for _, f := range ov.Fields {
			if s.inputFields[t][f.Name] == nil {
				return nil, fmt.Errorf("Field %q is not defined by type %q.", f.Name, t.Name)
			}
			given[f.Name] = f
		}
		out := map[string]any{}
		for _, fd := range t.Fields {
			f := given[fd.Name]
			if f != nil {
				val, err := s.coerceLiteral(f.Value, fd.Type, vars)
				if err == nil {
					out[fd.Name] = val
					continue
				}
				if err != errMissingVar {
					return nil, err
				}
			}
			if fd.Default != nil {
				val, err := s.coerceLiteral(fd.Default, fd.Type, nil)
				if err != nil {
					return nil, err
				}
				out[fd.Name] = val
				continue
			}
			if _, nn := fd.Type.(*NonNull); nn {
				return nil, fmt.Errorf("Field \"%s.%s\" of required type %q was not provided.", t.Name, fd.Name, TypeString(fd.Type))
			}
		}
		if t.Build != nil {
			return t.Build(out)
		}
		return out, nil
	case *Enum:
		ev, ok := v.(*language.EnumValue)
		if !ok {
			return nil, fmt.Errorf("Enum %q cannot represent non-enum value: %s.%s", t.Name, language.Print(v), enumHint(t, v))
		}
		for _, d := range t.Values {
			if d.Name == ev.Value {
				return enumValue(d), nil
			}
		}
		return nil, fmt.Errorf("Value %q does not exist in %q enum.%s", ev.Value, t.Name, didYouMean(ev.Value, enumNames(t)))
	case *Scalar:
		if containsVariable(v) {
			jv, err := literalJSON(v, vars)
			if err != nil {
				return nil, err
			}
			return t.ParseValue(jv)
		}
		if t.ParseLiteral != nil {
			return t.ParseLiteral(v)
		}
		jv, err := literalJSON(v, vars)
		if err != nil {
			return nil, err
		}
		return t.ParseValue(jv)
	}
	return nil, fmt.Errorf("%s is not an input type", TypeString(t))
}

func enumNames(t *Enum) []string {
	names := make([]string, len(t.Values))
	for i, v := range t.Values {
		names[i] = v.Name
	}
	return names
}

// enumHint suggests the enum value a string literal spells.
func enumHint(t *Enum, v language.Value) string {
	if s, ok := v.(*language.StringValue); ok {
		for _, d := range t.Values {
			if d.Name == s.Value {
				return fmt.Sprintf(" Did you mean the enum value %q?", d.Name)
			}
		}
		if m := didYouMean(s.Value, enumNames(t)); m != "" {
			return strings.Replace(m, "Did you mean", "Did you mean the enum value", 1)
		}
	}
	return ""
}

func buildList(t *List, items []any) (any, error) {
	if t.Build != nil {
		return t.Build(items)
	}
	return items, nil
}

func containsVariable(v language.Value) bool {
	switch x := v.(type) {
	case *language.Variable:
		return true
	case *language.ListValue:
		for _, i := range x.Values {
			if containsVariable(i) {
				return true
			}
		}
	case *language.ObjectValue:
		for _, f := range x.Fields {
			if containsVariable(f.Value) {
				return true
			}
		}
	}
	return false
}

// literalJSON is a literal as the JSON a variable would carry: numbers as
// json.Number, an enum value as its name.
func literalJSON(v language.Value, vars map[string]any) (any, error) {
	switch x := v.(type) {
	case *language.Variable:
		val, ok := vars[x.Name]
		if !ok {
			return nil, errMissingVar
		}
		return val, nil
	case *language.IntValue:
		return json.Number(x.Value), nil
	case *language.FloatValue:
		return json.Number(x.Value), nil
	case *language.StringValue:
		return x.Value, nil
	case *language.BooleanValue:
		return x.Value, nil
	case *language.NullValue:
		return nil, nil
	case *language.EnumValue:
		return x.Value, nil
	case *language.ListValue:
		out := make([]any, 0, len(x.Values))
		for _, i := range x.Values {
			val, err := literalJSON(i, vars)
			if err == errMissingVar {
				val, err = nil, nil
			}
			if err != nil {
				return nil, err
			}
			out = append(out, val)
		}
		return out, nil
	case *language.ObjectValue:
		out := map[string]any{}
		for _, f := range x.Fields {
			val, err := literalJSON(f.Value, vars)
			if err == errMissingVar {
				continue
			}
			if err != nil {
				return nil, err
			}
			out[f.Name] = val
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown literal %T", v)
}

// checkLiteral reports each problem of a constant-or-variable literal for
// an input of type t (ValuesOfCorrectType): variables are left to the
// rules on variables.
func (s *Schema) checkLiteral(v language.Value, t TypeDef, report func(at language.Loc, msg string)) {
	if _, ok := v.(*language.Variable); ok {
		return
	}
	if t == nil {
		return
	}
	if nn, ok := t.(*NonNull); ok {
		if _, null := v.(*language.NullValue); null {
			report(v.Location(), fmt.Sprintf("Expected value of type %q, found null.", TypeString(t)))
			return
		}
		s.checkLiteral(v, nn.Of, report)
		return
	}
	if _, null := v.(*language.NullValue); null {
		return
	}
	switch t := t.(type) {
	case *List:
		if lv, ok := v.(*language.ListValue); ok {
			for _, x := range lv.Values {
				s.checkLiteral(x, t.Of, report)
			}
			return
		}
		s.checkLiteral(v, t.Of, report)
	case *InputObject:
		ov, ok := v.(*language.ObjectValue)
		if !ok {
			report(v.Location(), fmt.Sprintf("Expected value of type %q, found %s.", t.Name, language.Print(v)))
			return
		}
		given := map[string]bool{}
		for _, f := range ov.Fields {
			given[f.Name] = true
			fd := s.inputFields[t][f.Name]
			if fd == nil {
				names := make([]string, len(t.Fields))
				for i, x := range t.Fields {
					names[i] = x.Name
				}
				report(f.Location(), fmt.Sprintf("Field %q is not defined by type %q.%s", f.Name, t.Name, didYouMean(f.Name, names)))
				continue
			}
			s.checkLiteral(f.Value, fd.Type, report)
		}
		for _, fd := range t.Fields {
			if _, nn := fd.Type.(*NonNull); nn && !given[fd.Name] && fd.Default == nil {
				report(v.Location(), fmt.Sprintf("Field \"%s.%s\" of required type %q was not provided.", t.Name, fd.Name, TypeString(fd.Type)))
			}
		}
	case *Enum:
		if _, err := s.coerceLiteral(v, t, nil); err != nil {
			report(v.Location(), err.Error())
		}
	case *Scalar:
		if containsVariable(v) {
			return
		}
		if _, err := s.coerceLiteral(v, t, nil); err != nil {
			msg := err.Error()
			if !strings.Contains(msg, "Expected value of type") && t != Int && t != Float && t != String && t != Boolean && t != ID && s.codecs[t] == nil {
				msg = fmt.Sprintf("Expected value of type %q, found %s; %s", t.Name, language.Print(v), msg)
			}
			report(v.Location(), msg)
		}
	}
}

// literalErrors are the problems of a default value for type t.
func literalErrors(s *Schema, v language.Value, t TypeDef) []string {
	var msgs []string
	s.checkLiteral(v, t, func(_ language.Loc, m string) { msgs = append(msgs, m) })
	return msgs
}

// inputError is a variable's value its type refuses: where in it, and why.
type inputError struct {
	path []any
	msg  string
	val  any
}

// coerceInput is the value a variable's JSON value v gives type t, each
// problem reported with its path inside the value.
func (s *Schema) coerceInput(v any, t TypeDef, path []any, errs *[]inputError) any {
	if nn, ok := t.(*NonNull); ok {
		if v == nil {
			*errs = append(*errs, inputError{path, fmt.Sprintf("Expected non-nullable type %q not to be null.", TypeString(t)), v})
			return nil
		}
		return s.coerceInput(v, nn.Of, path, errs)
	}
	if v == nil {
		return nil
	}
	switch t := t.(type) {
	case *List:
		arr, ok := v.([]any)
		if !ok {
			item := s.coerceInput(v, t.Of, path, errs)
			out, err := buildList(t, []any{item})
			if err != nil {
				*errs = append(*errs, inputError{path, err.Error(), v})
			}
			return out
		}
		items := make([]any, len(arr))
		for i, x := range arr {
			items[i] = s.coerceInput(x, t.Of, append(path[:len(path):len(path)], i), errs)
		}
		out, err := buildList(t, items)
		if err != nil {
			*errs = append(*errs, inputError{path, err.Error(), v})
		}
		return out
	case *InputObject:
		obj, ok := v.(map[string]any)
		if !ok {
			*errs = append(*errs, inputError{path, fmt.Sprintf("Expected type %q to be an object.", t.Name), v})
			return nil
		}
		out := map[string]any{}
		for _, fd := range t.Fields {
			fv, given := obj[fd.Name]
			if !given {
				if fd.Default != nil {
					val, err := s.coerceLiteral(fd.Default, fd.Type, nil)
					if err != nil {
						*errs = append(*errs, inputError{path, err.Error(), v})
					}
					out[fd.Name] = val
				} else if _, nn := fd.Type.(*NonNull); nn {
					*errs = append(*errs, inputError{path, fmt.Sprintf("Field %q of required type %q was not provided.", fd.Name, TypeString(fd.Type)), v})
				}
				continue
			}
			out[fd.Name] = s.coerceInput(fv, fd.Type, append(path[:len(path):len(path)], fd.Name), errs)
		}
		var names []string
		for _, fd := range t.Fields {
			names = append(names, fd.Name)
		}
		for k := range obj {
			if s.inputFields[t][k] == nil {
				*errs = append(*errs, inputError{path, fmt.Sprintf("Field %q is not defined by type %q.%s", k, t.Name, didYouMean(k, names)), v})
			}
		}
		if t.Build != nil {
			val, err := t.Build(out)
			if err != nil {
				*errs = append(*errs, inputError{path, err.Error(), v})
			}
			return val
		}
		return out
	case *Enum:
		name, ok := v.(string)
		if !ok {
			*errs = append(*errs, inputError{path, fmt.Sprintf("Enum %q cannot represent non-string value: %s.", t.Name, inspect(v)), v})
			return nil
		}
		for _, d := range t.Values {
			if d.Name == name {
				return enumValue(d)
			}
		}
		*errs = append(*errs, inputError{path, fmt.Sprintf("Value %q does not exist in %q enum.%s", name, t.Name, didYouMean(name, enumNames(t))), v})
		return nil
	case *Scalar:
		val, err := t.ParseValue(v)
		if err != nil {
			*errs = append(*errs, inputError{path, err.Error(), v})
			return nil
		}
		return val
	}
	*errs = append(*errs, inputError{path, TypeString(t) + " is not an input type", v})
	return nil
}

// printPath writes a path inside a value: value.a[0].b.
func printPath(path []any) string {
	var b strings.Builder
	for _, p := range path {
		switch p := p.(type) {
		case int:
			b.WriteString("[" + strconv.Itoa(p) + "]")
		default:
			b.WriteString("." + fmt.Sprint(p))
		}
	}
	return b.String()
}

// coerceVariables is the operation's variables' values (CoerceVariableValues).
func (s *Schema) coerceVariables(op *language.OperationDefinition, given map[string]any) (map[string]any, []*Error) {
	out := map[string]any{}
	var errs []*Error
	for _, def := range op.VariableDefinitions {
		t := s.typeFromRef(def.Type)
		loc := []Location{{def.Line, def.Column}}
		if t == nil || !isInputType(t) {
			errs = append(errs, &Error{Message: fmt.Sprintf("Variable \"$%s\" expected value of type %q which cannot be used as an input type.", def.Variable, def.Type.String()), Locations: loc, Extensions: code(CodeBadUserInput)})
			continue
		}
		v, given := given[def.Variable]
		if !given {
			if def.DefaultValue != nil {
				val, err := s.coerceLiteral(def.DefaultValue, t, nil)
				if err != nil {
					errs = append(errs, &Error{Message: fmt.Sprintf("Variable \"$%s\" has an invalid default value: %s", def.Variable, err), Locations: loc, Extensions: code(CodeBadUserInput)})
					continue
				}
				out[def.Variable] = val
				continue
			}
			if _, nn := t.(*NonNull); nn {
				errs = append(errs, &Error{Message: fmt.Sprintf("Variable \"$%s\" of required type %q was not provided.", def.Variable, def.Type.String()), Locations: loc, Extensions: code(CodeBadUserInput)})
			}
			continue
		}
		if v == nil {
			if _, nn := t.(*NonNull); nn {
				errs = append(errs, &Error{Message: fmt.Sprintf("Variable \"$%s\" of non-null type %q must not be null.", def.Variable, def.Type.String()), Locations: loc, Extensions: code(CodeBadUserInput)})
				continue
			}
			out[def.Variable] = nil
			continue
		}
		var ierrs []inputError
		val := s.coerceInput(v, t, nil, &ierrs)
		if len(ierrs) > 0 {
			for _, ie := range ierrs {
				msg := fmt.Sprintf("Variable \"$%s\" got invalid value %s", def.Variable, inspect(ie.val))
				if len(ie.path) > 0 {
					msg += fmt.Sprintf(" at \"%s%s\"", def.Variable, printPath(ie.path))
				}
				errs = append(errs, &Error{Message: msg + "; " + ie.msg, Locations: loc, Extensions: code(CodeBadUserInput)})
			}
			continue
		}
		out[def.Variable] = val
	}
	return out, errs
}

// typeFromRef is the schema's type a document's type reference names; nil
// when it names no type.
func (s *Schema) typeFromRef(r language.TypeRef) TypeDef {
	switch x := r.(type) {
	case *language.NamedType:
		if t := s.types[x.Name]; t != nil {
			return t
		}
		return nil
	case *language.ListType:
		if of := s.typeFromRef(x.Type); of != nil {
			return &List{Of: of}
		}
	case *language.NonNullType:
		if of := s.typeFromRef(x.Type); of != nil {
			return &NonNull{Of: of}
		}
	}
	return nil
}

// coerceArgs is a field's or directive's arguments (CoerceArgumentValues):
// absent ones without defaults left out.
func (s *Schema) coerceArgs(defs []*ArgDef, given []*language.Argument, vars map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(defs))
	for _, d := range defs {
		var arg *language.Argument
		for _, a := range given {
			if a.Name == d.Name {
				arg = a
				break
			}
		}
		if arg != nil {
			val, err := s.coerceLiteral(arg.Value, d.Type, vars)
			if err == nil {
				out[d.Name] = val
				continue
			}
			if err != errMissingVar {
				return nil, &argError{name: d.Name, err: err, at: arg.Location()}
			}
		}
		if d.Default != nil {
			val, err := s.coerceLiteral(d.Default, d.Type, nil)
			if err != nil {
				return nil, &argError{name: d.Name, err: err}
			}
			out[d.Name] = val
			continue
		}
		if _, nn := d.Type.(*NonNull); nn {
			if arg != nil {
				return nil, &argError{name: d.Name, err: fmt.Errorf("Argument %q of required type %q was provided the variable %q which was not provided a runtime value.", d.Name, TypeString(d.Type), language.Print(arg.Value)), at: arg.Location()}
			}
			return nil, &argError{name: d.Name, err: fmt.Errorf("Argument %q of required type %q was not provided.", d.Name, TypeString(d.Type))}
		}
	}
	return out, nil
}

// argError is an argument whose value can't be coerced.
type argError struct {
	name string
	err  error
	at   language.Loc
}

func (e *argError) Error() string {
	msg := e.err.Error()
	if strings.HasPrefix(msg, "Argument ") {
		return msg
	}
	return fmt.Sprintf("Argument %q has invalid value: %s", e.name, msg)
}
