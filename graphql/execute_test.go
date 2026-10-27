package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/web/v2/graphql/language"
)

func enumLit(s string) language.Value { return &language.EnumValue{Value: s} }
func strLit(s string) language.Value  { return &language.StringValue{Value: s} }

func constant(v any) func(context.Context, ResolveParams) (any, error) {
	return func(context.Context, ResolveParams) (any, error) { return v, nil }
}

func run(t *testing.T, s *Schema, query string, vars ...map[string]any) *Response {
	t.Helper()
	return runWith(t, Config{Schema: s, Introspection: true}, query, vars...)
}

func runWith(t *testing.T, c Config, query string, vars ...map[string]any) *Response {
	t.Helper()
	srv, err := NewServer(c)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Query: query}
	if len(vars) > 0 {
		req.Variables = vars[0]
	}
	return srv.Execute(context.Background(), req)
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func expectJSON(t *testing.T, res *Response, want string) {
	t.Helper()
	if got := jsonOf(t, res); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// The execution tests' domain: users, their friends and pets.

type tUser struct {
	ID      int
	Name    string
	Nick    *string
	Friends []*tUser
	Pets    []any
}

type tDog struct{ Name string }
type tCat struct{ Name string }

func strp(s string) *string { return &s }

var (
	alice = &tUser{ID: 1, Name: "Alice", Nick: strp("al"), Pets: []any{tDog{"Rex"}, tCat{"Tom"}}}
	bob   = &tUser{ID: 2, Name: "Bob"}
)

func init() { alice.Friends = []*tUser{bob} }

type testSchema struct {
	*Schema
	mutations atomic.Int64
	order     []string
	mu        sync.Mutex
}

func newTestSchema(t *testing.T) *testSchema {
	t.Helper()
	ts := &testSchema{}
	user := &Object{Name: "User", Value: func(v any) any {
		if u, ok := v.(*tUser); ok && u != nil {
			return u
		}
		return nil
	}}
	pet := &Interface{Name: "Pet", Fields: []*FieldDef{{Name: "name", Type: &NonNull{Of: String}}}}
	src := func(p ResolveParams) *tUser { return p.Source.(*tUser) }
	dogName := func(_ context.Context, p ResolveParams) (any, error) { return p.Source.(tDog).Name, nil }
	catName := func(_ context.Context, p ResolveParams) (any, error) { return p.Source.(tCat).Name, nil }
	dog := &Object{Name: "Dog", Interfaces: []*Interface{pet}, IsTypeOf: func(v any) bool { _, ok := v.(tDog); return ok }, Fields: []*FieldDef{
		{Name: "name", Type: &NonNull{Of: String}, Resolve: dogName, Trivial: true},
		{Name: "barks", Type: Boolean, Resolve: constant(true), Trivial: true},
	}}
	cat := &Object{Name: "Cat", Interfaces: []*Interface{pet}, IsTypeOf: func(v any) bool { _, ok := v.(tCat); return ok }, Fields: []*FieldDef{
		{Name: "name", Type: &NonNull{Of: String}, Resolve: catName, Trivial: true},
		{Name: "meows", Type: Boolean, Resolve: constant(true), Trivial: true},
	}}
	pet.ResolveType = func(v any) *Object {
		switch v.(type) {
		case tDog:
			return dog
		case tCat:
			return cat
		}
		return nil
	}
	thing := &Union{Name: "Thing", Types: []*Object{dog, cat}}
	mood := &Enum{Name: "Mood", Values: []*EnumValueDef{{Name: "HAPPY", Value: 1}, {Name: "SAD", Value: 2}, {Name: "OLD", Value: 3, Deprecated: true}}}
	filter := &InputObject{Name: "UserFilter", Fields: []*InputFieldDef{
		{Name: "name", Type: String},
		{Name: "mood", Type: mood, Default: enumLit("HAPPY")},
		{Name: "tags", Type: &List{Of: &NonNull{Of: String}}},
	}}
	sleep := func(d time.Duration, v any) func(context.Context, ResolveParams) (any, error) {
		return func(ctx context.Context, _ ResolveParams) (any, error) {
			select {
			case <-time.After(d):
				return v, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	user.Fields = []*FieldDef{
		{Name: "id", Type: &NonNull{Of: ID}, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return src(p).ID, nil }, Trivial: true},
		{Name: "name", Type: &NonNull{Of: String}, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return src(p).Name, nil }, Trivial: true},
		{Name: "nick", Type: String, Resolve: func(_ context.Context, p ResolveParams) (any, error) {
			if n := src(p).Nick; n != nil {
				return *n, nil
			}
			return nil, nil
		}, Trivial: true},
		{Name: "friends", Type: &NonNull{Of: &List{Of: &NonNull{Of: user}}}, Resolve: func(_ context.Context, p ResolveParams) (any, error) {
			out := []any{}
			for _, f := range src(p).Friends {
				out = append(out, f)
			}
			return out, nil
		}},
		{Name: "friendsWithHole", Type: &List{Of: &NonNull{Of: user}}, Resolve: constant([]any{bob, nil, bob})},
		{Name: "maybeFriends", Type: &List{Of: user}, Resolve: constant([]any{bob, nil})},
		{Name: "best", Type: user, Resolve: func(_ context.Context, p ResolveParams) (any, error) {
			if fs := src(p).Friends; len(fs) > 0 {
				return fs[0], nil
			}
			return nil, nil
		}},
		{Name: "pets", Type: &List{Of: &NonNull{Of: pet}}, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return src(p).Pets, nil }},
		{Name: "things", Type: &List{Of: thing}, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return src(p).Pets, nil }},
		{Name: "fail", Type: String, Resolve: func(context.Context, ResolveParams) (any, error) { return nil, errors.New("no access") }},
		{Name: "failNN", Type: &NonNull{Of: String}, Resolve: func(context.Context, ResolveParams) (any, error) { return nil, errors.New("gone") }},
		{Name: "nullNN", Type: &NonNull{Of: String}, Resolve: constant(nil)},
		{Name: "boom", Type: String, Resolve: func(context.Context, ResolveParams) (any, error) { panic("kaboom") }},
		{Name: "huge", Type: Int, Resolve: constant(int64(1) << 40)},
		{Name: "badMood", Type: mood, Resolve: constant(9)},
		{Name: "notList", Type: &List{Of: String}, Resolve: constant("x")},
		{Name: "slowA", Type: String, Resolve: sleep(150*time.Millisecond, "a")},
		{Name: "slowB", Type: String, Resolve: sleep(150*time.Millisecond, "b")},
		{Name: "guarded", Type: String, Resolve: constant("secret"), Guard: func(context.Context) error { return errors.New("refused") }},
		{Name: "search", Type: &List{Of: &NonNull{Of: String}}, Args: []*ArgDef{{Name: "term", Type: String, Default: strLit("x")}, {Name: "limit", Type: Int}}, Resolve: func(_ context.Context, p ResolveParams) (any, error) {
			return []any{fmt.Sprintf("term=%v limit=%v", p.Args["term"], p.Args["limit"])}, nil
		}},
	}
	echo := func(_ context.Context, p ResolveParams) (any, error) {
		keys := make([]string, 0, len(p.Args))
		for k := range p.Args {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%#v", k, p.Args[k]))
		}
		return strings.Join(parts, " "), nil
	}
	query := &Object{Name: "Query", Fields: []*FieldDef{
		{Name: "me", Type: user, Resolve: constant(alice)},
		{Name: "nobody", Type: user, Resolve: constant((*tUser)(nil))},
		{Name: "users", Type: &NonNull{Of: &List{Of: &NonNull{Of: user}}}, Args: []*ArgDef{{Name: "filter", Type: filter}, {Name: "first", Type: Int}}, Resolve: func(_ context.Context, p ResolveParams) (any, error) {
			return []any{alice, bob}, nil
		}},
		{Name: "echo", Type: String, Resolve: echo, Args: []*ArgDef{
			{Name: "s", Type: String}, {Name: "i", Type: Int}, {Name: "f", Type: Float}, {Name: "b", Type: Boolean}, {Name: "id", Type: ID},
			{Name: "list", Type: &List{Of: Int}}, {Name: "nn", Type: &NonNull{Of: Int}, Default: &language.IntValue{Value: "7"}}, {Name: "mood", Type: mood}, {Name: "filter", Type: filter},
		}},
		{Name: "mood", Type: mood, Args: []*ArgDef{{Name: "m", Type: &NonNull{Of: mood}}}, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return p.Args["m"], nil }},
		{Name: "nonNullFail", Type: &NonNull{Of: String}, Resolve: func(context.Context, ResolveParams) (any, error) { return nil, errors.New("root failure") }},
		{Name: "pet", Type: pet, Resolve: constant(tDog{"Rex"})},
		{Name: "slowA", Type: String, Resolve: sleep(150*time.Millisecond, "a")},
		{Name: "slowB", Type: String, Resolve: sleep(150*time.Millisecond, "b")},
		{Name: "slowC", Type: String, Resolve: sleep(150*time.Millisecond, "c")},
	}}
	step := func(name string, d time.Duration) func(context.Context, ResolveParams) (any, error) {
		return func(context.Context, ResolveParams) (any, error) {
			time.Sleep(d)
			ts.mu.Lock()
			ts.order = append(ts.order, name)
			ts.mu.Unlock()
			return int(ts.mutations.Add(1)), nil
		}
	}
	mutation := &Object{Name: "Mutation", Fields: []*FieldDef{
		{Name: "first", Type: &NonNull{Of: Int}, Resolve: step("first", 60*time.Millisecond)},
		{Name: "second", Type: &NonNull{Of: Int}, Resolve: step("second", 0)},
		{Name: "third", Type: Int, Resolve: step("third", 30*time.Millisecond)},
		{Name: "failing", Type: Int, Resolve: func(context.Context, ResolveParams) (any, error) { return nil, errors.New("cannot") }},
	}}
	subscription := &Object{Name: "Subscription", Fields: []*FieldDef{{Name: "ticks", Type: Int}}}
	s, err := NewSchema(SchemaConfig{Query: query, Mutation: mutation, Subscription: subscription})
	if err != nil {
		t.Fatal(err)
	}
	ts.Schema = s
	return ts
}

func TestExecuteBasics(t *testing.T) {
	s := newTestSchema(t)
	expectJSON(t, run(t, s.Schema, `{ me { id name nick who: name __typename } __typename }`),
		`{"data":{"me":{"id":"1","name":"Alice","nick":"al","who":"Alice","__typename":"User"},"__typename":"Query"}}`)
	// Fields of one key merge, in the order first selected.
	expectJSON(t, run(t, s.Schema, `{ me { name ... on User { id name } ...F } } fragment F on User { nick id }`),
		`{"data":{"me":{"name":"Alice","id":"1","nick":"al"}}}`)
	expectJSON(t, run(t, s.Schema, `{ me { best { name } friends { name best { name } } } nobody { name } }`),
		`{"data":{"me":{"best":{"name":"Bob"},"friends":[{"name":"Bob","best":null}]},"nobody":null}}`)
	expectJSON(t, run(t, s.Schema, `query Named { a: me { name } b: me { id } }`),
		`{"data":{"a":{"name":"Alice"},"b":{"id":"1"}}}`)
}

func TestExecuteSkipInclude(t *testing.T) {
	s := newTestSchema(t)
	q := `query ($yes: Boolean!, $no: Boolean!) {
	me {
		a: name @include(if: $yes)
		b: name @include(if: $no)
		c: name @skip(if: $yes)
		d: name @skip(if: $no)
		e: name @skip(if: false) @include(if: false)
		... @include(if: $no) { f: name }
		...F @skip(if: $no)
	}
}
fragment F on User { g: name }`
	expectJSON(t, run(t, s.Schema, q, map[string]any{"yes": true, "no": false}),
		`{"data":{"me":{"a":"Alice","d":"Alice","g":"Alice"}}}`)
}

func TestExecuteAbstract(t *testing.T) {
	s := newTestSchema(t)
	expectJSON(t, run(t, s.Schema, `{ me { pets { __typename name ... on Dog { barks } ... on Cat { meows } } things { __typename ... on Dog { name } ... on Cat { name } } } pet { name } }`),
		`{"data":{"me":{"pets":[{"__typename":"Dog","name":"Rex","barks":true},{"__typename":"Cat","name":"Tom","meows":true}],"things":[{"__typename":"Dog","name":"Rex"},{"__typename":"Cat","name":"Tom"}]},"pet":{"name":"Rex"}}}`)
}

func TestExecuteNullPropagation(t *testing.T) {
	s := newTestSchema(t)
	// A nullable field's error: the field is null, the error at its path.
	expectJSON(t, run(t, s.Schema, `{ me { name fail } }`),
		`{"errors":[{"message":"no access","locations":[{"line":1,"column":13}],"path":["me","fail"]}],"data":{"me":{"name":"Alice","fail":null}}}`)
	// A non-null field's error nulls its parent.
	expectJSON(t, run(t, s.Schema, `{ me { name failNN } other: me { name } }`),
		`{"errors":[{"message":"gone","locations":[{"line":1,"column":13}],"path":["me","failNN"]}],"data":{"me":null,"other":{"name":"Alice"}}}`)
	// Through non-null lists up to the nearest nullable field.
	expectJSON(t, run(t, s.Schema, `{ me { name friends { failNN } } }`),
		`{"errors":[{"message":"gone","locations":[{"line":1,"column":23}],"path":["me","friends",0,"failNN"]}],"data":{"me":null}}`)
	// At the root: data is null.
	expectJSON(t, run(t, s.Schema, `{ me { name } nonNullFail }`),
		`{"errors":[{"message":"root failure","locations":[{"line":1,"column":15}],"path":["nonNullFail"]}],"data":null}`)
	// A null where non-null is promised.
	expectJSON(t, run(t, s.Schema, `{ me { nullNN } }`),
		`{"errors":[{"message":"Cannot return null for non-nullable field User.nullNN.","locations":[{"line":1,"column":8}],"path":["me","nullNN"]}],"data":{"me":null}}`)
	// A null item of a list of non-null: the list is null, the path has the index.
	expectJSON(t, run(t, s.Schema, `{ me { friendsWithHole { name } maybeFriends { name } } }`),
		`{"errors":[{"message":"Cannot return null for non-nullable field User.friendsWithHole.","locations":[{"line":1,"column":8}],"path":["me","friendsWithHole",1]}],"data":{"me":{"friendsWithHole":null,"maybeFriends":[{"name":"Bob"},null]}}}`)
	// Errors read in document order, whatever ran first.
	res := run(t, s.Schema, `{ me { a: fail b: slowA c: fail } x: me { fail } }`)
	var paths []string
	for _, e := range res.Errors {
		paths = append(paths, fmt.Sprint(e.Path))
	}
	if got := strings.Join(paths, " "); got != "[me a] [me c] [x fail]" {
		t.Errorf("error order: %s", got)
	}
}

func TestExecuteResultCoercion(t *testing.T) {
	s := newTestSchema(t)
	expectJSON(t, run(t, s.Schema, `{ me { huge } }`),
		`{"errors":[{"message":"Int cannot represent non 32-bit signed integer value: 1099511627776","locations":[{"line":1,"column":8}],"path":["me","huge"]}],"data":{"me":{"huge":null}}}`)
	expectJSON(t, run(t, s.Schema, `{ me { badMood } }`),
		`{"errors":[{"message":"Enum \"Mood\" cannot represent value: 9","locations":[{"line":1,"column":8}],"path":["me","badMood"]}],"data":{"me":{"badMood":null}}}`)
	expectJSON(t, run(t, s.Schema, `{ me { notList } }`),
		`{"errors":[{"message":"Expected Iterable, but did not find one for field \"User.notList\".","locations":[{"line":1,"column":8}],"path":["me","notList"]}],"data":{"me":{"notList":null}}}`)
	expectJSON(t, run(t, s.Schema, `{ mood(m: SAD) }`), `{"data":{"mood":"SAD"}}`)
}

func TestExecutePanicAndPresent(t *testing.T) {
	s := newTestSchema(t)
	expectJSON(t, run(t, s.Schema, `{ me { boom name } }`),
		`{"errors":[{"message":"internal error","locations":[{"line":1,"column":8}],"path":["me","boom"],"extensions":{"code":"INTERNAL_SERVER_ERROR"}}],"data":{"me":{"boom":null,"name":"Alice"}}}`)
	var seen []error
	var mu sync.Mutex
	present := func(_ context.Context, err error) []*Error {
		mu.Lock()
		seen = append(seen, err)
		mu.Unlock()
		var p *PanicError
		if errors.As(err, &p) {
			return []*Error{{Message: "oops: " + fmt.Sprint(p.Value), Extensions: map[string]any{"code": "PANIC"}}}
		}
		return []*Error{{Message: "first: " + err.Error()}, {Message: "second", Extensions: map[string]any{"code": "TWO"}}}
	}
	res := runWith(t, Config{Schema: s.Schema, Present: present}, `{ me { boom fail guarded } }`)
	expectJSON(t, res, `{"errors":[{"message":"oops: kaboom","locations":[{"line":1,"column":8}],"path":["me","boom"],"extensions":{"code":"PANIC"}},{"message":"first: no access","locations":[{"line":1,"column":13}],"path":["me","fail"]},{"message":"second","locations":[{"line":1,"column":13}],"path":["me","fail"],"extensions":{"code":"TWO"}},{"message":"first: refused","locations":[{"line":1,"column":18}],"path":["me","guarded"]},{"message":"second","locations":[{"line":1,"column":18}],"path":["me","guarded"],"extensions":{"code":"TWO"}}],"data":{"me":{"boom":null,"fail":null,"guarded":null}}}`)
	if len(seen) != 3 {
		t.Errorf("Present saw %d errors", len(seen))
	}
	var p *PanicError
	for _, e := range seen {
		errors.As(e, &p)
	}
	if p == nil || p.Field != "User.boom" || !strings.Contains(string(p.Stack), "goroutine") {
		t.Errorf("panic error: %+v", p)
	}
}

func TestExecuteConcurrency(t *testing.T) {
	s := newTestSchema(t)
	start := time.Now()
	expectJSON(t, run(t, s.Schema, `{ slowA slowB slowC me { slowA slowB } }`),
		`{"data":{"slowA":"a","slowB":"b","slowC":"c","me":{"slowA":"a","slowB":"b"}}}`)
	if d := time.Since(start); d > 280*time.Millisecond {
		t.Errorf("siblings took %s: not concurrent", d)
	}
	// Mutations' root fields run one after the other, in order.
	start = time.Now()
	expectJSON(t, run(t, s.Schema, `mutation { a: first b: second c: third failing d: first }`),
		`{"errors":[{"message":"cannot","locations":[{"line":1,"column":40}],"path":["failing"]}],"data":{"a":1,"b":2,"c":3,"failing":null,"d":4}}`)
	if got := strings.Join(s.order, ","); got != "first,second,third,first" {
		t.Errorf("mutation order: %s", got)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("mutations took %s: not serial", d)
	}
}

func TestExecuteTimeout(t *testing.T) {
	s := newTestSchema(t)
	res := runWith(t, Config{Schema: s.Schema, Limits: Limits{Timeout: 40 * time.Millisecond}}, `{ me { name } slowA }`)
	expectJSON(t, res, `{"errors":[{"message":"The request took longer than the limit of 40ms.","locations":[{"line":1,"column":15}],"path":["slowA"],"extensions":{"code":"TIMEOUT"}}],"data":{"me":{"name":"Alice"},"slowA":null}}`)
	// A canceled request: what hasn't started fails.
	srv, _ := NewServer(Config{Schema: s.Schema})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	expectJSON(t, srv.Execute(ctx, Request{Query: `{ me { name } }`}),
		`{"errors":[{"message":"The request was canceled.","locations":[{"line":1,"column":3}],"path":["me"],"extensions":{"code":"TIMEOUT"}}],"data":{"me":null}}`)
}

func TestExecuteArguments(t *testing.T) {
	s := newTestSchema(t)
	for _, c := range []struct{ query, want string }{
		{`{ echo }`, `nn=7`},
		{`{ echo(s: "x", i: -3, f: 2, b: false, id: 42) }`, `b=false f=2 i=-3 id="42" nn=7 s="x"`},
		{`{ echo(id: "abc", list: 5) }`, `id="abc" list=[]interface {}{5} nn=7`},
		{`{ echo(list: [1, null, 3], nn: 1) }`, `list=[]interface {}{1, interface {}(nil), 3} nn=1`},
		{`{ echo(mood: SAD, s: null) }`, `mood=2 nn=7 s=<nil>`},
		{`{ echo(filter: {name: "a", tags: "solo"}) }`, `filter=map[string]interface {}{"mood":1, "name":"a", "tags":[]interface {}{"solo"}} nn=7`},
		{`{ echo(filter: {mood: null}) }`, `filter=map[string]interface {}{"mood":interface {}(nil)} nn=7`},
		{`{ me { search } }`, `term=x limit=<nil>`},
		{`{ me { search(term: "y", limit: 2) } }`, `term=y limit=2`},
	} {
		res := run(t, s.Schema, c.query)
		if len(res.Errors) > 0 {
			t.Errorf("%s: %s", c.query, errorsText(res.Errors))
			continue
		}
		var got any
		if me, ok := res.Data.Get("me"); ok {
			v, _ := me.(*OrderedMap).Get("search")
			got = v.([]any)[0]
		} else {
			got, _ = res.Data.Get("echo")
		}
		if got != c.want {
			t.Errorf("%s:\n got  %v\n want %v", c.query, got, c.want)
		}
	}
}

func TestExecuteVariables(t *testing.T) {
	s := newTestSchema(t)
	q := `query ($s: String, $i: Int, $f: Float, $b: Boolean, $id: ID, $list: [Int], $nn: Int! = 5, $mood: Mood, $filter: UserFilter) {
	echo(s: $s, i: $i, f: $f, b: $b, id: $id, list: $list, nn: $nn, mood: $mood, filter: $filter)
}`
	vars := func(js string) map[string]any {
		v, err := decodeJSON([]byte(js))
		if err != nil {
			t.Fatal(err)
		}
		return v.(map[string]any)
	}
	for _, c := range []struct{ vars, want string }{
		{`{}`, `nn=5`},
		{`{"s": "x", "i": 3, "f": 4, "b": true, "id": 9}`, `b=true f=4 i=3 id="9" nn=5 s="x"`},
		{`{"i": 1.0, "f": 1.5}`, `f=1.5 i=1 nn=5`},
		{`{"list": 4}`, `list=[]interface {}{4} nn=5`},
		{`{"list": [1, null]}`, `list=[]interface {}{1, interface {}(nil)} nn=5`},
		{`{"mood": "HAPPY", "s": null}`, `mood=1 nn=5 s=<nil>`},
		{`{"filter": {"name": "a"}}`, `filter=map[string]interface {}{"mood":1, "name":"a"} nn=5`},
		{`{"nn": 8}`, `nn=8`},
	} {
		res := run(t, s.Schema, q, vars(c.vars))
		if len(res.Errors) > 0 {
			t.Errorf("%s: %s", c.vars, errorsText(res.Errors))
			continue
		}
		if got, _ := res.Data.Get("echo"); got != c.want {
			t.Errorf("%s:\n got  %v\n want %v", c.vars, got, c.want)
		}
	}
	for _, c := range []struct{ vars, want string }{
		{`{"i": "3"}`, `Variable "$i" got invalid value "3"; Int cannot represent non-integer value: "3" (1:20)`},
		{`{"i": 2147483648}`, `Variable "$i" got invalid value 2147483648; Int cannot represent non 32-bit signed integer value: 2147483648 (1:20)`},
		{`{"i": 1.5}`, `Variable "$i" got invalid value 1.5; Int cannot represent non-integer value: 1.5 (1:20)`},
		{`{"s": 1}`, `Variable "$s" got invalid value 1; String cannot represent a non string value: 1 (1:8)`},
		{`{"b": "true"}`, `Variable "$b" got invalid value "true"; Boolean cannot represent a non boolean value: "true" (1:40)`},
		{`{"id": 1.5}`, `Variable "$id" got invalid value 1.5; ID cannot represent value: 1.5 (1:53)`},
		{`{"nn": null}`, `Variable "$nn" of non-null type "Int!" must not be null. (1:76)`},
		{`{"mood": "ANGRY"}`, `Variable "$mood" got invalid value "ANGRY"; Value "ANGRY" does not exist in "Mood" enum. (1:91)`},
		{`{"mood": 1}`, `Variable "$mood" got invalid value 1; Enum "Mood" cannot represent non-string value: 1. (1:91)`},
		{`{"list": [1, "x"]}`, `Variable "$list" got invalid value "x" at "list[1]"; Int cannot represent non-integer value: "x" (1:62)`},
		{`{"filter": {"nmae": "a", "tags": [null]}}`, `Variable "$filter" got invalid value null at "filter.tags[0]"; Expected non-nullable type "String!" not to be null. (1:104)` + "\n" +
			`Variable "$filter" got invalid value {"nmae":"a","tags":[null]}; Field "nmae" is not defined by type "UserFilter". Did you mean "name"? (1:104)`},
		{`{"filter": "x"}`, `Variable "$filter" got invalid value "x"; Expected type "UserFilter" to be an object. (1:104)`},
	} {
		res := run(t, s.Schema, q, vars(c.vars))
		if res.Executed {
			t.Errorf("%s: executed", c.vars)
		}
		for _, e := range res.Errors {
			if e.Extensions["code"] != CodeBadUserInput {
				t.Errorf("%s: code %v", c.vars, e.Extensions["code"])
			}
		}
		if got := errorsText(res.Errors); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.vars, got, c.want)
		}
	}
	res := run(t, s.Schema, `query ($m: Mood!) { mood(m: $m) }`)
	if got := errorsText(res.Errors); got != `Variable "$m" of required type "Mood!" was not provided. (1:8)` {
		t.Errorf("missing required: %s", got)
	}
}

func TestExecuteRequests(t *testing.T) {
	s := newTestSchema(t)
	for _, c := range []struct {
		req  Request
		want string
	}{
		{Request{Query: `{ me `}, `{"errors":[{"message":"Syntax Error: Expected Name, found \u003cEOF\u003e.","locations":[{"line":1,"column":6}],"extensions":{"code":"GRAPHQL_PARSE_FAILED"}}]}`},
		{Request{Query: `{ nope }`}, `{"errors":[{"message":"Cannot query field \"nope\" on type \"Query\".","locations":[{"line":1,"column":3}],"extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`},
		{Request{Query: `query A { me { name } } query B { me { id } }`}, `{"errors":[{"message":"Must provide operation name if query contains multiple operations.","extensions":{"code":"BAD_REQUEST"}}]}`},
		{Request{Query: `query A { me { name } }`, OperationName: "C"}, `{"errors":[{"message":"Unknown operation named \"C\".","extensions":{"code":"BAD_REQUEST"}}]}`},
		{Request{Query: `query A { me { name } } query B { me { id } }`, OperationName: "B"}, `{"data":{"me":{"id":"1"}}}`},
		{Request{Query: `fragment F on User { name }`}, `{"errors":[{"message":"Fragment \"F\" is never used.","locations":[{"line":1,"column":1}],"extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`},
		{Request{Query: `subscription { ticks }`}, `{"errors":[{"message":"Subscriptions are not supported over this transport.","extensions":{"code":"BAD_REQUEST"}}]}`},
		{Request{}, `{"errors":[{"message":"Must provide query string.","extensions":{"code":"BAD_REQUEST"}}]}`},
	} {
		srv, _ := NewServer(Config{Schema: s.Schema})
		expectJSON(t, srv.Execute(context.Background(), c.req), c.want)
	}
}

func TestExecuteObserveAndCache(t *testing.T) {
	s := newTestSchema(t)
	var ops []string
	srv, _ := NewServer(Config{Schema: s.Schema, CacheSize: 2, Observe: func(_ context.Context, op OperationInfo, res *Response, d time.Duration) {
		ops = append(ops, op.Type+" "+op.Name+fmt.Sprintf(" %v", res.Executed))
	}})
	ctx := context.Background()
	for range 3 {
		srv.Execute(ctx, Request{Query: `query Me { me { name } }`})
	}
	if n := srv.parses.Load(); n != 1 {
		t.Errorf("parsed %d times, want once", n)
	}
	srv.Execute(ctx, Request{Query: `{ me { id } }`})
	srv.Execute(ctx, Request{Query: `mutation M { second }`})
	srv.Execute(ctx, Request{Query: `query Me { me { name } }`}) // evicted
	if n := srv.parses.Load(); n != 4 {
		t.Errorf("parsed %d times, want 4", n)
	}
	srv.Execute(ctx, Request{Query: `{ bad`})
	srv.Execute(ctx, Request{Query: `{ bad`})
	if n := srv.parses.Load(); n != 5 {
		t.Errorf("parsed %d times, want 5 (failures are cached too)", n)
	}
	if got := strings.Join(ops, ", "); got != "query Me true, query Me true, query Me true, query  true, mutation M true, query Me true,   false,   false" {
		t.Errorf("observed: %s", got)
	}
	nocache, _ := NewServer(Config{Schema: s.Schema, CacheSize: -1})
	nocache.Execute(ctx, Request{Query: `{ me { id } }`})
	nocache.Execute(ctx, Request{Query: `{ me { id } }`})
	if n := nocache.parses.Load(); n != 2 {
		t.Errorf("parsed %d times without a cache", n)
	}
}
