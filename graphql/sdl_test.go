package graphql

import (
	"strings"
	"testing"

	"github.com/vuka-lang/web/v2/graphql/language"
)

func TestSDLPrint(t *testing.T) {
	node := &Interface{Name: "Node", Description: "Has an id.", Fields: []*FieldDef{{Name: "id", Type: &NonNull{Of: ID}}}}
	status := &Enum{Name: "Status", Values: []*EnumValueDef{{Name: "ACTIVE"}, {Name: "BANNED", Description: "Can't sign in.", Deprecated: true, DeprecationReason: "use SUSPENDED"}, {Name: "OLD", Deprecated: true}}}
	user := &Object{Name: "User", Description: "A person.\nWho signs in.", Interfaces: []*Interface{node}, Fields: []*FieldDef{
		{Name: "id", Type: &NonNull{Of: ID}},
		{Name: "name", Description: "Display name.", Type: &NonNull{Of: String}},
		{Name: "status", Type: status, Deprecated: true},
		{Name: "orders", Type: &NonNull{Of: &List{Of: &NonNull{Of: String}}}, Args: []*ArgDef{{Name: "first", Type: Int, Default: &language.IntValue{Value: "10"}}, {Name: "after", Type: String}}},
		{Name: "posts", Type: &List{Of: String}, Args: []*ArgDef{{Name: "tag", Description: "Only these.", Type: &NonNull{Of: String}}}},
	}}
	input := &InputObject{Name: "NewUser", Fields: []*InputFieldDef{{Name: "name", Type: &NonNull{Of: String}}, {Name: "status", Type: status, Default: &language.EnumValue{Value: "ACTIVE"}}, {Name: "nick", Type: String, Deprecated: true, DeprecationReason: "gone"}}}
	date := &Scalar{Name: "Date", SpecifiedByURL: "https://example.com/date", Serialize: func(v any) (any, error) { return v, nil }, ParseValue: func(v any) (any, error) { return v, nil }}
	search := &Union{Name: "Search", Types: []*Object{user}}
	q := &Object{Name: "Query", Fields: []*FieldDef{
		{Name: "me", Type: user},
		{Name: "search", Type: &List{Of: search}, Args: []*ArgDef{{Name: "text", Type: &NonNull{Of: String}}}},
		{Name: "today", Type: date},
	}}
	m := &Object{Name: "Mutation", Fields: []*FieldDef{{Name: "createUser", Type: user, Args: []*ArgDef{{Name: "in", Type: &NonNull{Of: input}}}}}}
	s, err := NewSchema(SchemaConfig{Query: q, Mutation: m, Directives: []*Directive{{Name: "cached", Locations: []string{"FIELD", "QUERY"}, Repeatable: true, Args: []*ArgDef{{Name: "seconds", Type: Int}}}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `directive @cached(seconds: Int) repeatable on FIELD | QUERY

type Query {
  me: User
  search(text: String!): [Search]
  today: Date
}

type Mutation {
  createUser(in: NewUser!): User
}

scalar Date @specifiedBy(url: "https://example.com/date")

input NewUser {
  name: String!
  status: Status = ACTIVE
  nick: String @deprecated(reason: "gone")
}

"""Has an id."""
interface Node {
  id: ID!
}

union Search = User

enum Status {
  ACTIVE
  """Can't sign in."""
  BANNED @deprecated(reason: "use SUSPENDED")
  OLD @deprecated
}

"""
A person.
Who signs in.
"""
type User implements Node {
  id: ID!
  """Display name."""
  name: String!
  status: Status @deprecated
  orders(first: Int = 10, after: String): [String!]!
  posts(
    """Only these."""
    tag: String!
  ): [String]
}
`
	got := s.SDL()
	if got != want {
		t.Errorf("SDL:\n%s\nwant:\n%s", got, want)
	}
	// It reads back as the same schema.
	again, err := ParseSchema(got)
	if err != nil {
		t.Fatal(err)
	}
	if again.SDL() != got {
		t.Errorf("round trip:\n%s", again.SDL())
	}
	if d := Diff(s, again); len(d) != 0 {
		t.Errorf("diff with itself: %v", d)
	}
	// Non-default root names get a schema block.
	r, _ := NewSchema(SchemaConfig{Query: &Object{Name: "Root", Fields: []*FieldDef{{Name: "a", Type: String}}}, Description: "The API."})
	if sdl := r.SDL(); !strings.HasPrefix(sdl, "\"\"\"The API.\"\"\"\nschema {\n  query: Root\n}\n\ntype Root {") {
		t.Errorf("schema block:\n%s", sdl)
	}
}

func TestParseSchemaErrors(t *testing.T) {
	for _, c := range []struct{ sdl, msg string }{
		{`type Query { a: Missing }`, `unknown type "Missing"`},
		{`type Query { a: Int } type Query { b: Int }`, `there can be only one type named "Query"`},
		{`query { a }`, `not operations or fragments`},
		{`extend type Nope { a: Int }`, `cannot extend type "Nope"`},
		{`type Query { a: Int } union U = Query | Int`, `union member "Int" is not an object type`},
		{`type Query implements Int { a: Int }`, `"Int" is not an interface`},
		{`schema { query: Nope } type Query { a: Int }`, `root type "Nope" is not defined`},
		{`type Query {`, `Syntax Error`},
		{`type Query { a: Int } type Query2 implements Node { id: ID } interface Node { id: ID! }`, `Interface field Node.id expects type ID! but Query2.id is type ID.`},
	} {
		if _, err := ParseSchema(c.sdl); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v, want %q", c.sdl, err, c.msg)
		}
	}
	s, err := ParseSchema(`type Query { a: Int } extend type Query { b: String } type Mutation { m: Int }`)
	if err != nil {
		t.Fatal(err)
	}
	if s.FieldOf(s.QueryType(), "b") == nil || s.MutationType() == nil {
		t.Errorf("extension or default mutation root missing:\n%s", s.SDL())
	}
}

func TestDiff(t *testing.T) {
	old := `
type Query {
  user(id: ID!, verbose: Boolean): User
  users(first: Int = 10): [User!]!
  legacy: String
  count: Int!
  tags: [String]
}
type User implements Node { id: ID! name: String nick: String email: String! }
interface Node { id: ID! }
enum Status { ACTIVE BANNED }
input Filter { name: String status: Status count: Int! }
union Search = User | Other
type Other { x: Int }
scalar Date
type Gone { x: Int }
directive @old on FIELD
`
	new := `
type Query {
  user(id: ID, extra: String!, more: Int): User
  users(first: Int = 20): [User]!
  count: Int
  tags: [String!]
  added: Int
}
type User { id: ID! name: String! nick: Int email: String! phone: String }
interface Node { id: ID! }
enum Status { ACTIVE SUSPENDED }
input Filter { name: String! status: Status count: Int maybe: Int must: Int! }
union Search = User
type Other { x: Int }
input Date { x: Int }
type Fresh { y: Int }
`
	a, err := ParseSchema(old)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseSchema(new)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range Diff(a, b) {
		lines = append(lines, c.Path+": "+c.String())
	}
	want := `Date: BREAKING Type Date changed from a scalar type to an input object type.
Filter.name: BREAKING Input field Filter.name changed type from String to String!.
Filter.count: Input field Filter.count changed type from Int! to Int.
Filter.maybe: An optional input field Filter.maybe was added.
Filter.must: BREAKING A required input field Filter.must was added.
Gone: BREAKING Type Gone was removed.
Query.user(id:): Argument Query.user(id:) changed type from ID! to ID.
Query.user(verbose:): BREAKING Argument Query.user(verbose:) was removed.
Query.user(extra:): BREAKING A required argument Query.user(extra:) was added.
Query.user(more:): An optional argument Query.user(more:) was added.
Query.users: BREAKING Field Query.users changed type from [User!]! to [User]!.
Query.users(first:): Argument Query.users(first:) changed its default value from 10 to 20.
Query.legacy: BREAKING Field Query.legacy was removed.
Query.count: BREAKING Field Query.count changed type from Int! to Int.
Query.tags: Field Query.tags changed type from [String] to [String!].
Query.added: Field Query.added was added.
Search: BREAKING Member Other was removed from union type Search.
Status.BANNED: BREAKING Enum value Status.BANNED was removed.
Status.SUSPENDED: Enum value Status.SUSPENDED was added.
User.name: Field User.name changed type from String to String!.
User.nick: BREAKING Field User.nick changed type from String to Int.
User.phone: Field User.phone was added.
User: BREAKING User no longer implements interface Node.
Fresh: Type Fresh was added.
@old: BREAKING Directive @old was removed.`
	if got := strings.Join(lines, "\n"); got != want {
		t.Errorf("diff:\n%s\nwant:\n%s", got, want)
	}
}
