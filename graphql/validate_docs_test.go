package graphql

import "testing"

// Documents and operations (5.1, 5.2).

func TestExecutableDefinitions(t *testing.T) {
	expectValid(t, `query Foo { dog { ...Frag } } fragment Frag on Dog { name }`)
	expectErrors(t, `
query Foo { dog { name } }
type Cow { name: String }
extend type Dog { color: String }
schema { query: QueryRoot }`,
		`The "Cow" definition is not executable. (3:1)`,
		`The "Dog" definition is not executable. (4:8)`,
		`The schema definition is not executable. (5:1)`)
}

func TestUniqueOperationNames(t *testing.T) {
	expectValid(t, `{ dog { name } }`)
	expectValid(t, `query Foo { dog { name } } query Bar { dog { name } }`)
	expectValid(t, `query Foo { dog { ...Foo } } fragment Foo on Dog { name }`)
	expectErrors(t, `
query Foo { dog { name } }
query Foo { cat { name } }`,
		`There can be only one operation named "Foo". (2:7, 3:7)`)
	expectErrors(t, `
query Foo { dog { name } }
mutation Foo { createDog { name } }
subscription Foo { newDog { name } }`,
		`There can be only one operation named "Foo". (2:7, 3:10)`,
		`There can be only one operation named "Foo". (2:7, 4:14)`)
}

func TestLoneAnonymousOperation(t *testing.T) {
	expectValid(t, `{ dog { name } }`)
	expectValid(t, `{ dog { ...F } } fragment F on Dog { name }`)
	expectErrors(t, `
{ dog { name } }
{ cat { name } }`,
		`This anonymous operation must be the only defined operation. (2:1)`,
		`This anonymous operation must be the only defined operation. (3:1)`)
	expectErrors(t, `
{ dog { name } }
mutation Foo { createDog { name } }`,
		`This anonymous operation must be the only defined operation. (2:1)`)
}

func TestSingleFieldSubscriptions(t *testing.T) {
	expectValid(t, `subscription S { newDog { name } }`)
	expectValid(t, `subscription S { newDog { name } newDog { nickname } }`)
	expectValid(t, `subscription S { ...F } fragment F on SubscriptionRoot { newDog { name } }`)
	expectErrors(t, `subscription S { newDog { name } newCat { name } }`,
		`Subscription "S" must select only one top level field. (1:34)`)
	expectErrors(t, `subscription { newDog { name } ... on SubscriptionRoot { newCat { name } } }`,
		`Anonymous Subscription must select only one top level field. (1:58)`)
	expectErrors(t, `subscription S { ...F } fragment F on SubscriptionRoot { newDog { name } newCat { name } }`,
		`Subscription "S" must select only one top level field. (1:74)`)
	expectErrors(t, `subscription S { __typename }`,
		`Subscription "S" must not select an introspection top level field. (1:18)`)
}

func TestUnsupportedOperation(t *testing.T) {
	s, err := NewSchema(SchemaConfig{Query: &Object{Name: "Query", Fields: []*FieldDef{{Name: "a", Type: String}}}})
	if err != nil {
		t.Fatal(err)
	}
	if errs := validate(t, s, `mutation { a }`); errorsText(errs) != `Schema is not configured to execute mutation operation. (1:1)`+"\n"+`Cannot query field "a" on type "Mutation".`+" (1:12)" && errorsText(errs) != `Schema is not configured to execute mutation operation. (1:1)` {
		t.Errorf("got:\n%s", errorsText(errs))
	}
}
