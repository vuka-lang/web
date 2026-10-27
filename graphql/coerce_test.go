package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/vuka-lang/web/v2/graphql/language"
)

func TestSerializeBuiltins(t *testing.T) {
	type myInt int
	for _, c := range []struct {
		s    *Scalar
		in   any
		want any
		err  string
	}{
		{Int, 1, 1, ""}, {Int, int8(-3), -3, ""}, {Int, uint16(7), 7, ""}, {Int, int64(math.MaxInt32), math.MaxInt32, ""},
		{Int, int64(math.MaxInt32) + 1, nil, "Int cannot represent non 32-bit signed integer value: 2147483648"},
		{Int, uint64(math.MaxUint64), nil, "non 32-bit"},
		{Int, 3.0, 3, ""}, {Int, 3.5, nil, "Int cannot represent non-integer value: 3.5"}, {Int, true, 1, ""}, {Int, "12", 12, ""},
		{Int, "x", nil, `Int cannot represent non-integer value: "x"`}, {Int, json.Number("5"), 5, ""}, {Int, myInt(1), nil, "non-integer"},
		{Float, 1, 1.0, ""}, {Float, float32(0.5), 0.5, ""}, {Float, "2.5", 2.5, ""}, {Float, math.Inf(1), nil, "Float cannot represent non numeric value"},
		{Float, []int{1}, nil, "Float cannot represent non numeric value: [1]"},
		{String, "s", "s", ""}, {String, true, "true", ""}, {String, 12, "12", ""}, {String, 1.5, "1.5", ""}, {String, uint64(math.MaxUint64), "18446744073709551615", ""},
		{String, struct{}{}, nil, "String cannot represent value: {}"},
		{Boolean, true, true, ""}, {Boolean, 0, false, ""}, {Boolean, 2.0, true, ""}, {Boolean, "true", nil, `Boolean cannot represent a non boolean value: "true"`},
		{ID, "abc", "abc", ""}, {ID, 42, "42", ""}, {ID, int64(-1), "-1", ""}, {ID, 7.0, "7", ""}, {ID, 7.5, nil, "ID cannot represent value: 7.5"}, {ID, true, nil, "ID cannot represent value: true"},
	} {
		got, err := c.s.Serialize(c.in)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s.Serialize(%#v) = %v, %v; want error %q", c.s.Name, c.in, got, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s.Serialize(%#v) = %#v, %v; want %#v", c.s.Name, c.in, got, err, c.want)
		}
	}
}

func TestParseLiteralBuiltins(t *testing.T) {
	lit := func(s string) language.Value {
		v, err := language.ParseValue(s, true)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		s    *Scalar
		in   string
		want any
		err  string
	}{
		{Int, "1", 1, ""}, {Int, "-2147483648", math.MinInt32, ""}, {Int, "2147483648", nil, "non 32-bit"}, {Int, "1.0", nil, "non-integer"}, {Int, `"1"`, nil, "non-integer"},
		{Float, "1", 1.0, ""}, {Float, "1.5e1", 15.0, ""}, {Float, "1e400", nil, "non numeric"}, {Float, "true", nil, "non numeric"},
		{String, `"x"`, "x", ""}, {String, `"""b"""`, "b", ""}, {String, "X", nil, "non string"},
		{Boolean, "false", false, ""}, {Boolean, "0", nil, "non boolean"},
		{ID, `"a"`, "a", ""}, {ID, "12", "12", ""}, {ID, "1.5", nil, "non-string and non-integer"}, {ID, "null", nil, "non-string"},
	} {
		got, err := c.s.ParseLiteral(lit(c.in))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s.ParseLiteral(%s) = %v, %v; want error %q", c.s.Name, c.in, got, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s.ParseLiteral(%s) = %#v, %v; want %#v", c.s.Name, c.in, got, err, c.want)
		}
	}
}

// What a Go binding builds: typed lists and inputs, enums of Go values,
// a custom scalar with only ParseValue.
type point struct{ X, Y int }

type shape struct {
	Name   string
	Points []point
	Tags   []string
}

func bindingSchema(t *testing.T) *Schema {
	t.Helper()
	color := &Enum{Name: "Color", Values: []*EnumValueDef{{Name: "RED", Value: 1}, {Name: "BLUE", Value: 2}},
		Serialize: func(v any) (string, error) {
			switch v {
			case 1:
				return "RED", nil
			case 2:
				return "BLUE", nil
			}
			return "", fmt.Errorf("not a color: %v", v)
		}}
	hex := &Scalar{Name: "Hex", ParseValue: func(v any) (any, error) {
		s, ok := v.(string)
		if !ok || !strings.HasPrefix(s, "#") {
			return nil, errors.New("a hex color starts with #")
		}
		return strings.ToUpper(s), nil
	}, Serialize: func(v any) (any, error) { return v, nil }}
	pointIn := &InputObject{Name: "PointInput", Fields: []*InputFieldDef{{Name: "x", Type: &NonNull{Of: Int}}, {Name: "y", Type: Int, Default: &language.IntValue{Value: "0"}}},
		Build: func(f map[string]any) (any, error) {
			p := point{X: f["x"].(int)}
			if y, ok := f["y"].(int); ok {
				p.Y = y
			}
			return p, nil
		}}
	points := &List{Of: &NonNull{Of: pointIn}, Build: func(items []any) (any, error) {
		out := make([]point, len(items))
		for i, x := range items {
			out[i] = x.(point)
		}
		return out, nil
	}}
	shapeIn := &InputObject{Name: "ShapeInput", Fields: []*InputFieldDef{{Name: "name", Type: &NonNull{Of: String}}, {Name: "points", Type: points}, {Name: "tags", Type: &List{Of: String}}},
		Build: func(f map[string]any) (any, error) {
			s := &shape{Name: f["name"].(string)}
			if p, ok := f["points"].([]point); ok {
				s.Points = p
			}
			if f["name"] == "invalid" {
				return nil, errors.New("that name is taken")
			}
			return s, nil
		}}
	pointOut := &Object{Name: "Point", Fields: []*FieldDef{
		{Name: "x", Type: &NonNull{Of: Int}, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return p.Source.(*point).X, nil }, Trivial: true},
		{Name: "y", Type: &NonNull{Of: Int}, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return p.Source.(*point).Y, nil }, Trivial: true},
	}, Value: func(v any) any {
		switch x := v.(type) {
		case point:
			return &x
		case *point:
			if x == nil {
				return nil
			}
			return x
		}
		return v
	}}
	pointList := &List{Of: &NonNull{Of: pointOut}, Items: func(v any) (int, func(int) any, bool) {
		ps, ok := v.([]point)
		return len(ps), func(i int) any { return ps[i] }, ok
	}}
	shapeOut := &Object{Name: "Shape", Fields: []*FieldDef{
		{Name: "name", Type: String, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return p.Source.(*shape).Name, nil }},
		{Name: "points", Type: pointList, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return p.Source.(*shape).Points, nil }},
	}}
	q := &Object{Name: "Query", Fields: []*FieldDef{
		{Name: "color", Type: color, Args: []*ArgDef{{Name: "c", Type: &NonNull{Of: color}}}, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return p.Args["c"], nil }},
		{Name: "hex", Type: hex, Args: []*ArgDef{{Name: "h", Type: hex}}, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return p.Args["h"], nil }},
		{Name: "shape", Type: shapeOut, Args: []*ArgDef{{Name: "in", Type: &NonNull{Of: shapeIn}}}, Resolve: func(_ context.Context, p ResolveParams) (any, error) { return p.Args["in"], nil }},
		{Name: "notPoints", Type: pointList, Resolve: constant([]any{})},
	}}
	s, err := NewSchema(SchemaConfig{Query: q})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBindingShapes(t *testing.T) {
	s := bindingSchema(t)
	expectJSON(t, run(t, s, `{ color(c: BLUE) shape(in: {name: "tri", points: [{x: 1}, {x: 2, y: 3}]}) { name points { x y } } }`),
		`{"data":{"color":"BLUE","shape":{"name":"tri","points":[{"x":1,"y":0},{"x":2,"y":3}]}}}`)
	// Variables build the same Go values.
	expectJSON(t, run(t, s, `query ($in: ShapeInput!, $c: Color!) { color(c: $c) shape(in: $in) { points { x } } }`, map[string]any{
		"c": "RED", "in": map[string]any{"name": "sq", "points": map[string]any{"x": json.Number("4")}},
	}), `{"data":{"color":"RED","shape":{"points":[{"x":4}]}}}`)
	// A custom scalar without ParseLiteral reads literals as JSON.
	expectJSON(t, run(t, s, `{ hex(h: "#ab") }`), `{"data":{"hex":"#AB"}}`)
	expectJSON(t, run(t, s, `query ($h: Hex) { hex(h: $h) }`, map[string]any{"h": "#cd"}), `{"data":{"hex":"#CD"}}`)
	expectJSON(t, run(t, s, `{ hex(h: "ab") }`), `{"errors":[{"message":"Expected value of type \"Hex\", found \"ab\"; a hex color starts with #","locations":[{"line":1,"column":10}],"extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`)
	// A Build error is the argument's.
	expectJSON(t, run(t, s, `{ shape(in: {name: "invalid"}) { name } }`), `{"errors":[{"message":"Argument \"in\" has invalid value: that name is taken","locations":[{"line":1,"column":3}],"path":["shape"],"extensions":{"code":"BAD_USER_INPUT"}}],"data":{"shape":null}}`)
	// Items saying "not a list".
	expectJSON(t, run(t, s, `{ notPoints { x } }`), `{"errors":[{"message":"Expected Iterable, but did not find one for field \"Query.notPoints\".","locations":[{"line":1,"column":3}],"path":["notPoints"]}],"data":{"notPoints":null}}`)
}
