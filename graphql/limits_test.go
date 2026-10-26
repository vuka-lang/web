package graphql

import (
	"strings"
	"testing"
	"time"
)

func TestLimitsDefaults(t *testing.T) {
	l := Limits{}.withDefaults()
	if l.Depth != 10 || l.Complexity != 1000 || l.DefaultListSize != 10 || l.Aliases != 30 || l.Fields != 500 || l.Bytes != 100000 || l.Timeout != 10*time.Second {
		t.Errorf("defaults: %+v", l)
	}
	if l := (Limits{Depth: -1}).withDefaults(); l.Depth != -1 {
		t.Errorf("negative turned into %d", l.Depth)
	}
}

func limited(t *testing.T, l Limits, query string, vars ...map[string]any) *Response {
	t.Helper()
	return runWith(t, Config{Schema: newTestSchema(t).Schema, Limits: l, Introspection: true}, query, vars...)
}

func TestLimitDepth(t *testing.T) {
	deep := `{ me { best { best { best { name } } } } }` // 5
	if res := limited(t, Limits{Depth: 5}, deep); len(res.Errors) > 0 {
		t.Errorf("depth 5 within 5: %s", errorsText(res.Errors))
	}
	res := limited(t, Limits{Depth: 4}, deep)
	expectJSON(t, res, `{"errors":[{"message":"The query is 5 levels deep, more than the limit of 4.","locations":[{"line":1,"column":1}],"extensions":{"code":"DEPTH_LIMIT"}}]}`)
	// Through fragments, and the default of 10.
	q := `{ me { ...A } } fragment A on User { best { ...B } } fragment B on User { best { best { best { best { best { best { best { best { name } } } } } } } } }`
	if res := limited(t, Limits{}, q); len(res.Errors) != 1 || res.Errors[0].Extensions["code"] != CodeDepthLimit || !strings.Contains(res.Errors[0].Message, "11 levels") {
		t.Errorf("default depth: %s", errorsText(res.Errors))
	}
	if res := limited(t, Limits{Depth: -1}, q); len(res.Errors) > 0 {
		t.Errorf("depth off: %s", errorsText(res.Errors))
	}
	// Introspection doesn't count.
	if res := limited(t, Limits{Depth: 2}, fullIntrospection); len(res.Errors) > 0 {
		t.Errorf("introspection: %s", errorsText(res.Errors))
	}
}

func TestLimitComplexity(t *testing.T) {
	// users: 1 + 10 (default list size) × (name 1 + friends(1 + 10×1)) = 1 + 10×12 = 121
	q := `{ users { name friends { name } } }`
	if res := limited(t, Limits{Complexity: 121}, q); len(res.Errors) > 0 {
		t.Errorf("121 within 121: %s", errorsText(res.Errors))
	}
	expectJSON(t, limited(t, Limits{Complexity: 120}, q),
		`{"errors":[{"message":"The query's complexity is 121, more than the limit of 120.","locations":[{"line":1,"column":1}],"extensions":{"code":"COMPLEXITY_LIMIT"}}]}`)
	// first multiplies its list field's selection: 1 + 2×(1 + 1 + 10) = 25
	if res := limited(t, Limits{Complexity: 24}, `{ users(first: 2) { name friends { name } } }`); !strings.Contains(errorsText(res.Errors), "complexity is 25") {
		t.Errorf("first literal: %s", errorsText(res.Errors))
	}
	// as a variable too: 1 + 3×12 = 37
	res := limited(t, Limits{Complexity: 30}, `query ($n: Int) { users(first: $n) { name friends { name } } }`, map[string]any{"n": 3})
	if !strings.Contains(errorsText(res.Errors), "complexity is 37") {
		t.Errorf("first variable: %s", errorsText(res.Errors))
	}
	// A list size of its own.
	if res := limited(t, Limits{Complexity: 20, DefaultListSize: 1}, q); len(res.Errors) > 0 {
		t.Errorf("list size 1: %s", errorsText(res.Errors))
	}
}

func TestLimitAliasesAndFields(t *testing.T) {
	var b strings.Builder
	b.WriteString("{ ")
	for i := range 31 {
		b.WriteString("a")
		b.WriteString(strings.Repeat("x", i))
		b.WriteString(": __typename ")
	}
	b.WriteString("}")
	res := limited(t, Limits{}, b.String())
	expectJSON(t, res, `{"errors":[{"message":"The query has 31 aliases, more than the limit of 30.","locations":[{"line":1,"column":1}],"extensions":{"code":"ALIAS_LIMIT"}}]}`)
	res = limited(t, Limits{Fields: 3}, `{ me { id name nick } }`)
	expectJSON(t, res, `{"errors":[{"message":"The query selects 4 fields, more than the limit of 3.","locations":[{"line":1,"column":1}],"extensions":{"code":"FIELD_LIMIT"}}]}`)
	// A fragment spread twice counts once.
	if res := limited(t, Limits{Fields: 4}, `{ a: me { ...F } b: me { ...F } } fragment F on User { id name }`); len(res.Errors) > 0 {
		t.Errorf("fragment once: %s", errorsText(res.Errors))
	}
}

func TestLimitBytes(t *testing.T) {
	q := `{ me { name } }`
	expectJSON(t, limited(t, Limits{Bytes: 10}, q),
		`{"errors":[{"message":"The query is 15 bytes, more than the limit of 10.","extensions":{"code":"SIZE_LIMIT"}}]}`)
	if res := limited(t, Limits{Bytes: 15}, q); len(res.Errors) > 0 {
		t.Errorf("15 within 15: %s", errorsText(res.Errors))
	}
}

func TestLimitsSeveral(t *testing.T) {
	res := limited(t, Limits{Depth: 1, Fields: 1}, `{ me { name } }`)
	if got := errorsText(res.Errors); got != "The query is 2 levels deep, more than the limit of 1. (1:1)\nThe query selects 2 fields, more than the limit of 1. (1:1)" {
		t.Errorf("got %s", got)
	}
}
