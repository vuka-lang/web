package nulltest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/web/v2/auth"
	"github.com/vuka-lang/web/v2/graphql"
)

// The app's Go, as written in .vuka files:
//
//	type Info struct {
//		SKU        Option[string]
//		ReleasedAt Option[time.Time]
//		OwnerID    Option[int64] // ID
//		Scores     []Option[int] // [Int]!
//	}
//
//	type Filter struct {
//		Name  Option[string]
//		Codes []Option[int]
//	}
//
//	@graphql.Query @auth.Public
//	func Infos(ctx context.Context, filter Option[Filter], @graphql.Arg("q") query Option[string]) ([]Info, error) { … }
//
//	@graphql.Query @auth.Public
//	func Latest(ctx context.Context, released bool) Option[Info] { … }
//
// and the registrations the compiler generates for them, below.

type Info struct {
	SKU        vuka.Option[string]
	ReleasedAt vuka.Option[time.Time]
	OwnerID    vuka.Option[int64]
	Scores     []vuka.Option[int]
}

type Filter struct {
	Name  vuka.Option[string]
	Codes []vuka.Option[int]
}

func describe[T any](o vuka.Option[T]) string {
	if v, ok := o.Get(); ok {
		return fmt.Sprintf("Some(%v)", v)
	}
	return "None"
}

func Infos(ctx context.Context, filter vuka.Option[Filter], query vuka.Option[string]) ([]Info, error) {
	got := "filter " + describe(filter) + ", q " + describe(query)
	if f, ok := filter.Get(); ok {
		got = "filter name " + describe(f.Name) + fmt.Sprintf(" codes %v", f.Codes) + ", q " + describe(query)
	}
	return []Info{{SKU: vuka.Some(got)}}, nil
}

func Latest(ctx context.Context, released bool) vuka.Option[Info] {
	if !released {
		return vuka.None[Info]()
	}
	return vuka.Some(Info{
		SKU:        vuka.Some("K-1"),
		ReleasedAt: vuka.Some(time.Date(2026, 10, 30, 9, 30, 0, 0, time.UTC)),
		OwnerID:    vuka.Some(int64(7)),
		Scores:     []vuka.Option[int]{vuka.Some(1), vuka.None[int](), vuka.Some(0)},
	})
}

func init() {
	vuka.RegisterFunc(Infos, append([]any{*new(graphql.Query)}, auth.Public().Attrs()...)...)
	vuka.RegisterFunc(Latest, append([]any{*new(graphql.Query)}, auth.Public().Attrs()...)...)
	graphql.Register(graphql.Package{
		Path: "github.com/vuka-lang/web/v2/graphql/internal/nulltest/option",
		Operations: []graphql.Operation{
			{
				Go: "Infos", Func: Infos,
				Params: []graphql.Param{{Name: "ctx"}, {Name: "filter", Type: graphql.InputOf[Filter]()}, {Name: "query", Arg: "q", Type: graphql.String}},
				Type:   &graphql.NonNull{Of: graphql.ListOf[Info](&graphql.NonNull{Of: graphql.ObjectOf[Info]()})},
				Resolve: func(ctx context.Context, c *graphql.Call) (any, error) {
					return Infos(ctx, graphql.GetOption[Filter](c, 1), graphql.GetOption[string](c, 2))
				},
			},
			{
				Go: "Latest", Func: Latest,
				Params: []graphql.Param{{Name: "ctx"}, {Name: "released", Type: &graphql.NonNull{Of: graphql.Boolean}}},
				Type:   graphql.ObjectOf[Info](),
				Resolve: func(ctx context.Context, c *graphql.Call) (any, error) {
					return graphql.OptionValue(Latest(ctx, graphql.Get[bool](c, 1))), nil
				},
			},
		},
		Types: []graphql.Binding{
			graphql.Struct[Info]{Fields: []graphql.Member[Info]{
				{Go: "SKU", Type: graphql.String, Get: func(i *Info) any { return graphql.OptionValue(i.SKU) }},
				{Go: "ReleasedAt", Type: graphql.DateTime, Get: func(i *Info) any { return graphql.OptionValue(i.ReleasedAt) }},
				{Go: "OwnerID", Type: graphql.IntIDOf[int64](), Get: func(i *Info) any { return graphql.OptionValue(i.OwnerID) }},
				{Go: "Scores", Type: &graphql.NonNull{Of: graphql.ListOfOption[int](graphql.Int)}, Get: func(i *Info) any { return i.Scores }},
			}},
			graphql.FormInput[Filter]{Fields: []graphql.InputMember[Filter]{
				{Go: "Name", Type: graphql.String, Set: func(f *Filter, v any) { f.Name = graphql.AsOption[string](v) }},
				{Go: "Codes", Type: graphql.ListOfOption[int](graphql.Int), Set: func(f *Filter, v any) { f.Codes = graphql.As[[]vuka.Option[int]](v) }},
			}},
		},
	})
}

// TestOption: a vuka.Option is a nullable value — None null, Some its value
// — as a result, a field, a list's item, an argument and an input field; an
// absent or null input is None.
func TestOption(t *testing.T) {
	h := newApp(t, graphql.Introspection(true))
	_, out := post(t, h, `{ __type(name: "Info") { fields { name type { kind name ofType { kind name } } } } }`, nil)
	for _, want := range []string{
		`{"name":"sku","type":{"kind":"SCALAR","name":"String","ofType":null}}`,
		`{"name":"ownerID","type":{"kind":"SCALAR","name":"ID","ofType":null}}`,
		`{"name":"scores","type":{"kind":"NON_NULL","name":null,"ofType":{"kind":"LIST","name":null}}}`,
	} {
		if got := js(out); !strings.Contains(got, want) {
			t.Errorf("no %s in %s", want, got)
		}
	}
	for _, c := range []struct{ query, want string }{
		{`{ latest(released: false) { sku } }`, `{"data":{"latest":null}}`},
		{`{ latest(released: true) { sku releasedAt ownerID scores } }`, `{"data":{"latest":{"ownerID":"7","releasedAt":"2026-10-30T09:30:00Z","scores":[1,null,0],"sku":"K-1"}}}`},
		{`{ infos { sku } }`, `{"data":{"infos":[{"sku":"filter None, q None"}]}}`},
		{`{ infos(filter: null, q: null) { sku } }`, `{"data":{"infos":[{"sku":"filter None, q None"}]}}`},
		{`{ infos(filter: {}, q: "x") { sku } }`, `{"data":{"infos":[{"sku":"filter name None codes [], q Some(x)"}]}}`},
		{`{ infos(filter: {name: "kettle", codes: [3, null]}) { sku } }`, `{"data":{"infos":[{"sku":"filter name Some(kettle) codes [Some(3) None], q None"}]}}`},
	} {
		if _, out := post(t, h, c.query, nil); js(out) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.query, js(out), c.want)
		}
	}
}
