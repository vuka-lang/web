// Package nulltest checks nullable lists, lists of nullable items and
// required arguments in a schema of their own: the registry is the test
// binary's.
package nulltest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/auth"
	"github.com/vuka-lang/web/v2/auth/authtest"
	"github.com/vuka-lang/web/v2/graphql"
)

// The app's Go, as written in .vuka files:
//
//	type Product struct {
//		Name  string
//		Codes *[]*int // [Int]
//	}
//
//	@graphql.Query @auth.Public
//	func Items(ctx context.Context, all bool) (*[]Product, error) { … } // [Product!]
//
//	@graphql.Query @auth.Public
//	func ItemsByCategory(ctx context.Context, @graphql.AsInt categoryId int) (*[]*Product, error) { … } // [Product]
//
//	@graphql.Query @auth.Public
//	func Echo(ctx context.Context, @graphql.Arg("values") xs *[]*int) ([]*int, error) { … } // [Int]!
//
// and the registrations the compiler generates for them, below.

type Product struct {
	Name  string
	Codes *[]*int
}

func intp(n int) *int { return &n }

func Items(ctx context.Context, all bool) (*[]Product, error) {
	if !all {
		return nil, nil
	}
	return &[]Product{{Name: "kettle", Codes: &[]*int{intp(10), nil, intp(2)}}, {Name: "mug"}}, nil
}

func ItemsByCategory(ctx context.Context, categoryId int) (*[]*Product, error) {
	switch categoryId {
	case 0:
		return nil, nil
	case 1:
		return &[]*Product{{Name: "kettle"}, nil}, nil
	}
	return &[]*Product{}, nil
}

func Echo(ctx context.Context, xs *[]*int) ([]*int, error) {
	if xs == nil {
		return []*int{intp(-1)}, nil
	}
	return *xs, nil
}

func init() {
	vuka.RegisterFunc(Items, append([]any{*new(graphql.Query)}, auth.Public().Attrs()...)...)
	vuka.RegisterFunc(ItemsByCategory, append([]any{*new(graphql.Query)}, auth.Public().Attrs()...)...)
	vuka.RegisterFunc(Echo, append([]any{*new(graphql.Query)}, auth.Public().Attrs()...)...)
	graphql.Register(graphql.Package{
		Path: "github.com/vuka-lang/web/v2/graphql/internal/nulltest",
		Operations: []graphql.Operation{
			{
				Go: "Items", Func: Items,
				Params: []graphql.Param{{Name: "ctx"}, {Name: "all", Type: &graphql.NonNull{Of: graphql.Boolean}}},
				Type:   graphql.ListOf[Product](&graphql.NonNull{Of: graphql.ObjectOf[Product]()}),
				Resolve: func(ctx context.Context, c *graphql.Call) (any, error) {
					v, err := Items(ctx, graphql.Get[bool](c, 1))
					return graphql.Deref(v), err
				},
			},
			{
				Go: "ItemsByCategory", Func: ItemsByCategory,
				Params: []graphql.Param{{Name: "ctx"}, {Name: "categoryId", Type: &graphql.NonNull{Of: graphql.Int}}},
				Type:   graphql.ListOf[*Product](graphql.ObjectOf[Product]()),
				Resolve: func(ctx context.Context, c *graphql.Call) (any, error) {
					v, err := ItemsByCategory(ctx, graphql.Get[int](c, 1))
					return graphql.Deref(v), err
				},
			},
			{
				Go: "Echo", Func: Echo,
				Params: []graphql.Param{{Name: "ctx"}, {Name: "xs", Arg: "values", Type: graphql.ListOfPtr[int](graphql.Int)}},
				Type:   &graphql.NonNull{Of: graphql.ListOfPtr[int](graphql.Int)},
				Resolve: func(ctx context.Context, c *graphql.Call) (any, error) {
					return Echo(ctx, graphql.GetPtr[[]*int](c, 1))
				},
			},
		},
		Types: []graphql.Binding{
			graphql.Struct[Product]{Fields: []graphql.Member[Product]{
				{Go: "Name", Type: &graphql.NonNull{Of: graphql.String}, Get: func(p *Product) any { return p.Name }},
				{Go: "Codes", Type: graphql.ListOfPtr[int](graphql.Int), Get: func(p *Product) any { return graphql.Deref(p.Codes) }},
			}},
		},
	})
}

// TestNullability: a nullable list result is null or a list, its items
// null where the slice's pointers are nil; a value parameter is a required
// argument, a pointer an optional one; @graphql.Arg before a parameter
// names it whatever the Naming.
func TestNullability(t *testing.T) {
	h := newApp(t, graphql.Introspection(true), graphql.Naming{Fields: graphql.SnakeCase})
	_, out := post(t, h, `{ __type(name: "Query") { fields { name type { ...T } args { name type { ...T } } } } }
		fragment T on __Type { kind name ofType { kind name ofType { kind name ofType { kind name } } } }`, nil)
	sig := map[string]string{}
	var str func(m map[string]any) string
	str = func(m map[string]any) string {
		switch m["kind"] {
		case "NON_NULL":
			return str(m["ofType"].(map[string]any)) + "!"
		case "LIST":
			return "[" + str(m["ofType"].(map[string]any)) + "]"
		}
		return m["name"].(string)
	}
	for _, f := range out["data"].(map[string]any)["__type"].(map[string]any)["fields"].([]any) {
		f := f.(map[string]any)
		var args []string
		for _, a := range f["args"].([]any) {
			a := a.(map[string]any)
			args = append(args, a["name"].(string)+": "+str(a["type"].(map[string]any)))
		}
		sig[f["name"].(string)] = "(" + strings.Join(args, ", ") + "): " + str(f["type"].(map[string]any))
	}
	for name, want := range map[string]string{
		"items":             "(all: Boolean!): [Product!]",
		"items_by_category": "(category_id: Int!): [Product]",
		"echo":              "(values: [Int]): [Int]!",
	} {
		if sig[name] != want {
			t.Errorf("%s%s, want %s", name, sig[name], want)
		}
	}
	_, out = post(t, h, `{ __type(name: "Product") { fields { name type { kind ofType { kind name } } } } }`, nil)
	if got := js(out); !strings.Contains(got, `{"name":"codes","type":{"kind":"LIST","ofType":{"kind":"SCALAR","name":"Int"}}}`) {
		t.Errorf("Product.codes: %s", got)
	}

	for _, c := range []struct{ query, want string }{
		{`{ items(all: false) { name } }`, `{"data":{"items":null}}`},
		{`{ items(all: true) { name codes } }`, `{"data":{"items":[{"codes":[10,null,2],"name":"kettle"},{"codes":null,"name":"mug"}]}}`},
		{`{ items_by_category(category_id: 0) { name } }`, `{"data":{"items_by_category":null}}`},
		{`{ items_by_category(category_id: 1) { name } }`, `{"data":{"items_by_category":[{"name":"kettle"},null]}}`},
		{`{ items_by_category(category_id: 2) { name } }`, `{"data":{"items_by_category":[]}}`},
		{`{ echo(values: [1, null, 3]) }`, `{"data":{"echo":[1,null,3]}}`},
		{`{ echo }`, `{"data":{"echo":[-1]}}`},
		{`{ echo(values: null) }`, `{"data":{"echo":[-1]}}`},
	} {
		if _, out := post(t, h, c.query, nil); js(out) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.query, js(out), c.want)
		}
	}
	for _, c := range []struct{ query, want string }{
		{`{ items_by_category { name } }`, `argument \"category_id\" of type \"Int!\" is required`},
		{`{ items_by_category(category_id: null) { name } }`, `Int!`},
	} {
		_, out := post(t, h, c.query, nil)
		if got := js(out); !strings.Contains(got, c.want) || !strings.Contains(got, "GRAPHQL_VALIDATION_FAILED") || out["data"] != nil {
			t.Errorf("%s: %s", c.query, got)
		}
	}
	if _, out := post(t, h, `query($c: Int) { items_by_category(category_id: $c) { name } }`, nil); !strings.Contains(js(out), "GRAPHQL_VALIDATION_FAILED") {
		t.Errorf("a nullable variable for a required argument: %s", js(out))
	}
}

func newApp(t *testing.T, opts ...any) http.Handler {
	t.Helper()
	app := web.New(auth.Module(auth.Config{Users: authtest.NewUsers()}), web.GraphQL("/graphql", opts...))
	if err := app.Build(); err != nil {
		t.Fatal(err)
	}
	return app.Handler()
}

func post(t *testing.T, h http.Handler, query string, vars map[string]any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	r := httptest.NewRequest("POST", "/graphql", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %v\n%s", query, err, w.Body)
	}
	return w.Code, out
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }
