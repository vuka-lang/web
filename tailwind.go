package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
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

	stop     chan struct{} // closed when the server shuts down: ends the event streams
	stopOnce sync.Once
	servers  sync.Map // *http.Server → struct{}: those told to end the streams on Shutdown
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
		return &stylesheet{file: f, dev: true, stop: make(chan struct{})}
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
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if srv, ok := r.Context().Value(http.ServerContextKey).(*http.Server); ok {
		if _, seen := s.servers.LoadOrStore(srv, struct{}{}); !seen {
			srv.RegisterOnShutdown(s.close)
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	sent := ""
	check := time.NewTicker(100 * time.Millisecond)
	defer check.Stop()
	idle := time.Now()
	for {
		if _, tag, ok := s.current(); ok && tag != sent {
			sent, idle = tag, time.Now()
			fmt.Fprintf(w, "data: %s\n\n", tag)
			flusher.Flush()
		} else if time.Since(idle) > 15*time.Second {
			idle = time.Now()
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
		select {
		case <-check.C:
		case <-r.Context().Done():
			return
		case <-s.stop:
			return
		}
	}
}

// close ends the event streams, so a graceful shutdown doesn't wait on them:
// on Shutdown of any server that served one, and on the app's Stop.
func (s *stylesheet) close() {
	if s != nil && s.stop != nil {
		s.stopOnce.Do(func() { close(s.stop) })
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
// that doesn't link it already, and in development the toolbar's script
// for the request r.
func (a *App) linkCSS(r *http.Request, html []byte) []byte {
	var tags string
	if a.css != nil && !bytes.Contains(html, []byte(cssPath)) {
		tags = a.css.headTags()
	}
	if a.tb != nil {
		tags += a.tb.tag(r)
	}
	if tags == "" {
		return html
	}
	lower := bytes.ToLower(html)
	i := bytes.Index(lower, []byte("</head>"))
	if i < 0 {
		if i = bytes.Index(lower, []byte("</body>")); i < 0 {
			return html
		}
	}
	return append(html[:i:i], append([]byte(tags), html[i:]...)...)
}

// cssWatchJS swaps the stylesheet's link when the server says it changed,
// the new one loaded before the old goes, so the page doesn't flash:
// development only. EventSource reconnects by itself when the app restarts.
const cssWatchJS = `(() => {
	const path = "` + cssPath + `";
	const link = () => document.querySelector('link[rel="stylesheet"][href^="' + path + '"]');
	new EventSource("` + cssEvents + `").onmessage = (e) => {
		const old = link();
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
