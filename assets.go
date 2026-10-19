package web

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vuka-lang/ui/live"
)

// A page's styles and scripts: web.Style, Script, CSS and JS name them on
// @web.Live, web.Layout, web.LiveLayout or web.New, and the vuka command
// compiles them — the stylesheets into the app's stylesheet (app.css),
// scoped to the component they belong to, the scripts into one module per
// component. What it compiled reaches the app as a manifest: registered by
// the file vuka build generates (RegisterAssets, RegisterScript), or, under
// vuka run, a directory it keeps up to date (VUKA_ASSETS). Every full page
// of an app with component scripts loads a small loader, which imports a
// component's module the first time the component renders on the page and
// runs its hooks.

// Asset is a stylesheet or script for a page: an option of @web.Live,
// web.Layout, web.LiveLayout and web.New. The vuka command reads it from the
// source and compiles it; the app itself only serves what it compiled.
type Asset struct {
	kind   string
	values []string
}

// Global, among an asset's arguments, makes it unscoped: web.Style("print.css",
// web.Global) styles the whole page, and a global script's hooks run once on
// the page, el being document.body.
const Global = ":global"

// Style names stylesheets for the component the option belongs to: the live
// component, or the layout's own markup (app-wide, for web.New). Paths are
// relative to the .vuka file and may be globs (parts/*.css, styles/**/*.css),
// expanded in name order; they must be string literals, since the vuka
// command reads them from the source. Selectors match only the elements the
// component's own markup renders: :global(…) and :root stay unscoped,
// :deep(…) reaches into child components, :scope is the component's root
// element. The stylesheets compile with the app's Tailwind CSS, in the order
// listed (later ones win), so @apply, theme() and variants work.
func Style(paths ...string) Asset { return Asset{"style", paths} }

// Script names hook modules (TypeScript or JavaScript) for the component the
// option belongs to, as Style does stylesheets. They are bundled in order
// into one module, loaded when the component first renders on a page; each
// may export default { mounted(el, ctx), updated(el, ctx), destroyed(el,
// ctx), …methods }. ui.JS.Call("name") reaches the component's own methods
// before vuka.register's and window's.
func Script(paths ...string) Asset { return Asset{"script", paths} }

// CSS is a stylesheet written inline, as Style's files are: web.CSS(`output { … }`).
func CSS(src ...string) Asset { return Asset{"css", src} }

// JS is a hook module written inline, as Script's files are.
func JS(src ...string) Asset { return Asset{"js", src} }

func (Asset) apply(*App)           {}
func (Asset) liveOption(*liveSpec) {}
func (Asset) Attrs() []any         { return nil }

const (
	assetsPrefix  = livePrefix + "c/"
	islandsPrefix = livePrefix + "i/"
	loaderPath    = assetsPrefix + "loader.js"
	assetEvents   = assetsPrefix + "events"
	// AssetsEnv names the directory vuka run keeps the compiled assets in:
	// manifest.json, the modules (c/), and the island chunks (i/).
	AssetsEnv = "VUKA_ASSETS"
	// islandsDist is where go tool vuka build-islands writes the islands in
	// a frontend's build, laid out as AssetsEnv's directory is.
	islandsDist = ".vuka"
)

// manifest is what the vuka command compiled: the scoped components and
// the app-wide (Global) assets.
type manifest struct {
	Scopes  map[string]*scopeAssets `json:"scopes,omitempty"`
	Global  *scopeAssets            `json:"global,omitempty"`
	Islands map[string]*islandAsset `json:"islands,omitempty"`
	// IslandCSS are the stylesheet chunks a framework island build emits
	// (e.g. Tailwind + a UI library's CSS), served from /_vuka/i/. A page
	// that carries an island links them so its components are styled.
	IslandCSS []string `json:"islandCSS,omitempty"`
}

// islandAsset is a registered island: its name (data-vk-island), the bundled
// component chunk (a file under /_vuka/i/), and an optional framework adapter
// chunk. A vanilla island's module exports mount directly, so Adapter is empty.
type islandAsset struct {
	Name      string `json:"name"`
	Module    string `json:"module"`
	Adapter   string `json:"adapter,omitempty"`
	Framework string `json:"framework,omitempty"`
}

// scopeAssets is a component's: its name, files, module (a file name under
// /_vuka/c/), the methods its hooks export, and the size of its CSS.
type scopeAssets struct {
	Name    string   `json:"name"`
	Styles  []string `json:"styles,omitempty"`
	Scripts []string `json:"scripts,omitempty"`
	Module  string   `json:"module,omitempty"`
	Methods []string `json:"methods,omitempty"`
	CSS     int      `json:"css,omitempty"`
	Type    string   `json:"type,omitempty"`   // a live component's struct type, as reflect spells it
	Pushes  []string `json:"pushes,omitempty"` // the methods its scripts push by name (ctx.push)
}

var (
	assetsMu  sync.Mutex
	assetsReg []byte
	scriptReg = map[string][]byte{}
	islandReg = map[string][]byte{}
)

// RegisterAssets registers the manifest of the app's compiled styles and
// scripts. vuka build calls it from a file it generates in the main package.
func RegisterAssets(manifest []byte) {
	assetsMu.Lock()
	defer assetsMu.Unlock()
	assetsReg = manifest
}

// RegisterScript registers a component's module, served at /_vuka/c/<name>
// and cached for good: its name carries its content's hash. vuka build calls
// it from the file it generates.
func RegisterScript(name string, js []byte) {
	assetsMu.Lock()
	defer assetsMu.Unlock()
	scriptReg[name] = js
}

// RegisterIsland registers an island's bundled chunk (component or adapter),
// served at /_vuka/i/<name> and cached for good: its name carries its
// content's hash. vuka build calls it from the file it generates.
func RegisterIsland(name string, js []byte) {
	assetsMu.Lock()
	defer assetsMu.Unlock()
	islandReg[name] = js
}

// assetSet is an app's compiled assets: registered, or a directory vuka run
// keeps up to date (re-read when its manifest changes, checked at most
// every 100ms).
type assetSet struct {
	dir    string
	files  fs.FS // the islands of a frontend's build (islandsDist), when not dir
	always bool  // Dev's island build: pages load the loader before the first build

	mu      sync.Mutex
	m       manifest
	loader  []byte // the loader with the manifest's modules
	tag     string
	mod     time.Time
	checked time.Time
	scripts map[string][]byte
	islands map[string][]byte
	allowed []string       // the types whose pushes it allowed
	fail    *assetsFailure // development: the last compile failed (vuka run's state.json)
	failMod time.Time

	streams *streams // development: the pages hearing of each new manifest
}

func (a *App) appAssets() *assetSet {
	if d := os.Getenv(AssetsEnv); d != "" {
		s := &assetSet{dir: d, streams: newStreams()}
		s.current()
		return s
	}
	if a.tools != nil && a.tools.islands != "" {
		s := &assetSet{dir: a.tools.islands, always: true, streams: newStreams()}
		if s.current(); s.loader == nil {
			s.loader, s.tag = s.loaderJS() // until the first build: islands say they aren't built yet
		}
		return s
	}
	assetsMu.Lock()
	defer assetsMu.Unlock()
	if assetsReg == nil {
		return nil
	}
	s := &assetSet{scripts: scriptReg, islands: islandReg}
	if json.Unmarshal(assetsReg, &s.m) != nil {
		return nil
	}
	s.loader, s.tag = s.loaderJS()
	s.allow()
	return s
}

// distAssets is the islands go tool vuka build-islands wrote into a
// frontend's build, for an app that registers none: nil when no frontend
// has them.
func distAssets(frontends []*frontend) *assetSet {
	for _, f := range frontends {
		sub, err := fs.Sub(f.files, islandsDist)
		if err != nil {
			continue
		}
		data, err := fs.ReadFile(sub, "manifest.json")
		if err != nil {
			continue
		}
		s := &assetSet{files: sub}
		if json.Unmarshal(data, &s.m) != nil {
			continue
		}
		s.loader, s.tag = s.loaderJS()
		s.allow()
		return s
	}
	return nil
}

// current is the manifest, and the loader and its tag.
func (s *assetSet) current() (manifest, []byte, string) {
	if s.dir == "" {
		return s.m, s.loader, s.tag
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now := time.Now(); now.Sub(s.checked) >= 100*time.Millisecond {
		s.checked = now
		file := filepath.Join(s.dir, "manifest.json")
		if fi, err := os.Stat(file); err == nil && !fi.ModTime().Equal(s.mod) {
			var m manifest
			if data, err := os.ReadFile(file); err == nil && json.Unmarshal(data, &m) == nil {
				s.m, s.mod = m, fi.ModTime()
				s.loader, s.tag = s.loaderJS()
				s.allow()
			}
		}
		if s.readFailure() {
			s.loader, s.tag = s.loaderJS()
		}
	}
	return s.m, s.loader, s.tag
}

// assetsFailure is vuka run's word that the assets' last compile failed:
// the manifest and modules served are the last good ones.
type assetsFailure struct {
	Failed  bool   `json:"failed"`
	Islands bool   `json:"islands,omitempty"` // an island's build is among the failures
	Output  string `json:"output"`
}

// readFailure reads the state vuka run leaves beside the manifest, and
// reports whether it changed; s.mu is held.
func (s *assetSet) readFailure() bool {
	file := filepath.Join(s.dir, "state.json")
	fi, err := os.Stat(file)
	if err != nil {
		changed := s.fail != nil
		s.fail, s.failMod = nil, time.Time{}
		return changed
	}
	if fi.ModTime().Equal(s.failMod) && s.fail != nil {
		return false
	}
	var f assetsFailure
	data, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(data, &f) != nil || !f.Failed {
		changed := s.fail != nil
		s.fail = nil
		return changed
	}
	s.fail, s.failMod = &f, fi.ModTime()
	return true
}

// failure is the assets' last failed compile in development; nil when it
// succeeded.
func (s *assetSet) failure() *assetsFailure {
	if s == nil || s.dir == "" {
		return nil
	}
	s.current()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fail
}

// allow lets each live component's scripts push the methods they name, and
// no others: the server, not the page, decides what the browser may call by
// name (live.AllowPush). Types an earlier manifest allowed lose theirs.
func (s *assetSet) allow() {
	var now []string
	for _, sc := range s.m.Scopes {
		if sc.Type != "" && len(sc.Pushes) > 0 {
			live.AllowPush(sc.Type, sc.Pushes...)
			now = append(now, sc.Type)
		}
	}
	for _, t := range s.allowed {
		if !slices.Contains(now, t) {
			live.AllowPush(t)
		}
	}
	s.allowed = now
}

// scripted reports whether the app has any component scripts: pages then
// load the loader.
func (m manifest) scripted() bool {
	return m.Global != nil && m.Global.Module != "" ||
		len(m.Islands) > 0 ||
		slices.ContainsFunc(mapValues(m.Scopes), func(s *scopeAssets) bool { return s.Module != "" })
}

func mapValues[K comparable, V any](m map[K]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

//go:embed loader.js
var loaderSrc string

// loaderInfo is what the loader knows of the manifest: each scope's module
// and methods, the global module, and in development the event stream.
type loaderInfo struct {
	Modules   map[string]string    `json:"m"`
	Methods   map[string][]string  `json:"f,omitempty"`
	Global    string               `json:"g,omitempty"`
	Events    string               `json:"e,omitempty"`
	Islands   map[string][2]string `json:"i,omitempty"`  // name → [moduleURL, adapterURL]
	IslandCSS []string             `json:"ic,omitempty"` // stylesheet URLs a page with an island links
	Dev       bool                 `json:"d,omitempty"`  // vuka run: an island not built shows why
	Failed    bool                 `json:"x,omitempty"`  // and the island build failed
}

func (s *assetSet) info() loaderInfo {
	li := loaderInfo{Modules: map[string]string{}, Methods: map[string][]string{}}
	for id, sc := range s.m.Scopes {
		if sc.Module != "" {
			li.Modules[id] = assetsPrefix + sc.Module
			if len(sc.Methods) > 0 {
				li.Methods[id] = sc.Methods
			}
		}
	}
	if g := s.m.Global; g != nil && g.Module != "" {
		li.Global = assetsPrefix + g.Module
		if len(g.Methods) > 0 {
			li.Methods[""] = g.Methods
		}
	}
	if len(s.m.Islands) > 0 {
		li.Islands = make(map[string][2]string, len(s.m.Islands))
		for name, is := range s.m.Islands {
			adapter := ""
			if is.Adapter != "" {
				adapter = islandsPrefix + is.Adapter
			}
			li.Islands[name] = [2]string{islandsPrefix + is.Module, adapter}
		}
	}
	for _, css := range s.m.IslandCSS {
		li.IslandCSS = append(li.IslandCSS, islandsPrefix+css)
	}
	if s.dir != "" {
		li.Events = assetEvents
		li.Dev = true
		li.Failed = s.fail != nil && s.fail.Islands
	}
	return li
}

func (s *assetSet) loaderJS() ([]byte, string) {
	info, _ := json.Marshal(s.info())
	js := []byte(strings.Replace(loaderSrc, "/*MANIFEST*/null", string(info), 1))
	return js, cssTag(js)
}

// headTag is the loader's script tag, for a full page of an app with
// component scripts.
func (s *assetSet) headTag() string {
	m, _, tag := s.current()
	s.mu.Lock()
	failed := s.fail != nil
	s.mu.Unlock()
	if !m.scripted() && !failed && !s.always {
		return ""
	}
	return `<script src="` + loaderPath + `?v=` + tag + `" defer></script>`
}

func (s *assetSet) serveLoader(w http.ResponseWriter, r *http.Request) {
	_, js, tag := s.current()
	serveTagged(w, r, "text/javascript; charset=utf-8", tag, js)
}

var moduleName = regexp.MustCompile(`^[a-z0-9]+(?:\.[a-z0-9]+)*\.js$`)

// islandFile is a chunk or stylesheet served under /_vuka/i/. A Vite island
// build hashes names with a mixed-case, base64url-ish alphabet and emits CSS
// assets, so the island route accepts more than moduleName does — still no
// slash or "..", so it can't escape the registry.
var islandFile = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*\.(?:js|css)$`)

// serveModule serves a component's module, named for its content: cached
// for good.
func (s *assetSet) serveModule(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, assetsPrefix)
	if !moduleName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	var js []byte
	if s.dir != "" {
		js, _ = os.ReadFile(filepath.Join(s.dir, "c", name))
	} else {
		assetsMu.Lock()
		js = s.scripts[name]
		assetsMu.Unlock()
	}
	if js == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	if s.dir != "" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(js))
}

// serveIsland serves an island's bundled chunk, named for its content: cached
// for good.
func (s *assetSet) serveIsland(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, islandsPrefix)
	if !islandFile.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	var js []byte
	if s.dir != "" {
		js, _ = os.ReadFile(filepath.Join(s.dir, "i", name))
	} else if s.files != nil {
		js, _ = fs.ReadFile(s.files, "i/"+name)
	} else {
		assetsMu.Lock()
		js = s.islands[name]
		assetsMu.Unlock()
	}
	if js == nil {
		http.NotFound(w, r)
		return
	}
	ctype := "text/javascript; charset=utf-8"
	if strings.HasSuffix(name, ".css") {
		ctype = "text/css; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	if s.dir != "" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(js))
}

// events streams the loader's view of the manifest to development pages,
// so a component mounted after its script changed loads the new module.
func (s *assetSet) events(w http.ResponseWriter, r *http.Request) {
	s.streams.serve(w, r, func() (string, bool) {
		s.current()
		s.mu.Lock()
		defer s.mu.Unlock()
		info, _ := json.Marshal(s.info())
		return string(info), true
	})
}

func (s *assetSet) close() {
	if s != nil {
		s.streams.close()
	}
}

func (a *App) routeAssets() {
	if a.assets == nil {
		return
	}
	a.router.Handle(http.MethodGet, assetsPrefix+"{name}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case loaderPath:
			a.assets.serveLoader(w, r)
		case assetEvents:
			if a.assets.dir != "" {
				a.assets.events(w, r)
				return
			}
			http.NotFound(w, r)
		default:
			a.assets.serveModule(w, r)
		}
	}))
	a.router.Handle(http.MethodGet, islandsPrefix+"{name}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.assets.serveIsland(w, r)
	}))
}

var scopeMark = regexp.MustCompile(` data-vk-s="([^"]+)"`)

// pageAssets lists, for the toolbar, the scoped components a page's HTML
// renders and the app-wide assets.
func (s *assetSet) pageAssets(html []byte) []tbAsset {
	m, _, _ := s.current()
	var out []tbAsset
	if g := m.Global; g != nil {
		out = append(out, tbAsset{Name: "app-wide", Styles: g.Styles, Scripts: g.Scripts, Module: g.Module, CSS: g.CSS})
	}
	seen := map[string]bool{}
	for _, mt := range scopeMark.FindAllSubmatch(html, -1) {
		id := string(mt[1])
		if sc := m.Scopes[id]; sc != nil && !seen[id] {
			seen[id] = true
			out = append(out, tbAsset{Name: sc.Name, Scope: id, Styles: sc.Styles, Scripts: sc.Scripts, Module: sc.Module, CSS: sc.CSS})
		}
	}
	return out
}

// tbAsset is a row of the toolbar's Assets panel.
type tbAsset struct {
	Name, Scope     string
	Styles, Scripts []string
	Module          string
	CSS             int
}
