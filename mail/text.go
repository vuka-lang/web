package mail

import (
	"html"
	"regexp"
	"strings"
)

var (
	// What a reader of the text part doesn't see: comments, and the
	// contents of head, script and style.
	hidden = regexp.MustCompile(`(?is)<!--.*?-->|<(head|script|style|title)\b[^>]*>.*?</(head|script|style|title)\s*>`)
	tagRe  = regexp.MustCompile(`(?s)<(/?)([a-zA-Z][a-zA-Z0-9]*)\b((?:[^>"']|"[^"]*"|'[^']*')*)>`)
	hrefRe = regexp.MustCompile(`(?i)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	spaces = regexp.MustCompile(`[ \t\f\r]+`)
	blanks = regexp.MustCompile(`\n{3,}`)
)

var blocks = map[string]bool{"p": true, "div": true, "section": true, "article": true, "header": true, "footer": true, "main": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "ul": true, "ol": true, "table": true, "tr": true,
	"blockquote": true, "pre": true, "hr": true, "address": true, "figure": true, "dl": true, "dt": true, "dd": true}

// htmlToText is the text part of a message whose text wasn't given: its
// HTML's text, paragraphs and list items on lines of their own, a link
// followed by its address.
func htmlToText(s string) string {
	s = hidden.ReplaceAllString(s, "")
	var b strings.Builder
	type link struct {
		href  string
		start int // where its text starts in b
	}
	var open []link
	last := 0
	for _, m := range tagRe.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(text(s[last:m[0]]))
		last = m[1]
		closing, name, attrs := s[m[2]:m[3]] == "/", strings.ToLower(s[m[4]:m[5]]), s[m[6]:m[7]]
		switch {
		case name == "br":
			b.WriteString("\n")
		case name == "li" && !closing:
			b.WriteString("\n- ")
		case name == "td" || name == "th":
			if closing {
				b.WriteString("  ")
			}
		case name == "a" && !closing:
			href := ""
			if h := hrefRe.FindStringSubmatch(attrs); h != nil {
				href = html.UnescapeString(h[1] + h[2] + h[3])
			}
			open = append(open, link{href, b.Len()})
		case name == "a" && len(open) > 0:
			l := open[len(open)-1]
			open = open[:len(open)-1]
			label := strings.TrimSpace(b.String()[l.start:])
			if (strings.HasPrefix(l.href, "http:") || strings.HasPrefix(l.href, "https:")) && label != l.href {
				b.WriteString(" (" + l.href + ")")
			}
		case blocks[name]:
			b.WriteString("\n\n")
		}
	}
	b.WriteString(text(s[last:]))
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(spaces.ReplaceAllString(l, " "))
	}
	return strings.TrimSpace(blanks.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// text is the text between tags: entities decoded, line breaks spaces (as
// HTML shows them).
func text(s string) string {
	return html.UnescapeString(strings.NewReplacer("\r\n", " ", "\n", " ").Replace(s))
}
