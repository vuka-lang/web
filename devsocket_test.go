package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// devDial connects to the app's development socket; next is its next message.
func devDial(t *testing.T, srv *httptest.Server) (next func() (channel, value string, ok bool)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+devSocketPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return func() (string, string, bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, b, err := c.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatal("no message")
			}
			return "", "", false
		}
		var m struct{ C, V string }
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m.C, m.V, true
	}
}

// A development page hears of each new stylesheet over the socket, which Stop
// ends.
func TestDevSocketCSS(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.css")
	os.WriteFile(file, []byte(".one{}"), 0o644)
	t.Setenv(CSSEnv, file)
	app := cssApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	next := devDial(t, srv)
	if c, v, _ := next(); c != "css" || v != cssTag([]byte(".one{}")) {
		t.Errorf("first message %s %q", c, v)
	}
	os.WriteFile(file, []byte(".two{}"), 0o644)
	if c, v, _ := next(); c != "css" || v != cssTag([]byte(".two{}")) {
		t.Errorf("after a rebuild %s %q", c, v)
	}
	app.Stop(context.Background())
	if _, _, ok := next(); ok {
		t.Error("the socket outlived Stop")
	}
}

// A shutdown of the server ends the connections: they're hijacked, so it
// wouldn't by itself.
func TestDevSocketShutdown(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.css")
	os.WriteFile(file, []byte(".one{}"), 0o644)
	t.Setenv(CSSEnv, file)
	srv := httptest.NewUnstartedServer(cssApp().Handler())
	srv.Start()
	defer srv.Close()
	next := devDial(t, srv)
	next()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Config.Shutdown(ctx)
	if _, _, ok := next(); ok {
		t.Error("the socket outlived Shutdown")
	}
}

// A page hears of each build state over the socket.
func TestDevSocketBuild(t *testing.T) {
	withToolbar(t)
	dir := t.TempDir()
	t.Setenv(DevDirEnv, dir)
	write := func(s string) { os.WriteFile(filepath.Join(dir, "build.json"), []byte(s), 0o644) }
	write(`{"state":"ok","took":1.5}`)
	srv := httptest.NewServer(toolbarApp().Handler())
	defer srv.Close()
	next := devDial(t, srv)
	state := func() buildState {
		t.Helper()
		c, v, ok := next()
		if c != "build" || !ok {
			t.Fatalf("%q %q %v", c, v, ok)
		}
		var st buildState
		json.Unmarshal([]byte(v), &st)
		return st
	}
	if st := state(); st.State != "ok" || st.Took != 1.5 {
		t.Errorf("on connect: %+v", st)
	}
	write(`{"state":"building","since":"2026-10-31T10:00:00Z"}`)
	if st := state(); st.State != "building" || st.Since.IsZero() || st.Elapsed != 0 {
		t.Errorf("building: %+v", st)
	}
	write(`{"state":"failed","output":"./main.vuka:3:1: undefined: x\n"}`)
	if st := state(); st.State != "failed" || !strings.Contains(st.Output, "undefined: x") {
		t.Errorf("failed: %+v", st)
	}
}

// A new manifest reaches a page, as the loader reads it.
func TestDevSocketAssets(t *testing.T) {
	dir := devAssets(t)
	srv := httptest.NewServer(scopedApp().Handler())
	defer srv.Close()
	next := devDial(t, srv)
	assets := func() string {
		t.Helper()
		for {
			c, v, ok := next()
			if !ok {
				t.Fatal("the socket closed")
			}
			if c == "assets" {
				return v
			}
		}
	}
	if v := assets(); !strings.Contains(v, "k1.0a1b2c3d.js") || !strings.Contains(v, `"e":true`) {
		t.Errorf("on connect %s", v)
	}
	manifest := filepath.Join(dir, "manifest.json")
	old, _ := os.ReadFile(manifest)
	os.WriteFile(manifest, []byte(strings.Replace(string(old), "k1.0a1b2c3d.js", "k1.0a1b2c3e.js", 1)), 0o644)
	if v := assets(); !strings.Contains(v, "k1.0a1b2c3e.js") {
		t.Errorf("after a build %s", v)
	}
}

// The socket refuses a page of another origin: a WebSocket ignores CORS, and
// LiveOrigins, for live connections, says nothing of this one.
func TestDevSocketOrigin(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.css")
	os.WriteFile(file, []byte(".one{}"), 0o644)
	t.Setenv(CSSEnv, file)
	srv := httptest.NewServer(cssApp(LiveOrigins("elsewhere.example")).Handler())
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + devSocketPath
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, origin := range []string{"https://evil.example", "https://elsewhere.example", "null"} {
		_, res, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}})
		if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %s: %v %v", origin, res, err)
		}
	}
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {srv.URL}}})
	if err != nil {
		t.Fatalf("its own origin: %v", err)
	}
	c.CloseNow()
}

// Nothing of the tooling holds an HTTP stream: the old endpoints answer as
// anything else does, and the build's state stays a plain answer.
func TestDevNoStreams(t *testing.T) {
	withToolbar(t)
	devAssets(t)
	file := filepath.Join(t.TempDir(), "app.css")
	os.WriteFile(file, []byte(".one{}"), 0o644)
	t.Setenv(CSSEnv, file)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "build.json"), []byte(`{"state":"ok","took":1.5}`), 0o644)
	t.Setenv(DevDirEnv, dir)
	srv := httptest.NewServer(scopedApp().Handler())
	defer srv.Close()
	get := func(path string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Accept", "text/event-stream")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	for _, path := range []string{"/_vuka/c/events", "/_vuka/app-css"} {
		if res, _ := get(path); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	res, body := get(toolbarBuild)
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") || res.Header.Get("Cache-Control") != "no-store" || !strings.Contains(body, `"state":"ok"`) {
		t.Errorf("the build's state: %q %q %s", ct, res.Header.Get("Cache-Control"), body)
	}
	for _, path := range []string{loaderPath, toolbarJS, cssWatch} {
		_, js := get(path)
		if strings.Contains(js, "new EventSource") || !strings.Contains(js, "__vukaDev.on(") || !strings.Contains(js, `window.__vukaDev || (`) {
			t.Errorf("%s: %.200s", path, js)
		}
	}
}

// A production app has no socket and no script of it.
func TestDevSocketNotInProduction(t *testing.T) {
	RegisterAssets([]byte(`{"scopes":{"k1":{"name":"c","module":"k1.12345678.js"}}}`))
	RegisterScript("k1.12345678.js", []byte("export default [];"))
	t.Cleanup(func() { RegisterAssets(nil); delete(scriptReg, "k1.12345678.js") })
	srv := httptest.NewServer(scopedApp().Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + devSocketPath)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("the socket's path: %d", res.StatusCode)
	}
	res, err = http.Get(srv.URL + loaderPath)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(b), "__vukaDev || (") {
		t.Error("the loader carries the development socket")
	}
}
