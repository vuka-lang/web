package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
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
	// CSSEnv names the stylesheet file vuka run keeps up to date.
	CSSEnv = "VUKA_TAILWIND_CSS"
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
		return &stylesheet{file: f, dev: true}
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
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("ETag", `"`+tag+`"`)
	if r.URL.Query().Get("v") == tag {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
}

// headTags is what a full page gets before </head>: the stylesheet's link,
// and in development the script swapping it when it changes.
func (s *stylesheet) headTags() string {
	_, tag, _ := s.current()
	t := `<link rel="stylesheet" href="` + cssPath + `?v=` + tag + `">`
	if s.dev {
		t += `<script src="` + cssWatch + `" defer></script>`
	}
	return t
}

// linkCSS adds the stylesheet to a full page (one with </head> or </body>)
// that doesn't link it already.
func (a *App) linkCSS(html []byte) []byte {
	if a.css == nil || bytes.Contains(html, []byte(cssPath)) {
		return html
	}
	lower := bytes.ToLower(html)
	i := bytes.Index(lower, []byte("</head>"))
	if i < 0 {
		if i = bytes.Index(lower, []byte("</body>")); i < 0 {
			return html
		}
	}
	return append(html[:i:i], append([]byte(a.css.headTags()), html[i:]...)...)
}

// cssWatchJS swaps the stylesheet's link when its ETag changes, the new one
// loaded before the old goes, so the page doesn't flash: development only.
const cssWatchJS = `(() => {
	const path = "` + cssPath + `";
	let tag = null;
	const link = () => document.querySelector('link[rel="stylesheet"][href^="' + path + '"]');
	setInterval(async () => {
		if (document.hidden || !link()) return;
		let res;
		try { res = await fetch(path, { method: "HEAD", cache: "no-store" }); } catch { return; }
		const t = (res.headers.get("ETag") || "").replace(/"/g, "");
		if (!res.ok || !t) return;
		if (tag === null) tag = new URL(link().href).searchParams.get("v");
		if (t === tag) return;
		tag = t;
		const old = link(), next = old.cloneNode();
		next.href = path + "?v=" + t;
		next.onload = () => old.remove();
		old.after(next);
	}, 1000);
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
