package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// The app's stylesheet: Tailwind CSS the vuka command builds from the
// project's sources. vuka build embeds it (RegisterCSS from a generated
// file); vuka run keeps it rebuilt on disk and names the file in
// VUKA_TAILWIND_CSS. Every full page links it.

const (
	cssName  = "app.css"
	cssPath  = livePrefix + cssName
	cssWatch = livePrefix + "app-css.js"
	// cssEvents streams the stylesheet's tag to development pages.
	cssEvents = livePrefix + "app-css"
	// CSSEnv names the stylesheet file vuka run keeps up to date.
	CSSEnv = "VUKA_TAILWIND_CSS"
	// cssInlineMark marks the development inline <style>, for the idempotency
	// check and the hot-reload.
	cssInlineMark = "data-vk-app"
)

var (
	cssMu  sync.Mutex
	cssReg = map[string][]byte{}
)

// RegisterCSS registers a stylesheet the app serves at /_vuka/<name>; the
// one named "app.css" is linked from every full page. vuka build calls it
// from a file it generates in the main package, with the Tailwind CSS built
// for the binary.
func RegisterCSS(name string, css []byte) {
	cssMu.Lock()
	defer cssMu.Unlock()
	cssReg[name] = css
}

// NoTailwind turns the app's stylesheet off: pages get no link to
// /_vuka/app.css and the vuka command builds none.
func NoTailwind() Option { return OptionFunc(func(a *App) { a.noCSS = true }) }

// Tailwind names the project's Tailwind stylesheet, relative to the module
// root, when it isn't app.css or styles/app.css. The vuka command reads the
// path from the source, so it must be a string literal.
func Tailwind(path string) Option {
	return OptionFunc(func(a *App) { a.noCSS, a.cssSource = false, path })
}

// stylesheet is a stylesheet's current content and tag (a hash of it).
type stylesheet struct {
	file string // a file re-read when it changes (development); else fixed

	mu         sync.Mutex
	css        []byte
	tag        string
	mod        time.Time
	size       int64
	checked    time.Time
	found, dev bool

	streams *streams // development: the pages hearing of each new tag
}

func newStylesheet(css []byte) *stylesheet {
	s := &stylesheet{found: true}
	s.css, s.tag = css, cssTag(css)
	return s
}

func cssTag(css []byte) string {
	sum := sha256.Sum256(css)
	return base64.RawURLEncoding.EncodeToString(sum[:6])
}

// appCSS is the app's stylesheet: the file vuka run names, else the one
// registered, else none.
func (a *App) appCSS() *stylesheet {
	if a.noCSS {
		return nil
	}
	if f := os.Getenv(CSSEnv); f != "" {
		return &stylesheet{file: f, dev: true, streams: newStreams()}
	}
	cssMu.Lock()
	defer cssMu.Unlock()
	if css, ok := cssReg[cssName]; ok {
		return newStylesheet(css)
	}
	return nil
}

// current is the content and tag, a file's re-read when its size or time
// changed (checked at most every 100ms).
func (s *stylesheet) current() ([]byte, string, bool) {
	if s.file == "" {
		return s.css, s.tag, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now := time.Now(); now.Sub(s.checked) >= 100*time.Millisecond {
		s.checked = now
		fi, err := os.Stat(s.file)
		switch {
		case err != nil:
			s.css, s.tag, s.found = nil, "", false
		case !s.found || !fi.ModTime().Equal(s.mod) || fi.Size() != s.size:
			if css, err := os.ReadFile(s.file); err == nil {
				s.css, s.tag, s.found, s.mod, s.size = css, cssTag(css), true, fi.ModTime(), fi.Size()
			}
		}
	}
	return s.css, s.tag, s.found
}

// events streams the stylesheet's tag: the current one when a page
// connects, then each new one as the file changes.
func (s *stylesheet) events(w http.ResponseWriter, r *http.Request) {
	s.streams.serve(w, r, func() (string, bool) {
		_, tag, ok := s.current()
		return tag, ok
	})
}

// close ends the event streams.
func (s *stylesheet) close() {
	if s != nil {
		s.streams.close()
	}
}

func (s *stylesheet) serve(w http.ResponseWriter, r *http.Request) {
	css, tag, ok := s.current()
	if !ok {
		http.Error(w, "the stylesheet isn't built yet", http.StatusNotFound)
		return
	}
	serveTagged(w, r, "text/css; charset=utf-8", tag, css)
}

// serveTagged answers with content tagged tag: immutable when asked for by
// that tag (?v=), revalidated by its ETag otherwise.
func serveTagged(w http.ResponseWriter, r *http.Request, ctype, tag string, content []byte) {
	serveContent(w, r, ctype, tag, r.URL.Query().Get("v") == tag, bytes.NewReader(content))
}

// serveContent answers with content tagged tag, cached for good when
// immutable, else revalidated by its ETag (a 304 when it matches). An empty
// ctype is sniffed.
func serveContent(w http.ResponseWriter, r *http.Request, ctype, tag string, immutable bool, content io.ReadSeeker) {
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.Header().Set("ETag", `"`+tag+`"`)
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, "", time.Time{}, content)
}

// headTags is what a full page gets before </head>: the stylesheet's link,
// and in development the script swapping it when it changes.
// headTags is the stylesheet's tags for a page's head. Development inlines it
// in a <style> so a full reload never flashes unstyled while the stylesheet
// (served no-cache so it stays fresh) revalidates, and adds the hot-reload
// script; the build links it, which the browser caches across loads.
func (s *stylesheet) headTags() string {
	css, tag, _ := s.current()
	if s.dev {
		return `<style ` + cssInlineMark + `>` + string(css) + `</style>` +
			`<script src="` + cssWatch + `" defer></script>`
	}
	return `<link rel="stylesheet" href="` + cssPath + `?v=` + tag + `">`
}

// present reports whether html already carries the stylesheet, so linkCSS
// does not add it twice.
func (s *stylesheet) present(html []byte) bool {
	if s.dev && bytes.Contains(html, []byte(cssInlineMark)) {
		return true
	}
	return bytes.Contains(html, []byte(cssPath))
}

// linkCSS adds the stylesheet to a full page (one with </head> or </body>)
// that doesn't link it already, the loader of component scripts, and in
// development the toolbar's script for the request r.
func (a *App) linkCSS(r *http.Request, html []byte) []byte {
	tags := a.headTags(r, html)
	if tags == "" {
		return html
	}
	i := headAt(html)
	if i < 0 {
		return html
	}
	return append(html[:i:i], append([]byte(tags), html[i:]...)...)
}

// headTags is what linkCSS adds to html.
func (a *App) headTags(r *http.Request, html []byte) string {
	var tags string
	if a.css != nil && !a.css.present(html) {
		tags = a.css.headTags()
	}
	if a.assets != nil && !bytes.Contains(html, []byte(loaderPath)) {
		tags += a.assets.headTag()
	}
	if a.tb != nil {
		tags += a.tb.tag(r)
	}
	return tags
}

// headAt is where linkCSS adds its tags to html: before its first </head>,
// else its first </body>, else -1.
func headAt(html []byte) int {
	if i := indexTag(html, "</head>", false); i >= 0 {
		return i
	}
	return indexTag(html, "</body>", false)
}

// indexTag is the index of html's first (last) tag, a lowercase ASCII
// string, matched ignoring ASCII case; -1 when there is none.
func indexTag(html []byte, tag string, last bool) int {
	rest := html
	for {
		var i int
		if last {
			i = bytes.LastIndexByte(rest, '<')
		} else {
			i = bytes.IndexByte(rest, '<')
		}
		if i < 0 {
			return -1
		}
		if len(rest)-i >= len(tag) && foldEq(rest[i:i+len(tag)], tag) {
			if last {
				return i
			}
			return len(html) - len(rest) + i
		}
		if last {
			rest = rest[:i]
		} else {
			rest = rest[i+1:]
		}
	}
}

func foldEq(b []byte, lower string) bool {
	for i := range len(lower) {
		c := b[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

// cssWatchJS swaps the stylesheet's link when the server says it changed,
// the new one loaded before the old goes, so the page doesn't flash:
// development only. EventSource reconnects by itself when the app restarts.
const cssWatchJS = `(() => {
	const path = "` + cssPath + `";
	new EventSource("` + cssEvents + `").onmessage = async (e) => {
		const style = document.querySelector('style[` + cssInlineMark + `]');
		if (style) {
			style.textContent = await fetch(path + "?v=" + e.data).then((r) => r.text());
			return;
		}
		const old = document.querySelector('link[rel="stylesheet"][href^="' + path + '"]');
		if (!old || new URL(old.href).searchParams.get("v") === e.data) return;
		const next = old.cloneNode();
		next.href = path + "?v=" + e.data;
		next.onload = () => old.remove();
		old.after(next);
	};
})();
`

var cssWatchTag = cssTag([]byte(cssWatchJS))

func serveCSSWatch(w http.ResponseWriter, r *http.Request) {
	serveTagged(w, r, "text/javascript; charset=utf-8", cssWatchTag, []byte(cssWatchJS))
}

// routeCSS serves the app's stylesheet, the dev script, and every other
// registered stylesheet.
func (a *App) routeCSS() {
	if a.css != nil {
		a.router.Handle(http.MethodGet, cssPath, http.HandlerFunc(a.css.serve))
		if a.css.dev {
			a.router.Handle(http.MethodGet, cssWatch, http.HandlerFunc(serveCSSWatch))
			a.router.Handle(http.MethodGet, cssEvents, http.HandlerFunc(a.css.events))
		}
	}
	cssMu.Lock()
	defer cssMu.Unlock()
	for name, css := range cssReg {
		if name != cssName {
			s := newStylesheet(css)
			a.router.Handle(http.MethodGet, livePrefix+name, http.HandlerFunc(s.serve))
		}
	}
}
