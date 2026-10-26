package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/vuka-lang/vuka/v2/diag"
	"github.com/vuka-lang/vuka/v2/docs"
)

// TestDiagnosticCodes: every code web's checks report has a page in vuka's
// catalogue, which `vuka explain` prints.
func TestDiagnosticCodes(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	more, _ := filepath.Glob("graphql/*.go")
	files = append(files, more...)
	codes := regexp.MustCompile(`"([EW]\d{4})"`)
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range codes.FindAllSubmatch(b, -1) {
			n++
			if _, ok := docs.Lookup(string(m[1])); !ok {
				t.Errorf("%s: %s has no page in vuka's docs/errors", f, m[1])
			}
		}
	}
	if n == 0 {
		t.Fatal("no codes found")
	}
}

// TestBootDiagnostics: a boot error names its code, where the declaration
// is, the other declaration it clashes with and what to do.
func TestBootDiagnostics(t *testing.T) {
	p := pkgOf(t)
	first := decl(p, "listPets", func() int { return 0 })
	second := decl(p, "adminPets", func() int { return 0 })
	route(Get("/pets"), first)
	route(Get("/pets"), second)
	err := New(Only(p)).Build()
	if diag.Of(err) != "E0401" {
		t.Fatalf("code %q: %v", diag.Of(err), err)
	}
	want := "error[E0401]: GET /pets is declared twice: by main.listPets and by main.adminPets\n" +
		" --> t.vuka:" + itoa(second.Line) + "\n" +
		" ::: t.vuka:" + itoa(first.Line) + ": declared here first\n" +
		"  = help: give one of them another path or method\n" +
		"  = note: vuka explain E0401 (" + diag.DocsURL + "E0401)"
	if err.Error() != want {
		t.Errorf("got:\n%v\nwant:\n%s", err, want)
	}
	ds, rest := diag.From(err)
	if len(ds) != 1 || len(rest) != 0 || ds[0].Span.File != "t.vuka" || ds[0].Related[0].Line != first.Line {
		t.Errorf("From = %+v, %v", ds, rest)
	}
}
