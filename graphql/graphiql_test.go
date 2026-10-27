package graphql_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vuka-lang/web/v2/graphql"
	"github.com/vuka-lang/web/v2/graphql/language"
	"github.com/vuka-lang/web/v2/internal/testenv"
)

// graphiqlSDL is a schema with what the GraphiQL panel's completion must
// find its way through: interfaces, unions, enums, nested input objects,
// required arguments, defaults, deprecations, every root.
const graphiqlSDL = `
type Query {
  "A node by its id."
  node(id: ID!): Node
  search(text: String!, kinds: [Kind!]): [SearchResult!]!
  user(id: ID!): User
  users(first: Int = 10, filter: UserFilter): [User!]!
  old: String @deprecated(reason: "use node")
}

type Mutation {
  createUser(in: NewUser!): User!
}

type Subscription {
  userAdded: User!
}

interface Node {
  id: ID!
}

type User implements Node {
  id: ID!
  name: String!
  email: String
  status: Status!
  friends(first: Int!): [User!]!
  address: Address
}

type Address {
  city: String
  street: String
}

type Post implements Node {
  id: ID!
  title: String!
  author: User!
}

union SearchResult = User | Post

enum Status {
  ACTIVE
  PAST_DUE @deprecated(reason: "gone")
}

enum Kind {
  USER
  POST
}

input UserFilter {
  name: String
  status: Status
  tags: [String!]
  near: GeoInput
}

input GeoInput {
  lat: Float!
  lng: Float!
}

input NewUser {
  name: String!
  email: String!
  status: Status = ACTIVE
}
`

// introspect is the panel's own introspection query, run against sdl's
// schema: what the panel reads in a browser.
func introspect(t *testing.T, sdl string) []byte {
	t.Helper()
	src, err := os.ReadFile("../toolbar_graphiql.js")
	if err != nil {
		t.Fatal(err)
	}
	const start = "const INTROSPECTION = `"
	s := string(src)
	i := strings.Index(s, start) + len(start)
	query := s[i : i+strings.Index(s[i:], "`;")]
	schema, err := graphql.ParseSchema(sdl)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := graphql.NewServer(graphql.Config{Schema: schema, Introspection: true})
	if err != nil {
		t.Fatal(err)
	}
	res := srv.Execute(context.Background(), graphql.Request{Query: query})
	if len(res.Errors) > 0 {
		t.Fatalf("introspection: %v", res.Errors[0])
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGraphiQLLanguage runs the panel's language tests in node (completion
// at the cursor, insertion, formatting, operations, the variables' JSON)
// over what introspection gives it, then parses each document of their
// formatting corpus before and after: formatting changes no document.
func TestGraphiQLLanguage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		testenv.Needs(t, "no node")
	}
	dir := t.TempDir()
	schemaFile, outFile := filepath.Join(dir, "schema.json"), filepath.Join(dir, "formatted.json")
	if err := os.WriteFile(schemaFile, introspect(t, graphiqlSDL), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "../testdata/livejs/graphiql.js", "../toolbar_graphiql.js", schemaFile, outFile).CombinedOutput()
	if err != nil {
		t.Fatalf("%s", out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
	b, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	var corpus []struct{ Src, Formatted string }
	if err := json.Unmarshal(b, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus) < 10 {
		t.Fatalf("the corpus has %d documents", len(corpus))
	}
	schema, _ := graphql.ParseSchema(graphiqlSDL)
	for _, c := range corpus {
		before, err := language.Parse(c.Src)
		if err != nil {
			t.Fatalf("%s: %v", c.Src, err)
		}
		after, err := language.Parse(c.Formatted)
		if err != nil {
			t.Errorf("the formatted document doesn't parse: %v\n%s", err, c.Formatted)
			continue
		}
		if a, b := language.Print(before), language.Print(after); a != b {
			t.Errorf("formatting changed the document:\n%s\n---\n%s", a, b)
		}
		if errs := schema.Validate(after, true); len(errs) > 0 {
			t.Errorf("the formatted document isn't valid: %v\n%s", errs[0], c.Formatted)
		}
	}
}
