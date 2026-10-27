package graphql_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
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
//	@graphql.Query("user") @auth.Public
//	func FindUser(ctx context.Context, id int64, store *Store) (*User, error) { … }
//
//	@graphql.Query("me") @auth.Public
//	func CurrentUser(ctx context.Context, store *Store) (*User, error) { … }
//
//	@graphql.Query
//	func UsersPage(ctx context.Context, page graphql.Page, store *Store) (graphql.Connection[User], error) { … }
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
	ManagerID  int64
}

func (u *User) FullName(ctx context.Context, upper bool) string {
	if upper {
		return strings.ToUpper(u.Name)
	}
	return u.Name
}

// usersByID is @graphql.Loader UsersByID's function: the users of ids.
func usersByID(ctx context.Context, ids []int64, store *Store) (map[int64]*User, error) {
	store.batches = append(store.batches, len(ids))
	web.NoteStatement(ctx, web.Statement{SQL: "SELECT * FROM users WHERE id IN (?)", Rows: -1})
	out := map[int64]*User{}
	for i := range store.users {
		if slices.Contains(ids, store.users[i].ID) {
			out[store.users[i].ID] = &store.users[i]
		}
	}
	return out, nil
}

// UsersByID is the loader @graphql.Loader makes of it.
type UsersByID struct {
	*graphql.LoaderOf[int64, *User]
}

func NewUsersByID(store *Store) UsersByID {
	return UsersByID{graphql.NewLoader("UsersByID", func(ctx context.Context, keys []int64) (map[int64]*User, error) {
		return usersByID(ctx, keys, store)
	})}
}

// Manager is the user's manager, through the loader.
func (u *User) Manager(ctx context.Context, users UsersByID) (*User, error) {
	if u.ManagerID == 0 {
		return nil, nil
	}
	return users.Load(ctx, u.ManagerID)
}

type NewUser struct {
	Name      string `validate:"required,max=10"`
	Email     string `validate:"required,email"`
	ManagerID int64
}

func (n *NewUser) CleanName(ctx context.Context, store *Store) error {
	for _, u := range store.users {
		if u.Name == n.Name {
			return forms.Errorf("That name is taken")
		}
	}
	return nil
}

type Store struct {
	users   []User
	batches []int // UsersByID's calls' key counts
}

func NewStore() *Store {
	return &Store{users: []User{
		{ID: 1, Name: "ada", Email: "ada@example.com", Joined: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{ID: 2, Name: "bob", Email: "bob@example.com", Status: StatusPastDue, ManagerID: 1},
	}}
}

func Users(ctx context.Context, first int, store *Store) ([]User, error) {
	if first < 0 {
		return nil, errors.New("database exploded")
	}
	return store.users[:min(first, len(store.users))], nil
}

func FindUser(ctx context.Context, id int64, store *Store) (*User, error) {
	for i := range store.users {
		if store.users[i].ID == id {
			return &store.users[i], nil
		}
	}
	return nil, nil
}

func CurrentUser(ctx context.Context, store *Store) (*User, error) {
	if id := auth.Current(ctx); id != nil {
		return &store.users[0], nil
	}
	return nil, nil
}

// UsersPage pages the users.
func UsersPage(ctx context.Context, page graphql.Page, store *Store) (graphql.Connection[User], error) {
	return graphql.Paginate(ctx, graphql.Slice(store.users), page)
}

func CreateUser(ctx context.Context, in NewUser, store *Store) (*User, error) {
	u := User{ID: int64(len(store.users) + 1), Name: in.Name, Email: in.Email, ManagerID: in.ManagerID}
	store.users = append(store.users, u)
	return &u, nil
}

func init() {
	vuka.RegisterFields[User](map[string][]any{"Email": {auth.Requires("users.email")}})
	vuka.RegisterFunc((*User).FullName, graphql.Field("displayName"))
	vuka.RegisterFunc(Users, *new(graphql.Query))
	vuka.RegisterFunc(FindUser, append([]any{graphql.Query("user")}, auth.Public().Attrs()...)...)
	vuka.RegisterFunc(UsersPage, *new(graphql.Query))
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
				Go: "FindUser", Func: FindUser,
				Params: []graphql.Param{{Name: "ctx"}, {Name: "id", Type: graphql.IntIDOf[int64]()}, {Name: "store", Dep: reflect.TypeFor[*Store]()}},
				Type:   graphql.ObjectOf[User](),
				Resolve: func(ctx context.Context, c *graphql.Call) (any, error) {
					return FindUser(ctx, graphql.Get[int64](c, 1), graphql.Get[*Store](c, 2))
				},
			},
			{
				Go: "UsersPage", Func: UsersPage,
				Params: []graphql.Param{{Name: "ctx"}, {Name: "page", Type: graphql.PageArgs}, {Name: "store", Dep: reflect.TypeFor[*Store]()}},
				Type:   &graphql.NonNull{Of: graphql.ObjectOf[graphql.Connection[User]]()},
				Resolve: func(ctx context.Context, c *graphql.Call) (any, error) {
					return UsersPage(ctx, graphql.Get[graphql.Page](c, 1), graphql.Get[*Store](c, 2))
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
				{Go: "ManagerID", Type: &graphql.NonNull{Of: graphql.IntIDOf[int64]()}, Get: func(u *User) any { return u.ManagerID }},
				{Go: "LoginCount", Type: &graphql.NonNull{Of: graphql.Int}, Get: func(u *User) any { return u.LoginCount }},
				{Go: "FullName", Type: &graphql.NonNull{Of: graphql.String},
					Params: []graphql.Param{{Name: "ctx"}, {Name: "upper", Type: graphql.Boolean}},
					Resolve: func(ctx context.Context, u *User, c *graphql.Call) (any, error) {
						return u.FullName(ctx, graphql.Get[bool](c, 1)), nil
					}},
				{Go: "Manager", Type: graphql.ObjectOf[User](),
					Params: []graphql.Param{{Name: "ctx"}, {Name: "users", Dep: reflect.TypeFor[UsersByID]()}},
					Resolve: func(ctx context.Context, u *User, c *graphql.Call) (any, error) {
						return u.Manager(ctx, graphql.Get[UsersByID](c, 1))
					}},
			}},
			graphql.Struct[graphql.Connection[User]]{Fields: []graphql.Member[graphql.Connection[User]]{
				{Go: "Edges", Type: &graphql.NonNull{Of: graphql.ListOf[graphql.Edge[User]](&graphql.NonNull{Of: graphql.ObjectOf[graphql.Edge[User]]()})}, Get: func(c *graphql.Connection[User]) any { return c.Edges }},
				{Go: "PageInfo", Type: &graphql.NonNull{Of: graphql.ObjectOf[graphql.PageInfo]()}, Get: func(c *graphql.Connection[User]) any { return c.PageInfo }},
				{Go: "TotalCount", Type: &graphql.NonNull{Of: graphql.Int}, Params: []graphql.Param{{Name: "ctx"}},
					Resolve: func(ctx context.Context, v *graphql.Connection[User], c *graphql.Call) (any, error) {
						return v.TotalCount(ctx)
					}},
			}},
			graphql.Struct[graphql.Edge[User]]{Fields: []graphql.Member[graphql.Edge[User]]{
				{Go: "Cursor", Type: &graphql.NonNull{Of: graphql.String}, Get: func(e *graphql.Edge[User]) any { return e.Cursor }},
				{Go: "Node", Type: &graphql.NonNull{Of: graphql.ObjectOf[User]()}, Get: func(e *graphql.Edge[User]) any { return e.Node }},
			}},
			graphql.Struct[graphql.PageInfo]{Fields: []graphql.Member[graphql.PageInfo]{
				{Go: "HasNextPage", Type: &graphql.NonNull{Of: graphql.Boolean}, Get: func(p *graphql.PageInfo) any { return p.HasNextPage }},
				{Go: "HasPreviousPage", Type: &graphql.NonNull{Of: graphql.Boolean}, Get: func(p *graphql.PageInfo) any { return p.HasPreviousPage }},
				{Go: "StartCursor", Type: graphql.String, Get: func(p *graphql.PageInfo) any { return graphql.Deref(p.StartCursor) }},
				{Go: "EndCursor", Type: graphql.String, Get: func(p *graphql.PageInfo) any { return graphql.Deref(p.EndCursor) }},
			}},
			graphql.FormInput[NewUser]{Fields: []graphql.InputMember[NewUser]{
				{Go: "Name", Type: graphql.String, Set: func(f *NewUser, v any) { f.Name = graphql.As[string](v) }},
				{Go: "Email", Type: graphql.String, Set: func(f *NewUser, v any) { f.Email = graphql.As[string](v) }},
				{Go: "ManagerID", Type: graphql.IntIDOf[int64](), Set: func(f *NewUser, v any) { f.ManagerID = graphql.As[int64](v) }},
			}},
			graphql.Consts[Status]{Values: []graphql.Const[Status]{
				{Go: "StatusActive", Value: StatusActive},
				{Go: "StatusPastDue", Value: StatusPastDue},
			}},
		},
		Loaders: []graphql.LoaderDef{{
			Go: "UsersByID", Type: reflect.TypeFor[UsersByID](),
			Params: []graphql.Param{{Name: "store", Dep: reflect.TypeFor[*Store]()}},
			New:    func(c *graphql.Call) any { return NewUsersByID(graphql.Get[*Store](c, 0)) },
		}},
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

func TestCrossOrigin(t *testing.T) {
	h := newApp(t, &auth.Identity{ID: "1"})
	r := httptest.NewRequest("POST", "/graphql", strings.NewReader(`{"query":"{ users(first: 1) { name } }"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Origin", "https://elsewhere.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("a cross-origin POST: %d %s", w.Code, w.Body)
	}
	r = httptest.NewRequest("GET", "/graphql?query=%7Busers(first%3A1)%7Bname%7D%7D", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"ada"`) {
		t.Errorf("a cross-origin GET of a query: %d %s", w.Code, w.Body)
	}
}

// TestIDs: an id goes out as a string, and comes back as the string or the
// integer, as a literal or a variable; anything else is refused.
func TestIDs(t *testing.T) {
	h := newApp(t, &auth.Identity{ID: "1", Perms: []string{"users.*"}})
	_, out := post(t, h, `{ users(first: 2) { id } }`, nil)
	if got := js(out); got != `{"data":{"users":[{"id":"1"},{"id":"2"}]}}` {
		t.Fatalf("ids out: %s", got)
	}
	for _, q := range []struct {
		query string
		vars  map[string]any
	}{
		{`{ user(id: "2") { name } }`, nil},
		{`{ user(id: 2) { name } }`, nil},
		{`query($id: ID) { user(id: $id) { name } }`, map[string]any{"id": "2"}},
		{`query($id: ID) { user(id: $id) { name } }`, map[string]any{"id": 2}},
	} {
		if _, out := post(t, h, q.query, q.vars); js(out) != `{"data":{"user":{"name":"bob"}}}` {
			t.Errorf("%s %v: %s", q.query, q.vars, js(out))
		}
	}
	_, out = post(t, h, `{ user(id: "two") { name } }`, nil)
	if got := js(out); !strings.Contains(got, `isn't a whole number`) || !strings.Contains(got, "GRAPHQL_VALIDATION_FAILED") {
		t.Errorf("a literal that isn't a number: %s", got)
	}
	_, out = post(t, h, `query($id: ID) { user(id: $id) { name } }`, map[string]any{"id": "two"})
	if got := js(out); !strings.Contains(got, `isn't a whole number`) || !strings.Contains(got, "BAD_USER_INPUT") {
		t.Errorf("a variable that isn't a number: %s", got)
	}
	// The round trip: what a query gave, a mutation takes back.
	_, out = post(t, h, `mutation($m: ID) { createUser(in: {name: "cy", email: "c@b.co", managerID: $m}) { id managerID } }`, map[string]any{"m": "1"})
	if got := js(out); got != `{"data":{"createUser":{"id":"3","managerID":"1"}}}` {
		t.Errorf("round trip: %s", got)
	}
}

// TestLoaderField: a field method taking a loader type gets the request's
// loader, made with its dependencies; the managers of every user listed load
// in one call, and the next request has a loader of its own.
func TestLoaderField(t *testing.T) {
	app := web.New(auth.Module(auth.Config{Users: authtest.NewUsers()}), authtest.As(&auth.Identity{ID: "1"}), web.GraphQL("/graphql"))
	var store *Store
	app.Service(func() *Store {
		store = NewStore()
		store.users = append(store.users, User{ID: 3, Name: "cy", ManagerID: 2})
		return store
	})
	if err := app.Build(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, out := post(t, app.Handler(), `{ users(first: 3) { name manager { name manager { name } } } }`, nil)
		if got := js(out); got != `{"data":{"users":[{"manager":null,"name":"ada"},{"manager":{"manager":null,"name":"ada"},"name":"bob"},{"manager":{"manager":{"name":"ada"},"name":"bob"},"name":"cy"}]}}` {
			t.Errorf("managers: %s", got)
		}
	}
	// Each request: users 1 and 2 at the first level, then nothing new at
	// the second (1 is kept): one call.
	if !slices.Equal(store.batches, []int{2, 2}) {
		t.Errorf("UsersByID calls = %v, want one of 2 keys per request", store.batches)
	}
}

// TestConnection: a Page parameter is four arguments, a Connection a page of
// edges with cursors, pageInfo and a lazy totalCount; the page's size
// counts in the query's complexity.
func TestConnection(t *testing.T) {
	app := web.New(auth.Module(auth.Config{Users: authtest.NewUsers()}), authtest.As(&auth.Identity{ID: "1"}),
		web.GraphQL("/graphql", graphql.Introspection(true), graphql.Limits{Complexity: 40}))
	app.Service(func() *Store {
		s := NewStore()
		s.users = append(s.users, User{ID: 3, Name: "cy"}, User{ID: 4, Name: "dee"})
		return s
	})
	if err := app.Build(); err != nil {
		t.Fatal(err)
	}
	h := app.Handler()
	_, out := post(t, h, `{ usersPage(first: 2) { edges { cursor node { name } } pageInfo { hasNextPage hasPreviousPage endCursor } totalCount } }`, nil)
	page := out["data"].(map[string]any)["usersPage"].(map[string]any)
	if got := js(page["edges"]); !strings.Contains(got, `"node":{"name":"ada"}`) || !strings.Contains(got, `"node":{"name":"bob"}`) || page["totalCount"] != 4.0 {
		t.Fatalf("first page: %s", js(out))
	}
	info := page["pageInfo"].(map[string]any)
	if info["hasNextPage"] != true || info["hasPreviousPage"] != false {
		t.Errorf("first page's info: %s", js(info))
	}
	_, out = post(t, h, `query($c: String) { usersPage(first: 5, after: $c) { edges { node { name } } pageInfo { hasNextPage hasPreviousPage } } }`, map[string]any{"c": info["endCursor"]})
	if got := js(out); got != `{"data":{"usersPage":{"edges":[{"node":{"name":"cy"}},{"node":{"name":"dee"}}],"pageInfo":{"hasNextPage":false,"hasPreviousPage":true}}}}` {
		t.Errorf("next page: %s", got)
	}
	_, out = post(t, h, `{ usersPage(last: 1) { edges { node { name } } pageInfo { hasPreviousPage } } }`, nil)
	if got := js(out); got != `{"data":{"usersPage":{"edges":[{"node":{"name":"dee"}}],"pageInfo":{"hasPreviousPage":true}}}}` {
		t.Errorf("last: %s", got)
	}
	for q, want := range map[string]string{
		`{ usersPage(first: 1, last: 1) { totalCount } }`:            "Give first or last, not both.",
		`{ usersPage(first: -1) { totalCount } }`:                    "a page holds 0 rows or more",
		`{ usersPage(first: 1, after: "nope") { totalCount } }`:      "The cursor isn't one of this list's.",
		`{ usersPage(first: 30) { edges { node { name email } } } }`: "The query's complexity is 92",
	} {
		_, out := post(t, h, q, nil)
		if got := js(out); !strings.Contains(got, want) || !strings.Contains(got, "_") {
			t.Errorf("%s: %s, want %s", q, got, want)
		}
	}
	_, out = post(t, h, `{ __type(name: "Query") { fields { name args { name type { name } } type { ofType { name } } } } }`, nil)
	if got := js(out); !strings.Contains(got, `"args":[{"name":"first","type":{"name":"Int"}},{"name":"after","type":{"name":"String"}},{"name":"last","type":{"name":"Int"}},{"name":"before","type":{"name":"String"}}],"name":"usersPage","type":{"ofType":{"name":"UserConnection"}}`) {
		t.Errorf("the schema: %s", got)
	}
}

// TestToolbarTrace: in development the toolbar's record of a GraphQL
// request has a GraphQL panel — each field resolved with its path and its
// SQL, each loader batch — the SQL panel names what ran each statement, and
// the GraphiQL panel is there to run queries.
func TestToolbarTrace(t *testing.T) {
	t.Setenv(web.DevDirEnv, t.TempDir())
	app := web.New(auth.Module(auth.Config{Users: authtest.NewUsers()}), authtest.As(&auth.Identity{ID: "1"}), web.GraphQL("/graphql"))
	app.Service(NewStore)
	if err := app.Build(); err != nil {
		t.Fatal(err)
	}
	h := app.Handler()
	body := `{"query":"{ users(first: 2) { name manager { name } } }"}`
	r := httptest.NewRequest("POST", "/graphql", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	id := w.Header().Get(web.ToolbarHeader)
	if id == "" {
		t.Fatalf("no toolbar record: %s", w.Body)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/_vuka/toolbar/r/"+id+"?panels", nil))
	var rec struct{ HTML string }
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatal(err, w.Body)
	}
	for _, want := range []string{
		`data-tab="GraphQL"`, `<code>users.1.manager</code>`, `User.manager`, `<code>loader UsersByID</code>`,
		`from <code>loader UsersByID</code>`, `data-tab="GraphiQL"`, `data-endpoint="/graphql"`,
	} {
		if !strings.Contains(rec.HTML, want) {
			t.Errorf("the panels have no %s:\n%s", want, rec.HTML)
		}
	}
}
