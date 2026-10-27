package graphql_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/auth"
	"github.com/vuka-lang/web/v2/auth/authtest"
	"github.com/vuka-lang/web/v2/graphql"
	"github.com/vuka-lang/web/v2/graphql/language"
	"github.com/vuka-lang/web/v2/internal/chrome"
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

// graphiqlApp is the test app with its toolbar on, served.
func graphiqlApp(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv(web.DevDirEnv, t.TempDir())
	app := web.New(auth.Module(auth.Config{Users: authtest.NewUsers()}), authtest.As(&auth.Identity{ID: "1", Perms: []string{"*"}}),
		web.GraphQL("/graphql", graphql.Introspection(true)))
	app.Service(NewStore)
	if err := app.Build(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// TestGraphiQLCheck: the toolbar checks a query as the app would before
// running it — its syntax, the schema's rules with their suggestions, the
// variables' values — runs nothing, and records no request. Only the
// toolbar's own requests are answered; an app without a toolbar has no
// such route.
func TestGraphiQLCheck(t *testing.T) {
	srv := graphiqlApp(t)
	check := func(body string, toolbar bool) (int, string) {
		r := httptest.NewRequest("POST", srv.URL+"/_vuka/toolbar/graphql/check", strings.NewReader(body))
		r.RequestURI = ""
		if toolbar {
			r.Header.Set(web.ToolbarHeader, "1")
		}
		r.Header.Set("Content-Type", "application/json")
		res, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if id := res.Header.Get(web.ToolbarHeader); id != "" {
			t.Errorf("the check was recorded as a request: %s", id)
		}
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(out)
	}
	for _, c := range []struct{ body, want string }{
		{`{"query":"{ users(first: 1) { nmae } }"}`, `Cannot query field \"nmae\" on type \"User\". Did you mean \"name\"?","locations":[{"line":1,"column":21}]`},
		{`{"query":"{ users(first: 1) { name "}`, `Syntax Error: `},
		{`{"query":"query($n: Int!) { users(first: $n) { name } }","variables":{"n":"x"}}`, `Variable \"$n\" got invalid value`},
		{`{"query":"query($n: Int!) { users(first: $n) { name } }","variables":{}}`, `was not provided`},
		{`{"query":"query($n: Int!) { users(first: $n) { name } }"}`, `{"errors":[]}`},
		{`{"query":"mutation { createUser(in: {name: \"x\", email: \"x@example.com\"}) { id } }"}`, `{"errors":[]}`},
	} {
		code, out := check(c.body, true)
		if code != 200 || !strings.Contains(out, c.want) {
			t.Errorf("%s: %d %s, want %s", c.body, code, out, c.want)
		}
	}
	if code, _ := check(`{"query":"{ users(first: 1) { name } }"}`, false); code != 403 {
		t.Errorf("without the toolbar's header: %d", code)
	}
	// nothing ran: the store's users are as they were
	_, out := post(t, newApp(t, &auth.Identity{ID: "1", Perms: []string{"*"}}), `{ users(first: 9) { name } }`, nil)
	if got := js(out); strings.Count(got, `"name"`) != 2 {
		t.Errorf("users: %s", got)
	}
	r := httptest.NewRecorder()
	newApp(t, &auth.Identity{ID: "1"}).ServeHTTP(r, httptest.NewRequest("POST", "/_vuka/toolbar/graphql/check", strings.NewReader(`{"query":"{a}"}`)))
	if r.Code == 200 || strings.Contains(r.Body.String(), "errors") {
		t.Errorf("an app without a toolbar answered the check: %d %s", r.Code, r.Body)
	}
}

// TestBrowserGraphiQL opens the page a browser gets opening the endpoint
// in development and works its GraphiQL panel with keys and the mouse:
// completion at the cursor as one types and on Ctrl-Space, what accepting
// inserts, the app's checks marked in place with a fix, hover, running
// with Mod-Enter, formatting as one undo step, the explorer's search, the
// operations list, full screen. VUKA_SCREENSHOTS names a directory for
// screenshots.
func TestBrowserGraphiQL(t *testing.T) {
	srv := graphiqlApp(t)
	b := chrome.Launch(t)
	p := b.Load(srv.URL + "/graphql")
	p.Viewport(1500, 900, false)
	tb := `document.querySelector("vuka-toolbar").shadowRoot`
	ta := tb + `.querySelector(".gql-query textarea")`
	p.Wait(tb + `.querySelector('.gql[data-schema="ready"]')`)
	var mod string
	p.Eval(`/Mac|iPhone|iPad/.test(navigator.platform) ? "meta" : "ctrl"`, &mod)
	set := func(text string, cursor int) {
		t.Helper()
		js, _ := json.Marshal(text)
		if err := p.Eval(fmt.Sprintf(`(() => { const t = %s; t.focus(); t.value = %s; t.setSelectionRange(%d, %d); t.dispatchEvent(new Event("input")); })()`, ta, js, cursor, cursor), nil); err != nil {
			t.Fatal(err)
		}
	}
	items := func() string {
		var s string
		p.Eval(`(() => { const l = `+tb+`.querySelector(".gql-query .ed-list"); return !l || l.hidden ? "" : [...l.querySelectorAll(".ed-label")].map((x) => x.textContent).join(","); })()`, &s)
		return s
	}
	waitItems := func(want string) {
		t.Helper()
		got := ""
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if got = items(); got == want {
				return
			}
		}
		var v string
		p.Eval(ta+`.value + " @" + `+ta+`.selectionStart`, &v)
		t.Fatalf("completions %q, want %q (the query %q)", got, want, v)
	}

	// Completion opens as one types, at the cursor.
	set("{\n  \n}", 4)
	p.Keys("us")
	waitItems("user,users,usersPage")
	var place struct{ ListTop, ListLeft, AreaTop, AreaLeft float64 }
	p.Eval(`(() => { const l = `+tb+`.querySelector(".gql-query .ed-list").getBoundingClientRect(), a = `+ta+`.getBoundingClientRect();
		return {ListTop: l.top, ListLeft: l.left, AreaTop: a.top, AreaLeft: a.left}; })()`, &place)
	if d := place.ListTop - place.AreaTop; d < 30 || d > 60 { // below the second line
		t.Errorf("the list is %.0fpx below the editor's top, not under the cursor's line", d)
	}
	if d := place.ListLeft - place.AreaLeft; d > 40 {
		t.Errorf("the list is %.0fpx right of the editor's left, not at the cursor", d)
	}
	if dir := os.Getenv("VUKA_SCREENSHOTS"); dir != "" {
		p.Screenshot(filepath.Join(dir, "graphiql-complete.png"))
	}
	// users is a list of objects: { } on lines of their own, the cursor
	// inside, where its fields complete as one types.
	p.Key("ArrowDown")
	p.Key("Enter")
	p.Wait(ta + `.value === "{\n  users {\n    \n  }\n}" && ` + ta + `.selectionStart === 16`)
	p.Keys("dn")
	waitItems("displayName")
	p.Key("Tab")
	p.Wait(ta + `.value === "{\n  users {\n    displayName\n  }\n}"`)

	// Ctrl-Space: the fields of the type at the cursor, nested.
	set("{ users(first: 2) { manager { na } } }", 32)
	p.Key(" ", "ctrl")
	waitItems("name,displayName,manager,managerID,__typename") // ranked: the prefix first
	p.Key("Escape")
	if items() != "" {
		t.Errorf("Escape left the list open")
	}
	// An input object's fields, a declared variable.
	set(`mutation { createUser(in: {}) { id } }`, 27)
	p.Key(" ", "ctrl")
	waitItems("name,email,managerID")
	p.Key("Escape")
	set("query Q($n: Int!) { users(first: ) { id } }", 33)
	p.Keys("$")
	waitItems("$n")
	p.Key("Escape")

	// The app's checks, marked where they are, with the fix suggested.
	set("{ users(first: 1) { nmae } }", 0)
	p.Wait(tb + `.querySelector(".gql-query .ed-hl .squiggle") && ` + tb + `.querySelector(".gql-problems").textContent.includes("Did you mean")`)
	var marked string
	p.Eval(tb+`.querySelector(".gql-query .ed-hl .squiggle").textContent`, &marked)
	if marked != "nmae" {
		t.Errorf("the mark is on %q", marked)
	}
	p.ClickElement(tb + `.querySelector(".gql-fix")`)
	p.Wait(ta + `.value === "{ users(first: 1) { name } }"`)
	p.Wait(`!` + tb + `.querySelector(".gql-query .ed-hl .squiggle")`)

	// Hover: a field's type and description.
	var at struct{ X, Y float64 }
	p.Eval(`(() => { const r = `+ta+`.getBoundingClientRect(), cw = `+tb+`.querySelector(".gql-query .ed-probe").getBoundingClientRect().width / 100;
		return {X: r.left + 10 + cw * 4, Y: r.top + 8 + 9}; })()`, &at)
	p.MouseAt(at.X, at.Y)
	p.Wait(tb + `.querySelector(".gql-tip:not([hidden])") && ` + tb + `.querySelector(".gql-tip").textContent.includes("Query.users(first: Int): [User!]!")`)

	// Mod-Enter runs; the result, its status, its trace.
	p.Eval(ta+`.focus()`, nil)
	p.Key("Enter", mod)
	p.Wait(tb + `.querySelector(".gql-result").textContent.includes("ada")`)
	p.Wait(tb + `.querySelector(".gql-status").textContent.includes("200")`)
	p.Wait(`!` + tb + `.querySelector(".gql-trace").hidden`)

	// Formatting: Shift-Mod-P, the cursor on its token, one undo step.
	set("{users(first:1){name email}}", 23)
	p.Key("p", mod, "shift")
	p.Wait(ta + `.value === "{\n  users(first: 1) {\n    name\n    email\n  }\n}\n"`)
	var caret string
	p.Eval(`(() => { const t = `+ta+`; return t.value.slice(t.selectionStart - 2, t.selectionStart + 3); })()`, &caret)
	if caret != "email" {
		t.Errorf("after formatting the cursor is at %q", caret)
	}
	// one undo step takes it back (the browser's own undo, which Mod-Z runs)
	p.Eval(ta+`.focus(); document.execCommand("undo")`, nil)
	p.Wait(ta + `.value === "{users(first:1){name email}}"`)
	// The button formats too, the variables with it.
	p.Eval(`(() => { const v = `+tb+`.querySelector(".gql-vars-ed textarea"); v.value = '{"n":1}'; v.dispatchEvent(new Event("input")); })()`, nil)
	p.ClickElement(tb + `.querySelector(".gql-format")`)
	p.Wait(ta + `.value.startsWith("{\n  users(first: 1) {") && ` + tb + `.querySelector(".gql-vars-ed textarea").value === '{\n  "n": 1\n}'`)

	// The explorer: search across types and fields; the operations list.
	p.ClickElement(tb + `.querySelector(".gql-side-btn")`)
	p.ClickElement(tb + `.querySelector('[data-side="docs"]')`)
	p.Eval(`(() => { const s = `+tb+`.querySelector(".gql-search"); s.value = "displ"; s.dispatchEvent(new Event("input")); })()`, nil)
	p.Wait(tb + `.querySelector(".gql-hits") && ` + tb + `.querySelector(".gql-hits").textContent.includes("User.displayName")`)
	p.ClickElement(tb + `.querySelector('.gql-hits a[data-fieldname="displayName"]')`)
	p.Wait(tb + `.querySelector(".gql-side-body").textContent.includes("upper: Boolean")`)
	p.ClickElement(tb + `.querySelector('[data-side="ops"]')`)
	p.Wait(tb + `.querySelector('.gql-ops[data-kind="query"] h5') && ` + tb + `.querySelector('.gql-ops[data-kind="query"] h5').textContent.includes("Query 4")`)
	var muts string
	p.Eval(tb+`.querySelector('.gql-ops[data-kind="mutation"] h5').textContent`, &muts)
	if !strings.Contains(muts, "Mutation 1") {
		t.Errorf("the mutations: %q", muts)
	}
	if dir := os.Getenv("VUKA_SCREENSHOTS"); dir != "" {
		p.Screenshot(filepath.Join(dir, "graphiql-explorer.png"))
	}
	p.ClickElement(tb + `.querySelector('.gql-opitem[data-field="user"] .gql-op-tab')`)
	p.Wait(ta + `.value.startsWith("query User($id: ID) {\n  user(id: $id) {\n    id\n")`)
	p.Eval(`(() => { const t = `+ta+`; return t.value.slice(t.selectionStart, t.selectionStart + 2); })()`, &caret)
	if caret != "id" {
		t.Errorf("a new operation's cursor is at %q, not on its selection", caret)
	}
	p.Key("Enter", mod)
	p.Wait(tb + `.querySelector(".gql-result").textContent.includes("null")`)

	// Full screen: the key, Esc back, focus and caret kept, remembered.
	p.Eval(ta+`.focus(); `+ta+`.setSelectionRange(5, 5)`, nil)
	p.Key("f", mod, "shift")
	p.Wait(tb + `.querySelector(".vk.gql-full") && Math.abs(` + tb + `.querySelector(".drawer").getBoundingClientRect().width - innerWidth) < 1`)
	var kept bool
	p.Eval(tb+`.activeElement === `+ta+` && `+ta+`.selectionStart === 5`, &kept)
	if !kept {
		t.Errorf("full screen lost the focus or the caret")
	}
	if dir := os.Getenv("VUKA_SCREENSHOTS"); dir != "" {
		p.Screenshot(filepath.Join(dir, "graphiql-full.png"))
		p.Viewport(1500, 900, true)
		p.Screenshot(filepath.Join(dir, "graphiql-full-dark.png"))
		p.Viewport(1500, 900, false)
	}
	p.Key("Escape")
	p.Wait(`!` + tb + `.querySelector(".vk.gql-full") && !` + tb + `.querySelector(".drawer").hidden`)
	p.ClickElement(tb + `.querySelector(".gql-full-btn")`)
	p.Wait(tb + `.querySelector(".vk.gql-full")`)
	p = b.Load(srv.URL + "/graphql")
	p.Wait(tb + `.querySelector('.gql[data-schema="ready"]') && ` + tb + `.querySelector(".vk.gql-full")`)
	var tabs int
	p.Eval(tb+`.querySelectorAll(".gql-tab").length`, &tabs)
	if tabs != 2 {
		t.Errorf("after a reload, %d tabs, want the 2 kept", tabs)
	}
}
