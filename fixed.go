package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
)

// Fixed content — the runtime, the stylesheet, component modules, island
// chunks, Static's and Frontend's files — is served in an encoding made once:
// by vuka build (RegisterEncoded, or a .br/.gz file beside the original),
// else by web the first time it is asked for. Each encoding has its own ETag,
// so a browser's conditional request is answered with a 304 for the one it
// holds.

var (
	encodedMu  sync.Mutex
	encodedReg = map[string]map[string][]byte{} // path → encoding → bytes
	fixedCache sync.Map                         // path → *fixedBody
)

// RegisterEncoded registers a fixed asset's encoding made at build time:
// path is where the app serves it (/_vuka/app.css, /_vuka/c/<module>,
// /_vuka/i/<chunk>), encoding "br" or "gzip", data the encoded bytes. vuka
// build calls it from the file it generates, so the binary serves its
// assets compressed with no work at run time.
func RegisterEncoded(path, encoding string, data []byte) {
	encodedMu.Lock()
	defer encodedMu.Unlock()
	if encodedReg[path] == nil {
		encodedReg[path] = map[string][]byte{}
	}
	encodedReg[path][encoding] = data
}

func registeredEncoding(path, enc string) []byte {
	encodedMu.Lock()
	defer encodedMu.Unlock()
	return encodedReg[path][enc]
}

// fixedBody is a fixed asset's content and the encodings made of it.
type fixedBody struct {
	tag  string
	data []byte
	br   lazyBytes
	gz   lazyBytes
}

type lazyBytes struct {
	once sync.Once
	b    []byte // nil: no smaller than the content
}

func fixedFor(key, tag string, data []byte) *fixedBody {
	if v, ok := fixedCache.Load(key); ok {
		if f := v.(*fixedBody); f.tag == tag && len(f.data) == len(data) {
			return f
		}
	}
	f := &fixedBody{tag: tag, data: data}
	fixedCache.Store(key, f)
	return f
}

// encoded is the content in enc, nil when that is no smaller.
func (f *fixedBody) encoded(key, enc string) []byte {
	l := &f.gz
	if enc == encBrotli {
		l = &f.br
	}
	l.once.Do(func() {
		b := registeredEncoding(key, enc)
		if b == nil {
			b = encodeBytes(enc, f.data)
		}
		if len(b) < len(f.data) {
			l.b = b
		}
	})
	return l.b
}

// encodeBytes is data in enc, compressed hard: it is made once.
func encodeBytes(enc string, data []byte) []byte {
	var b bytes.Buffer
	var w io.WriteCloser
	if enc == encBrotli {
		w = brotli.NewWriterLevel(&b, staticBrotli)
	} else {
		w, _ = gzip.NewWriterLevel(&b, staticGzip)
	}
	w.Write(data)
	w.Close()
	return b.Bytes()
}

// compressWriterOf is the app's compression in w's chain of writers; nil
// when the app doesn't compress (web.Off) or the request isn't compressed
// (HEAD, Range, an upgrade).
func compressWriterOf(w http.ResponseWriter) *compressWriter {
	for w != nil {
		if cw, ok := w.(*compressWriter); ok {
			return cw
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
	return nil
}

// takeEncoding is the encoding to send fixed content of type ctype in, ""
// for identity; the response's compression is then left to the caller.
func takeEncoding(w http.ResponseWriter, ctype string, size int) string {
	cw := compressWriterOf(w)
	if cw == nil {
		return ""
	}
	enc := cw.enc
	cw.enc = "" // the caller encodes, or sends it as it is
	if !compressible(ctype) {
		return ""
	}
	addVary(w.Header())
	if size < compressMin {
		return ""
	}
	return enc
}

// serveFixed answers with fixed content tagged tag: in the encoding the
// browser takes when that is smaller, immutable when cached for good, else
// revalidated by its ETag (a 304 when it matches).
func serveFixed(w http.ResponseWriter, r *http.Request, ctype, tag string, immutable bool, data []byte) {
	h := w.Header()
	if ctype != "" {
		h.Set("Content-Type", ctype)
	}
	setCache(h, immutable, "no-cache")
	body, etag := data, tag
	if enc := takeEncoding(w, ctype, len(data)); enc != "" {
		if b := fixedFor(r.URL.Path, tag, data).encoded(r.URL.Path, enc); b != nil {
			body, etag = b, tag+"-"+encSuffix(enc)
			setEncoded(h, r, enc, len(b))
		}
	}
	h.Set("ETag", `"`+etag+`"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

// setEncoded marks a response as enc of n bytes: http.ServeContent sets no
// Content-Length on an encoded body, and only it would answer a failed
// precondition (412) with no body.
func setEncoded(h http.Header, r *http.Request, enc string, n int) {
	h.Set("Content-Encoding", enc)
	if r.Header.Get("If-Match") == "" && r.Header.Get("If-Unmodified-Since") == "" {
		h.Set("Content-Length", strconv.Itoa(n))
	}
}

func encSuffix(enc string) string {
	if enc == encBrotli {
		return "br"
	}
	return "gz"
}

func setCache(h http.Header, immutable bool, otherwise string) {
	if immutable {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", otherwise)
	}
}

// precompressed is the file beside name in fsys holding it in enc
// (name.br, name.gz), opened: what vuka build writes beside a frontend's
// hashed files, and what an app may put beside its static ones.
func precompressed(fsys fs.FS, name, enc string) fs.File {
	ext := ".gz"
	if enc == encBrotli {
		ext = ".br"
	}
	f, err := fsys.Open(name + ext)
	if err != nil {
		return nil
	}
	if fi, err := f.Stat(); err != nil || fi.IsDir() {
		f.Close()
		return nil
	}
	return f
}

// staticTags is the ETag of each file Static and Frontend serve, by name,
// size and modification time.
var staticTags sync.Map

func fileTag(fsys fs.FS, name string, fi fs.FileInfo, content io.ReadSeeker) (string, bool) {
	type key struct {
		fsys       any
		name       string
		size, mod  int64
		comparable bool
	}
	k := key{name: name, size: fi.Size(), mod: fi.ModTime().UnixNano()}
	if isComparable(fsys) {
		k.fsys, k.comparable = fsys, true
	}
	if k.comparable {
		if t, ok := staticTags.Load(k); ok {
			return t.(string), true
		}
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return "", false
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return "", false
	}
	tag := cssTag(data)
	if k.comparable {
		staticTags.Store(k, tag)
	}
	return tag, true
}

func isComparable(v any) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	_ = map[any]bool{v: true}
	return true
}

// staticHashed reports whether a file's name carries a content hash, so it
// may be cached for good: app.3f9a1c2b.css, index-BkL2x9aQ.js. A hex run of
// 8 or more with a digit and a letter, or a run of letters and digits with
// upper and lower case and a digit, between dots or dashes before the
// extension.
func staticHashed(name string) bool {
	base := path.Base(name)
	m := hashSegment.FindAllStringSubmatch(base, -1)
	for _, s := range m {
		seg := s[1]
		hasDigit := strings.ContainsAny(seg, "0123456789")
		if !hasDigit {
			continue
		}
		if hexRun.MatchString(seg) && strings.ContainsAny(seg, "abcdef") {
			return true
		}
		if strings.ToLower(seg) != seg && strings.ToUpper(seg) != seg {
			return true
		}
	}
	return false
}

var (
	hashSegment = regexp.MustCompile(`[.-]([A-Za-z0-9_]{8,})(?:\.[A-Za-z0-9]+)+$`)
	hexRun      = regexp.MustCompile(`^[0-9a-f]+$`)
)

// serveFile answers with a file of fsys (Static's, Frontend's): its ETag a
// hash of its content, Last-Modified its time when it has one; the .br or
// .gz beside it, or an encoding made once, when the browser takes one.
func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name, ctype string, immutable bool, uncached string) {
	file, err := fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	content, seeks := file.(io.ReadSeeker)
	if !seeks || fi.Size() > staticHashMax {
		data, err := io.ReadAll(io.LimitReader(file, staticHashMax+1))
		if err != nil || int64(len(data)) > staticHashMax {
			// too big to hash: its time is its validator
			if ctype != "" {
				w.Header().Set("Content-Type", ctype)
			}
			setCache(w.Header(), false, uncached)
			if seeks {
				http.ServeContent(w, r, name, fi.ModTime(), content)
			} else {
				http.Error(w, "reading "+name, http.StatusInternalServerError)
			}
			return
		}
		content = bytes.NewReader(data)
	}
	tag, ok := fileTag(fsys, name, fi, content)
	if !ok {
		http.Error(w, "reading "+name, http.StatusInternalServerError)
		return
	}
	h := w.Header()
	if ctype == "" {
		ctype = contentType(name)
	}
	if ctype != "" {
		h.Set("Content-Type", ctype)
	}
	setCache(h, immutable, uncached)
	if enc := takeEncoding(w, ctype, int(min(fi.Size(), staticHashMax))); enc != "" {
		var body []byte
		if pf := precompressed(fsys, name, enc); pf != nil {
			body, err = io.ReadAll(pf)
			pf.Close()
			if err != nil {
				body = nil
			}
		} else if fi.Size() <= staticEncodeMax {
			if data, err := io.ReadAll(content); err == nil {
				body = fixedFor("file:"+name, tag, data).encoded("", enc)
				content = bytes.NewReader(data)
			}
		}
		if body != nil {
			setEncoded(h, r, enc, len(body))
			h.Set("ETag", `"`+tag+"-"+encSuffix(enc)+`"`)
			http.ServeContent(w, r, "", fi.ModTime(), bytes.NewReader(body))
			return
		}
	}
	h.Set("ETag", `"`+tag+`"`)
	http.ServeContent(w, r, "", fi.ModTime(), content)
}

const (
	staticHashMax   = 32 << 20 // larger files are validated by their time alone
	staticEncodeMax = 4 << 20  // larger ones without a .br or .gz go as they are
)

// staticRevalidate is the Cache-Control of a file Static serves whose name
// carries no content hash: kept, and revalidated on each use.
const staticRevalidate = "public, max-age=0, must-revalidate"
