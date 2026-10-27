package graphql

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// harnessSDL is the schema the validation tests run against: interfaces
// implementing interfaces, unions, every scalar as an argument, an enum,
// an input object with required and defaulted fields, and directives for
// every executable location.
const harnessSDL = `
schema { query: QueryRoot mutation: MutationRoot subscription: SubscriptionRoot }

directive @onQuery on QUERY
directive @onMutation on MUTATION
directive @onSubscription on SUBSCRIPTION
directive @onField on FIELD
directive @onFragmentDefinition on FRAGMENT_DEFINITION
directive @onFragmentSpread on FRAGMENT_SPREAD
directive @onInlineFragment on INLINE_FRAGMENT
directive @onVariableDefinition on VARIABLE_DEFINITION
directive @repeatable repeatable on FIELD
directive @withArgs(req: Int!, opt: String = "x") on FIELD

interface Being { name(surname: Boolean): String }
interface Mammal { mother: Mammal father: Mammal }
interface Pet implements Being { name(surname: Boolean): String }
interface Canine implements Mammal { name(surname: Boolean): String mother: Canine father: Canine }

enum DogCommand { SIT HEEL DOWN }

type Dog implements Being & Pet & Mammal & Canine {
  name(surname: Boolean): String
  nickname: String
  barkVolume: Int
  barks: Boolean
  doesKnowCommand(dogCommand: DogCommand): Boolean
  isHouseTrained(atOtherHomes: Boolean = true): Boolean
  isAtLocation(x: Int, y: Int): Boolean
  mother: Dog
  father: Dog
}

type Cat implements Being & Pet {
  name(surname: Boolean): String
  nickname: String
  meows: Boolean
  meowsVolume: Int
  furColor: FurColor
}

union CatOrDog = Cat | Dog

interface Intelligent { iq: Int }

type Human implements Being & Intelligent {
  name(surname: Boolean): String
  pets: [Pet]
  relatives: [Human]
  iq: Int
}

type Alien implements Being & Intelligent {
  name(surname: Boolean): String
  numEyes: Int
  iq: Int
}

union DogOrHuman = Dog | Human
union HumanOrAlien = Human | Alien

enum FurColor { BROWN BLACK TAN SPOTTED NO_FUR UNKNOWN }

input ComplexInput {
  requiredField: Boolean!
  nonNullField: Boolean! = false
  intField: Int
  stringField: String
  booleanField: Boolean
  stringListField: [String]
}

type ComplicatedArgs {
  intArgField(intArg: Int): String
  nonNullIntArgField(nonNullIntArg: Int!): String
  stringArgField(stringArg: String): String
  booleanArgField(booleanArg: Boolean): String
  enumArgField(enumArg: FurColor): String
  floatArgField(floatArg: Float): String
  idArgField(idArg: ID): String
  stringListArgField(stringListArg: [String]): String
  stringListNonNullArgField(stringListNonNullArg: [String!]): String
  complexArgField(complexArg: ComplexInput): String
  multipleReqs(req1: Int!, req2: Int!): String
  nonNullFieldWithDefault(arg: Int! = 0): String
  multipleOpts(opt1: Int = 0, opt2: Int = 0): String
  multipleOptAndReq(req1: Int!, req2: Int!, opt1: Int = 0, opt2: Int = 0): String
}

type QueryRoot {
  human(id: ID): Human
  alien: Alien
  dog: Dog
  cat: Cat
  pet: Pet
  catOrDog: CatOrDog
  dogOrHuman: DogOrHuman
  humanOrAlien: HumanOrAlien
  complicatedArgs: ComplicatedArgs
  being: Being
  intelligent: Intelligent
  canine: Canine
}

type MutationRoot { createDog(name: String): Dog renameDog(name: String!): Dog }

type SubscriptionRoot { newDog: Dog newCat: Cat }
`

var harness = func() *Schema {
	s, err := ParseSchema(harnessSDL)
	if err != nil {
		panic(err)
	}
	return s
}()

// errorsText writes errors as "message (line:col, …)", one a line.
func errorsText(errs []*Error) string {
	var lines []string
	for _, e := range errs {
		var locs []string
		for _, l := range e.Locations {
			locs = append(locs, fmt.Sprintf("%d:%d", l.Line, l.Column))
		}
		lines = append(lines, e.Message+" ("+strings.Join(locs, ", ")+")")
	}
	return strings.Join(lines, "\n")
}

func validate(t *testing.T, s *Schema, query string) []*Error {
	t.Helper()
	doc, err := language.Parse(query)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, query)
	}
	return s.Validate(doc, true)
}

// expectValid checks the query passes every rule.
func expectValid(t *testing.T, query string) {
	t.Helper()
	if errs := validate(t, harness, query); len(errs) > 0 {
		t.Errorf("expected no errors for\n%s\ngot:\n%s", query, errorsText(errs))
	}
}

// expectErrors checks the query fails with exactly want, in order.
func expectErrors(t *testing.T, query string, want ...string) {
	t.Helper()
	errs := validate(t, harness, query)
	for _, e := range errs {
		if e.Extensions["code"] != CodeValidationFailed {
			t.Errorf("error without the validation code: %+v", e)
		}
	}
	if got := errorsText(errs); got != strings.Join(want, "\n") {
		t.Errorf("for\n%s\ngot:\n%s\nwant:\n%s", query, got, strings.Join(want, "\n"))
	}
}
