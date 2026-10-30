package web

import (
	"os"
	"testing"
)

// bootEnv stands for the start's reading of the default files, which
// init did before the test moved into its sandbox.
func bootEnv(t *testing.T) {
	t.Helper()
	l := loadEnv(nil)
	envState.Lock()
	envState.boot = l
	envState.Unlock()
	t.Cleanup(func() {
		envState.Lock()
		envState.boot = nil
		envState.Unlock()
	})
}

func TestEnvReadAtStart(t *testing.T) {
	envSandbox(t, map[string]string{".env": "SHOP_KEY=from-file\n"})
	bootEnv(t)
	// What an option built before New reads.
	if v := os.Getenv("SHOP_KEY"); v != "from-file" {
		t.Fatalf("before New: %q", v)
	}
	New(Only("none"))
	if err := LoadEnv(); err != nil || os.Getenv("SHOP_KEY") != "from-file" {
		t.Errorf("New and LoadEnv() after the start's reading: %v %q", err, os.Getenv("SHOP_KEY"))
	}
}

func TestEnvOtherFilesUndoTheStart(t *testing.T) {
	envSandbox(t, map[string]string{".env": "SHOP_KEY=from-file\nSHOP_ONLY=1\n", "prod.env": "SHOP_KEY=prod\n"})
	bootEnv(t)
	t.Setenv("SHOP_REAL", "real")
	New(Only("none"), DotEnv("prod.env"))
	if os.Getenv("SHOP_KEY") != "prod" || os.Getenv("SHOP_ONLY") != "" {
		t.Errorf("DotEnv: SHOP_KEY=%q SHOP_ONLY=%q", os.Getenv("SHOP_KEY"), os.Getenv("SHOP_ONLY"))
	}
	if os.Getenv("SHOP_REAL") != "real" {
		t.Error("the real environment was touched")
	}
}

func TestEnvNoDotEnvUndoesTheStart(t *testing.T) {
	envSandbox(t, map[string]string{".env": "SHOP_KEY=from-file\n"})
	bootEnv(t)
	os.Setenv("SHOP_KEY", "changed") // set since: kept
	New(Only("none"), NoDotEnv())
	if os.Getenv("SHOP_KEY") != "changed" {
		t.Errorf("a value changed since the start: %q", os.Getenv("SHOP_KEY"))
	}
}

func TestEnvNoDotEnvUndoesTheStartsValues(t *testing.T) {
	envSandbox(t, map[string]string{".env": "SHOP_KEY=from-file\n"})
	bootEnv(t)
	New(Only("none"), NoDotEnv())
	if v, ok := os.LookupEnv("SHOP_KEY"); ok {
		t.Errorf("NoDotEnv kept the start's %q", v)
	}
}
