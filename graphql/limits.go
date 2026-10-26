package graphql

import (
	"fmt"
	"strings"
	"time"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// Limits bound what one request may ask, checked once the document
// validates and before anything resolves: each one exceeded is an error of
// its own (extensions.code DEPTH_LIMIT, COMPLEXITY_LIMIT, ALIAS_LIMIT,
// FIELD_LIMIT, SIZE_LIMIT), and nothing runs. Introspection's fields
// (__schema, __type and what they select) count towards none of them, so
// tools reading the schema aren't refused. A zero value takes the default;
// a negative one turns the limit off.
type Limits struct {
	// Depth is how deep fields may nest: { a { b } } is 2. Default 10.
	Depth int
	// Complexity bounds a query's cost: each field costs its FieldDef.Cost
	// (1 by default), and what a list field selects counts once per item
	// it may return — its first, last or limit argument when it has one,
	// else DefaultListSize. Default 1000.
	Complexity int
	// DefaultListSize is how many items a list field without a first,
	// last or limit argument counts for. Default 10.
	DefaultListSize int
	// Aliases is how many aliased fields the operation may hold. Default 30.
	Aliases int
	// Fields is how many field selections the operation may hold, its
	// fragments' counted once each. Default 500.
	Fields int
	// Bytes is the largest query document, in bytes. Default 100000. Over
	// HTTP the body may carry up to 1 MiB more, for the variables.
	Bytes int
	// Timeout bounds a request's execution: resolvers are given a context
	// that ends then, and fields not resolved by then fail with
	// extensions.code TIMEOUT. Default 10 seconds.
	Timeout time.Duration
}

// Limits' defaults.
const (
	DefaultDepth           = 10
	DefaultComplexity      = 1000
	DefaultListSize        = 10
	DefaultAliases         = 30
	DefaultFields          = 500
	DefaultBytes           = 100_000
	DefaultTimeout         = 10 * time.Second
	defaultCacheSize       = 256
	maxConcurrentResolvers = 64
)

func (l Limits) withDefaults() Limits {
	def := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	def(&l.Depth, DefaultDepth)
	def(&l.Complexity, DefaultComplexity)
	def(&l.DefaultListSize, DefaultListSize)
	def(&l.Aliases, DefaultAliases)
	def(&l.Fields, DefaultFields)
	def(&l.Bytes, DefaultBytes)
	if l.Timeout == 0 {
		l.Timeout = DefaultTimeout
	}
	return l
}

// check reports each limit the operation exceeds.
func (l Limits) check(s *Schema, op *language.OperationDefinition, frags map[string]*language.FragmentDefinition, vars map[string]any) []*Error {
	var errs []*Error
	at := []Location{{op.Line, op.Column}}
	fail := func(c, format string, args ...any) {
		errs = append(errs, &Error{Message: fmt.Sprintf(format, args...), Locations: at, Extensions: code(c)})
	}
	m := &measure{s: s, frags: frags, vars: vars, listSize: l.DefaultListSize}
	if l.Depth > 0 {
		if d := m.depth(op.SelectionSet, map[string]bool{}); d > l.Depth {
			fail(CodeDepthLimit, "The query is %d levels deep, more than the limit of %d.", d, l.Depth)
		}
	}
	if l.Complexity > 0 {
		if c := m.cost(op.SelectionSet, s.root(op.Operation), map[string]bool{}); c > l.Complexity {
			fail(CodeComplexityLimit, "The query's complexity is %d, more than the limit of %d.", c, l.Complexity)
		}
	}
	aliases, fields := m.count(op.SelectionSet, map[string]bool{})
	if l.Aliases > 0 && aliases > l.Aliases {
		fail(CodeAliasLimit, "The query has %d aliases, more than the limit of %d.", aliases, l.Aliases)
	}
	if l.Fields > 0 && fields > l.Fields {
		fail(CodeFieldLimit, "The query selects %d fields, more than the limit of %d.", fields, l.Fields)
	}
	return errs
}

type measure struct {
	s        *Schema
	frags    map[string]*language.FragmentDefinition
	vars     map[string]any
	listSize int
}

func introspective(name string) bool { return strings.HasPrefix(name, "__") }

// depth is how deep ss's fields nest; onPath guards fragment cycles.
func (m *measure) depth(ss *language.SelectionSet, onPath map[string]bool) int {
	max := 0
	for _, sel := range ss.Selections {
		d := 0
		switch sel := sel.(type) {
		case *language.Field:
			if introspective(sel.Name) {
				continue
			}
			d = 1
			if sel.SelectionSet != nil {
				d += m.depth(sel.SelectionSet, onPath)
			}
		case *language.InlineFragment:
			d = m.depth(sel.SelectionSet, onPath)
		case *language.FragmentSpread:
			f := m.frags[sel.Name]
			if f == nil || onPath[sel.Name] {
				continue
			}
			onPath[sel.Name] = true
			d = m.depth(f.SelectionSet, onPath)
			delete(onPath, sel.Name)
		}
		if d > max {
			max = d
		}
	}
	return max
}

// cost is ss's complexity under parent.
func (m *measure) cost(ss *language.SelectionSet, parent NamedType, onPath map[string]bool) int {
	total := 0
	for _, sel := range ss.Selections {
		switch sel := sel.(type) {
		case *language.Field:
			if introspective(sel.Name) {
				continue
			}
			var def *FieldDef
			if parent != nil {
				def = m.s.fieldDef(parent, sel.Name)
			}
			c, child, list := 1, 0, false
			var sub NamedType
			if def != nil {
				if def.Cost > 0 {
					c = def.Cost
				}
				_, list = nullable(def.Type).(*List)
				sub = NamedOf(def.Type)
			}
			if sel.SelectionSet != nil {
				child = m.cost(sel.SelectionSet, sub, onPath)
			}
			if list {
				child *= m.size(sel)
			}
			total += c + child
		case *language.InlineFragment:
			t := parent
			if sel.TypeCondition != nil {
				t = m.s.types[sel.TypeCondition.Name]
			}
			total += m.cost(sel.SelectionSet, t, onPath)
		case *language.FragmentSpread:
			f := m.frags[sel.Name]
			if f == nil || onPath[sel.Name] {
				continue
			}
			onPath[sel.Name] = true
			total += m.cost(f.SelectionSet, m.s.types[f.TypeCondition.Name], onPath)
			delete(onPath, sel.Name)
		}
	}
	return total
}

// size is how many items a list field counts for.
func (m *measure) size(f *language.Field) int {
	for _, a := range f.Arguments {
		if a.Name != "first" && a.Name != "last" && a.Name != "limit" {
			continue
		}
		var v any
		switch x := a.Value.(type) {
		case *language.IntValue:
			v, _ = parseIntLiteral(x)
		case *language.Variable:
			v = m.vars[x.Name]
		}
		if n, ok, _ := asInt64(v); ok && n >= 0 {
			return int(n)
		}
	}
	return m.listSize
}

// count is how many aliases and fields ss holds, each fragment counted
// once.
func (m *measure) count(ss *language.SelectionSet, seen map[string]bool) (aliases, fields int) {
	for _, sel := range ss.Selections {
		switch sel := sel.(type) {
		case *language.Field:
			if introspective(sel.Name) && sel.Name != "__typename" {
				continue
			}
			fields++
			if sel.Alias != "" {
				aliases++
			}
			if sel.SelectionSet != nil {
				a, f := m.count(sel.SelectionSet, seen)
				aliases, fields = aliases+a, fields+f
			}
		case *language.InlineFragment:
			a, f := m.count(sel.SelectionSet, seen)
			aliases, fields = aliases+a, fields+f
		case *language.FragmentSpread:
			if seen[sel.Name] || m.frags[sel.Name] == nil {
				continue
			}
			seen[sel.Name] = true
			a, f := m.count(m.frags[sel.Name].SelectionSet, seen)
			aliases, fields = aliases+a, fields+f
		}
	}
	return aliases, fields
}
