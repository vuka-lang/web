package graphql

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Naming is an app's convention for the names its schema takes from Go,
// given to web.GraphQL: Fields names every field and argument whose name
// isn't written out (a graphql:"name" tag, @graphql.Field, @graphql.Arg),
// from its Go name. The default is CamelCase.
//
//	web.GraphQL("/graphql", graphql.Naming{Fields: graphql.SnakeCase})
type Naming struct {
	Fields func(goName string) string
}

// CamelCase is a Go name in lower camel case, as fields are named by
// default: CreatedAt is createdAt, ID is id, URLPath is urlPath, UserID is
// userID.
func CamelCase(s string) string {
	r := []rune(s)
	i := 0
	for i < len(r) && unicode.IsUpper(r[i]) {
		i++
	}
	switch {
	case i == 0:
		return s
	case i == len(r):
		return strings.ToLower(s)
	case i > 1:
		i-- // the last capital starts the next word: URLPath is urlPath
	}
	for j := range i {
		r[j] = unicode.ToLower(r[j])
	}
	return string(r)
}

// SnakeCase is a Go name in snake case: CreatedAt is created_at, UserID is
// user_id, URLPath is url_path.
func SnakeCase(s string) string { return joinWords(words(s), "_", unicode.ToLower) }

// screamingSnake is an enum value's default name: ACTIVE, PAST_DUE.
func screamingSnake(s string) string { return joinWords(words(s), "_", unicode.ToUpper) }

func joinWords(ws []string, sep string, conv func(rune) rune) string {
	for i, w := range ws {
		ws[i] = strings.Map(conv, w)
	}
	return strings.Join(ws, sep)
}

// words splits a Go name at its case changes, digits staying with what
// precedes them: CreatedAt is Created At, URLPath URL Path, Line2 Line2.
func words(s string) []string {
	var out []string
	r := []rune(s)
	start := 0
	for i := 1; i < len(r); i++ {
		prev, cur := r[i-1], r[i]
		next := rune(0)
		if i+1 < len(r) {
			next = r[i+1]
		}
		switch {
		case cur == '_':
			if i > start {
				out = append(out, string(r[start:i]))
			}
			start = i + 1
		case unicode.IsUpper(cur) && (unicode.IsLower(prev) || unicode.IsDigit(prev)),
			unicode.IsUpper(cur) && unicode.IsUpper(prev) && unicode.IsLower(next):
			if i > start {
				out = append(out, string(r[start:i]))
			}
			start = i
		}
	}
	if start < len(r) {
		out = append(out, string(r[start:]))
	}
	return out
}

// lowerFirst is s with its first letter lowered: an operation's default
// name, CreateUser is createUser.
func lowerFirst(s string) string { return CamelCase(s) }

// enumValueName is the default name of the constant goName of the enum
// type typeName: StatusActive of Status is ACTIVE; a constant not starting
// with the type's name keeps its whole name, Paid is PAID.
func enumValueName(goName, typeName string) string {
	rest := goName
	if after, ok := strings.CutPrefix(goName, typeName); ok && after != "" {
		if c, _ := utf8.DecodeRuneInString(after); unicode.IsUpper(c) || unicode.IsDigit(c) || c == '_' {
			rest = strings.TrimPrefix(after, "_")
		}
	}
	return screamingSnake(rest)
}
