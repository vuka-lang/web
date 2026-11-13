package web

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveMinFresh: live.min.js is what internal/minjs makes of live.js
// (go generate rewrites it).
func TestLiveMinFresh(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command")
	}
	out := filepath.Join(t.TempDir(), "live.min.js")
	src, _ := filepath.Abs("live.js")
	cmd := exec.Command(gobin, "run", ".", src, out)
	cmd.Dir = filepath.Join("internal", "minjs")
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("minjs: %v\n%s", err, b)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, liveMinJS) {
		t.Fatal("live.min.js is stale: run go generate")
	}
}

// TestRuntimeServed: development serves the runtime as written, the build
// its minified copy, each under its own tag.
func TestRuntimeServed(t *testing.T) {
	for _, dev := range []bool{false, true} {
		var opts []Option
		if dev {
			opts = append(opts, Dev(), NoToolbar())
		}
		a := New(append(opts, Only("none"))...)
		a.Live("/p", func() *scopedPage { return newScopedPage(&scopedLog{}) })
		h := a.Handler()
		want, tag := liveMinJS, liveMinJSTag
		if dev {
			want, tag = liveJS, liveJSTag
		}
		page := httptest.NewRecorder()
		h.ServeHTTP(page, httptest.NewRequest("GET", "/p", nil))
		if !strings.Contains(page.Body.String(), liveScript+"?v="+tag+`"`) {
			t.Fatalf("dev=%v: the page doesn't load the runtime tagged %s", dev, tag)
		}
		js := httptest.NewRecorder()
		h.ServeHTTP(js, httptest.NewRequest("GET", liveScript+"?v="+tag, nil))
		if !bytes.Equal(js.Body.Bytes(), want) {
			t.Fatalf("dev=%v: live.js served differs", dev)
		}
	}
}
