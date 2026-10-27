package graphql

import "testing"

// Fields (5.3).

func TestFieldsOnCorrectType(t *testing.T) {
	expectValid(t, `{ dog { name barkVolume ... on Dog { nickname } } }`)
	expectValid(t, `{ pet { __typename name ... on Dog { barks } } catOrDog { __typename ... on Cat { meows } } }`)
	expectValid(t, `{ __typename __schema { queryType { name } } __type(name: "Dog") { name } }`)
	expectValid(t, `{ dog { aliased: name } }`)
	expectErrors(t, `{ dog { meowVolume } }`,
		`Cannot query field "meowVolume" on type "Dog". Did you mean "barkVolume"? (1:9)`)
	expectErrors(t, `{ dog { unknown { deeper } } }`,
		`Cannot query field "unknown" on type "Dog". (1:9)`)
	expectErrors(t, `{ pet { barks } }`,
		`Cannot query field "barks" on type "Pet". Did you mean to use an inline fragment on "Dog"? (1:9)`)
	expectErrors(t, `{ catOrDog { name } }`,
		`Cannot query field "name" on type "CatOrDog". Did you mean to use an inline fragment on "Canine", "Cat", "Dog", or "Pet"? (1:14)`)
	expectErrors(t, `fragment F on Dog { __schema { types { name } } } { dog { ...F } }`,
		`Cannot query field "__schema" on type "Dog". (1:21)`)
	expectErrors(t, `{ dog { name { first } } }`,
		`Field "name" must not have a selection since type "String" has no subfields. (1:14)`)
}

func TestScalarLeafs(t *testing.T) {
	expectValid(t, `{ dog { barks } }`)
	expectErrors(t, `{ dog }`,
		`Field "dog" of type "Dog" must have a selection of subfields. Did you mean "dog { ... }"? (1:3)`)
	expectErrors(t, `{ human { pets } }`,
		`Field "pets" of type "[Pet]" must have a selection of subfields. Did you mean "pets { ... }"? (1:11)`)
	expectErrors(t, `{ dog { barks { sinceWhen } } }`,
		`Field "barks" must not have a selection since type "Boolean" has no subfields. (1:15)`)
	expectErrors(t, `{ dog { doesKnowCommand(dogCommand: SIT) { x } } }`,
		`Field "doesKnowCommand" must not have a selection since type "Boolean" has no subfields. (1:42)`)
	expectErrors(t, `{ __type(name: "Dog") }`,
		`Field "__type" of type "__Type" must have a selection of subfields. Did you mean "__type { ... }"? (1:3)`)
}

func TestOverlappingFieldsCanBeMerged(t *testing.T) {
	expectValid(t, `{ dog { name name } }`)
	expectValid(t, `{ dog { otherName: name otherName: name } }`)
	expectValid(t, `{ dog { doesKnowCommand(dogCommand: SIT) doesKnowCommand(dogCommand: SIT) } }`)
	expectValid(t, `query ($c: DogCommand) { dog { doesKnowCommand(dogCommand: $c) doesKnowCommand(dogCommand: $c) } }`)
	expectValid(t, `{ dog { isAtLocation(x: 0, y: 1) isAtLocation(y: 1, x: 0) } }`)
	// Different object types can't both apply: same key, different fields.
	expectValid(t, `{ catOrDog { ... on Dog { name: barks } ... on Cat { name: meows } } }`)
	expectValid(t, `{ pet { ... on Dog { name } ... on Cat { name: nickname } } }`)
	expectErrors(t, `{ dog { fido: name fido: nickname } }`,
		`Fields "fido" conflict because "name" and "nickname" are different fields. Use different aliases on the fields to fetch both if this was intentional. (1:9, 1:20)`)
	expectErrors(t, `{ dog { doesKnowCommand(dogCommand: SIT) doesKnowCommand(dogCommand: HEEL) } }`,
		`Fields "doesKnowCommand" conflict because they have differing arguments. Use different aliases on the fields to fetch both if this was intentional. (1:9, 1:42)`)
	expectErrors(t, `{ dog { doesKnowCommand doesKnowCommand(dogCommand: HEEL) } }`,
		`Fields "doesKnowCommand" conflict because they have differing arguments. Use different aliases on the fields to fetch both if this was intentional. (1:9, 1:25)`)
	expectErrors(t, `query ($a: DogCommand, $b: DogCommand) { dog { doesKnowCommand(dogCommand: $a) doesKnowCommand(dogCommand: $b) } }`,
		`Fields "doesKnowCommand" conflict because they have differing arguments. Use different aliases on the fields to fetch both if this was intentional. (1:48, 1:80)`)
	// Through fragments.
	expectErrors(t, `{ dog { ...A ...B } } fragment A on Dog { x: name } fragment B on Dog { x: nickname }`,
		`Fields "x" conflict because "name" and "nickname" are different fields. Use different aliases on the fields to fetch both if this was intentional. (1:43, 1:73)`)
	expectErrors(t, `{ dog { x: barks ...A } } fragment A on Dog { x: name }`,
		`Fields "x" conflict because "barks" and "name" are different fields. Use different aliases on the fields to fetch both if this was intentional. (1:9, 1:47)`)
	// Even across different object types, the shapes must agree.
	expectErrors(t, `{ catOrDog { ... on Dog { v: barkVolume } ... on Cat { v: nickname } } }`,
		`Fields "v" conflict because they return conflicting types "Int" and "String". Use different aliases on the fields to fetch both if this was intentional. (1:27, 1:56)`)
	expectErrors(t, `{ dogOrHuman { ... on Dog { m: mother { name } } ... on Human { m: relatives { name } } } }`,
		`Fields "m" conflict because they return conflicting types "Dog" and "[Human]". Use different aliases on the fields to fetch both if this was intentional. (1:29, 1:65)`)
	// Deep conflicts name the subfields.
	expectErrors(t, `{ dog { mother { x: name } mother { x: barks } } }`,
		`Fields "mother" conflict because subfields "x" conflict because "name" and "barks" are different fields. Use different aliases on the fields to fetch both if this was intentional. (1:9, 1:18, 1:28, 1:37)`)
	expectErrors(t, `{ dog { mother { father { x: name } } mother { father { x: nickname } } } }`,
		`Fields "mother" conflict because subfields "father" conflict because subfields "x" conflict because "name" and "nickname" are different fields. Use different aliases on the fields to fetch both if this was intentional. (1:9, 1:18, 1:27, 1:39, 1:48, 1:57)`)
	// Recursive fragments don't loop.
	expectErrors(t, `{ dog { ...A } } fragment A on Dog { name ...A }`,
		`Cannot spread fragment "A" within itself. (1:43)`)
	// Compared once per pair of fragments.
	expectValid(t, `{ dog { ...A ...B ...A } } fragment A on Dog { name } fragment B on Dog { name }`)
}
