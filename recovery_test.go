package web

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui"
)

func TestRecoveryMiddleware(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	a := New(Only(pkgOf(t)))
	a.Get("/ok", func() string { return "ok" })
	a.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/boom" {
				w.Header().Set("Content-Length", "9999")
				w.Header().Set("Content-Encoding", "gzip")
				panic("secret-database-password")
			}
			next.ServeHTTP(w, r)
		})
	})
	h := a.Handler()
	for _, accept := range []string{"application/json", "text/html"} {
		code, body, headers := do(t, h, "GET", "/boom", "", "Accept", accept)
		if code != 500 || !strings.Contains(body, "Internal Server Error") || strings.Contains(body, "secret-database-password") {
			t.Fatalf("%d %s", code, body)
		}
		if headers.Get("Content-Length") != "" || headers.Get("Content-Encoding") != "" {
			t.Fatalf("stale headers: %v", headers)
		}
		if headers.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("lost security headers: %v", headers)
		}
		if accept == "text/html" && !strings.Contains(body, "<h1>") {
			t.Fatal(body)
		}
	}
	if !strings.Contains(logs.String(), "secret-database-password") || !strings.Contains(logs.String(), "goroutine") {
		t.Fatalf("missing panic and stack: %s", logs.String())
	}
	expect(t, h, "GET", "/ok", "", 200, "ok")
}

type panicRouter struct{ Router }

func (*panicRouter) ServeHTTP(http.ResponseWriter, *http.Request) { panic("custom router crashed") }

func TestRecoveryCustomRouter(t *testing.T) {
	a := New(Only(pkgOf(t)), WithRouter(&panicRouter{Router: NewServeMux()}))
	expect(t, a.Handler(), "GET", "/anything", "", 500, "Internal Server Error")
}

func TestRecoveryCustomErrorHandler(t *testing.T) {
	for _, broken := range []bool{false, true} {
		calls := 0
		a := New(Only(pkgOf(t)), OnError(func(w http.ResponseWriter, r *http.Request, e *Error, html bool) {
			calls++
			if e.Status != 500 {
				t.Errorf("panic status: %d", e.Status)
			}
			if broken {
				panic("error-page-secret")
			}
			w.WriteHeader(e.Status)
			io.WriteString(w, "custom error page")
		}))
		a.Get("/boom", func() string { panic("handler-secret") })
		code, body, _ := do(t, a.Handler(), "GET", "/boom", "")
		if code != 500 || calls != 1 || strings.Contains(body, "secret") {
			t.Fatalf("broken=%v: %d calls=%d %s", broken, code, calls, body)
		}
		if !broken && body != "custom error page" {
			t.Fatal(body)
		}
	}
}

func TestRecoveryCommittedResponseAborts(t *testing.T) {
	for _, stream := range []bool{false, true} {
		a := New(Only(pkgOf(t)))
		a.Get("/stream", func(w http.ResponseWriter) {
			io.WriteString(w, "partial")
			if stream {
				w.(http.Flusher).Flush()
			}
			panic("partial-response-secret")
		})
		rec := httptest.NewRecorder()
		var got any
		func() {
			defer func() { got = recover() }()
			a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/stream", nil))
		}()
		if got != http.ErrAbortHandler || rec.Body.String() != "partial" {
			t.Fatalf("panic=%v body=%s", got, rec.Body)
		}
	}
}

func TestRecoveryPreservesAbortHandler(t *testing.T) {
	a := New(Only(pkgOf(t)))
	a.Use(func(http.Handler) http.Handler {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	})
	var got any
	func() {
		defer func() { got = recover() }()
		a.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	if got != http.ErrAbortHandler {
		t.Fatalf("got %v", got)
	}
}

type recoveryLivePage struct {
	ui.Live
	crash bool
}

func (p *recoveryLivePage) Crash() { p.crash = true }
func (p *recoveryLivePage) Render() ui.Node {
	if p.crash {
		panic("live-render-secret")
	}
	return ui.El("main", nil, ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(p.Crash)}}, ui.Text("crash")))
}

func TestRecoveryLiveRender(t *testing.T) {
	app := New(Only(pkgOf(t)))
	app.Live("/page", &recoveryLivePage{})
	app.Get("/ok", func() string { return "ok" })
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	ws, html := joined(t, srv, "/page")
	send(t, ws, map[string]any{"type": "event", "ref": 2, "target": handlerOf(t, html, "click", 0), "event": "click"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := ws.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusInternalError || strings.Contains(err.Error(), "live-render-secret") {
		t.Fatalf("close: %v", err)
	}
	expect(t, app.Handler(), "GET", "/ok", "", 200, "ok")
}

type pushRecorder struct {
	*httptest.ResponseRecorder
	pushed string
}

func (w *pushRecorder) Push(target string, _ *http.PushOptions) error { w.pushed = target; return nil }

func TestRecoveryPreservesPush(t *testing.T) {
	a := New(Only(pkgOf(t)))
	a.Get("/push", func(w http.ResponseWriter) {
		if err := w.(http.Pusher).Push("/style.css", nil); err != nil {
			t.Error(err)
		}
		io.WriteString(w, "ok")
	})
	rec := &pushRecorder{ResponseRecorder: httptest.NewRecorder()}
	a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/push", nil))
	if rec.pushed != "/style.css" || rec.Body.String() != "ok" {
		t.Fatalf("push=%s body=%s", rec.pushed, rec.Body)
	}
}

type hijackRecorder struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func TestRecoveryClosesHijackedPanic(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	defer server.Close()
	rec := &hijackRecorder{ResponseRecorder: httptest.NewRecorder(), conn: server}
	a := New(Only(pkgOf(t)))
	a.Get("/raw", func(w http.ResponseWriter) {
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Error(err)
		}
		panic("raw-connection-secret")
	})
	var got any
	func() {
		defer func() { got = recover() }()
		a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/raw", nil))
	}()
	client.SetReadDeadline(time.Now().Add(time.Second))
	_, err := client.Read(make([]byte, 1))
	if got != http.ErrAbortHandler || err != io.EOF {
		t.Fatalf("panic=%v read=%v", got, err)
	}
}
