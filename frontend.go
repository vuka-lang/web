package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vuka-lang/vuka/v2/diag"
)

// A frontend is a single-page app built by its own toolchain (Vite, say)
// and served from the app's binary: Frontend mounts the built files, an
// embedded dist directory. In development a Vite dev server can stand in
// for the build: the vuka() Vite plugin (vite/vuka.js in this module)
// writes <dist>/.vite/vuka-hot.json with the server's origin and pid while
// it runs, and the app's pages then load their modules from it, so the
// browser stays on the app's own address and gets hot module replacement.

const (
	// hotFile is where the Vite plugin announces its dev server, under the
	// build's output directory.
	hotFile = ".vite/vuka-hot.json"
	// manifestFile is Vite's build manifest: which files are build output.
	manifestFile = ".vite/manifest.json"
	// frontendEnvGlobal is the global the page's public config values are
	// set on; the Vite plugin points import.meta.env.KEY at it.
	frontendEnvGlobal = "__VUKA_ENV__"
)

// Frontend serves a built single-page app from fsys, the files under root:
//
//	//go:embed all:frontend/dist
//	var dist embed.FS
//
//	app := web.New(web.Frontend(dist, "frontend/dist"))
//
// A GET path naming a file (its last segment has an extension) is that
// file, or 404; any other GET path is index.html, so the app's client-side
// routes work on a reload and from a link. The app's own routes, live pages
// and /_vuka/ are matched first. Files Vite's manifest lists as build output
// whose names carry a content hash are cached for good; everything else,
// index.html included, is revalidated by its ETag. The .vite directory is
// never served.
//
// The app's islands are embedded with it: go tool vuka build-islands writes
// them into root's .vuka directory, and the app serves them from there. A
// build of islands alone, with no index.html, serves them and answers other
// paths 404:
//
//	//go:generate go tool vuka build-islands
//
// Outside development a root without index.html or islands is a Build
// error. In development (Dev, VUKA_ENV=development, or vuka run) the files
// are read from disk when root is found there (beside the source calling
// Frontend, else in the working directory), a running Vite dev server
// serves the page's modules, and a frontend not built yet shows a page
// saying how to build it.
func Frontend(fsys fs.FS, root string, opts ...FrontendOption) Option {
	f := &frontend{fsys: fsys, root: strings.Trim(path.Clean("/"+filepath.ToSlash(root)), "/")}
	if _, file, _, ok := runtime.Caller(1); ok && filepath.IsAbs(file) {
		f.srcDir = filepath.Dir(file)
	}
	for _, o := range opts {
		o(f)
	}
	return OptionFunc(func(a *App) {
		a.mutable("Frontend")
		a.frontends = append(a.frontends, f)
	})
}

// FrontendOption configures a Frontend.
type FrontendOption func(*frontend)

// FrontendRoot names the frontend project folder — the directory that holds
// the Vite project (package.json, src, node_modules) whose build output is the
// dist Frontend serves. The vuka command reads it to root the island build:
// it is esbuild's resolve base (node_modules for framework islands) and where
// islands.d.ts is written. It does not change where an @ui.Island("…") path
// resolves — that is relative to the Go file declaring the island. When unset,
// the build uses the parent of Frontend's root (its dist's directory).
func FrontendRoot(dir string) FrontendOption {
	return func(f *frontend) {
		f.srcRoot = strings.Trim(path.Clean("/"+filepath.ToSlash(dir)), "/")
	}
}

// FrontendAt serves the frontend under prefix ("/admin") instead of the
// root: build it with the same base (Vite's base: "/admin/").
func FrontendAt(prefix string) FrontendOption {
	return func(f *frontend) {
		if f.prefix = strings.TrimSuffix(path.Clean("/"+prefix), "/"); f.prefix == "/" {
			f.prefix = ""
		}
	}
}

// FrontendEnv passes the values of these environment variables (and .env
// keys, which New has loaded) to the frontend's pages: index.html sets
// them on a global before any module runs, and the Vite plugin turns
// import.meta.env.KEY into a read of it. Only the keys named are passed;
// they reach every visitor, so name only public values. Unset keys are
// left out.
func FrontendEnv(keys ...string) FrontendOption {
	return func(f *frontend) { f.env = append(f.env, keys...) }
}

type frontend struct {
	fsys    fs.FS
	root    string
	srcRoot string // the frontend project folder (FrontendRoot); "" = parent of root
	prefix  string
	env     []string
	srcDir  string // the directory of the source calling Frontend

	// set by prepare
	a         *App
	dev       bool
	files     fs.FS           // what is served: root on disk in development, else in fsys
	disk      string          // development: root on disk; "" when not found there
	immutable map[string]bool // build output with hashed names, from the manifest
	envJS     []byte          // the <script> setting the public values; nil when none
	index     []byte          // production: index.html as served
	indexTag  string
	hot       *viteHot
	warned    sync.Map // what development has logged once
}

// prepare checks the frontend for Build and readies what it serves.
func (f *frontend) prepare(a *App) error {
	f.a = a
	f.dev = a.dev || os.Getenv(DevDirEnv) != ""
	name := fmt.Sprintf("web: Frontend(%q)", f.root)
	if f.root == "" {
		f.files = f.fsys
	} else if sub, err := fs.Sub(f.fsys, f.root); err == nil {
		f.files = sub
	} else {
		return fmt.Errorf("%s: %w", name, err)
	}
	if f.dev {
		for _, dir := range []string{f.srcDir, "."} {
			if dir == "" {
				continue
			}
			if d, err := filepath.Abs(filepath.Join(dir, filepath.FromSlash(f.root))); err == nil {
				if fi, err := os.Stat(d); err == nil && fi.IsDir() {
					f.disk = d
					break
				}
			}
		}
		if f.disk != "" {
			f.files = os.DirFS(f.disk)
			f.hot = &viteHot{file: filepath.Join(f.disk, filepath.FromSlash(hotFile))}
		}
	}
	if err := f.prepareEnv(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if f.dev {
		return nil // read per request: the build, or the dev server, may come later
	}
	index, err := fs.ReadFile(f.files, "index.html")
	if err != nil && f.islandsOnly() {
		return nil
	}
	if err != nil {
		what := "has no index.html"
		if errors.Is(err, fs.ErrNotExist) && !hasFiles(f.files) {
			what = "is empty or missing"
		}
		return diag.New("E0601", "%s: the frontend %s", strings.TrimPrefix(name, "web: "), what).
			WithHelp("build it first (vuka build runs vite build; or npm run build in the frontend's directory),\nor run in development (vuka run, VUKA_ENV=development) for a placeholder page")
	}
	f.immutable = readManifest(f.files)
	f.index = f.withEnv(index)
	f.indexTag = cssTag(f.index)
	return nil
}

// islandsOnly reports whether the build holds islands (go tool vuka
// build-islands) and no page of its own: an app's islands embedded with no
// single-page app beside them.
func (f *frontend) islandsOnly() bool {
	_, err := fs.Stat(f.files, islandsDist+"/manifest.json")
	return err == nil
}

func hasFiles(fsys fs.FS) bool {
	found := false
	fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && path.Base(p) != ".gitkeep" {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// routes are the patterns the frontend answers; key is what clashes with
// another route.
func (f *frontend) routes() []string {
	if f.prefix == "" {
		return []string{"/{path...}"}
	}
	return []string{f.prefix, f.prefix + "/{path...}"}
}

func (f *frontend) prepareEnv() error {
	if len(f.env) == 0 {
		return nil
	}
	vals := map[string]string{}
	for _, k := range f.env {
		if k == "" || strings.ContainsAny(k, " =") {
			return fmt.Errorf("FrontendEnv: %q is not a variable name", k)
		}
		if v, ok := os.LookupEnv(k); ok {
			vals[k] = v
		}
	}
	js, _ := json.Marshal(vals) // escapes <, > and & as <…: safe inside <script>
	f.envJS = []byte("<script>globalThis." + frontendEnvGlobal + "=Object.freeze(" + string(js) + ")</script>")
	return nil
}

// withEnv puts the public values' script at the start of the document's
// head, before any module script.
func (f *frontend) withEnv(page []byte) []byte {
	if f.envJS == nil {
		return page
	}
	i := 0
	if m := headTag.FindIndex(page); m != nil {
		i = m[1]
	}
	return slices.Concat(page[:i], f.envJS, page[i:])
}

var headTag = regexp.MustCompile(`(?i)<head\b[^>]*>`)

// readManifest lists the files of a Vite build's manifest whose names carry
// a content hash: the only ones that may be cached for good.
func readManifest(fsys fs.FS) map[string]bool {
	data, err := fs.ReadFile(fsys, manifestFile)
	if err != nil {
		return nil
	}
	var m map[string]struct {
		File   string   `json:"file"`
		CSS    []string `json:"css"`
		Assets []string `json:"assets"`
	}
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	out := map[string]bool{}
	for _, c := range m {
		for _, f := range append(append([]string{c.File}, c.CSS...), c.Assets...) {
			if hashedName.MatchString(path.Base(f)) {
				out[strings.TrimPrefix(path.Clean("/"+f), "/")] = true
			}
		}
	}
	return out
}

// hashedName is a name with a content hash, as Vite writes them
// (index-B3x_9kQa.js): a segment of eight or more letters, digits, - or _.
var hashedName = regexp.MustCompile(`[-.][A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$`)

func (f *frontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, f.prefix)
	if strings.HasPrefix(r.URL.Path, livePrefix) || f.prefix != "" && rel != "" && rel[0] != '/' {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+rel), "/")
	if hiddenPath(name) { // .vite (the manifest), .env, .git: never served; .well-known is
		http.NotFound(w, r)
		return
	}
	if name == "" || name == "index.html" || path.Ext(name) == "" {
		f.serveIndex(w, r)
		return
	}
	f.serveFile(w, r, name)
}

func (f *frontend) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	if h := f.devServer(); h != nil {
		// The dev server has the current files, public/ ones included: send
		// the browser there (Vite answers this machine's pages with CORS).
		http.Redirect(w, r, h.Origin+joinPath(strings.TrimSuffix(h.Base, "/"), "/"+name), http.StatusTemporaryRedirect)
		return
	}
	serveFile(w, r, f.files, name, contentType(name), !f.dev && f.immutable[name], "no-cache")
}

// serveIndex answers a client-side route with the frontend's page: Vite's
// while its dev server runs, else index.html; in development, a page saying
// how to build when there is neither.
func (f *frontend) serveIndex(w http.ResponseWriter, r *http.Request) {
	if !f.dev {
		if f.index == nil {
			http.NotFound(w, r) // a build of islands only
			return
		}
		serveFixed(w, r, "text/html; charset=utf-8", f.indexTag, false, f.index)
		return
	}
	var page []byte
	if h := f.devServer(); h != nil {
		var err error
		if page, err = h.page(r.Context()); err != nil {
			f.once("fetch "+h.Origin, "web: frontend: the Vite dev server at %s didn't send its page (%v); serving the build instead", h.Origin, err)
			page = nil
		}
	}
	if page == nil {
		page, _ = fs.ReadFile(f.files, "index.html")
		if page == nil && f.islandsOnly() {
			http.NotFound(w, r)
			return
		}
		if page == nil {
			page = []byte(placeholderPage(f))
		}
	}
	page = f.withEnv(page)
	if f.a.tb != nil {
		page = insertHead(page, f.a.tb.tag(r))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		w.Write(page)
	}
}

// devServer is the running Vite dev server announcing itself in the build's
// directory, in development; nil when there is none.
func (f *frontend) devServer() *hotInfo {
	if !f.dev || f.hot == nil {
		return nil
	}
	h, stale := f.hot.current()
	if stale != nil {
		f.once("stale "+stale.Origin+strconv.Itoa(stale.PID), "web: frontend: ignoring %s: its Vite dev server (pid %d, %s) isn't running", f.hot.file, stale.PID, stale.Origin)
	}
	return h
}

func (f *frontend) once(key, format string, args ...any) {
	if _, seen := f.warned.LoadOrStore(key, true); !seen {
		log.Printf(format, args...)
	}
}

func placeholderPage(f *frontend) string {
	where := f.root
	if f.disk != "" {
		where = f.disk
	}
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Frontend not built</title><style>
body{font:15px/1.6 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1.5rem;color:#1f2937}
code,pre{font:13px ui-monospace,monospace;background:#f3f4f6;border-radius:4px}code{padding:.1em .3em}pre{padding:.8em 1em;overflow:auto}
@media (prefers-color-scheme:dark){body{background:#111827;color:#e5e7eb}code,pre{background:#1f2937}}
</style></head><body><h1>The frontend isn't built yet</h1>
<p>This app serves a frontend from <code>` + html.EscapeString(where) + `</code>, which has no <code>index.html</code>, and no Vite dev server is running for it.</p>
<p>Either start the dev server, whose pages then load from it with hot reloading (<code>vuka run</code> starts it when the frontend has a <code>package.json</code>):</p>
<pre>cd frontend &amp;&amp; npm run dev</pre>
<p>or build the frontend once:</p><pre>cd frontend &amp;&amp; npm run build</pre>
<p>Its <code>vite.config</code> needs the <code>vuka()</code> plugin for the dev server to be found. Outside development an app without a built frontend doesn't start.</p>
</body></html>`
}

// insertHead puts tags before </head>, else before </body>, else at the end.
func insertHead(page []byte, tags string) []byte {
	if tags == "" {
		return page
	}
	lower := bytes.ToLower(page)
	i := bytes.Index(lower, []byte("</head>"))
	if i < 0 {
		if i = bytes.Index(lower, []byte("</body>")); i < 0 {
			i = len(page)
		}
	}
	return slices.Concat(page[:i], []byte(tags), page[i:])
}

var extraTypes = map[string]string{
	".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8",
	".json": "application/json", ".map": "application/json", ".webmanifest": "application/manifest+json",
	".svg": "image/svg+xml", ".ico": "image/x-icon", ".wasm": "application/wasm", ".txt": "text/plain; charset=utf-8",
	".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf",
}

// contentType is a file's type by its extension: the usual web types fixed
// (a host's MIME table may lack or misname them), others from the table; ""
// leaves it to sniffing.
func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := extraTypes[ext]; ok {
		return t
	}
	return mime.TypeByExtension(ext)
}

// viteHot reads the hot file the Vite plugin writes.
type viteHot struct {
	file string

	mu      sync.Mutex
	mod     time.Time
	info    *hotInfo
	checked time.Time
	alive   bool
}

type hotInfo struct {
	Origin string `json:"origin"`
	Base   string `json:"base"`
	PID    int    `json:"pid"`
}

// current is the dev server the hot file names while it runs; else nil,
// and the file's when it names one that has stopped.
func (v *viteHot) current() (live, stale *hotInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	fi, err := os.Stat(v.file)
	if err != nil {
		v.info = nil
		return nil, nil
	}
	if v.info == nil || !fi.ModTime().Equal(v.mod) {
		data, err := os.ReadFile(v.file)
		var h hotInfo
		if err != nil || json.Unmarshal(data, &h) != nil || h.Origin == "" {
			return nil, nil // being written
		}
		if u, err := url.Parse(h.Origin); err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" {
			return nil, nil
		}
		h.Origin = strings.TrimSuffix(h.Origin, "/")
		if h.Base == "" || h.Base[0] != '/' {
			h.Base = "/"
		}
		v.info, v.mod, v.checked = &h, fi.ModTime(), time.Time{}
	}
	if now := time.Now(); now.Sub(v.checked) > time.Second {
		v.checked, v.alive = now, running(v.info)
	}
	if !v.alive {
		return nil, v.info
	}
	return v.info, nil
}

// running reports whether the dev server is up: its process, where signals
// tell (signal 0 checks without sending one), else its port.
func running(h *hotInfo) bool {
	if h.PID > 0 && runtime.GOOS != "windows" {
		p, err := os.FindProcess(h.PID)
		if err != nil {
			return false
		}
		err = p.Signal(syscall.Signal(0))
		return err == nil || errors.Is(err, syscall.EPERM)
	}
	u, _ := url.Parse(h.Origin)
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), map[string]string{"http": "80", "https": "443"}[u.Scheme])
	}
	c, err := net.DialTimeout("tcp", host, 300*time.Millisecond)
	if err == nil {
		c.Close()
	}
	return err == nil
}

var devClient = &http.Client{Timeout: 5 * time.Second}

// page fetches the dev server's index.html, as Vite transforms it, with its
// module URLs pointed at the dev server.
func (h *hotInfo) page(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.Origin+h.Base, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html")
	res, err := devClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return devPage(body, h.Origin), nil
}

var (
	// a script's src or a link's href naming a path on this origin
	tagURL = regexp.MustCompile(`(?is)(<(?:script|link)\b[^>]*?\s(?:src|href)\s*=\s*["']?)(/[^/"'\s>][^"'\s>]*|/)`)
	// an inline module script's body
	inlineModule = regexp.MustCompile(`(?is)(<script\b[^>]*\btype\s*=\s*["']?module["']?[^>]*>)(.*?)(</script>)`)
	// an import of a path on this origin in a module's code
	importURL = regexp.MustCompile(`((?:\bfrom|\bimport)\s*\(?\s*["'])(/[^/"'])`)
)

// devPage points a dev server's page at it: the scripts and stylesheets it
// links by path, and the paths its inline modules import (React's refresh
// preamble), load from origin, so the page itself can come from the app.
func devPage(page []byte, origin string) []byte {
	page = tagURL.ReplaceAll(page, []byte("${1}"+origin+"${2}"))
	return inlineModule.ReplaceAllFunc(page, func(m []byte) []byte {
		sub := inlineModule.FindSubmatch(m)
		body := importURL.ReplaceAll(sub[2], []byte("${1}"+origin+"${2}"))
		return slices.Concat(sub[1], body, sub[3])
	})
}

// IslandRoot names the folder island components build in, for an app that
// serves no frontend of its own (no Frontend): the one holding package.json
// and node_modules (vue, react, svelte, esbuild…), where islands.d.ts and
// vuka-live.d.ts are written. The vuka command reads it; it changes nothing
// at run time. The module root is the default.
func IslandRoot(dir string) Option { return OptionFunc(func(*App) {}) }
