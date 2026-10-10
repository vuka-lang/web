package web

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type Third struct{ *Life }
type Worker struct{ *Life }
type Unused struct{ *Life }

// lib is an option a library defines: several things at once, through the
// App's methods.
func lib(log *[]string, fail error) Option {
	return Options(
		Provide(&Third{&Life{"supplied", log}}),
		OptionFunc(func(a *App) {
			a.OnStart(func(context.Context) error { *log = append(*log, "hook start"); return nil })
			a.OnStop(func(context.Context) error { *log = append(*log, "hook stop"); return nil })
			a.Get("/lib", func(t *Third) string { return "lib " + t.name })
			a.Use(func(h http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-Lib", "1")
					h.ServeHTTP(w, r)
				})
			})
		}),
		nil,
		Check(func(*App) error { return fail }),
	)
}

func TestLibraryOption(t *testing.T) {
	var log []string
	a := New(Only("none"), lib(&log, nil))
	_, body, h := do(t, a.Handler(), "GET", "/lib", "")
	if body != `"lib supplied"`+"\n" || h.Get("X-Lib") != "1" {
		t.Errorf("GET /lib: %q %v", body, h)
	}
	err := New(Only("none"), lib(&log, errors.New("lib: bad config")), Provide(func(*DB) *Repo { return nil }), Eager(func(*Repo) *Unused { return nil })).Build()
	if err == nil || !strings.Contains(err.Error(), "lib: bad config") || !strings.Contains(err.Error(), "no provider for *web.DB") {
		t.Errorf("Build: %v", err)
	}
}

func TestLifecycleOrder(t *testing.T) {
	p := pkgOf(t)
	var log []string
	Get("/")(decl(p, "home", func(s *Second) string { return s.name }, "s"))
	a := New(Only(p), lib(&log, nil),
		Provide(
			func(f *First) *Second { return &Second{&Life{"second", &log}} },
			func() *First { return &First{&Life{"first", &log}} },
			func() *Unused { return &Unused{&Life{"unused", &log}} },
		),
		Eager(func(f *First) *Worker { return &Worker{&Life{"worker", &log}} }),
		OnStart(func(context.Context) error { log = append(log, "late hook"); return nil }),
	)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "start supplied, hook start, late hook, start first, start second, start worker, " +
		"stop worker, stop second, stop first, hook stop, stop supplied"
	if got := strings.Join(log, ", "); got != want {
		t.Errorf("lifecycle:\n got %s\nwant %s", got, want)
	}

	failing := New(Only("none"), OnStart(func(context.Context) error { return errors.New("no network") }))
	if err := failing.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "no network") {
		t.Errorf("failing hook: %v", err)
	}
}

func TestHandlerWritesItself(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	a := New(Only("none"))
	a.Get("/raw", func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("raw"))
	})
	a.Get("/bare", func(w http.ResponseWriter) { w.Write([]byte("bare")) })
	a.Get("/none", func() {})
	a.Get("/late", func(w http.ResponseWriter) error { w.Write([]byte("partial")); return errors.New("late") })
	srv := a.Handler()
	expect(t, srv, "GET", "/raw", "", http.StatusAccepted, "raw")
	expect(t, srv, "GET", "/bare", "", http.StatusOK, "bare")
	expect(t, srv, "GET", "/none", "", http.StatusNoContent, "")
	if code, body, _ := do(t, srv, "GET", "/late", ""); code != 200 || body != "partial" {
		t.Errorf("late: %d %q", code, body)
	}
	if !strings.Contains(buf.String(), "late (after the handler wrote the response)") {
		t.Errorf("log: %q", buf.String())
	}
}

func TestNoSuperfluousWriteHeader(t *testing.T) {
	var buf bytes.Buffer
	a := New(Only("none"))
	a.Get("/raw", func(w http.ResponseWriter) { w.WriteHeader(http.StatusCreated) })
	srv := httptest.NewUnstartedServer(a.Handler())
	srv.Config.ErrorLog = log.New(&buf, "", 0)
	srv.Start()
	defer srv.Close()
	res, err := http.Get(srv.URL + "/raw")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated || strings.Contains(buf.String(), "superfluous") {
		t.Errorf("%d, log %q", res.StatusCode, buf.String())
	}
}
