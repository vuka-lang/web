package graphql

import (
	"fmt"
	"strings"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// OverlappingFieldsCanBeMerged: fields of one response key, wherever they
// are selected for the same object, must be the same field with the same
// arguments (unless their parents are different object types) and give
// values of the same shape, their own selections merging alike.

type fieldEntry struct {
	parent NamedType
	node   *language.Field
	def    *FieldDef
}

type fieldMap struct {
	keys []string
	m    map[string][]fieldEntry
}

type fieldsAndFrags struct {
	fields *fieldMap
	frags  []string
}

type conflict struct {
	name   string
	msg    string     // the reason, or ""
	subs   []conflict // the subfields' conflicts, when the reason is theirs
	f1, f2 []*language.Field
}

type overlapCache struct {
	v      *validator
	cached map[*language.SelectionSet]*fieldsAndFrags
	pairs  map[[2]string]bool // fragment pair → compared as mutually exclusive only
}

func newOverlapCache(v *validator) *overlapCache {
	return &overlapCache{v: v, cached: map[*language.SelectionSet]*fieldsAndFrags{}, pairs: map[[2]string]bool{}}
}

func (c *overlapCache) check(ss *language.SelectionSet, parent NamedType) {
	for _, cf := range c.withinSelectionSet(parent, ss) {
		locs := make([]language.Loc, 0, len(cf.f1)+len(cf.f2))
		for _, f := range cf.f1 {
			locs = append(locs, f.Location())
		}
		for _, f := range cf.f2 {
			locs = append(locs, f.Location())
		}
		c.v.report(fmt.Sprintf("Fields %q conflict because %s. Use different aliases on the fields to fetch both if this was intentional.", cf.name, reasonText(cf)), locs...)
	}
}

func reasonText(cf conflict) string {
	if len(cf.subs) == 0 {
		return cf.msg
	}
	parts := make([]string, len(cf.subs))
	for i, s := range cf.subs {
		parts[i] = fmt.Sprintf("subfields %q conflict because %s", s.name, reasonText(s))
	}
	return strings.Join(parts, " and ")
}

func (c *overlapCache) fieldsOf(parent NamedType, ss *language.SelectionSet) *fieldsAndFrags {
	if r, ok := c.cached[ss]; ok {
		return r
	}
	fm := &fieldMap{m: map[string][]fieldEntry{}}
	var frags []string
	seen := map[string]bool{}
	var collect func(parent NamedType, ss *language.SelectionSet)
	collect = func(parent NamedType, ss *language.SelectionSet) {
		for _, sel := range ss.Selections {
			switch sel := sel.(type) {
			case *language.Field:
				var def *FieldDef
				if parent != nil {
					def = c.v.s.fieldDef(parent, sel.Name)
				}
				k := sel.ResponseKey()
				if _, ok := fm.m[k]; !ok {
					fm.keys = append(fm.keys, k)
				}
				fm.m[k] = append(fm.m[k], fieldEntry{parent, sel, def})
			case *language.FragmentSpread:
				if !seen[sel.Name] {
					seen[sel.Name] = true
					frags = append(frags, sel.Name)
				}
			case *language.InlineFragment:
				t := parent
				if sel.TypeCondition != nil {
					t = c.v.s.types[sel.TypeCondition.Name]
				}
				collect(t, sel.SelectionSet)
			}
		}
	}
	collect(parent, ss)
	r := &fieldsAndFrags{fm, frags}
	c.cached[ss] = r
	return r
}

func (c *overlapCache) fragmentFields(name string) *fieldsAndFrags {
	f := c.v.frags[name]
	if f == nil {
		return nil
	}
	var t NamedType
	if x := c.v.s.types[f.TypeCondition.Name]; x != nil {
		t = x
	}
	return c.fieldsOf(t, f.SelectionSet)
}

func (c *overlapCache) withinSelectionSet(parent NamedType, ss *language.SelectionSet) []conflict {
	var out []conflict
	ff := c.fieldsOf(parent, ss)
	c.within(&out, ff.fields)
	for i, name := range ff.frags {
		c.fieldsAndFragment(&out, map[string]bool{}, false, ff.fields, name)
		for _, other := range ff.frags[i+1:] {
			c.fragments(&out, false, name, other)
		}
	}
	return out
}

func (c *overlapCache) within(out *[]conflict, fm *fieldMap) {
	for _, k := range fm.keys {
		fs := fm.m[k]
		for i := 0; i < len(fs); i++ {
			for j := i + 1; j < len(fs); j++ {
				if cf, ok := c.find(false, k, fs[i], fs[j]); ok {
					*out = append(*out, cf)
				}
			}
		}
	}
}

func (c *overlapCache) between(out *[]conflict, mutEx bool, a, b *fieldMap) {
	for _, k := range a.keys {
		bs, ok := b.m[k]
		if !ok {
			continue
		}
		for _, f1 := range a.m[k] {
			for _, f2 := range bs {
				if cf, ok := c.find(mutEx, k, f1, f2); ok {
					*out = append(*out, cf)
				}
			}
		}
	}
}

func (c *overlapCache) fieldsAndFragment(out *[]conflict, compared map[string]bool, mutEx bool, fm *fieldMap, name string) {
	if compared[name] {
		return
	}
	compared[name] = true
	ff := c.fragmentFields(name)
	if ff == nil || ff.fields == fm {
		return
	}
	c.between(out, mutEx, fm, ff.fields)
	for _, ref := range ff.frags {
		c.fieldsAndFragment(out, compared, mutEx, fm, ref)
	}
}

func (c *overlapCache) seenPair(a, b string, mutEx bool) bool {
	if a > b {
		a, b = b, a
	}
	exclusiveOnly, ok := c.pairs[[2]string{a, b}]
	if !ok {
		return false
	}
	return mutEx || !exclusiveOnly
}

func (c *overlapCache) addPair(a, b string, mutEx bool) {
	if a > b {
		a, b = b, a
	}
	if prev, ok := c.pairs[[2]string{a, b}]; ok && !prev {
		return
	}
	c.pairs[[2]string{a, b}] = mutEx
}

func (c *overlapCache) fragments(out *[]conflict, mutEx bool, a, b string) {
	if a == b || c.seenPair(a, b, mutEx) {
		return
	}
	c.addPair(a, b, mutEx)
	fa, fb := c.fragmentFields(a), c.fragmentFields(b)
	if fa == nil || fb == nil {
		return
	}
	c.between(out, mutEx, fa.fields, fb.fields)
	for _, ref := range fb.frags {
		c.fragments(out, mutEx, a, ref)
	}
	for _, ref := range fa.frags {
		c.fragments(out, mutEx, ref, b)
	}
}

func (c *overlapCache) subSelections(mutEx bool, p1 NamedType, ss1 *language.SelectionSet, p2 NamedType, ss2 *language.SelectionSet) []conflict {
	var out []conflict
	a, b := c.fieldsOf(p1, ss1), c.fieldsOf(p2, ss2)
	c.between(&out, mutEx, a.fields, b.fields)
	for _, f := range b.frags {
		c.fieldsAndFragment(&out, map[string]bool{}, mutEx, a.fields, f)
	}
	for _, f := range a.frags {
		c.fieldsAndFragment(&out, map[string]bool{}, mutEx, b.fields, f)
	}
	for _, f1 := range a.frags {
		for _, f2 := range b.frags {
			c.fragments(&out, mutEx, f1, f2)
		}
	}
	return out
}

func isObjectType(t NamedType) bool {
	_, ok := t.(*Object)
	return ok
}

func (c *overlapCache) find(parentMutEx bool, name string, e1, e2 fieldEntry) (conflict, bool) {
	mutEx := parentMutEx || e1.parent != e2.parent && e1.parent != nil && e2.parent != nil && isObjectType(e1.parent) && isObjectType(e2.parent)
	one := func(msg string) (conflict, bool) {
		return conflict{name: name, msg: msg, f1: []*language.Field{e1.node}, f2: []*language.Field{e2.node}}, true
	}
	if !mutEx {
		if e1.node.Name != e2.node.Name {
			return one(fmt.Sprintf("%q and %q are different fields", e1.node.Name, e2.node.Name))
		}
		if !sameArguments(e1.node.Arguments, e2.node.Arguments) {
			return one("they have differing arguments")
		}
	}
	var t1, t2 TypeDef
	if e1.def != nil {
		t1 = e1.def.Type
	}
	if e2.def != nil {
		t2 = e2.def.Type
	}
	if t1 != nil && t2 != nil && typesConflict(t1, t2) {
		return one(fmt.Sprintf("they return conflicting types %q and %q", TypeString(t1), TypeString(t2)))
	}
	if e1.node.SelectionSet != nil && e2.node.SelectionSet != nil {
		subs := c.subSelections(mutEx, NamedOf(t1), e1.node.SelectionSet, NamedOf(t2), e2.node.SelectionSet)
		if len(subs) > 0 {
			cf := conflict{name: name, subs: subs, f1: []*language.Field{e1.node}, f2: []*language.Field{e2.node}}
			for _, s := range subs {
				cf.f1 = append(cf.f1, s.f1...)
				cf.f2 = append(cf.f2, s.f2...)
			}
			return cf, true
		}
	}
	return conflict{}, false
}

func sameArguments(a, b []*language.Argument) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		found := false
		for _, y := range b {
			if x.Name == y.Name {
				found = language.Print(x.Value) == language.Print(y.Value)
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// typesConflict reports whether two fields' types give values of different
// shapes: one a list and not the other, one non-null and not the other, or
// different leaf types.
func typesConflict(t1, t2 TypeDef) bool {
	if l1, ok := t1.(*List); ok {
		l2, ok := t2.(*List)
		return !ok || typesConflict(l1.Of, l2.Of)
	}
	if _, ok := t2.(*List); ok {
		return true
	}
	if n1, ok := t1.(*NonNull); ok {
		n2, ok := t2.(*NonNull)
		return !ok || typesConflict(n1.Of, n2.Of)
	}
	if _, ok := t2.(*NonNull); ok {
		return true
	}
	if isLeaf(t1) || isLeaf(t2) {
		return !sameType(t1, t2)
	}
	return false
}
