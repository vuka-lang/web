package web

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/vuka-lang/vuka/v2/diag"
)

// whereSpan is the place a "Name at file:line" description names.
func whereSpan(where string) diag.Span {
	_, loc, ok := strings.Cut(where, " at ")
	i := strings.LastIndexByte(loc, ':')
	if !ok || i < 0 {
		return diag.Span{}
	}
	n, err := strconv.Atoi(loc[i+1:])
	if err != nil {
		return diag.Span{}
	}
	return diag.Span{File: loc[:i], Line: n}
}

// whereName is the name a "Name at file:line" description gives.
func whereName(where string) string {
	name, _, _ := strings.Cut(where, " at ")
	return name
}

// failure is a boot error about what, declared at where: err's own code when
// it is a diagnostic, else code.
func failure(code diag.Code, what, where string, err error) *diag.Diagnostic {
	var d *diag.Diagnostic
	if errors.As(err, &d) {
		c := *d
		c.Message = what + ": " + c.Message
		if c.Span.File == "" {
			c.Span = whereSpan(where)
		}
		return &c
	}
	return diag.New(code, "%s: %v", what, err).At(whereSpan(where))
}

// failf is a boot error about what, declared at where.
func failf(code diag.Code, what, where, format string, args ...any) *diag.Diagnostic {
	return diag.New(code, "%s: %s", what, fmt.Sprintf(format, args...)).At(whereSpan(where))
}

func withLabel(s diag.Span, label string) diag.Span {
	s.Label = label
	return s
}
