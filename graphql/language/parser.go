package language

import "fmt"

// Parse parses a GraphQL document: operations and fragments, and
// type-system definitions (the SDL) too — a document of either kind, or a
// mix, which validation then refuses where it runs operations.
func Parse(src string) (doc *Document, err error) {
	p := &parser{lex: newLexer(src)}
	defer func() {
		if r := recover(); r != nil {
			se, ok := r.(*SyntaxError)
			if !ok {
				panic(r)
			}
			doc, err = nil, se
		}
	}()
	p.advance()
	return p.document(), nil
}

// ParseValue parses a single value, such as a default value written on
// its own: "[1, 2]", "{a: true}". Variables are allowed unless constant.
func ParseValue(src string, constant bool) (v Value, err error) {
	p := &parser{lex: newLexer(src)}
	defer func() {
		if r := recover(); r != nil {
			se, ok := r.(*SyntaxError)
			if !ok {
				panic(r)
			}
			v, err = nil, se
		}
	}()
	p.advance()
	v = p.value(constant)
	p.expect(tokEOF)
	return v, nil
}

// ParseType parses a type reference: "[String!]!".
func ParseType(src string) (t TypeRef, err error) {
	p := &parser{lex: newLexer(src)}
	defer func() {
		if r := recover(); r != nil {
			se, ok := r.(*SyntaxError)
			if !ok {
				panic(r)
			}
			t, err = nil, se
		}
	}()
	p.advance()
	t = p.typeRef()
	p.expect(tokEOF)
	return t, nil
}

type parser struct {
	lex *lexer
	tok token
}

func (p *parser) fail(loc Loc, format string, args ...any) {
	panic(&SyntaxError{Message: fmt.Sprintf(format, args...), Line: loc.Line, Column: loc.Column})
}

func (p *parser) advance() token {
	prev := p.tok
	t, err := p.lex.next()
	if err != nil {
		panic(err)
	}
	p.tok = t
	return prev
}

func (p *parser) peek(k tokKind) bool { return p.tok.kind == k }

func (p *parser) peekKeyword(name string) bool { return p.tok.kind == tokName && p.tok.value == name }

// skip advances past a token of kind k, reporting whether there was one.
func (p *parser) skip(k tokKind) bool {
	if p.tok.kind == k {
		p.advance()
		return true
	}
	return false
}

func (p *parser) expect(k tokKind) token {
	if p.tok.kind != k {
		p.fail(p.tok.loc, "Expected %s, found %s.", k, p.tok.describe())
	}
	return p.advance()
}

func (p *parser) expectKeyword(name string) token {
	if !p.peekKeyword(name) {
		p.fail(p.tok.loc, "Expected %q, found %s.", name, p.tok.describe())
	}
	return p.advance()
}

func (p *parser) unexpected() {
	p.fail(p.tok.loc, "Unexpected %s.", p.tok.describe())
}

func (p *parser) name() (string, Loc) {
	t := p.expect(tokName)
	return t.value, t.loc
}

func (p *parser) document() *Document {
	doc := &Document{Loc: p.tok.loc}
	for {
		doc.Definitions = append(doc.Definitions, p.definition())
		if p.peek(tokEOF) {
			return doc
		}
	}
}

func (p *parser) definition() Definition {
	if p.peek(tokBraceL) {
		return p.operation()
	}
	desc := p.peek(tokString) || p.peek(tokBlockString)
	at := p.tok
	if desc {
		p.advance()
	}
	if p.peek(tokName) {
		if !desc {
			switch p.tok.value {
			case "query", "mutation", "subscription":
				return p.operation()
			case "fragment":
				return p.fragment()
			case "extend":
				return p.extension()
			}
		}
		switch p.tok.value {
		case "schema", "scalar", "type", "interface", "union", "enum", "input", "directive":
			return p.typeSystem(at, desc)
		}
		if desc {
			p.unexpected()
		}
	}
	if desc {
		p.unexpected()
	}
	p.unexpected()
	return nil
}

func (p *parser) operation() *OperationDefinition {
	op := &OperationDefinition{Loc: p.tok.loc}
	if p.peek(tokBraceL) {
		op.Operation, op.Shorthand = OpQuery, true
		op.SelectionSet = p.selectionSet()
		return op
	}
	op.Operation = p.operationType()
	if p.peek(tokName) {
		op.Name, op.NameLoc = p.name()
	}
	op.VariableDefinitions = p.variableDefinitions()
	op.Directives = p.directives(false)
	op.SelectionSet = p.selectionSet()
	return op
}

func (p *parser) operationType() string {
	t := p.expect(tokName)
	switch t.value {
	case OpQuery, OpMutation, OpSubscription:
		return t.value
	}
	p.fail(t.loc, "Unexpected %s.", t.describe())
	return ""
}

func (p *parser) variableDefinitions() []*VariableDefinition {
	if !p.peek(tokParenL) {
		return nil
	}
	p.advance()
	var defs []*VariableDefinition
	for {
		v := &VariableDefinition{Loc: p.tok.loc}
		v.Variable = p.variable().Name
		p.expect(tokColon)
		v.Type = p.typeRef()
		if p.skip(tokEquals) {
			v.DefaultValue = p.value(true)
		}
		v.Directives = p.directives(true)
		defs = append(defs, v)
		if p.skip(tokParenR) {
			return defs
		}
	}
}

func (p *parser) variable() *Variable {
	loc := p.expect(tokDollar).loc
	name, _ := p.name()
	return &Variable{Loc: loc, Name: name}
}

func (p *parser) selectionSet() *SelectionSet {
	s := &SelectionSet{Loc: p.expect(tokBraceL).loc}
	for {
		s.Selections = append(s.Selections, p.selection())
		if p.skip(tokBraceR) {
			return s
		}
	}
}

func (p *parser) selection() Selection {
	if p.peek(tokSpread) {
		loc := p.advance().loc
		if p.peek(tokName) && p.tok.value != "on" {
			fs := &FragmentSpread{Loc: loc}
			fs.Name, _ = p.name()
			fs.Directives = p.directives(false)
			return fs
		}
		in := &InlineFragment{Loc: loc}
		if p.peekKeyword("on") {
			p.advance()
			in.TypeCondition = p.namedType()
		}
		in.Directives = p.directives(false)
		in.SelectionSet = p.selectionSet()
		return in
	}
	f := &Field{Loc: p.tok.loc}
	name, _ := p.name()
	if p.skip(tokColon) {
		f.Alias = name
		name, _ = p.name()
	}
	f.Name = name
	f.Arguments = p.arguments(false)
	f.Directives = p.directives(false)
	if p.peek(tokBraceL) {
		f.SelectionSet = p.selectionSet()
	}
	return f
}

func (p *parser) arguments(constant bool) []*Argument {
	if !p.peek(tokParenL) {
		return nil
	}
	p.advance()
	var args []*Argument
	for {
		a := &Argument{Loc: p.tok.loc}
		a.Name, _ = p.name()
		p.expect(tokColon)
		a.Value = p.value(constant)
		args = append(args, a)
		if p.skip(tokParenR) {
			return args
		}
	}
}

func (p *parser) fragment() *FragmentDefinition {
	f := &FragmentDefinition{Loc: p.expectKeyword("fragment").loc}
	if p.peekKeyword("on") {
		p.unexpected()
	}
	f.Name, f.NameLoc = p.name()
	p.expectKeyword("on")
	f.TypeCondition = p.namedType()
	f.Directives = p.directives(false)
	f.SelectionSet = p.selectionSet()
	return f
}

func (p *parser) value(constant bool) Value {
	t := p.tok
	switch t.kind {
	case tokBracketL:
		p.advance()
		l := &ListValue{Loc: t.loc}
		for !p.skip(tokBracketR) {
			l.Values = append(l.Values, p.value(constant))
		}
		return l
	case tokBraceL:
		p.advance()
		o := &ObjectValue{Loc: t.loc}
		for !p.skip(tokBraceR) {
			f := &ObjectField{Loc: p.tok.loc}
			f.Name, _ = p.name()
			p.expect(tokColon)
			f.Value = p.value(constant)
			o.Fields = append(o.Fields, f)
		}
		return o
	case tokInt:
		p.advance()
		return &IntValue{Loc: t.loc, Value: t.value}
	case tokFloat:
		p.advance()
		return &FloatValue{Loc: t.loc, Value: t.value}
	case tokString, tokBlockString:
		p.advance()
		return &StringValue{Loc: t.loc, Value: t.value, Block: t.kind == tokBlockString}
	case tokName:
		p.advance()
		switch t.value {
		case "true", "false":
			return &BooleanValue{Loc: t.loc, Value: t.value == "true"}
		case "null":
			return &NullValue{Loc: t.loc}
		}
		return &EnumValue{Loc: t.loc, Value: t.value}
	case tokDollar:
		if !constant {
			return p.variable()
		}
		p.advance()
		if p.peek(tokName) {
			p.fail(t.loc, "Unexpected variable \"$%s\" in constant value.", p.tok.value)
		}
		p.fail(t.loc, "Unexpected \"$\".")
	}
	p.unexpected()
	return nil
}

func (p *parser) directives(constant bool) []*Directive {
	var ds []*Directive
	for p.peek(tokAt) {
		d := &Directive{Loc: p.advance().loc}
		d.Name, _ = p.name()
		d.Arguments = p.arguments(constant)
		ds = append(ds, d)
	}
	return ds
}

func (p *parser) typeRef() TypeRef {
	var t TypeRef
	if p.peek(tokBracketL) {
		loc := p.advance().loc
		inner := p.typeRef()
		p.expect(tokBracketR)
		t = &ListType{Loc: loc, Type: inner}
	} else {
		t = p.namedType()
	}
	if p.peek(tokBang) {
		p.advance()
		return &NonNullType{Loc: t.Location(), Type: t}
	}
	return t
}

func (p *parser) namedType() *NamedType {
	t := p.expect(tokName)
	return &NamedType{Loc: t.loc, Name: t.value}
}

// description reads an optional description.
func (p *parser) description() string {
	if p.peek(tokString) || p.peek(tokBlockString) {
		return p.advance().value
	}
	return ""
}

func (p *parser) typeSystem(at token, hasDesc bool) Definition {
	desc := ""
	loc := p.tok.loc
	if hasDesc {
		desc, loc = at.value, at.loc
	}
	switch kw := p.advance(); kw.value {
	case "schema":
		d := &SchemaDefinition{Loc: loc, Description: desc}
		d.Directives = p.directives(true)
		p.expect(tokBraceL)
		for {
			d.OperationTypes = append(d.OperationTypes, p.operationTypeDefinition())
			if p.skip(tokBraceR) {
				return d
			}
		}
	case "scalar":
		d := &ScalarTypeDefinition{Loc: loc, Description: desc}
		d.Name, _ = p.name()
		d.Directives = p.directives(true)
		return d
	case "type":
		d := &ObjectTypeDefinition{Loc: loc, Description: desc}
		d.Name, _ = p.name()
		d.Interfaces = p.implements()
		d.Directives = p.directives(true)
		d.Fields = p.fieldsDefinition()
		return d
	case "interface":
		d := &InterfaceTypeDefinition{Loc: loc, Description: desc}
		d.Name, _ = p.name()
		d.Interfaces = p.implements()
		d.Directives = p.directives(true)
		d.Fields = p.fieldsDefinition()
		return d
	case "union":
		d := &UnionTypeDefinition{Loc: loc, Description: desc}
		d.Name, _ = p.name()
		d.Directives = p.directives(true)
		d.Types = p.unionMembers()
		return d
	case "enum":
		d := &EnumTypeDefinition{Loc: loc, Description: desc}
		d.Name, _ = p.name()
		d.Directives = p.directives(true)
		d.Values = p.enumValues()
		return d
	case "input":
		d := &InputObjectTypeDefinition{Loc: loc, Description: desc}
		d.Name, _ = p.name()
		d.Directives = p.directives(true)
		d.Fields = p.inputFields()
		return d
	default: // directive
		d := &DirectiveDefinition{Loc: loc, Description: desc}
		p.expect(tokAt)
		d.Name, _ = p.name()
		d.Arguments = p.argumentDefinitions()
		if p.peekKeyword("repeatable") {
			p.advance()
			d.Repeatable = true
		}
		p.expectKeyword("on")
		p.skip(tokPipe)
		for {
			t := p.expect(tokName)
			if !directiveLocations[t.value] {
				p.fail(t.loc, "Unexpected %s; expected a directive location.", t.describe())
			}
			d.Locations = append(d.Locations, t.value)
			if !p.skip(tokPipe) {
				return d
			}
		}
	}
}

// directiveLocations are the locations a directive definition may name.
var directiveLocations = map[string]bool{
	"QUERY": true, "MUTATION": true, "SUBSCRIPTION": true, "FIELD": true, "FRAGMENT_DEFINITION": true,
	"FRAGMENT_SPREAD": true, "INLINE_FRAGMENT": true, "VARIABLE_DEFINITION": true,
	"SCHEMA": true, "SCALAR": true, "OBJECT": true, "FIELD_DEFINITION": true, "ARGUMENT_DEFINITION": true,
	"INTERFACE": true, "UNION": true, "ENUM": true, "ENUM_VALUE": true, "INPUT_OBJECT": true, "INPUT_FIELD_DEFINITION": true,
}

func (p *parser) operationTypeDefinition() *OperationTypeDefinition {
	d := &OperationTypeDefinition{Loc: p.tok.loc}
	d.Operation = p.operationType()
	p.expect(tokColon)
	d.Type = p.namedType()
	return d
}

func (p *parser) implements() []*NamedType {
	if !p.peekKeyword("implements") {
		return nil
	}
	p.advance()
	p.skip(tokAmp)
	var out []*NamedType
	for {
		out = append(out, p.namedType())
		if !p.skip(tokAmp) {
			return out
		}
	}
}

func (p *parser) fieldsDefinition() []*FieldDefinition {
	if !p.peek(tokBraceL) {
		return nil
	}
	p.advance()
	var out []*FieldDefinition
	for {
		f := &FieldDefinition{Loc: p.tok.loc}
		f.Description = p.description()
		f.Name, _ = p.name()
		f.Arguments = p.argumentDefinitions()
		p.expect(tokColon)
		f.Type = p.typeRef()
		f.Directives = p.directives(true)
		out = append(out, f)
		if p.skip(tokBraceR) {
			return out
		}
	}
}

func (p *parser) argumentDefinitions() []*InputValueDefinition {
	if !p.peek(tokParenL) {
		return nil
	}
	p.advance()
	var out []*InputValueDefinition
	for {
		out = append(out, p.inputValueDefinition())
		if p.skip(tokParenR) {
			return out
		}
	}
}

func (p *parser) inputValueDefinition() *InputValueDefinition {
	d := &InputValueDefinition{Loc: p.tok.loc}
	d.Description = p.description()
	d.Name, _ = p.name()
	p.expect(tokColon)
	d.Type = p.typeRef()
	if p.skip(tokEquals) {
		d.DefaultValue = p.value(true)
	}
	d.Directives = p.directives(true)
	return d
}

func (p *parser) unionMembers() []*NamedType {
	if !p.skip(tokEquals) {
		return nil
	}
	p.skip(tokPipe)
	var out []*NamedType
	for {
		out = append(out, p.namedType())
		if !p.skip(tokPipe) {
			return out
		}
	}
}

func (p *parser) enumValues() []*EnumValueDefinition {
	if !p.peek(tokBraceL) {
		return nil
	}
	p.advance()
	var out []*EnumValueDefinition
	for {
		d := &EnumValueDefinition{Loc: p.tok.loc}
		d.Description = p.description()
		t := p.expect(tokName)
		if t.value == "true" || t.value == "false" || t.value == "null" {
			p.fail(t.loc, "Name %q is reserved and cannot be used for an enum value.", t.value)
		}
		d.Name = t.value
		d.Directives = p.directives(true)
		out = append(out, d)
		if p.skip(tokBraceR) {
			return out
		}
	}
}

func (p *parser) inputFields() []*InputValueDefinition {
	if !p.peek(tokBraceL) {
		return nil
	}
	p.advance()
	var out []*InputValueDefinition
	for {
		out = append(out, p.inputValueDefinition())
		if p.skip(tokBraceR) {
			return out
		}
	}
}

func (p *parser) extension() Definition {
	p.expectKeyword("extend")
	if !p.peek(tokName) {
		p.unexpected()
	}
	at := p.tok
	if at.value == "schema" {
		p.advance()
		d := &SchemaDefinition{Loc: at.loc, Extend: true}
		d.Directives = p.directives(true)
		if p.skip(tokBraceL) {
			for !p.skip(tokBraceR) {
				d.OperationTypes = append(d.OperationTypes, p.operationTypeDefinition())
			}
		}
		if len(d.Directives) == 0 && len(d.OperationTypes) == 0 {
			p.unexpected()
		}
		return d
	}
	switch at.value {
	case "scalar", "type", "interface", "union", "enum", "input":
	default:
		p.unexpected()
	}
	d := p.typeSystem(token{}, false)
	empty := false
	switch d := d.(type) {
	case *ScalarTypeDefinition:
		d.Extend = true
		empty = len(d.Directives) == 0
	case *ObjectTypeDefinition:
		d.Extend = true
		empty = len(d.Directives) == 0 && len(d.Interfaces) == 0 && len(d.Fields) == 0
	case *InterfaceTypeDefinition:
		d.Extend = true
		empty = len(d.Directives) == 0 && len(d.Interfaces) == 0 && len(d.Fields) == 0
	case *UnionTypeDefinition:
		d.Extend = true
		empty = len(d.Directives) == 0 && len(d.Types) == 0
	case *EnumTypeDefinition:
		d.Extend = true
		empty = len(d.Directives) == 0 && len(d.Values) == 0
	case *InputObjectTypeDefinition:
		d.Extend = true
		empty = len(d.Directives) == 0 && len(d.Fields) == 0
	}
	if empty {
		p.unexpected()
	}
	return d
}
