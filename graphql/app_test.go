package graphql_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2/forms"
	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/auth"
	"github.com/vuka-lang/web/v2/auth/authtest"
	"github.com/vuka-lang/web/v2/graphql"
)

// The app's Go, as written in .vuka files:
//
//	type Status int
//	const (StatusActive Status = iota; StatusPastDue)
//
//	type User struct {
//		ID     int64 `graphql:",id"`
//		Name   string
//		Email  string @auth.Requires("users.email")
//		Nick   *string
//		Joined time.Time
//		Status Status
//		LoginCount int
//	}
//
//	@graphql.Field("displayName")
//	func (u *User) FullName(ctx context.Context, upper bool) string { … }
//
//	@graphql.Query
//	func Users(ctx context.Context, first int, store *Store) ([]User, error) { … }
//
//	@graphql.Query("me") @auth.Public
//	func CurrentUser(ctx context.Context, store *Store) (*User, error) { … }
//
//	@graphql.Mutation @auth.Requires("users.create")
//	func CreateUser(ctx context.Context, in NewUser, store *Store) (*User, error) { … }
//
// and the registrations the compiler generates for them, below.

type Status int

const (
	StatusActive Status = iota
	StatusPastDue
)

type User struct {
	ID         int64
	Name       string
	Email      string
	Nick       *string
	Joined     time.Time
	Status     Status
	LoginCount int
}

func (u *User) FullName(ctx context.Context, upper bool) string {
	if upper {
		return strings.ToUpper(u.Name)
	}
	return u.Name
}

type NewUser struct {
	Name  string `validate:"required,max=10"`
	Email string `validate:"required,email"`
}

func (n *NewUser) CleanName(ctx context.Context, store *Store) error {
	for _, u := range store.users {
		if u.Name == n.Name {
			return forms.Errorf("That name is taken")
		}
	}
	return nil
}

type Store struct{ users []User }

func NewStore() *Store {
	return &Store{users: []User{
		{ID: 1, Name: "ada", Email: "ada@example.com", Joined: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{ID: 2, Name: "bob", Email: "bob@example.com", Status: StatusPastDue},
	}}
}

func Users(ctx context.Context, first int, store *Store) ([]User, error) {
	if first < 0 {
		return nil, errors.New("database exploded")
	}
	return store.users[:min(first, len(store.users))], nil
}

func CurrentUser(ctx context.Context, store *Store) (*User, error) {
	if id := auth.Current(ctx); id != nil {
		return &store.users[0], nil
	}
	return nil, nil
}

func CreateUser(ctx context.Context, in NewUser, store *Store) (*User, error) {
	u := User{ID: int64(len(store.users) + 1), Name: in.Name, Email: in.Email}
	store.users = append(store.users, u)
	return &u, nil
}

func init() {
	vuka.RegisterFields[User](map[string][]any{"Email": {auth.Requires("users.email")}})
	vuka.RegisterFunc((*User).FullName, graphql.Field("displayName"))
	vuka.RegisterFunc(Users, *new(graphql.Query))
	vuka.RegisterFunc(CurrentUser, append([]any{graphql.Query("me")}, auth.Public().Attrs()...)...)
	vuka.RegisterFunc(CreateUser, append([]any{*new(graphql.Mutation)}, auth.Requires("users.create").Attrs()...)...)
	graphql.RegisterValue(StatusPastDue, graphql.Deprecated("use ACTIVE"))

	graphql.Register(graphql.Package{
		Path: "github.com/vuka-lang/web/v2/graphql_test",
		Operations: []graphql.Operation{
			{
				Go: "Users", Func: Users, Doc: "Users lists the users.",
				Params: []graphql.Param{{Name: "ctx"}, {Name: "first", Type: graphql.Int}, {Name: "store", Dep: reflect.TypeFor[*Store]()}},
				Type:   &graphql.NonNull{Of: graphql.ListOf[User](&graphql.NonNull{Of: graphql.ObjectOf[User]()})},
				Resolve: func(ctx context.Context, c *graphql.Call) (any, error) {
					return Users(ctx, graphql.Get[int](c, 1), graphql.Get[*Store](c, 2))
				},
			},
			{
				Go: "CurrentUser", Func: CurrentUser,
				Params: []graphql.Param{{Name: "ctx"}, {Name: "store", Dep: reflect.TypeFor[*Store]()}},
				Type:   graphql.ObjectOf[User](),
				Resolve: func(ctx context.Context, c *graphql.Call) (any, error) {
					return CurrentUser(ctx, graphql.Get[*Store](c, 1))
				},
			},
			{
				Go: "CreateUser", Func: CreateUser, Mutation: true,
				Params: []graphql.Param{{Name: "ctx"}, {Name: "in", Type: graphql.InputOf[NewUser]()}, {Name: "store", Dep: reflect.TypeFor[*Store]()}},
				Type:   graphql.ObjectOf[User](),
				Resolve: func(ctx context.Context, c *graphql.Call) (any, error) {
					return CreateUser(ctx, graphql.Get[NewUser](c, 1), graphql.Get[*Store](c, 2))
				},
			},
		},
		Types: []graphql.Binding{
			graphql.Struct[User]{Fields: []graphql.Member[User]{
				{Go: "ID", Type: &graphql.NonNull{Of: graphql.IntIDOf[int64]()}, Get: func(u *User) any { return u.ID }},
				{Go: "Name", Type: &graphql.NonNull{Of: graphql.String}, Get: func(u *User) any { return u.Name }},
				{Go: "Email", Type: &graphql.NonNull{Of: graphql.String}, Get: func(u *User) any { return u.Email }},
				{Go: "Nick", Type: graphql.String, Get: func(u *User) any {
					if u.Nick == nil {
						return nil
					}
					return *u.Nick
				}},
				{Go: "Joined", Type: &graphql.NonNull{Of: graphql.DateTime}, Get: func(u *User) any { return u.Joined }},
				{Go: "Status", Type: &graphql.NonNull{Of: graphql.EnumOf[Status]()}, Get: func(u *User) any { return u.Status }},
				{Go: "LoginCount", Type: &graphql.NonNull{Of: graphql.Int}, Get: func(u *User) any { return u.LoginCount }},
				{Go: "FullName", Type: &graphql.NonNull{Of: graphql.String},
					Params: []graphql.Param{{Name: "ctx"}, {Name: "upper", Type: graphql.Boolean}},
					Resolve: func(ctx context.Context, u *User, c *graphql.Call) (any, error) {
						return u.FullName(ctx, graphql.Get[bool](c, 1)), nil
					}},
			}},
			graphql.FormInput[NewUser]{Fields: []graphql.InputMember[NewUser]{
				{Go: "Name", Type: graphql.String, Set: func(f *NewUser, v any) { f.Name = graphql.As[string](v) }},
				{Go: "Email", Type: graphql.String, Set: func(f *NewUser, v any) { f.Email = graphql.As[string](v) }},
			}},
			graphql.Consts[Status]{Values: []graphql.Const[Status]{
				{Go: "StatusActive", Value: StatusActive},
				{Go: "StatusPastDue", Value: StatusPastDue},
			}},
		},
	})
}

func newApp(t *testing.T, id *auth.Identity, opts ...any) http.Handler {
	t.Helper()
	app := web.New(
		auth.Module(auth.Config{Users: authtest.NewUsers()}),
		authtest.As(id),
		web.GraphQL("/graphql", opts...),
	)
	app.Service(NewStore)
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

func TestSchemaFromBindings(t *testing.T) {
	app := web.New(auth.Module(auth.Config{Users: authtest.NewUsers()}), web.GraphQL("/graphql"))
	app.Service(NewStore)
	if err := app.Build(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/graphql?query="+strings.ReplaceAll("{__schema{types{name}}}", "{", "%7B"), nil)
	app.Handler().ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "Introspection is disabled") {
		t.Errorf("introspection outside development: %s", w.Body)
	}
}

func TestQueries(t *testing.T) {
	h := newApp(t, &auth.Identity{ID: "1", Perms: []string{"users.*"}})
	code, out := post(t, h, `{ users(first: 2) { id name email nick joined status displayName(upper: true) } me { name } }`, nil)
	want := `{"data":{"me":{"name":"ada"},"users":[{"displayName":"ADA","email":"ada@example.com","id":"1","joined":"2026-01-02T03:04:05Z","name":"ada","nick":null,"status":"ACTIVE"},{"displayName":"BOB","email":"bob@example.com","id":"2","joined":"0001-01-01T00:00:00Z","name":"bob","nick":null,"status":"PAST_DUE"}]}}`
	if code != 200 || js(out) != want {
		t.Errorf("%d %s\nwant %s", code, js(out), want)
	}
}

func TestGates(t *testing.T) {
	// Signed in without users.email: the field is null, its error at its
	// path, and the null propagates up to the nearest nullable field.
	h := newApp(t, &auth.Identity{ID: "1"})
	_, out := post(t, h, `{ users(first: 1) { name email } }`, nil)
	if got := js(out); got != `{"data":null,"errors":[{"extensions":{"code":"FORBIDDEN"},"locations":[{"column":26,"line":1}],"message":"forbidden","path":["users",0,"email"]}]}` {
		t.Errorf("field gate: %s", got)
	}
	// Anonymous: users needs a sign-in (deny by default); me is public.
	h = newApp(t, nil)
	_, out = post(t, h, `{ me { name } users(first: 1) { name } }`, nil)
	if got := js(out); got != `{"data":null,"errors":[{"extensions":{"code":"UNAUTHENTICATED"},"locations":[{"column":15,"line":1}],"message":"unauthorized","path":["users"]}]}` {
		t.Errorf("operation gate: %s", got)
	}
}

func TestInputs(t *testing.T) {
	h := newApp(t, &auth.Identity{ID: "1", Perms: []string{"users.create"}})
	_, out := post(t, h, `mutation($in: NewUser) { createUser(in: $in) { id name } }`, map[string]any{"in": map[string]any{"name": "ada", "email": "nope"}})
	// Each error of the form is one of the response's: the rules', then
	// the Clean methods' of the fields passing them.
	errs, _ := out["errors"].([]any)
	if len(errs) != 2 || js(errs[0]) != `{"extensions":{"argument":"in","code":"INVALID_INPUT","key":"validate.email","path":"email","rule":"email"},"locations":[{"column":26,"line":1}],"message":"Enter an email address in the correct format, like name@example.com","path":["createUser"]}` {
		t.Errorf("rules: %s", js(out))
	}
	_, out = post(t, h, `mutation { createUser(in: {name: "ada", email: "a@b.co"}) { id } }`, nil)
	if got := js(out["errors"]); !strings.Contains(got, `"message":"That name is taken"`) || !strings.Contains(got, `"path":"name"`) {
		t.Errorf("Clean: %s", js(out))
	}
	_, out = post(t, h, `mutation { createUser(in: {name: "cy", email: "c@b.co"}) { id name } }`, nil)
	if got := js(out); got != `{"data":{"createUser":{"id":"3","name":"cy"}}}` {
		t.Errorf("created: %s", got)
	}
}

func TestErrors(t *testing.T) {
	h := newApp(t, &auth.Identity{ID: "1"})
	_, out := post(t, h, `{ users(first: -1) { name } }`, nil)
	if got := js(out); got != `{"data":null,"errors":[{"extensions":{"code":"INTERNAL_SERVER_ERROR"},"locations":[{"column":3,"line":1}],"message":"internal error","path":["users"]}]}` {
		t.Errorf("masked: %s", got)
	}
}

func TestNaming(t *testing.T) {
	h := newApp(t, &auth.Identity{ID: "1"}, graphql.Naming{Fields: graphql.SnakeCase}, graphql.Introspection(true))
	_, out := post(t, h, `{ users(first: 1) { displayName login_count } }`, nil)
	if got := js(out); got != `{"data":{"users":[{"displayName":"ada","login_count":0}]}}` {
		t.Errorf("snake case: %s", got)
	}
}

func TestSDL(t *testing.T) {
	h := newApp(t, &auth.Identity{ID: "1"}, graphql.Introspection(true))
	_, out := post(t, h, `{ __type(name: "Status") { enumValues(includeDeprecated: true) { name isDeprecated deprecationReason } } }`, nil)
	if got := js(out); got != `{"data":{"__type":{"enumValues":[{"deprecationReason":null,"isDeprecated":false,"name":"ACTIVE"},{"deprecationReason":"use ACTIVE","isDeprecated":true,"name":"PAST_DUE"}]}}}` {
		t.Errorf("enum: %s", got)
	}
}

func TestBuildErrors(t *testing.T) {
	app := web.New(web.GraphQL("/graphql"))
	app.Service(NewStore)
	err := app.Build()
	if err == nil || !strings.Contains(err.Error(), "needs auth.Module") {
		t.Errorf("gates without auth: %v", err)
	}
	app = web.New(auth.Module(auth.Config{Users: authtest.NewUsers()}), web.GraphQL("/graphql"))
	err = app.Build()
	if err == nil || !strings.Contains(err.Error(), "*graphql_test.Store is not provided") {
		t.Errorf("no store: %v", err)
	}
}
