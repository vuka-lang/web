package graphql

import "testing"

// Arguments (5.4) and directives (5.7).

func TestKnownArgumentNames(t *testing.T) {
	expectValid(t, `{ dog { doesKnowCommand(dogCommand: SIT) isAtLocation(x: 1, y: 2) } }`)
	expectValid(t, `{ dog { name @include(if: true) } }`)
	expectErrors(t, `{ dog { doesKnowCommand(command: SIT) } }`,
		`Unknown argument "command" on field "Dog.doesKnowCommand". Did you mean "dogCommand"? (1:25)`)
	expectErrors(t, `{ dog { name(unknown: 1) } }`,
		`Unknown argument "unknown" on field "Dog.name". (1:14)`)
	expectErrors(t, `{ dog { name @skip(unless: true, if: false) } }`,
		`Unknown argument "unless" on directive "@skip". (1:20)`)
	// An unknown field's arguments aren't checked against anything.
	expectErrors(t, `{ dog { unknownField(x: 1) } }`,
		`Cannot query field "unknownField" on type "Dog". (1:9)`)
}

func TestUniqueArgumentNames(t *testing.T) {
	expectValid(t, `{ dog { isAtLocation(x: 1, y: 2) } }`)
	expectErrors(t, `{ dog { isAtLocation(x: 1, x: 2) } }`,
		`There can be only one argument named "x". (1:22, 1:28)`)
	expectErrors(t, `{ dog { name @withArgs(req: 1, req: 2) } }`,
		`There can be only one argument named "req". (1:24, 1:32)`)
}

func TestProvidedRequiredArguments(t *testing.T) {
	expectValid(t, `{ complicatedArgs { multipleReqs(req1: 1, req2: 2) nonNullFieldWithDefault multipleOpts multipleOptAndReq(req1: 1, req2: 2) } }`)
	expectValid(t, `{ dog { isHouseTrained } }`)
	expectErrors(t, `{ complicatedArgs { multipleReqs(req2: 2) } }`,
		`Field "multipleReqs" argument "req1" of type "Int!" is required, but it was not provided. (1:21)`)
	expectErrors(t, `{ complicatedArgs { multipleReqs } }`,
		`Field "multipleReqs" argument "req1" of type "Int!" is required, but it was not provided. (1:21)`,
		`Field "multipleReqs" argument "req2" of type "Int!" is required, but it was not provided. (1:21)`)
	expectErrors(t, `{ dog { name @include } }`,
		`Directive "@include" argument "if" of type "Boolean!" is required, but it was not provided. (1:14)`)
	expectErrors(t, `{ dog { name @withArgs(opt: "y") } }`,
		`Directive "@withArgs" argument "req" of type "Int!" is required, but it was not provided. (1:14)`)
}

func TestKnownDirectives(t *testing.T) {
	expectValid(t, `query Q($v: Boolean! @onVariableDefinition) @onQuery { dog @onField { ...F @onFragmentSpread ... @onInlineFragment { barks @include(if: $v) } } } fragment F on Dog @onFragmentDefinition { name }`)
	expectValid(t, `mutation @onMutation { createDog { name } } `)
	expectValid(t, `subscription @onSubscription { newDog { name } }`)
	expectErrors(t, `{ dog @unknown { name } }`,
		`Unknown directive "@unknown". (1:7)`)
	expectErrors(t, `query @onField { dog @onQuery { name @onInlineFragment } }`,
		`Directive "@onField" may not be used on QUERY. (1:7)`,
		`Directive "@onQuery" may not be used on FIELD. (1:22)`,
		`Directive "@onInlineFragment" may not be used on FIELD. (1:38)`)
	expectErrors(t, `query Q($v: Int @onField) { dog { name } }`,
		`Directive "@onField" may not be used on VARIABLE_DEFINITION. (1:17)`,
		`Variable "$v" is never used in operation "Q". (1:9)`)
	expectErrors(t, `mutation @onQuery { createDog { name } }`,
		`Directive "@onQuery" may not be used on MUTATION. (1:10)`)
	expectErrors(t, `{ dog { ...F } } fragment F on Dog @onField { name }`,
		`Directive "@onField" may not be used on FRAGMENT_DEFINITION. (1:36)`)
}

func TestUniqueDirectivesPerLocation(t *testing.T) {
	expectValid(t, `{ dog @onField { name } }`)
	expectValid(t, `{ dog @repeatable @repeatable { name } }`)
	expectValid(t, `{ dog { name @skip(if: false) @include(if: true) } }`)
	expectErrors(t, `{ dog @onField @onField { name } }`,
		`The directive "@onField" can only be used once at this location. (1:7, 1:16)`)
	expectErrors(t, `{ dog { name @skip(if: true) @skip(if: false) @skip(if: true) } }`,
		`The directive "@skip" can only be used once at this location. (1:14, 1:30)`,
		`The directive "@skip" can only be used once at this location. (1:14, 1:47)`)
}
