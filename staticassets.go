package web

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// Links to static files. web.Asset("/static/images/logo.png") is the path
// an app.Static mount serves the file at: under vuka run the plain path,
// revalidated on each use; in a binary vuka build made, the name carrying
// the file's content hash (/static/images/logo.3f9a1c2b.png), which the
// mount serves cached for good. vuka build hashes the files of every mount
// of an embedded directory and registers the names (RegisterStatic); it
// checks each literal path against them too.

var (
	staticMu     sync.RWMutex
	staticHashes map[string]string // plain URL path → hashed one
	staticPlains map[string]string // hashed URL path → plain one

	assetWarned sync.Map            // the paths a warning was logged for
	lastApp     atomic.Pointer[App] // the app built last: Asset's without a request
)

// RegisterStatic registers the content-hashed names of the files app.Static
// serves: each plain URL path (/static/images/logo.png) to its hashed one
// (/static/images/logo.3f9a1c2b.png). vuka build calls it from a file it
// generates in the main package.
func RegisterStatic(manifest map[string]string) {
	staticMu.Lock()
	defer staticMu.Unlock()
	if staticHashes == nil {
		staticHashes, staticPlains = map[string]string{}, map[string]string{}
	}
	for p, h := range manifest {
		staticHashes[p], staticPlains[h] = h, p
	}
}

func hashedStatic(p string) (string, bool) {
	staticMu.RLock()
	defer staticMu.RUnlock()
	h, ok := staticHashes[p]
	return h, ok
}

func plainStatic(p string) (string, bool) {
	staticMu.RLock()
	defer staticMu.RUnlock()
	h, ok := staticPlains[p]
	return h, ok
}

func staticRegistered() bool {
	staticMu.RLock()
	defer staticMu.RUnlock()
	return len(staticHashes) > 0
}

// Asset is the URL path of a file an app.Static mount serves, given as the
// path it is served at: web.Asset(ctx, "/static/images/logo.png"). In a
// binary vuka build made it is the file's content-hashed name, cached for
// good; under vuka run, the path as given. The path the app is mounted
// under (http.StripPrefix, or BaseURL's path without a request) comes first.
// A path no mount serves stays as given, and a warning is logged once.
// In markup the context is implicit: {web.Asset("/static/site.css")}.
func Asset(ctx context.Context, p string) string {
	r, a := requestApp(ctx)
	prefix := ""
	if r != nil {
		prefix = mountPrefix(r)
	} else if a != nil && a.baseURL != nil {
		prefix = strings.TrimSuffix(a.baseURL.Path, "/")
	}
	return prefix + assetPath(ctx, a, p)
}

// AssetURL is Asset's path as an absolute URL, for an email or a feed: the
// scheme and host the request came to, or, without one (a cron, a job),
// the app's BaseURL. Without either it is Asset's path, and a warning is
// logged once.
func AssetURL(ctx context.Context, p string) string {
	r, a := requestApp(ctx)
	if r != nil {
		scheme := "http"
		if IsHTTPS(r) {
			scheme = "https"
		}
		return scheme + "://" + r.Host + mountPrefix(r) + assetPath(ctx, a, p)
	}
	if a != nil && a.baseURL != nil {
		return strings.TrimSuffix(a.baseURL.String(), "/") + assetPath(ctx, a, p)
	}
	warnAsset(ctx, "base:"+p, "web: AssetURL has no request and the app no web.BaseURL: the path is relative", p)
	return assetPath(ctx, a, p)
}

// BaseURL is the app's public URL (https://example.com, or with the path
// it is mounted under): what AssetURL builds on where there is no request.
func BaseURL(u string) Option {
	return OptionFunc(func(a *App) {
		p, err := url.Parse(u)
		if err != nil || p.Scheme == "" || p.Host == "" {
			a.c.errs = append(a.c.errs, fmt.Errorf("web.BaseURL(%q): not an absolute URL", u))
			return
		}
		a.baseURL = p
	})
}

func requestApp(ctx context.Context) (*http.Request, *App) {
	if ctx != nil {
		if rq, _ := ctx.Value(ctxKey{}).(*request); rq != nil {
			return rq.r, rq.app
		}
	}
	return nil, lastApp.Load()
}

// assetPath is p's hashed path when vuka build registered one, else p,
// with a warning when no mount of a's serves it.
func assetPath(ctx context.Context, a *App, p string) string {
	clean, rest := p, ""
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		clean, rest = p[:i], p[i:]
	}
	if h, ok := hashedStatic(clean); ok {
		return h + rest
	}
	if a == nil {
		a = lastApp.Load()
	}
	if a != nil && !a.serves(clean) {
		warnAsset(ctx, p, "web: no app.Static mount serves the asset", p)
	}
	return p
}

func warnAsset(ctx context.Context, key, msg, p string) {
	if _, seen := assetWarned.LoadOrStore(key, true); !seen {
		if ctx == nil {
			ctx = context.Background()
		}
		slog.WarnContext(ctx, msg, "path", p)
	}
}

type staticMount struct {
	prefix string
	fsys   fs.FS
}

// serves reports whether one of the app's mounts has a file at URL path p.
func (a *App) serves(p string) bool {
	for _, m := range a.mounts {
		rel, ok := strings.CutPrefix(p, strings.TrimSuffix(m.prefix, "/")+"/")
		if !ok || rel == "" || hiddenPath(rel) {
			continue
		}
		if fi, err := fs.Stat(m.fsys, rel); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}

// mountPrefix is the path the app is mounted under: what http.StripPrefix
// took off the path the request came with.
func mountPrefix(r *http.Request) string {
	uri := r.RequestURI
	if i := strings.IndexAny(uri, "?#"); i >= 0 {
		uri = uri[:i]
	}
	if u, err := url.Parse(uri); err == nil && u.Path != "" {
		uri = u.Path
	}
	p := r.URL.Path
	if p == "" || len(uri) <= len(p) || !strings.HasSuffix(uri, p) {
		return ""
	}
	return strings.TrimSuffix(uri[:len(uri)-len(p)], "/")
}

// cssURL is a url(…) in a stylesheet, quoted or not.
var cssURL = regexp.MustCompile(`url\(\s*(["']?)([^"')\s]+)(["']?)\s*\)`)

// rewriteCSS has the url(…)s of the stylesheet served at at name the
// registered hashed files: an absolute path, or one relative to at. Only the
// file's name changes, so a relative reference stays relative.
func rewriteCSS(css []byte, at string) []byte {
	if !staticRegistered() || !bytes.Contains(css, []byte("url(")) {
		return css
	}
	return cssURL.ReplaceAllFunc(css, func(m []byte) []byte {
		sub := cssURL.FindSubmatch(m)
		ref := string(sub[2])
		if strings.Contains(ref, ":") || strings.HasPrefix(ref, "//") || strings.HasPrefix(ref, "#") {
			return m
		}
		clean, rest := ref, ""
		if i := strings.IndexAny(ref, "?#"); i >= 0 {
			clean, rest = ref[:i], ref[i:]
		}
		full := clean
		if !strings.HasPrefix(clean, "/") {
			full = path.Join(path.Dir(at), clean)
		}
		h, ok := hashedStatic(full)
		if !ok {
			return m
		}
		out := clean[:strings.LastIndexByte(clean, '/')+1] + path.Base(h) + rest
		return []byte("url(" + string(sub[1]) + out + string(sub[3]) + ")")
	})
}
