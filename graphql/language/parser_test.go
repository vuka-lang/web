package language

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) *Document {
	t.Helper()
	doc, err := Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return doc
}

func TestParseOperation(t *testing.T) {
	doc := mustParse(t, `
query Users($first: Int = 10, $after: String!, $ids: [ID!]! @dir) @op(a: 1) {
  list: users(first: $first, after: $after, filter: {name: "x", tags: ["a", B], deep: {ok: true}}) @include(if: $flag) {
    id
    ...UserBits @spread
    ... on User @inline {
      name
    }
    ... @skip(if: false) {
      email
    }
  }
}

fragment UserBits on User @frag {
  id
}`)
	if len(doc.Definitions) != 2 {
		t.Fatalf("definitions: %d", len(doc.Definitions))
	}
	op := doc.Definitions[0].(*OperationDefinition)
	if op.Operation != OpQuery || op.Name != "Users" || op.Loc != (Loc{2, 1}) || op.NameLoc != (Loc{2, 7}) {
		t.Fatalf("op: %+v", op)
	}
	if len(op.VariableDefinitions) != 3 {
		t.Fatalf("vars: %d", len(op.VariableDefinitions))
	}
	v0 := op.VariableDefinitions[0]
	if v0.Variable != "first" || v0.Type.String() != "Int" || v0.DefaultValue.(*IntValue).Value != "10" {
		t.Errorf("v0: %+v", v0)
	}
	if got := op.VariableDefinitions[2].Type.String(); got != "[ID!]!" {
		t.Errorf("v2 type %s", got)
	}
	if op.VariableDefinitions[2].Directives[0].Name != "dir" || op.Directives[0].Name != "op" {
		t.Errorf("directives")
	}
	f := op.SelectionSet.Selections[0].(*Field)
	if f.Alias != "list" || f.Name != "users" || f.ResponseKey() != "list" || len(f.Arguments) != 3 || f.Loc != (Loc{3, 3}) {
		t.Fatalf("field: %+v", f)
	}
	obj := f.Arguments[2].Value.(*ObjectValue)
	if len(obj.Fields) != 3 || obj.Fields[1].Value.(*ListValue).Values[1].(*EnumValue).Value != "B" {
		t.Errorf("object: %s", Print(obj))
	}
	sels := f.SelectionSet.Selections
	if _, ok := sels[1].(*FragmentSpread); !ok {
		t.Errorf("spread: %T", sels[1])
	}
	if in := sels[2].(*InlineFragment); in.TypeCondition.Name != "User" || in.Directives[0].Name != "inline" {
		t.Errorf("inline: %+v", in)
	}
	if in := sels[3].(*InlineFragment); in.TypeCondition != nil || in.Directives[0].Name != "skip" {
		t.Errorf("inline without type: %+v", in)
	}
	fr := doc.Definitions[1].(*FragmentDefinition)
	if fr.Name != "UserBits" || fr.TypeCondition.Name != "User" || fr.Directives[0].Name != "frag" {
		t.Errorf("fragment: %+v", fr)
	}
	if len(doc.Operations()) != 1 || doc.Fragments()["UserBits"] != fr {
		t.Errorf("accessors")
	}
}

func TestParseShorthandAndKinds(t *testing.T) {
	doc := mustParse(t, `{ a } mutation { b } subscription S { c }`)
	ops := doc.Operations()
	if !ops[0].Shorthand || ops[0].Operation != OpQuery || ops[1].Operation != OpMutation || ops[2].Name != "S" {
		t.Fatalf("ops: %+v %+v %+v", ops[0], ops[1], ops[2])
	}
}

func TestParseValues(t *testing.T) {
	for _, c := range []struct {
		src  string
		want string
	}{
		{`123`, `123`}, {`-1.5e3`, `-1.5e3`}, {`"str"`, `"str"`}, {`"""block"""`, `"""block"""`},
		{`true`, `true`}, {`false`, `false`}, {`null`, `null`}, {`ENUM`, `ENUM`},
		{`[]`, `[]`}, {`[1, [2, 3]]`, `[1, [2, 3]]`}, {`{}`, `{}`}, {`{a: 1, b: {c: [d]}}`, `{a: 1, b: {c: [d]}}`},
		{`$var`, `$var`}, {`[$a, {b: $b}]`, `[$a, {b: $b}]`},
	} {
		v, err := ParseValue(c.src, false)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if got := Print(v); got != c.want {
			t.Errorf("%s: printed %s", c.src, got)
		}
	}
	if _, err := ParseValue("$x", true); err == nil || !strings.Contains(err.Error(), `Unexpected variable "$x" in constant value.`) {
		t.Errorf("const variable: %v", err)
	}
	if _, err := ParseValue("[1", false); err == nil {
		t.Errorf("unclosed list parsed")
	}
	if b := mustParseValue(t, "true").(*BooleanValue); !b.Value {
		t.Errorf("bool")
	}
}

func mustParseValue(t *testing.T, src string) Value {
	t.Helper()
	v, err := ParseValue(src, false)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseType(t *testing.T) {
	for _, s := range []string{"Int", "Int!", "[Int]", "[Int!]!", "[[String]!]"} {
		ty, err := ParseType(s)
		if err != nil || ty.String() != s {
			t.Errorf("%s: %v %v", s, ty, err)
		}
	}
	if _, err := ParseType("Int!!"); err == nil {
		t.Errorf("Int!! parsed")
	}
}

func TestParseErrors(t *testing.T) {
	for _, c := range []struct {
		src       string
		msg       string
		line, col int
	}{
		{"", "Unexpected <EOF>.", 1, 1},
		{"{", "Expected Name, found <EOF>.", 1, 2},
		{"{ }", "Expected Name, found \"}\".", 1, 3},
		{"{ a(  ) }", "Expected Name, found \")\".", 1, 7},
		{"query", "Expected \"{\", found <EOF>.", 1, 6},
		{"query Name($a) { b }", "Expected \":\", found \")\".", 1, 14},
		{"query ($a: Int = $b) { c }", "Unexpected variable \"$b\" in constant value.", 1, 18},
		{"{ ...on }", "Expected Name, found \"}\".", 1, 9},
		{"fragment on on T { a }", "Unexpected Name \"on\".", 1, 10},
		{"fragment F T { a }", "Expected \"on\", found Name \"T\".", 1, 12},
		{"notanoperation { a }", "Unexpected Name \"notanoperation\".", 1, 1},
		{"{ a: }", "Expected Name, found \"}\".", 1, 6},
		{"{ a(b: ) }", "Unexpected \")\".", 1, 8},
		{"{ a } }", "Unexpected \"}\".", 1, 7},
		{"\"desc\" query { a }", "Unexpected Name \"query\".", 1, 8},
		{"{ a @ }", "Expected Name, found \"}\".", 1, 7},
		{"query Q($v: ) { a }", "Expected Name, found \")\".", 1, 13},
		{"{ a(b: [1 }", "Unexpected \"}\".", 1, 11},
	} {
		_, err := Parse(c.src)
		se, ok := err.(*SyntaxError)
		if !ok || se.Message != c.msg || se.Line != c.line || se.Column != c.col {
			t.Errorf("%q: got %v, want %q at %d:%d", c.src, err, c.msg, c.line, c.col)
		}
	}
}

func TestParseSDL(t *testing.T) {
	doc := mustParse(t, `
"""
The schema.
"""
schema @s { query: Q mutation: M }

"A scalar" scalar Date @specifiedBy(url: "https://example.com")

type User implements & Node & Named @t {
  "the id"
  id: ID!
  friends("how many" first: Int = 10 @deprecated, after: String): [User!]! @deprecated(reason: "no")
}

interface Node implements Named { id: ID! }
interface Named { name: String }

union Pet = | Dog | Cat
union Empty

enum Color { RED "green" GREEN @deprecated BLUE }

input Filter { name: String = "x", tags: [String!] @deprecated }

directive @auth(role: String!) repeatable on FIELD_DEFINITION | OBJECT

extend type User { email: String }
extend schema @x
extend scalar Date @y
`)
	defs := doc.Definitions
	sd := defs[0].(*SchemaDefinition)
	if sd.Description != "The schema." || len(sd.OperationTypes) != 2 || sd.OperationTypes[1].Operation != OpMutation {
		t.Errorf("schema: %+v", sd)
	}
	if s := defs[1].(*ScalarTypeDefinition); s.Name != "Date" || s.Description != "A scalar" {
		t.Errorf("scalar: %+v", s)
	}
	u := defs[2].(*ObjectTypeDefinition)
	if len(u.Interfaces) != 2 || u.Fields[0].Description != "the id" || u.Fields[1].Arguments[0].Description != "how many" || u.Fields[1].Arguments[0].DefaultValue == nil {
		t.Errorf("type: %+v", u)
	}
	if i := defs[3].(*InterfaceTypeDefinition); i.Interfaces[0].Name != "Named" {
		t.Errorf("interface: %+v", i)
	}
	if un := defs[5].(*UnionTypeDefinition); len(un.Types) != 2 {
		t.Errorf("union: %+v", un)
	}
	if e := defs[7].(*EnumTypeDefinition); len(e.Values) != 3 || e.Values[1].Description != "green" {
		t.Errorf("enum: %+v", e)
	}
	if in := defs[8].(*InputObjectTypeDefinition); len(in.Fields) != 2 {
		t.Errorf("input: %+v", in)
	}
	if d := defs[9].(*DirectiveDefinition); !d.Repeatable || len(d.Locations) != 2 || d.Arguments[0].Name != "role" {
		t.Errorf("directive: %+v", d)
	}
	if x := defs[10].(*ObjectTypeDefinition); !x.Extend || x.Fields[0].Name != "email" {
		t.Errorf("extend: %+v", x)
	}
	if x := defs[11].(*SchemaDefinition); !x.Extend {
		t.Errorf("extend schema: %+v", x)
	}
	for _, c := range []struct{ src, msg string }{
		{"enum E { true }", `Name "true" is reserved and cannot be used for an enum value.`},
		{"directive @d on NOWHERE", `Unexpected Name "NOWHERE"; expected a directive location.`},
		{"extend type T", `Unexpected <EOF>.`},
		{"type T { f(): Int }", `Expected Name, found ")".`},
		{"type T { f: }", `Expected Name, found "}".`},
	} {
		if _, err := Parse(c.src); err == nil || err.(*SyntaxError).Message != c.msg {
			t.Errorf("%q: %v, want %q", c.src, err, c.msg)
		}
	}
}

func TestPrintRoundTrip(t *testing.T) {
	src := `query Q($a: Int = 1, $b: [String!]! @d) @op {
  alias: field(arg: {x: [1, 2.5, "s", ENUM, null, true]}, v: $a) @include(if: true) {
    sub
    ...F
    ... on T {
      x
    }
    ... @skip(if: false) {
      y
    }
  }
}

fragment F on T {
  z(s: """block
  string""")
}

{
  short
}`
	doc := mustParse(t, src)
	out := Print(doc)
	again := Print(mustParse(t, out))
	if out != again {
		t.Fatalf("printing isn't stable:\n%s\n---\n%s", out, again)
	}
	if !strings.Contains(out, "alias: field(arg: {x: [1, 2.5, \"s\", ENUM, null, true]}, v: $a) @include(if: true) {") {
		t.Errorf("printed:\n%s", out)
	}
	if !strings.HasSuffix(out, "{\n  short\n}") {
		t.Errorf("shorthand printed:\n%s", out)
	}
}

func TestPrintStrings(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain", `"plain"`},
		{"q\"\\\n\t\x01", `"q\"\\\n\t\u0001"`},
	} {
		if got := PrintString(c.in); got != c.want {
			t.Errorf("PrintString(%q) = %s", c.in, got)
		}
	}
	for _, in := range []string{"one line", "two\nlines", "  indented first", `ends with "`, `has """ inside`, "trailing\\", ""} {
		lit := PrintBlockString(in, "")
		v, err := ParseValue(lit, true)
		if err != nil {
			t.Errorf("%q printed as %s: %v", in, lit, err)
			continue
		}
		if got := v.(*StringValue).Value; got != in {
			t.Errorf("%q printed as %s reads back as %q", in, lit, got)
		}
	}
}
