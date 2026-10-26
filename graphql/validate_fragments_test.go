package graphql

import "testing"

// Fragments (5.5).

func TestUniqueFragmentNames(t *testing.T) {
	expectValid(t, `{ dog { ...A ...B } } fragment A on Dog { name } fragment B on Dog { nickname }`)
	expectErrors(t, `{ dog { ...A } } fragment A on Dog { name } fragment A on Dog { nickname }`,
		`There can be only one fragment named "A". (1:27, 1:54)`)
}

func TestKnownTypeNamesInFragments(t *testing.T) {
	expectValid(t, `{ dog { ... on Dog { name } ...F } } fragment F on Being { name }`)
	expectErrors(t, `{ dog { ... on Badger { name } ...F } } fragment F on Peat { name }`,
		`Unknown type "Badger". (1:16)`,
		`Unknown type "Peat". Did you mean "Pet" or "Cat"? (1:55)`)
}

func TestFragmentsOnCompositeTypes(t *testing.T) {
	expectValid(t, `{ dog { ...A ...B ...C ... { name } } } fragment A on Dog { name } fragment B on Pet { name } fragment C on CatOrDog { __typename }`)
	expectErrors(t, `{ dog { ...F } } fragment F on Boolean { x }`,
		`Fragment "F" cannot condition on non composite type "Boolean". (1:32)`)
	expectErrors(t, `{ dog { ... on FurColor { x } } }`,
		`Fragment cannot condition on non composite type "FurColor". (1:16)`)
	expectErrors(t, `{ dog { ...F } } fragment F on ComplexInput { x }`,
		`Fragment "F" cannot condition on non composite type "ComplexInput". (1:32)`)
}

func TestKnownFragmentNames(t *testing.T) {
	expectValid(t, `{ dog { ...F } } fragment F on Dog { name }`)
	expectErrors(t, `{ dog { ...Missing name } }`,
		`Unknown fragment "Missing". (1:9)`)
}

func TestNoUnusedFragments(t *testing.T) {
	expectValid(t, `{ dog { ...A } } fragment A on Dog { ...B } fragment B on Dog { name }`)
	expectErrors(t, `{ dog { name } } fragment Unused on Dog { name } fragment AlsoUnused on Dog { ...Unused }`,
		`Fragment "Unused" is never used. (1:18)`,
		`Fragment "AlsoUnused" is never used. (1:50)`)
	// A fragment only an unused fragment spreads is unused too, cycles
	// included.
	expectErrors(t, `{ dog { name } } fragment A on Dog { ...B } fragment B on Dog { ...A }`,
		`Cannot spread fragment "A" within itself via "B". (1:38, 1:65)`,
		`Fragment "A" is never used. (1:18)`,
		`Fragment "B" is never used. (1:45)`)
}

func TestPossibleFragmentSpreads(t *testing.T) {
	expectValid(t, `{ dog { ... on Dog { name } ... on Pet { name } ... on CatOrDog { __typename } ... on Canine { name } } }`)
	expectValid(t, `{ pet { ... on Dog { barks } ... on Cat { meows } ... on Being { name } } }`)
	expectValid(t, `{ catOrDog { ... on Pet { name } ... on Dog { barks } } }`)
	expectValid(t, `{ being { ... on Pet { name } ... on Intelligent { iq } } }`)
	expectErrors(t, `{ dog { ... on Cat { meows } } }`,
		`Fragment cannot be spread here as objects of type "Dog" can never be of type "Cat". (1:9)`)
	expectErrors(t, `{ dog { ...C } } fragment C on Cat { meows }`,
		`Fragment "C" cannot be spread here as objects of type "Dog" can never be of type "Cat". (1:9)`)
	expectErrors(t, `{ pet { ... on Human { iq } } }`,
		`Fragment cannot be spread here as objects of type "Pet" can never be of type "Human". (1:9)`)
	expectErrors(t, `{ catOrDog { ... on HumanOrAlien { __typename } } }`,
		`Fragment cannot be spread here as objects of type "CatOrDog" can never be of type "HumanOrAlien". (1:14)`)
	expectErrors(t, `{ pet { ... on Intelligent { iq } } }`,
		`Fragment cannot be spread here as objects of type "Pet" can never be of type "Intelligent". (1:9)`)
	expectErrors(t, `{ human { ... on DogOrHuman { __typename } ... on CatOrDog { __typename } } }`,
		`Fragment cannot be spread here as objects of type "Human" can never be of type "CatOrDog". (1:44)`)
}

func TestNoFragmentCycles(t *testing.T) {
	expectValid(t, `{ dog { ...A ...B } } fragment A on Dog { ...B } fragment B on Dog { name }`)
	expectValid(t, `{ dog { ...A } } fragment A on Dog { mother { ...B } father { ...B } } fragment B on Dog { name }`)
	expectErrors(t, `{ dog { ...A } } fragment A on Dog { ...A }`,
		`Cannot spread fragment "A" within itself. (1:38)`)
	expectErrors(t, `{ dog { ...A } } fragment A on Dog { mother { ...A } }`,
		`Cannot spread fragment "A" within itself. (1:47)`)
	expectErrors(t, `{ dog { ...A } } fragment A on Dog { ...B } fragment B on Dog { ...C } fragment C on Dog { ...A }`,
		`Cannot spread fragment "A" within itself via "B", "C". (1:38, 1:65, 1:92)`)
	// Each cycle once, even reached from several places.
	expectErrors(t, `{ dog { ...A ...B } } fragment A on Dog { ...B } fragment B on Dog { ...A }`,
		`Cannot spread fragment "A" within itself via "B". (1:43, 1:70)`)
}
