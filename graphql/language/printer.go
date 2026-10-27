package language

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Print writes a node as GraphQL: a document, a definition, a selection, a
// value or a type, two spaces an indentation level.
func Print(n Node) string {
	var p printer
	p.node(n)
	return p.b.String()
}

type printer struct {
	b      strings.Builder
	indent int
}

func (p *printer) w(s string) { p.b.WriteString(s) }

func (p *printer) nl() {
	p.b.WriteByte('\n')
	p.b.WriteString(strings.Repeat("  ", p.indent))
}

func (p *printer) node(n Node) {
	switch n := n.(type) {
	case *Document:
		for i, d := range n.Definitions {
			if i > 0 {
				p.w("\n\n")
			}
			p.node(d)
		}
	case *OperationDefinition:
		if n.Shorthand && n.Name == "" && len(n.VariableDefinitions) == 0 && len(n.Directives) == 0 {
			p.selectionSet(n.SelectionSet)
			return
		}
		p.w(n.Operation)
		if n.Name != "" {
			p.w(" " + n.Name)
		}
		if len(n.VariableDefinitions) > 0 {
			p.w("(")
			for i, v := range n.VariableDefinitions {
				if i > 0 {
					p.w(", ")
				}
				p.w("$" + v.Variable + ": " + v.Type.String())
				if v.DefaultValue != nil {
					p.w(" = ")
					p.node(v.DefaultValue)
				}
				p.directives(v.Directives)
			}
			p.w(")")
		}
		p.directives(n.Directives)
		p.w(" ")
		p.selectionSet(n.SelectionSet)
	case *FragmentDefinition:
		p.w("fragment " + n.Name + " on " + n.TypeCondition.Name)
		p.directives(n.Directives)
		p.w(" ")
		p.selectionSet(n.SelectionSet)
	case *SelectionSet:
		p.selectionSet(n)
	case *Field:
		if n.Alias != "" {
			p.w(n.Alias + ": ")
		}
		p.w(n.Name)
		p.args(n.Arguments)
		p.directives(n.Directives)
		if n.SelectionSet != nil {
			p.w(" ")
			p.selectionSet(n.SelectionSet)
		}
	case *FragmentSpread:
		p.w("..." + n.Name)
		p.directives(n.Directives)
	case *InlineFragment:
		p.w("...")
		if n.TypeCondition != nil {
			p.w(" on " + n.TypeCondition.Name)
		}
		p.directives(n.Directives)
		p.w(" ")
		p.selectionSet(n.SelectionSet)
	case *Directive:
		p.w("@" + n.Name)
		p.args(n.Arguments)
	case *Argument:
		p.w(n.Name + ": ")
		p.node(n.Value)
	case *Variable:
		p.w("$" + n.Name)
	case *IntValue:
		p.w(n.Value)
	case *FloatValue:
		p.w(n.Value)
	case *StringValue:
		if n.Block {
			p.w(PrintBlockString(n.Value, strings.Repeat("  ", p.indent)))
		} else {
			p.w(PrintString(n.Value))
		}
	case *BooleanValue:
		if n.Value {
			p.w("true")
		} else {
			p.w("false")
		}
	case *NullValue:
		p.w("null")
	case *EnumValue:
		p.w(n.Value)
	case *ListValue:
		p.w("[")
		for i, v := range n.Values {
			if i > 0 {
				p.w(", ")
			}
			p.node(v)
		}
		p.w("]")
	case *ObjectValue:
		p.w("{")
		for i, f := range n.Fields {
			if i > 0 {
				p.w(", ")
			}
			p.w(f.Name + ": ")
			p.node(f.Value)
		}
		p.w("}")
	case TypeRef:
		p.w(n.String())
	default:
		p.w(fmt.Sprintf("<%T>", n))
	}
}

func (p *printer) selectionSet(s *SelectionSet) {
	p.w("{")
	p.indent++
	for _, sel := range s.Selections {
		p.nl()
		p.node(sel)
	}
	p.indent--
	p.nl()
	p.w("}")
}

func (p *printer) args(args []*Argument) {
	if len(args) == 0 {
		return
	}
	p.w("(")
	for i, a := range args {
		if i > 0 {
			p.w(", ")
		}
		p.node(a)
	}
	p.w(")")
}

func (p *printer) directives(ds []*Directive) {
	for _, d := range ds {
		p.w(" ")
		p.node(d)
	}
}

// PrintString writes s as a GraphQL string literal, quoted and escaped.
func PrintString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f || r == utf8.RuneError {
				fmt.Fprintf(&b, `\u%04X`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// PrintBlockString writes s as a block string ("""…"""), its lines after
// the first indented with indent; a string a block string can't hold as
// it is (one with leading spaces on its first line, or ending in a quote)
// is written so it reads back the same.
func PrintBlockString(s, indent string) string {
	escaped := strings.ReplaceAll(s, `"""`, `\"""`)
	lines := strings.Split(escaped, "\n")
	multi := len(lines) > 1
	firstIndented := len(s) > 0 && (s[0] == ' ' || s[0] == '\t')
	endsQuote := strings.HasSuffix(s, `"`) && !strings.HasSuffix(s, `\"""`)
	endsSlash := strings.HasSuffix(s, `\`)
	var b strings.Builder
	b.WriteString(`"""`)
	newlineFirst := multi && !firstIndented
	if newlineFirst {
		b.WriteString("\n" + indent)
	}
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\n")
			if l != "" {
				b.WriteString(indent)
			}
		}
		b.WriteString(l)
	}
	if multi || endsQuote || endsSlash {
		b.WriteString("\n" + indent)
	}
	b.WriteString(`"""`)
	return b.String()
}
