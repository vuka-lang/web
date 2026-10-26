package graphql

import (
	"strings"
	"testing"

	"github.com/vuka-lang/web/v2/graphql/language"
)

func queryOf(fields ...*FieldDef) *Object { return &Object{Name: "Query", Fields: fields} }

func schemaErr(t *testing.T, c SchemaConfig, want ...string) {
	t.Helper()
	_, err := NewSchema(c)
	if err == nil {
		t.Fatalf("NewSchema succeeded, want %q", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error\n%s\ndoesn't say %q", err, w)
		}
	}
}

func TestSchemaValid(t *testing.T) {
	node := &Interface{Name: "Node", Fields: []*FieldDef{{Name: "id", Type: &NonNull{Of: ID}}}}
	named := &Interface{Name: "Named", Interfaces: []*Interface{node}, Fields: []*FieldDef{{Name: "id", Type: &NonNull{Of: ID}}, {Name: "name", Type: String, Args: []*ArgDef{{Name: "upper", Type: Boolean}}}}}
	user := &Object{Name: "User", Interfaces: []*Interface{node, named}, Fields: []*FieldDef{
		{Name: "id", Type: &NonNull{Of: ID}},
		{Name: "name", Type: &NonNull{Of: String}, Args: []*ArgDef{{Name: "upper", Type: Boolean}, {Name: "extra", Type: Int}}},
		{Name: "friends", Type: &List{Of: &NonNull{Of: named}}},
	}}
	s, err := NewSchema(SchemaConfig{Query: queryOf(&FieldDef{Name: "me", Type: user}, &FieldDef{Name: "node", Type: node})})
	if err != nil {
		t.Fatal(err)
	}
	if s.Type("User") != user || s.QueryType().Name != "Query" || s.Type("Float") != nil {
		t.Errorf("types: %v", s.Types())
	}
	if got := s.PossibleTypes(node); len(got) != 1 || got[0] != user {
		t.Errorf("possible types of Node: %v", got)
	}
	if s.Type("__Schema") == nil || s.Type("String") != String || s.Directive("skip") == nil {
		t.Errorf("built-ins missing")
	}
}

func TestSchemaRules(t *testing.T) {
	schemaErr(t, SchemaConfig{}, "Query root type must be provided.")
	q := queryOf(&FieldDef{Name: "a", Type: String})
	schemaErr(t, SchemaConfig{Query: q, Mutation: q}, `All root types must be different, "Query" type is used as query and mutation root types.`)
	schemaErr(t, SchemaConfig{Query: queryOf()}, "Type Query must define one or more fields.")
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "__a", Type: String}, &FieldDef{Name: "b-c", Type: String})},
		`Name "__a" must not begin with "__"`, `Names must match /^[_a-zA-Z][_a-zA-Z0-9]*$/ but field "b-c" does not.`)
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: String}, &FieldDef{Name: "a", Type: Int})}, "Field Query.a can only be defined once.")
	in := &InputObject{Name: "In", Fields: []*InputFieldDef{{Name: "x", Type: String}}}
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: in})}, "The type of Query.a must be Output Type but got: In.")
	obj := &Object{Name: "Obj", Fields: []*FieldDef{{Name: "x", Type: String}}}
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: String, Args: []*ArgDef{{Name: "o", Type: obj}}})}, "The type of Query.a(o:) must be Input Type but got: Obj.")
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: String, Args: []*ArgDef{{Name: "r", Type: &NonNull{Of: Int}, Deprecated: true}}})}, "Required argument Query.a(r:) cannot be deprecated.")
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: String, Args: []*ArgDef{{Name: "r", Type: Int, Default: &language.StringValue{Value: "x"}}}})}, "Query.a(r:) has an invalid default value: Int cannot represent non-integer value")
	dup := &Object{Name: "Query", Fields: []*FieldDef{{Name: "x", Type: String}}}
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: dup})}, `Schema must contain uniquely named types but contains multiple types named "Query".`)

	iface := &Interface{Name: "I", Fields: []*FieldDef{{Name: "id", Type: &NonNull{Of: ID}, Args: []*ArgDef{{Name: "x", Type: Int}}}, {Name: "f", Type: String}}}
	impl := &Object{Name: "Impl", Interfaces: []*Interface{iface, iface}, Fields: []*FieldDef{{Name: "id", Type: ID, Args: []*ArgDef{{Name: "x", Type: String}, {Name: "y", Type: &NonNull{Of: Int}}}}}}
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: impl})},
		"Type Impl can only implement I once.",
		"Interface field I.id expects type ID! but Impl.id is type ID.",
		"Interface field argument I.id(x:) expects type Int but Impl.id(x:) is type String.",
		"Argument Impl.id(y:) must not be required type Int! if not provided by the Interface field I.id.",
		"Interface field I.f expected but Impl does not provide it.")
	base := &Interface{Name: "Base", Fields: []*FieldDef{{Name: "id", Type: ID}}}
	mid := &Interface{Name: "Mid", Interfaces: []*Interface{base}, Fields: []*FieldDef{{Name: "id", Type: ID}}}
	leaf := &Object{Name: "Leaf", Interfaces: []*Interface{mid}, Fields: []*FieldDef{{Name: "id", Type: ID}}}
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: leaf})}, "Type Leaf must implement Base because it is implemented by Mid.")
	self := &Interface{Name: "Self", Fields: []*FieldDef{{Name: "id", Type: ID}}}
	self.Interfaces = []*Interface{self}
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: self})}, "Type Self cannot implement itself")

	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: &Union{Name: "U"}})}, "Union type U must define one or more member types.")
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: &Union{Name: "U", Types: []*Object{obj, obj}}})}, "Union type U can only include type Obj once.")
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: &Enum{Name: "E"}})}, "Enum type E must define one or more values.")
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: &Enum{Name: "E", Values: []*EnumValueDef{{Name: "true"}, {Name: "A"}, {Name: "A"}}}})},
		"Enum type E cannot include value: true.", "Enum value E.A can only be defined once.")
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: String, Args: []*ArgDef{{Name: "i", Type: &InputObject{Name: "Empty"}}}})}, "Input Object type Empty must define one or more fields.")
	loop := &InputObject{Name: "Loop"}
	other := &InputObject{Name: "Other", Fields: []*InputFieldDef{{Name: "back", Type: &NonNull{Of: loop}}}}
	loop.Fields = []*InputFieldDef{{Name: "next", Type: &NonNull{Of: other}}, {Name: "ok", Type: &List{Of: &NonNull{Of: loop}}}}
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: String, Args: []*ArgDef{{Name: "i", Type: loop}}})}, `Cannot reference Input Object "Loop" within itself through a series of non-null fields: "Loop.next.Other.back".`)
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a", Type: String, Args: []*ArgDef{{Name: "i", Type: &InputObject{Name: "Bad", Fields: []*InputFieldDef{{Name: "o", Type: obj}}}}}})}, "The type of Bad.o must be Input Type but got: Obj.")
	schemaErr(t, SchemaConfig{Query: q, Directives: []*Directive{{Name: "skip", Locations: []string{"FIELD"}}}}, "Directive @skip is declared twice.")
	schemaErr(t, SchemaConfig{Query: q, Directives: []*Directive{{Name: "nowhere"}}}, "Directive @nowhere must include 1 or more locations.")
	schemaErr(t, SchemaConfig{Query: queryOf(&FieldDef{Name: "a"})}, "Query.a has no type.")
}

// Several scalars may share a name: the schema shows one, each converts
// values where it is used.
func TestScalarCodecs(t *testing.T) {
	age := &Scalar{Name: "Int", Serialize: func(v any) (any, error) { return v.(int) * 2, nil }, ParseValue: Int.ParseValue, ParseLiteral: Int.ParseLiteral}
	date1 := &Scalar{Name: "Date", Serialize: func(v any) (any, error) { return "d1", nil }, ParseValue: func(v any) (any, error) { return v, nil }}
	date2 := &Scalar{Name: "Date", Serialize: func(v any) (any, error) { return "d2", nil }, ParseValue: func(v any) (any, error) { return v, nil }}
	s, err := NewSchema(SchemaConfig{Query: queryOf(
		&FieldDef{Name: "age", Type: age, Resolve: constant(21)},
		&FieldDef{Name: "plain", Type: Int, Resolve: constant(21)},
		&FieldDef{Name: "a", Type: date1, Resolve: constant(1)},
		&FieldDef{Name: "b", Type: date2, Resolve: constant(1)},
	)})
	if err != nil {
		t.Fatal(err)
	}
	if s.Type("Int") != Int || s.Type("Date") != date1 {
		t.Errorf("shown: %v %v", s.Type("Int"), s.Type("Date"))
	}
	res := run(t, s, `{ age plain a b }`)
	if got := jsonOf(t, res); got != `{"data":{"age":42,"plain":21,"a":"d1","b":"d2"}}` {
		t.Errorf("got %s", got)
	}
	if sdl := s.SDL(); strings.Count(sdl, "scalar Date") != 1 || strings.Contains(sdl, "scalar Int") {
		t.Errorf("SDL:\n%s", sdl)
	}
}
