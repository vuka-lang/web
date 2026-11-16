package web

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
)

func TestIsAppFrame(t *testing.T) {
	for _, c := range []struct {
		name, function, file string
		module               string // appModule for the case; "" keeps the test binary's
		want                 bool
	}{
		{"stdlib", "net/http.(*conn).serve", "/usr/local/go/src/net/http/server.go", "", false},
		{"runtime", "runtime.gopanic", "/usr/local/go/src/runtime/panic.go", "", false},
		{"reflect", "reflect.Value.Call", "/usr/local/go/src/reflect/value.go", "", false},
		{"web", "github.com/vuka-lang/web/v2.(*App).respond", "/src/web/bind.go", "", false},
		{"ui", "github.com/vuka-lang/ui/v2.Must", "/src/ui/must.go", "", false},
		{"package main", "main.ShowOrders", "/app/orders.vuka", "", true},
		{"a test file plays the app", "github.com/vuka-lang/web/v2.TestX.func1", "/src/web/web_test.go", "", true},
		{"the main module", "myapp/modules/orders.List", "/app/modules/orders/store.go", "myapp", true},
		{"a dependency", "github.com/go-sql-driver/mysql.(*mysqlConn).readPacket", "/go/pkg/mysql/packets.go", "myapp", false},
		{"no build info, domain path", "example.com/shop/store.List", "/app/store.go", "command-line-arguments", true},
		{"no build info, stdlib", "encoding/json.Marshal", "/usr/local/go/src/encoding/json/encode.go", "command-line-arguments", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.module != "" {
				prev := appModule
				appModule = c.module
				defer func() { appModule = prev }()
			}
			if got := isAppFrame(c.function, c.file); got != c.want {
				t.Errorf("isAppFrame(%q, %q) = %v; want %v", c.function, c.file, got, c.want)
			}
		})
	}
}

func TestAppFrame(t *testing.T) {
	got := appFrame(0) // this test file is an app frame (its stack above is the framework's)
	if !strings.HasPrefix(got, "errsource_test.go:") {
		t.Errorf("appFrame = %q; want a frame in errsource_test.go", got)
	}
}

// capture runs f with the log collected, and returns what it wrote.
func capture(t *testing.T, f func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	f()
	return buf.String()
}

func TestDev500Log(t *testing.T) {
	p := pkgOf(t)
	route(Get("/boom"), decl(p, "boom", func() (int, error) { return 0, errors.New("kaboom") }))
	route(Get("/deep"), decl(p, "deep", func() int { return ui.Must(0, errors.New("table missing")) }))
	dev := New(Only(p), Dev()).Handler()

	logs := capture(t, func() { expect(t, dev, "GET", "/boom?page=2", "", 500, "kaboom") })
	for _, want := range []string{"web: 500 GET /boom?page=2: kaboom", "| handler main.boom at t.vuka:"} {
		if !strings.Contains(logs, want) {
			t.Errorf("dev 500 log %q; want it to contain %q", logs, want)
		}
	}
	if strings.Contains(logs, "| at ") {
		t.Errorf("dev 500 log %q names a frame for a returned error, whose stack is gone", logs)
	}

	// A ui.Must failure panics at the error's site, so the log names the
	// app frame the failing call sits in.
	logs = capture(t, func() { expect(t, dev, "GET", "/deep", "", 500, "table missing") })
	for _, want := range []string{"web: 500 GET /deep: table missing", "| at errsource_test.go:"} {
		if !strings.Contains(logs, want) {
			t.Errorf("dev 500 log %q; want it to contain %q", logs, want)
		}
	}
}

func TestProduction500LogUnchanged(t *testing.T) {
	for _, k := range []string{"VUKA_ENV", "VUKA_DEV", DevDirEnv} {
		t.Setenv(k, "")
	}
	p := pkgOf(t)
	route(Get("/boom"), decl(p, "boom", func() (int, error) { return 0, errors.New("kaboom") }))
	h := New(Only(p)).Handler()
	logs := capture(t, func() { expect(t, h, "GET", "/boom?page=2", "", 500, "Internal Server Error") })
	if !strings.Contains(logs, "web: 500 GET /boom: kaboom\n") {
		t.Errorf("production 500 log %q; want the plain line", logs)
	}
	for _, not := range []string{"handler", "| at", "page=2"} {
		if strings.Contains(logs, not) {
			t.Errorf("production 500 log %q; must not contain %q", logs, not)
		}
	}
}
