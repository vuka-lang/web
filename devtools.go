package web

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Dev's tools: for `go run .` in development, the app runs tailwindcss
// --watch and its frontends' Vite dev servers itself, as vuka run does, and
// serves through them: the stylesheet Tailwind keeps built, the modules the
// dev server announces in its hot file.

const (
	// devParentEnv tells a dev server's Vite plugin the app's pid: it exits
	// when the app is gone, however the app ended.
	devParentEnv = "VUKA_DEV_PARENT"
	twDir        = ".vuka/tailwind"
	islandsDir   = ".vuka/assets"
)

var (
	//go:embed vite/vuka.js
	vitePlugin []byte
	//go:embed vite/vuka.d.ts
	vitePluginTypes []byte

	testBinary    = testing.Testing() // tests set it false to be a real binary
	devStopWait   = 2 * time.Second   // SIGTERM, then SIGKILL after this
	twImportsLine = regexp.MustCompile(`@import\s+["']tailwindcss["']`)
)

// devTools are the processes Dev runs beside the app.
type devTools struct {
	root    string // the module root
	twBin   string // tailwindcss; "" when the app has no Tailwind to run
	twSheet string // the project's stylesheet; "" for the built-in one
	css     string // the stylesheet Tailwind keeps built
	islands string // the directory vuka build-islands -watch keeps the islands in; "" for none
	missing []string

	mu      sync.Mutex
	procs   []*devProc
	started bool
	stopped bool
}

// devActive reports whether Dev runs its tools: in development (VUKA_DEV=1,
// VUKA_ENV=development) when vuka run, which runs them itself, isn't
// running the app, and never in a test binary.
func devActive() bool {
	return IsDev() && os.Getenv(DevDirEnv) == "" && !testBinary
}

// newDevTools finds what the app's Tailwind needs: its stylesheet, the
// project's or the built-in one when the app has a built one, and the CLI.
func (a *App) newDevTools() *devTools {
	cwd, _ := os.Getwd()
	t := &devTools{root: moduleRoot(cwd)}
	if os.Getenv(AssetsEnv) == "" && registersIslands(t.root) {
		if hasVukaTool(t.root) {
			t.islands = filepath.Join(t.root, filepath.FromSlash(islandsDir))
		} else {
			t.missing = append(t.missing, "islands: go get -tool github.com/vuka-lang/vuka/cmd/vuka/v2")
		}
	}
	if a.noCSS || os.Getenv(CSSEnv) != "" {
		return t
	}
	if a.cssSource != "" {
		t.twSheet = filepath.Join(t.root, filepath.FromSlash(a.cssSource))
	} else {
		for _, c := range []string{"app.css", "styles/app.css"} {
			p := filepath.Join(t.root, filepath.FromSlash(c))
			if css, err := os.ReadFile(p); err == nil && twImportsLine.Match(css) {
				t.twSheet = p
				break
			}
		}
	}
	cssMu.Lock()
	_, built := cssReg[cssName]
	cssMu.Unlock()
	if t.twSheet == "" && !built {
		return t
	}
	if t.twBin = tailwindBinary(t.root); t.twBin == "" {
		t.missing = append(t.missing, "live CSS: brew install tailwindcss (or npm i -D @tailwindcss/cli)")
		return t
	}
	t.css = filepath.Join(t.root, filepath.FromSlash(twDir), "app.css")
	return t
}

// moduleRoot is the nearest directory from dir up holding go.mod; dir when
// there is none.
func moduleRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if fileExists(filepath.Join(d, "go.mod")) {
			return d
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// tailwindBinary is tailwindcss on PATH, else the npm package's in the
// module or its frontend/ directory; "" when there is none.
func tailwindBinary(root string) string {
	if p, err := exec.LookPath("tailwindcss"); err == nil {
		return p
	}
	for _, dir := range []string{root, filepath.Join(root, "frontend")} {
		if p := filepath.Join(dir, "node_modules", ".bin", "tailwindcss"); fileExists(p) {
			return p
		}
	}
	return ""
}

// start runs Tailwind and the frontends' dev servers that aren't running
// yet, and logs one line for those that can't run.
func (t *devTools) start(frontends []*frontend) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return
	}
	t.started = true
	if t.twBin != "" {
		go t.startTailwind()
	}
	if t.islands != "" {
		go t.startIslands()
	}
	for _, f := range frontends {
		dir := f.projectDir()
		if dir == "" || f.devServer() != nil {
			continue
		}
		vite := filepath.Join(dir, "node_modules", ".bin", "vite")
		if !fileExists(vite) {
			t.missing = append(t.missing, "the frontend's dev server: install its packages in "+relTo(t.root, dir))
			continue
		}
		if err := syncVitePlugin(dir); err != nil {
			log.Printf("web: Dev: %v", err)
		}
		if err := t.spawn("vite", dir, false, []string{devParentEnv + "=" + strconv.Itoa(os.Getpid())}, vite); err != nil {
			log.Printf("web: Dev: vite: %v", err)
		}
	}
	if len(t.missing) > 0 {
		log.Printf("web: Dev: serving the last build; for %s", strings.Join(t.missing, "; for "))
	}
}

// startTailwind writes Tailwind's entry, with the component libraries the
// app links as sources, and runs tailwindcss --watch on it.
func (t *devTools) startTailwind() {
	entry, err := t.writeEntry(componentDirs(t.root))
	if err != nil {
		log.Printf("web: Dev: tailwind: %v", err)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.stopped {
		if err := t.spawn("tailwind", t.root, true, nil, t.twBin, "-i", entry, "-o", t.css, "--watch"); err != nil {
			log.Printf("web: Dev: tailwind: %v", err)
		}
	}
}

// startIslands runs vuka build-islands -watch, which keeps the islands
// built in t.islands, where the app reads them.
func (t *devTools) startIslands() {
	bin := vukaTool(t.root)
	t.mu.Lock()
	defer t.mu.Unlock()
	if bin == "" || t.stopped {
		return
	}
	if err := t.spawn("islands", t.root, true, nil, bin, "build-islands", "-watch"); err != nil {
		log.Printf("web: Dev: islands: %v", err)
	}
}

// registersIslands reports whether the module's Go source registers an
// island: ui.RegisterIsland or a ui.Island option.
func registersIslands(root string) bool {
	found := false
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return fs.SkipAll
		}
		n := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") || n == "node_modules" || n == "testdata" || n == "vendor" || fileExists(filepath.Join(p, "go.mod"))) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			src, _ := os.ReadFile(p)
			found = bytes.Contains(src, []byte("RegisterIsland[")) || bytes.Contains(src, []byte(".Island("))
		}
		return nil
	})
	return found
}

var toolLine = regexp.MustCompile(`(?m)^\s*(?:tool\s+)?github\.com/vuka-lang/vuka/cmd/vuka/v2\s*$`)

// hasVukaTool reports whether the vuka command is at hand: a tool of the
// module (go get -tool), or on PATH.
func hasVukaTool(root string) bool {
	mod, _ := os.ReadFile(filepath.Join(root, "go.mod"))
	if toolLine.Match(mod) {
		return true
	}
	_, err := exec.LookPath("vuka")
	return err == nil
}

// vukaTool is the vuka command: the module's tool, built by go tool -n,
// else vuka on PATH; "" when there is neither.
func vukaTool(root string) string {
	c := exec.Command("go", "tool", "-n", "vuka")
	c.Dir = root
	if out, err := c.Output(); err == nil {
		if p := strings.TrimSpace(string(out)); fileExists(p) {
			return p
		}
	}
	p, _ := exec.LookPath("vuka")
	return p
}

// writeEntry writes the stylesheet Tailwind builds, as vuka run does: the
// app's, then a source for the module's files and for each of deps.
func (t *devTools) writeEntry(deps []string) (string, error) {
	dir := filepath.Join(t.root, filepath.FromSlash(twDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := writeIfChanged(filepath.Join(t.root, ".vuka", ".gitignore"), []byte("*\n")); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("/* Generated by web.Dev: the app's stylesheet and the files Tailwind scans for class names. */\n")
	if t.twSheet != "" {
		fmt.Fprintf(&b, "@import %q;\n", filepath.ToSlash(t.twSheet))
	} else {
		b.WriteString("@import \"tailwindcss\";\n")
	}
	fmt.Fprintf(&b, "@source %q;\n", filepath.ToSlash(t.root)+"/**/*.{vuka,templ,html,go}")
	for _, d := range deps {
		fmt.Fprintf(&b, "@source %q;\n", filepath.ToSlash(d)+"/*.{vuka,templ,go}")
	}
	entry := filepath.Join(dir, "input.css")
	return entry, writeIfChanged(entry, []byte(b.String()))
}

// componentDirs are the directories of the other modules' packages the
// module's ones depend on that hold .vuka or .templ files: the component
// libraries whose class names the stylesheet needs. None without go.
func componentDirs(root string) []string {
	c := exec.Command("go", "list", "-e", "-deps", "-f", "{{if .Module}}{{if not .Module.Main}}{{.Dir}}{{end}}{{end}}", "./...")
	c.Dir = root
	out, _ := c.Output()
	var dirs []string
	for _, dir := range strings.Fields(string(out)) {
		entries, _ := os.ReadDir(dir)
		if slices.ContainsFunc(entries, func(e fs.DirEntry) bool {
			n := e.Name()
			return strings.HasSuffix(n, ".templ") || strings.HasSuffix(n, ".vuka") && !strings.HasSuffix(n, "_test.vuka")
		}) {
			dirs = append(dirs, dir)
		}
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
}

// projectDir is the frontend's Vite project on disk: FrontendRoot's folder,
// else the parent of the build's; "" when it has no package.json.
func (f *frontend) projectDir() string {
	rel := f.srcRoot
	if rel == "" {
		rel = filepath.Dir(filepath.FromSlash(f.root))
	}
	for _, base := range []string{f.srcDir, "."} {
		if base == "" {
			continue
		}
		if d, err := filepath.Abs(filepath.Join(base, filepath.FromSlash(rel))); err == nil && fileExists(filepath.Join(d, "package.json")) {
			return d
		}
	}
	return ""
}

// syncVitePlugin brings the frontend's copy of the Vite plugin up to date
// when its vite.config imports it or a copy is there, as vuka run does.
func syncVitePlugin(dir string) error {
	target := filepath.Join(dir, "vuka", "vite.js")
	if !fileExists(target) && !slices.ContainsFunc([]string{"js", "ts", "mjs", "mts"}, func(ext string) bool {
		src, _ := os.ReadFile(filepath.Join(dir, "vite.config."+ext))
		return bytes.Contains(src, []byte("vuka/vite"))
	}) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := writeIfChanged(target, vitePlugin); err != nil {
		return err
	}
	return writeIfChanged(strings.TrimSuffix(target, ".js")+".d.ts", vitePluginTypes)
}

// writeIfChanged writes data to path unless it holds it already, through a
// rename, so a reader never sees half a file.
func writeIfChanged(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func relTo(root, dir string) string {
	if r, err := filepath.Rel(root, dir); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return dir
}

// devProc is a tool running in a process group of its own.
type devProc struct {
	cmd   *exec.Cmd
	stdin io.Closer // tailwindcss --watch stops when its stdin closes
	done  chan struct{}
}

// spawn starts bin in dir, its output logged line by line as name's; with
// stdin, its stdin is a pipe that closes when the app exits.
func (t *devTools) spawn(name, dir string, stdin bool, env []string, bin string, args ...string) error {
	c := exec.Command(bin, args...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	ownGroup(c)
	w := &toolLog{name: name}
	c.Stdout, c.Stderr = w, w
	p := &devProc{cmd: c, done: make(chan struct{})}
	if stdin {
		in, err := c.StdinPipe()
		if err != nil {
			return err
		}
		p.stdin = in
	}
	if err := c.Start(); err != nil {
		return err
	}
	go func() { c.Wait(); w.flush(); close(p.done) }()
	t.procs = append(t.procs, p)
	return nil
}

// stop ends every tool: SIGTERM to its process group, SIGKILL to what is
// left of it after devStopWait, and waits for it.
func (t *devTools) stop() {
	t.mu.Lock()
	t.stopped = true
	procs := t.procs
	t.procs = nil
	t.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Go(func() {
			if p.stdin != nil {
				p.stdin.Close()
			}
			terminate(p.cmd)
			select {
			case <-p.done:
			case <-time.After(devStopWait):
			}
			kill(p.cmd) // the group's other processes too
			<-p.done
		})
	}
	wg.Wait()
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// toolLog logs a tool's output line by line, leaving out the noise: Vite's
// banner with its own address (the app's address is the one to open),
// Tailwind's timings.
type toolLog struct {
	name string
	mu   sync.Mutex
	buf  []byte
}

func (l *toolLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		l.line(string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
}

func (l *toolLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.line(string(l.buf))
	l.buf = nil
}

func (l *toolLog) line(s string) {
	s = strings.TrimSpace(ansiEscape.ReplaceAllString(s, ""))
	switch {
	case s == "", strings.HasPrefix(s, "≈"), strings.HasPrefix(s, "Done in"), strings.HasPrefix(s, "➜"),
		strings.HasPrefix(s, "Local:"), strings.HasPrefix(s, "Network:"), strings.Contains(s, "press h"),
		strings.Contains(s, "DeprecationWarning"), strings.Contains(s, "--trace-deprecation"):
		return
	case strings.Contains(s, "ready in"):
		s = "dev server ready: open the app's address; its pages load their modules from Vite"
	}
	log.Printf("%s: %s", l.name, s)
}
