package graphql

import "testing"

// Values (5.6) and variables (5.8).

func TestValuesOfCorrectType(t *testing.T) {
	for _, q := range []string{
		`{ complicatedArgs { intArgField(intArg: 2) } }`,
		`{ complicatedArgs { intArgField(intArg: -2147483648) } }`,
		`{ complicatedArgs { floatArgField(floatArg: 1) } }`,
		`{ complicatedArgs { floatArgField(floatArg: 1.1e3) } }`,
		`{ complicatedArgs { idArgField(idArg: 1) } }`,
		`{ complicatedArgs { idArgField(idArg: "someIdString") } }`,
		`{ complicatedArgs { stringArgField(stringArg: "x") } }`,
		`{ complicatedArgs { stringArgField(stringArg: """block""") } }`,
		`{ complicatedArgs { booleanArgField(booleanArg: true) } }`,
		`{ complicatedArgs { enumArgField(enumArg: NO_FUR) } }`,
		`{ complicatedArgs { intArgField(intArg: null) } }`,
		`{ complicatedArgs { stringListArgField(stringListArg: ["one", null, "two"]) } }`,
		`{ complicatedArgs { stringListArgField(stringListArg: []) } }`,
		`{ complicatedArgs { stringListArgField(stringListArg: "one") } }`,
		`{ complicatedArgs { stringListNonNullArgField(stringListNonNullArg: ["one", "two"]) } }`,
		`{ complicatedArgs { complexArgField(complexArg: {requiredField: true}) } }`,
		`{ complicatedArgs { complexArgField(complexArg: {requiredField: false, nonNullField: true, intField: 4, stringListField: ["a"]}) } }`,
		`{ complicatedArgs { complexArgField(complexArg: {stringListField: "single", requiredField: true}) } }`,
		`{ dog { isHouseTrained(atOtherHomes: true) } }`,
	} {
		expectValid(t, q)
	}
	expectErrors(t, `{ complicatedArgs { intArgField(intArg: "3") } }`,
		`Int cannot represent non-integer value: "3" (1:41)`)
	expectErrors(t, `{ complicatedArgs { intArgField(intArg: 2147483648) } }`,
		`Int cannot represent non 32-bit signed integer value: 2147483648 (1:41)`)
	expectErrors(t, `{ complicatedArgs { intArgField(intArg: 3.5) } }`,
		`Int cannot represent non-integer value: 3.5 (1:41)`)
	expectErrors(t, `{ complicatedArgs { intArgField(intArg: FOUR) } }`,
		`Int cannot represent non-integer value: FOUR (1:41)`)
	expectErrors(t, `{ complicatedArgs { floatArgField(floatArg: "3.5") } }`,
		`Float cannot represent non numeric value: "3.5" (1:45)`)
	expectErrors(t, `{ complicatedArgs { stringArgField(stringArg: 1) } }`,
		`String cannot represent a non string value: 1 (1:47)`)
	expectErrors(t, `{ complicatedArgs { booleanArgField(booleanArg: "true") } }`,
		`Boolean cannot represent a non boolean value: "true" (1:49)`)
	expectErrors(t, `{ complicatedArgs { idArgField(idArg: 1.0) } }`,
		`ID cannot represent a non-string and non-integer value: 1.0 (1:39)`)
	expectErrors(t, `{ complicatedArgs { enumArgField(enumArg: "BROWN") } }`,
		`Enum "FurColor" cannot represent non-enum value: "BROWN". Did you mean the enum value "BROWN"? (1:43)`)
	expectErrors(t, `{ complicatedArgs { enumArgField(enumArg: brown) } }`,
		`Value "brown" does not exist in "FurColor" enum. Did you mean "BROWN"? (1:43)`)
	expectErrors(t, `{ complicatedArgs { enumArgField(enumArg: 1) } }`,
		`Enum "FurColor" cannot represent non-enum value: 1. (1:43)`)
	expectErrors(t, `{ complicatedArgs { nonNullIntArgField(nonNullIntArg: null) } }`,
		`Expected value of type "Int!", found null. (1:55)`)
	expectErrors(t, `{ complicatedArgs { stringListArgField(stringListArg: ["one", 2]) } }`,
		`String cannot represent a non string value: 2 (1:63)`)
	expectErrors(t, `{ complicatedArgs { stringListNonNullArgField(stringListNonNullArg: ["one", null]) } }`,
		`Expected value of type "String!", found null. (1:77)`)
	expectErrors(t, `{ complicatedArgs { complexArgField(complexArg: {intField: 4}) } }`,
		`Field "ComplexInput.requiredField" of required type "Boolean!" was not provided. (1:49)`)
	expectErrors(t, `{ complicatedArgs { complexArgField(complexArg: {requiredField: true, nonNullField: null}) } }`,
		`Expected value of type "Boolean!", found null. (1:85)`)
	expectErrors(t, `{ complicatedArgs { complexArgField(complexArg: {requiredField: true, unknownField: "x", requierdField: true}) } }`,
		`Field "unknownField" is not defined by type "ComplexInput". (1:71)`,
		`Field "requierdField" is not defined by type "ComplexInput". Did you mean "requiredField"? (1:90)`)
	expectErrors(t, `{ complicatedArgs { complexArgField(complexArg: "x") } }`,
		`Expected value of type "ComplexInput", found "x". (1:49)`)
	expectErrors(t, `{ complicatedArgs { complexArgField(complexArg: {requiredField: true, requiredField: false}) } }`,
		`There can be only one input field named "requiredField". (1:50, 1:71)`)
	// Directive arguments are values too.
	expectErrors(t, `{ dog { name @include(if: "yes") } }`,
		`Boolean cannot represent a non boolean value: "yes" (1:27)`)
	// Variables' defaults.
	expectValid(t, `query ($a: Int = 1, $b: String = "ok", $c: ComplexInput = {requiredField: true}) { complicatedArgs { a: intArgField(intArg: $a) b: stringArgField(stringArg: $b) c: complexArgField(complexArg: $c) } }`)
	expectErrors(t, `query ($a: Int = "one", $c: ComplexInput = {intField: 1}) { complicatedArgs { a: intArgField(intArg: $a) c: complexArgField(complexArg: $c) } }`,
		`Int cannot represent non-integer value: "one" (1:18)`,
		`Field "ComplexInput.requiredField" of required type "Boolean!" was not provided. (1:44)`)
}

func TestUniqueVariableNames(t *testing.T) {
	expectValid(t, `query ($a: Int, $b: Int) { complicatedArgs { a: intArgField(intArg: $a) b: intArgField(intArg: $b) } }`)
	expectErrors(t, `query ($a: Int, $a: Int) { complicatedArgs { intArgField(intArg: $a) } }`,
		`There can be only one variable named "$a". (1:8, 1:17)`)
}

func TestVariablesAreInputTypes(t *testing.T) {
	expectValid(t, `query ($a: String, $c: ComplexInput, $d: FurColor) { complicatedArgs { a: stringArgField(stringArg: $a) c: complexArgField(complexArg: $c) d: enumArgField(enumArg: $d) } }`)
	expectErrors(t, `query ($a: Dog, $b: [[CatOrDog!]]!, $c: Pet) { dog { name } }`,
		`Variable "$a" cannot be non-input type "Dog". (1:12)`,
		`Variable "$b" cannot be non-input type "[[CatOrDog!]]!". (1:21)`,
		`Variable "$c" cannot be non-input type "Pet". (1:41)`,
		`Variable "$a" is never used. (1:8)`,
		`Variable "$b" is never used. (1:17)`,
		`Variable "$c" is never used. (1:37)`)
	expectErrors(t, `query ($a: Unknown) { dog { name } }`,
		`Unknown type "Unknown". (1:12)`,
		`Variable "$a" is never used. (1:8)`)
}

func TestNoUndefinedVariables(t *testing.T) {
	expectValid(t, `query Q($a: Int) { complicatedArgs { intArgField(intArg: $a) } }`)
	expectValid(t, `query Q($a: Boolean) { dog { ...F } } fragment F on Dog { name(surname: $a) }`)
	expectErrors(t, `query Q { complicatedArgs { intArgField(intArg: $a) } }`,
		`Variable "$a" is not defined by operation "Q". (1:49, 1:1)`)
	expectErrors(t, `{ complicatedArgs { intArgField(intArg: $a) } }`,
		`Variable "$a" is not defined. (1:41, 1:1)`)
	// A fragment's variables are checked against each operation spreading it.
	expectErrors(t, `query A($s: Boolean) { dog { ...F } } query B { dog { ...F } } fragment F on Dog { name(surname: $s) }`,
		`Variable "$s" is not defined by operation "B". (1:98, 1:39)`)
	expectErrors(t, `{ dog { name @include(if: $flag) } }`,
		`Variable "$flag" is not defined. (1:27, 1:1)`)
}

func TestNoUnusedVariables(t *testing.T) {
	expectValid(t, `query Q($a: Boolean) { dog { ...F } } fragment F on Dog { ...G } fragment G on Dog { name(surname: $a) }`)
	expectErrors(t, `query Q($a: Int, $b: Boolean) { complicatedArgs { intArgField(intArg: $a) } }`,
		`Variable "$b" is never used in operation "Q". (1:18)`)
	expectErrors(t, `query A($s: Boolean) { dog { ...F } } query B($s: Boolean) { dog { name } } fragment F on Dog { name(surname: $s) }`,
		`Variable "$s" is never used in operation "B". (1:47)`)
}

func TestVariablesInAllowedPosition(t *testing.T) {
	for _, q := range []string{
		`query ($a: Boolean) { dog { name(surname: $a) } }`,
		`query ($a: Boolean!) { dog { name(surname: $a) } }`,
		`query ($a: Int!) { complicatedArgs { nonNullIntArgField(nonNullIntArg: $a) } }`,
		`query ($a: Int = 1) { complicatedArgs { nonNullIntArgField(nonNullIntArg: $a) } }`,
		`query ($a: Int) { complicatedArgs { nonNullFieldWithDefault(arg: $a) } }`,
		`query ($a: [String]) { complicatedArgs { stringListArgField(stringListArg: $a) } }`,
		`query ($a: [String!]) { complicatedArgs { stringListArgField(stringListArg: $a) } }`,
		`query ($a: String) { complicatedArgs { stringListArgField(stringListArg: [$a]) } }`,
		`query ($a: String!) { complicatedArgs { stringListNonNullArgField(stringListNonNullArg: [$a]) } }`,
		`query ($a: Boolean = true) { complicatedArgs { complexArgField(complexArg: {requiredField: $a}) } }`,
		`query ($a: Boolean) { complicatedArgs { complexArgField(complexArg: {requiredField: true, nonNullField: $a}) } }`,
		`query ($a: Boolean!) { dog { name @include(if: $a) } }`,
		`query ($a: Boolean = false) { dog { name @include(if: $a) } }`,
		`query ($a: ComplexInput) { complicatedArgs { complexArgField(complexArg: $a) } }`,
	} {
		expectValid(t, q)
	}
	expectErrors(t, `query ($a: Int) { complicatedArgs { nonNullIntArgField(nonNullIntArg: $a) } }`,
		`Variable "$a" of type "Int" used in position expecting type "Int!". (1:8, 1:71)`)
	expectErrors(t, `query ($a: Int = null) { complicatedArgs { nonNullIntArgField(nonNullIntArg: $a) } }`,
		`Variable "$a" of type "Int" used in position expecting type "Int!". (1:8, 1:78)`)
	expectErrors(t, `query ($a: String) { complicatedArgs { stringListNonNullArgField(stringListNonNullArg: [$a]) } }`,
		`Variable "$a" of type "String" used in position expecting type "String!". (1:8, 1:89)`)
	expectErrors(t, `query ($a: String) { complicatedArgs { stringListArgField(stringListArg: $a) } }`,
		`Variable "$a" of type "String" used in position expecting type "[String]". (1:8, 1:74)`)
	expectErrors(t, `query ($a: [String]) { complicatedArgs { stringArgField(stringArg: $a) } }`,
		`Variable "$a" of type "[String]" used in position expecting type "String". (1:8, 1:68)`)
	expectErrors(t, `query ($a: Boolean) { dog { name @include(if: $a) } }`,
		`Variable "$a" of type "Boolean" used in position expecting type "Boolean!". (1:8, 1:47)`)
	expectErrors(t, `query ($a: Int) { dog { name(surname: $a) } }`,
		`Variable "$a" of type "Int" used in position expecting type "Boolean". (1:8, 1:39)`)
	expectErrors(t, `query Q($a: FurColor) { dog { ...F } } fragment F on Dog { doesKnowCommand(dogCommand: $a) }`,
		`Variable "$a" of type "FurColor" used in position expecting type "DogCommand". (1:9, 1:88)`)
}
