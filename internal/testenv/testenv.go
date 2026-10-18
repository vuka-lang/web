// Package testenv decides what a test does when a tool it needs is missing.
package testenv

import (
	"fmt"
	"os"
	"testing"
)

// Needs skips a test whose tool (node, Chrome, Tailwind) is missing. Under
// CI (CI set) it fails instead: there a missing tool is a broken setup,
// never a pass.
func Needs(t testing.TB, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("CI") != "" {
		t.Fatalf("%s (CI is set, so this fails instead of skipping)", msg)
	}
	t.Skip(msg)
}
