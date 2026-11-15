package viewtest

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Timeout bounds every wait: an Expect's, a Wait's, a reply's.
var Timeout = 5 * time.Second

// surface is what Expect reads: a page over its connection, or in Chrome.
type surface interface {
	tb() testing.TB
	// look is what the page shows of the element loc names now.
	look(loc string, kind int) look
	// until waits for ok, re-checking it as the page changes, and fails
	// the test with what and last() once Timeout passes.
	until(what string, ok func() bool, last func() string)
}

// look is an element as a page shows it.
type look struct {
	Found   bool
	Text    string // its text, white space squashed
	Value   string // a field's value
	Visible bool
	Markup  string // its outer HTML, for a failure's message
}

// An Expectation is an element to check, named by a locator. Each check
// waits until it holds — the page's patches arriving, its deferred groups
// loading — failing the test with what the element showed last once
// Timeout passes.
type Expectation struct {
	s   surface
	loc string
}

func (e *Expectation) check(kind int, what string, ok func(look) bool) {
	e.s.tb().Helper()
	var l look
	e.s.until(fmt.Sprintf("%s %s", e.loc, what), func() bool {
		l = e.s.look(e.loc, kind)
		return ok(l)
	}, func() string {
		if !l.Found {
			return "no element matches " + quote(e.loc)
		}
		return fmt.Sprintf("it shows text %q, value %q, visible %v:\n%s", l.Text, l.Value, l.Visible, clip(l.Markup))
	})
}

// Text waits for the element's text — white space squashed, trimmed — to
// be want.
func (e *Expectation) Text(want string) {
	e.s.tb().Helper()
	e.check(anyKind, "to have text "+quote(want), func(l look) bool { return l.Found && l.Text == squash(want) })
}

// Contains waits for the element's text to contain sub.
func (e *Expectation) Contains(sub string) {
	e.s.tb().Helper()
	e.check(anyKind, "to contain "+quote(sub), func(l look) bool { return l.Found && strings.Contains(l.Text, squash(sub)) })
}

// Value waits for a field's value to be want: an input's, a textarea's,
// a select's chosen option's; a checkbox's or radio button's is its value
// ("on" when it has none) when checked, "" when not.
func (e *Expectation) Value(want string) {
	e.s.tb().Helper()
	e.check(fieldKind, "to have value "+quote(want), func(l look) bool { return l.Found && l.Value == want })
}

// Visible waits for the element to be on the page and shown. Over a
// connection shown means no ancestor hides it (hidden, display:none
// inline, a closed dialog); in Chrome it is what the browser lays out.
func (e *Expectation) Visible() {
	e.s.tb().Helper()
	e.check(anyKind, "to be visible", func(l look) bool { return l.Found && l.Visible })
}

// Absent waits for no element to match the locator.
func (e *Expectation) Absent() {
	e.s.tb().Helper()
	e.check(anyKind, "to be absent", func(l look) bool { return !l.Found })
}

func quote(s string) string { return fmt.Sprintf("%q", s) }

func clip(s string) string {
	if len(s) > 2000 {
		return s[:2000] + "…"
	}
	return s
}
