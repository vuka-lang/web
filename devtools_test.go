package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
)

// devEnv sets the environment Dev looks at (env, pairs of names and values)
// outside vuka run, in a binary that isn't a test's.
func devEnv(t *testing.T, env ...string) {
	t.Helper()
	for _, k := range []string{"VUKA_ENV", "VUKA_DEV", DevDirEnv, CSSEnv} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	for i := 0; i+1 < len(env); i += 2 {
		t.Setenv(env[i], env[i+1])
	}
	testBinary = false
	t.Cleanup(func() { testBinary = true })
}

// devProject makes a module with a Tailwind stylesheet and a Vite frontend
// in a temporary directory, the working directory for the test; with fakes,
// the fake tailwindcss is on PATH and the fake vite is installed.
func devProject(t *testing.T, fakes bool) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":                      "module example.com/devapp\n\ngo 1.25\n",
		"app.css":                     "@import \"tailwindcss\";\n",
		"frontend/package.json":       "{}\n",
		"frontend/vite.config.js":     "import vuka from './vuka/vite.js'\n",
		"frontend/dist/index.html":    "<html><head></head><body>built page</body></html>",
		"frontend/node_modules/.keep": "",
	}
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	if fakes {
		copyExec(t, "testdata/dev/tailwindcss", filepath.Join(bin, "tailwindcss"))
		copyExec(t, "testdata/dev/vite", filepath.Join(dir, "frontend/node_modules/.bin/vite"))
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	} else {
		t.Setenv("PATH", bin)
	}
	t.Chdir(dir)
	return dir
}

func copyExec(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(to), 0o755)
	if err := os.WriteFile(to, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDevGating(t *testing.T) {
	cases := []struct {
		name       string
		env        []string
		opts       []Option
		test       bool
		dev, tools bool
	}{
		{"production", nil, []Option{Dev()}, false, false, false},
		{"VUKA_DEV=1", []string{"VUKA_DEV", "1"}, []Option{Dev()}, false, true, true},
		{"VUKA_ENV=development", []string{"VUKA_ENV", "development"}, []Option{Dev()}, false, true, true},
		{"VUKA_ENV=staging", []string{"VUKA_ENV", "staging"}, []Option{Dev()}, false, false, false},
		{"vuka run", []string{"VUKA_DEV", "1", DevDirEnv, t.TempDir()}, []Option{Dev()}, false, true, false},
		{"without Dev", []string{"VUKA_DEV", "1"}, nil, false, true, false},
		{"go test", nil, []Option{Dev()}, true, true, false},
		{"go test, VUKA_DEV=1", []string{"VUKA_DEV", "1"}, []Option{Dev()}, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			devEnv(t, c.env...)
			testBinary = c.test
			a := New(append([]Option{Only("none"), NoToolbar()}, c.opts...)...)
			if a.dev != c.dev || (a.tools != nil) != c.tools {
				t.Errorf("dev = %v, tools = %v; want %v, %v", a.dev, a.tools != nil, c.dev, c.tools)
			}
		})
	}
	t.Run("VUKA_DEV=1 is development", func(t *testing.T) {
		devEnv(t, "VUKA_DEV", "1")
		if !IsDev() {
			t.Error("IsDev() = false")
		}
	})
}

func TestDevMissingTools(t *testing.T) {
	devEnv(t, "VUKA_DEV", "1")
	devProject(t, false)
	logs := captureSlog(t)
	a := New(Only("none"), NoToolbar(), Dev(), Frontend(os.DirFS("."), "frontend/dist"))
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	out := logs.String()
	if n := strings.Count(out, "web: Dev:"); n != 1 {
		t.Fatalf("%d lines from Dev, want one:\n%s", n, out)
	}
	for _, want := range []string{"serving the last build", "brew install tailwindcss", "install its packages in frontend"} {
		if !strings.Contains(out, want) {
			t.Errorf("the line doesn't say %q:\n%s", want, out)
		}
	}
	if a.css != nil && a.css.dev {
		t.Error("the stylesheet is a development one with no Tailwind to build it")
	}
	if code, body := devGet(t, a, "/"); code != 200 || !strings.Contains(body, "built page") {
		t.Errorf("GET / = %d %q, want the build", code, body)
	}
}

func TestDevLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fakes are shell scripts")
	}
	for _, stubborn := range []bool{false, true} {
		t.Run("stubborn="+strconv.FormatBool(stubborn), func(t *testing.T) {
			devEnv(t, "VUKA_DEV", "1")
			if stubborn {
				t.Setenv("FAKE_VITE_STUBBORN", "1")
			}
			wait := devStopWait
			devStopWait = 300 * time.Millisecond
			t.Cleanup(func() { devStopWait = wait })
			vite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`<html><head><script type="module" src="/src/main.js"></script></head><body>vite page</body></html>`))
			}))
			defer vite.Close()
			t.Setenv("FAKE_VITE_ORIGIN", vite.URL)
			dir := devProject(t, true)

			a := New(Only("none"), NoToolbar(), Dev(), Frontend(os.DirFS("."), "frontend/dist"))
			if err := a.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the frontend's page from Vite", func() bool {
				_, body := devGet(t, a, "/")
				return strings.Contains(body, vite.URL+"/src/main.js")
			})
			eventually(t, "the stylesheet Tailwind built", func() bool {
				_, body := devGet(t, a, cssPath)
				return strings.Contains(body, ".built")
			})
			css := filepath.Join(dir, ".vuka/tailwind/app.css")
			pids := append(readPids(t, css+".pids"), readPids(t, filepath.Join(dir, "frontend/pids"))...)
			if pids[3] != os.Getpid() {
				t.Errorf("Vite's %s = %d, want the app's pid %d", devParentEnv, pids[3], os.Getpid())
			}
			pids = pids[:3]
			if plugin, err := os.ReadFile(filepath.Join(dir, "frontend/vuka/vite.js")); err != nil || string(plugin) != string(vitePlugin) {
				t.Errorf("the frontend's plugin isn't web's: %v", err)
			}
			entry, _ := os.ReadFile(filepath.Join(dir, ".vuka/tailwind/input.css"))
			if !strings.Contains(string(entry), filepath.ToSlash(filepath.Join(dir, "app.css"))) {
				t.Errorf("Tailwind's entry doesn't import the app's stylesheet:\n%s", entry)
			}

			start := time.Now()
			if err := a.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if took := time.Since(start); stubborn && took < devStopWait {
				t.Errorf("Stop took %v: it didn't wait before SIGKILL", took)
			}
			eventually(t, "every process gone", func() bool {
				for _, pid := range pids {
					if processRuns(pid) {
						return false
					}
				}
				return true
			})
			if !stubborn {
				if _, err := os.Stat(filepath.Join(dir, "frontend/dist/.vite/vuka-hot.json")); err == nil {
					t.Error("Vite didn't get SIGTERM: its hot file is still there")
				}
			}
		})
	}
}

func readPids(t *testing.T, file string) []int {
	t.Helper()
	var pids []int
	eventually(t, file, func() bool {
		data, _ := os.ReadFile(file)
		pids = nil
		for _, f := range strings.Fields(string(data)) {
			if n, err := strconv.Atoi(f); err == nil {
				pids = append(pids, n)
			}
		}
		return len(pids) == 2
	})
	return pids
}

func devGet(t *testing.T, a *App, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w.Code, w.Body.String()
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// A Vite dev server or Tailwind watcher running already (npm run dev, a
// stylesheet named in VUKA_TAILWIND_CSS) is used, not started again.
func TestDevLeavesRunningTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fakes are shell scripts")
	}
	devEnv(t, "VUKA_DEV", "1")
	vite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head></head><body>running vite</body></html>`))
	}))
	defer vite.Close()
	dir := devProject(t, true)
	css := filepath.Join(dir, "running.css")
	os.WriteFile(css, []byte(".running{}"), 0o644)
	t.Setenv(CSSEnv, css)
	hot := `{"origin":"` + vite.URL + `","base":"/","pid":` + strconv.Itoa(os.Getpid()) + `}`
	os.MkdirAll(filepath.Join(dir, "frontend/dist/.vite"), 0o755)
	os.WriteFile(filepath.Join(dir, "frontend/dist/.vite/vuka-hot.json"), []byte(hot), 0o644)

	a := New(Only("none"), NoToolbar(), Dev(), Frontend(os.DirFS("."), "frontend/dist"))
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if _, body := devGet(t, a, "/"); !strings.Contains(body, "running vite") {
		t.Errorf("GET / = %q, want the running dev server's page", body)
	}
	if _, body := devGet(t, a, cssPath); body != ".running{}" {
		t.Errorf("the stylesheet = %q, want the running watcher's", body)
	}
	if len(a.tools.procs) != 0 {
		t.Errorf("started %d processes beside running ones", len(a.tools.procs))
	}
}

func processRuns(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}

// TestDevIslands: an app registering islands runs vuka build-islands
// -watch and serves what it builds; its pages load the loader before the
// first build is there. Without the vuka command, Dev says how to get it.
func TestDevIslands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake is a shell script")
	}
	fake, _ := filepath.Abs("testdata/dev/vuka")
	devEnv(t, "VUKA_DEV", "1")
	dir := t.TempDir()
	for name, data := range map[string]string{
		"go.mod":  "module example.com/devapp\n\ngo 1.25\n",
		"main.go": "package main\n\nfunc init() { ui.RegisterIsland[Chart]() }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	bin := filepath.Join(dir, "bin")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")

	logs := captureSlog(t)
	a := New(Only("none"), NoToolbar(), Dev())
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.Stop(context.Background())
	if !strings.Contains(logs.String(), "for islands: go get -tool github.com/vuka-lang/vuka/cmd/vuka/v2") {
		t.Errorf("no word of the vuka command:\n%s", logs)
	}

	copyExec(t, fake, filepath.Join(bin, "vuka"))
	a = New(Only("none"), NoToolbar(), Dev())
	a.Get("/page", func() ui.Node { return ui.El("html", nil, ui.El("head", nil), ui.El("body", nil)) })
	if _, body := devGet(t, a, "/page"); !strings.Contains(body, `<script src="/_vuka/c/loader.js?v=`) {
		t.Errorf("no loader before the first build: %s", body)
	}
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the island in the loader", func() bool {
		_, js := devGet(t, a, "/_vuka/c/loader.js")
		return strings.Contains(js, `"Chart":["/_vuka/i/chart.0a1b2c3d.js",""]`)
	})
	if code, js := devGet(t, a, "/_vuka/i/chart.0a1b2c3d.js"); code != 200 || js != "export function mount() {}" {
		t.Errorf("the chunk: %d %q", code, js)
	}
	data, _ := os.ReadFile(filepath.Join(dir, ".vuka/islands.pid"))
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	a.Stop(context.Background())
	eventually(t, "vuka build-islands gone", func() bool { return !processRuns(pid) })
}
