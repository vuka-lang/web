package graphql

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Error is an error of a response, as the specification shapes it: a
// message, where in the document it arose, the path of the field it
// belongs to, and extensions (extensions.code says what kind it is). Err is
// its cause, never sent.
type Error struct {
	Message    string         `json:"message"`
	Locations  []Location     `json:"locations,omitempty"`
	Path       []any          `json:"path,omitempty"`
	Extensions map[string]any `json:"extensions,omitempty"`
	Err        error          `json:"-"`
}

func (e *Error) Error() string { return e.Message }

// Unwrap is the cause.
func (e *Error) Unwrap() error { return e.Err }

// Location is a line and column of the document, 1-based.
type Location struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// The extensions.code of the errors the engine itself reports. Errors a
// resolver returns carry what Config.Present gives them.
const (
	CodeParseFailed      = "GRAPHQL_PARSE_FAILED"      // the document doesn't parse
	CodeValidationFailed = "GRAPHQL_VALIDATION_FAILED" // a validation rule refuses it
	CodeBadUserInput     = "BAD_USER_INPUT"            // a variable's or argument's value its type refuses
	CodeBadRequest       = "BAD_REQUEST"               // no operation to run, a malformed HTTP request, a subscription
	CodeDepthLimit       = "DEPTH_LIMIT"               // Limits.Depth
	CodeComplexityLimit  = "COMPLEXITY_LIMIT"          // Limits.Complexity
	CodeAliasLimit       = "ALIAS_LIMIT"               // Limits.Aliases
	CodeFieldLimit       = "FIELD_LIMIT"               // Limits.Fields
	CodeSizeLimit        = "SIZE_LIMIT"                // Limits.Bytes
	CodeTimeout          = "TIMEOUT"                   // Limits.Timeout, or the request's context ending
	CodeInternal         = "INTERNAL_SERVER_ERROR"     // a resolver's panic, without Config.Present
)

func code(c string) map[string]any { return map[string]any{"code": c} }

// Response is the result of a request. Executed says execution started:
// the response then has data, null when Data is nil; a request refused
// before (a document that doesn't parse or validate, variables of the
// wrong type, a limit) has errors only.
type Response struct {
	Data       *OrderedMap
	Errors     []*Error
	Extensions map[string]any
	Executed   bool
}

// MarshalJSON writes the response as the specification shapes it:
// "errors" first when there are any, then "data" (when executed) and
// "extensions".
func (r *Response) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	sep := false
	if len(r.Errors) > 0 {
		e, err := json.Marshal(r.Errors)
		if err != nil {
			return nil, err
		}
		b.WriteString(`"errors":`)
		b.Write(e)
		sep = true
	}
	if r.Executed {
		if sep {
			b.WriteByte(',')
		}
		b.WriteString(`"data":`)
		if r.Data == nil {
			b.WriteString("null")
		} else {
			d, err := json.Marshal(r.Data)
			if err != nil {
				return nil, err
			}
			b.Write(d)
		}
		sep = true
	}
	if len(r.Extensions) > 0 {
		if sep {
			b.WriteByte(',')
		}
		e, err := json.Marshal(r.Extensions)
		if err != nil {
			return nil, err
		}
		b.WriteString(`"extensions":`)
		b.Write(e)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// OrderedMap is an object of a response: its keys in the order the query
// selected them.
type OrderedMap struct {
	keys   []string
	values []any
}

// NewOrderedMap is an empty map with room for n keys.
func NewOrderedMap(n int) *OrderedMap {
	return &OrderedMap{keys: make([]string, 0, n), values: make([]any, 0, n)}
}

// Set sets key to v, adding it at the end when it is new.
func (m *OrderedMap) Set(key string, v any) {
	for i, k := range m.keys {
		if k == key {
			m.values[i] = v
			return
		}
	}
	m.keys = append(m.keys, key)
	m.values = append(m.values, v)
}

// Get is key's value.
func (m *OrderedMap) Get(key string) (any, bool) {
	for i, k := range m.keys {
		if k == key {
			return m.values[i], true
		}
	}
	return nil, false
}

// Keys are the keys, in order.
func (m *OrderedMap) Keys() []string { return m.keys }

// Len is how many keys it has.
func (m *OrderedMap) Len() int { return len(m.keys) }

// MarshalJSON writes the object, its keys in order.
func (m *OrderedMap) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(m.values[i])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// didYouMean suggests the options close to input: ` Did you mean "a" or
// "b"?`, or "" when none is.
func didYouMean(input string, options []string) string {
	s := suggest(input, options)
	if len(s) == 0 {
		return ""
	}
	quoted := make([]string, len(s))
	for i, x := range s {
		quoted[i] = fmt.Sprintf("%q", x)
	}
	switch len(quoted) {
	case 1:
		return " Did you mean " + quoted[0] + "?"
	case 2:
		return " Did you mean " + quoted[0] + " or " + quoted[1] + "?"
	}
	return " Did you mean " + strings.Join(quoted[:len(quoted)-1], ", ") + ", or " + quoted[len(quoted)-1] + "?"
}

// suggest is the options close to input, closest first, at most five.
func suggest(input string, options []string) []string {
	type cand struct {
		s string
		d int
	}
	var cs []cand
	threshold := len(input)*4/10 + 1
	for _, o := range options {
		d := 0
		if !strings.EqualFold(o, input) {
			d = editDistance(strings.ToLower(input), strings.ToLower(o))
			if d > threshold {
				continue
			}
		} else if o == input {
			continue
		} else {
			d = 1
		}
		cs = append(cs, cand{o, d})
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].d != cs[j].d {
			return cs[i].d < cs[j].d
		}
		return cs[i].s < cs[j].s
	})
	var out []string
	for i := 0; i < len(cs) && i < 5; i++ {
		out = append(out, cs[i].s)
	}
	return out
}

// editDistance is the optimal string alignment distance of a and b.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	d := make([][]int, len(ra)+1)
	for i := range d {
		d[i] = make([]int, len(rb)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(ra)][len(rb)]
}
