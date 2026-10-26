package graphql

import (
	"strings"
	"testing"
)

// fullIntrospection is the query tools send to read a whole schema.
const fullIntrospection = `
query IntrospectionQuery {
  __schema {
    description
    queryType { name }
    mutationType { name }
    subscriptionType { name }
    types { ...FullType }
    directives {
      name
      description
      isRepeatable
      locations
      args(includeDeprecated: true) { ...InputValue }
    }
  }
}

fragment FullType on __Type {
  kind
  name
  description
  specifiedByURL
  fields(includeDeprecated: true) {
    name
    description
    args(includeDeprecated: true) { ...InputValue }
    type { ...TypeRef }
    isDeprecated
    deprecationReason
  }
  inputFields(includeDeprecated: true) { ...InputValue }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) { name description isDeprecated deprecationReason }
  possibleTypes { ...TypeRef }
}

fragment InputValue on __InputValue {
  name
  description
  type { ...TypeRef }
  defaultValue
  isDeprecated
  deprecationReason
}

fragment TypeRef on __Type {
  kind
  name
  ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } } } }
}`

func introspectionSchema(t *testing.T) *Schema {
	t.Helper()
	s, err := ParseSchema(`
"A pet store."
schema { query: Root }

"Something with an id."
interface Node { id: ID! }

"A pet."
type Pet implements Node {
  id: ID!
  "What it's called."
  name(upper: Boolean = false @deprecated(reason: "always upper")): String!
  tags: [String!]!
  kind: Kind @deprecated(reason: "use species")
  species: Kind
}

enum Kind { DOG CAT BIRD @deprecated }

input PetFilter { kind: Kind = DOG, name: String @deprecated }

scalar Date @specifiedBy(url: "https://tools.ietf.org/html/rfc3339")

union Search = Pet

type Root {
  pet(id: ID!): Pet
  pets(filter: PetFilter, first: Int = 10): [Pet!]!
  search(text: String!): [Search]
  born: Date
}

directive @cached(seconds: Int = 60) repeatable on FIELD | QUERY
`)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIntrospectionFull(t *testing.T) {
	s := introspectionSchema(t)
	res := run(t, s, fullIntrospection)
	if len(res.Errors) > 0 {
		t.Fatalf("errors: %s", errorsText(res.Errors))
	}
	js := jsonOf(t, res)
	for _, want := range []string{
		`"description":"A pet store."`,
		`"queryType":{"name":"Root"},"mutationType":null,"subscriptionType":null`,
		`{"kind":"OBJECT","name":"Pet","description":"A pet.","specifiedByURL":null,"fields":[{"name":"id","description":null,"args":[],"type":{"kind":"NON_NULL","name":null,"ofType":{"kind":"SCALAR","name":"ID","ofType":null}},"isDeprecated":false,"deprecationReason":null}`,
		`{"name":"name","description":"What it's called.","args":[{"name":"upper","description":null,"type":{"kind":"SCALAR","name":"Boolean","ofType":null},"defaultValue":"false","isDeprecated":true,"deprecationReason":"always upper"}]`,
		`{"name":"tags","description":null,"args":[],"type":{"kind":"NON_NULL","name":null,"ofType":{"kind":"LIST","name":null,"ofType":{"kind":"NON_NULL","name":null,"ofType":{"kind":"SCALAR","name":"String","ofType":null}}}},"isDeprecated":false,"deprecationReason":null}`,
		`{"name":"kind","description":null,"args":[],"type":{"kind":"ENUM","name":"Kind","ofType":null},"isDeprecated":true,"deprecationReason":"use species"}`,
		`"interfaces":[{"kind":"INTERFACE","name":"Node","ofType":null}],"enumValues":null,"possibleTypes":null}`,
		`{"kind":"INTERFACE","name":"Node","description":"Something with an id.","specifiedByURL":null,"fields":[{"name":"id"`,
		`"interfaces":[],"enumValues":null,"possibleTypes":[{"kind":"OBJECT","name":"Pet","ofType":null}]}`,
		`"enumValues":[{"name":"DOG","description":null,"isDeprecated":false,"deprecationReason":null},{"name":"CAT","description":null,"isDeprecated":false,"deprecationReason":null},{"name":"BIRD","description":null,"isDeprecated":true,"deprecationReason":"No longer supported"}]`,
		`{"kind":"INPUT_OBJECT","name":"PetFilter","description":null,"specifiedByURL":null,"fields":null,"inputFields":[{"name":"kind","description":null,"type":{"kind":"ENUM","name":"Kind","ofType":null},"defaultValue":"DOG","isDeprecated":false,"deprecationReason":null},{"name":"name","description":null,"type":{"kind":"SCALAR","name":"String","ofType":null},"defaultValue":null,"isDeprecated":true,"deprecationReason":"No longer supported"}]`,
		`{"kind":"SCALAR","name":"Date","description":null,"specifiedByURL":"https://tools.ietf.org/html/rfc3339"`,
		`{"kind":"UNION","name":"Search","description":null,"specifiedByURL":null,"fields":null,"inputFields":null,"interfaces":null,"enumValues":null,"possibleTypes":[{"kind":"OBJECT","name":"Pet","ofType":null}]}`,
		`{"name":"cached","description":null,"isRepeatable":true,"locations":["FIELD","QUERY"],"args":[{"name":"seconds","description":null,"type":{"kind":"SCALAR","name":"Int","ofType":null},"defaultValue":"60","isDeprecated":false,"deprecationReason":null}]}`,
		`{"name":"skip","description":"Directs the executor to skip this field or fragment when the ` + "`if`" + ` argument is true.","isRepeatable":false,"locations":["FIELD","FRAGMENT_SPREAD","INLINE_FRAGMENT"]`,
		`{"kind":"OBJECT","name":"__Schema"`,
		`{"kind":"ENUM","name":"__TypeKind"`,
		`{"name":"deprecated","description":"Marks an element of a GraphQL schema as no longer supported."`,
		`"defaultValue":"\"No longer supported\""`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("introspection lacks %s", want)
		}
	}
	// Types sorted by name, built-ins and introspection types included.
	var names []string
	types, _ := res.Data.values[0].(*OrderedMap).Get("types")
	for _, ty := range types.([]any) {
		n, _ := ty.(*OrderedMap).Get("name")
		names = append(names, n.(string))
	}
	if got := strings.Join(names, " "); got != "Boolean Date ID Int Kind Node Pet PetFilter Root Search String __Directive __DirectiveLocation __EnumValue __Field __InputValue __Schema __Type __TypeKind" {
		t.Errorf("types: %s", got)
	}
}

func TestIntrospectionFields(t *testing.T) {
	s := introspectionSchema(t)
	expectJSON(t, run(t, s, `{ __type(name: "Pet") { name kind fields { name } } missing: __type(name: "Nope") { name } }`),
		`{"data":{"__type":{"name":"Pet","kind":"OBJECT","fields":[{"name":"id"},{"name":"name"},{"name":"tags"},{"name":"species"}]},"missing":null}}`)
	expectJSON(t, run(t, s, `{ __type(name: "Kind") { enumValues { name } } }`),
		`{"data":{"__type":{"enumValues":[{"name":"DOG"},{"name":"CAT"}]}}}`)
	expectJSON(t, run(t, s, `{ __typename }`),
		`{"data":{"__typename":"Root"}}`)
}

func TestIntrospectionDisabled(t *testing.T) {
	s := introspectionSchema(t)
	res := runWith(t, Config{Schema: s}, `{ __schema { types { name } } __type(name: "Pet") { name } __typename }`)
	expectJSON(t, res, `{"errors":[{"message":"Introspection is disabled: \"__schema\" can't be queried.","locations":[{"line":1,"column":3}],"extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}},{"message":"Introspection is disabled: \"__type\" can't be queried.","locations":[{"line":1,"column":31}],"extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`)
	expectJSON(t, runWith(t, Config{Schema: s}, `{ __typename }`), `{"data":{"__typename":"Root"}}`)
}
