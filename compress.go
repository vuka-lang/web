package web

import (
	"bufio"
	"compress/gzip"
	"context"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
)

// Responses are compressed: brotli when the browser takes it, else gzip.
// Pages, endpoints and JSON are compressed as they are written; the
// stylesheet, the runtime, component modules and island chunks are fixed
// content, served in an encoding made once (or by vuka build), and files
// under Static and Frontend in the .br or .gz beside them when there is
// one. Small bodies (under 1 KB), types already compressed (images, video,
// archives, woff2), Range requests, HEAD, 1xx, 204, 206 and 304 answers,
// responses a handler encoded itself or marked no-transform, event streams
// and WebSocket upgrades are sent as they are.

// CompressionLevel is how hard the app compresses what handlers write: 1
// (fastest) to 11 (smallest), brotli's quality; gzip's level is the same, up
// to 9. Fixed content (the runtime, stylesheets, scripts) is compressed once,
// harder.
type CompressionLevel int

const (
	// DefaultCompression is brotli's quality 5 (gzip's level 5): a page of
	// 250 KB in about 2 ms, to a 30th of its size.
	DefaultCompression CompressionLevel = 0
	// BestSpeed and BestCompression are the ends of the scale.
	BestSpeed       CompressionLevel = 1
	BestCompression CompressionLevel = 11
)

// Compression sets how hard the app compresses its responses; Off turns
// compression off, for an app behind a proxy that already compresses. It is
// on, at DefaultCompression, unless set. Live sockets' messages are
// LiveCompression's.
func Compression(level CompressionLevel) Option {
	return OptionFunc(func(a *App) { a.compression = level })
}

const (
	compressMin    = 1024 // smaller bodies are sent as they are
	defaultBrotli  = 5
	staticBrotli   = 9 // fixed content web compresses itself, once
	staticGzip     = 9
	encBrotli      = "br"
	encGzip        = "gzip"
	varyEncoding   = "Accept-Encoding"
	noTransformDir = "no-transform"
)

// compressor is an app's compression of what handlers write.
type compressor struct {
	br, gz int
	brPool sync.Pool
	gzPool sync.Pool
}

func newCompressor(level CompressionLevel) *compressor {
	if level == Off {
		return nil
	}
	q := int(level)
	if q <= 0 {
		q = defaultBrotli
	}
	q = min(q, 11)
	return &compressor{br: q, gz: min(q, 9)}
}

// encoder is a pooled brotli or gzip writer.
type encoder interface {
	io.WriteCloser
	Flush() error
}

func (c *compressor) encoder(enc string, w io.Writer) encoder {
	if enc == encBrotli {
		if e, ok := c.brPool.Get().(*brotli.Writer); ok {
			e.Reset(w)
			return e
		}
		return brotli.NewWriterLevel(w, c.br)
	}
	if e, ok := c.gzPool.Get().(*gzip.Writer); ok {
		e.Reset(w)
		return e
	}
	e, _ := gzip.NewWriterLevel(w, c.gz)
	return e
}

func (c *compressor) release(e encoder) {
	switch e := e.(type) {
	case *brotli.Writer:
		e.Reset(io.Discard)
		c.brPool.Put(e)
	case *gzip.Writer:
		e.Reset(io.Discard)
		c.gzPool.Put(e)
	}
}

// acceptEncoding is the encoding to answer r in: br, gzip, or "" for
// identity. Of the two the browser takes, the one with the higher q wins,
// brotli on a tie; "*" stands for those it doesn't name.
func acceptEncoding(r *http.Request) string {
	header := r.Header.Values("Accept-Encoding")
	if len(header) == 0 {
		return ""
	}
	br, gz, star := -1.0, -1.0, -1.0
	for _, h := range header {
		for _, part := range strings.Split(h, ",") {
			name, params, _ := strings.Cut(part, ";")
			q := 1.0
			for _, p := range strings.Split(params, ";") {
				k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
				if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
					if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
						q = f
					} else {
						q = 0
					}
				}
			}
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "br":
				br = q
			case "gzip", "x-gzip":
				gz = q
			case "*":
				star = q
			}
		}
	}
	if br < 0 {
		br = star
	}
	if gz < 0 {
		gz = star
	}
	switch {
	case br > 0 && br >= gz:
		return encBrotli
	case gz > 0:
		return encGzip
	}
	return ""
}

// compressible reports whether a response of type ctype gains by
// compression: text, scripts, JSON, XML, SVG, WebAssembly and the font
// formats that aren't compressed already; not images, video, audio,
// archives, woff/woff2, nor an event stream.
func compressible(ctype string) bool {
	mt, _, err := mime.ParseMediaType(ctype)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(strings.Split(ctype, ";")[0]))
	}
	switch {
	case mt == "text/event-stream":
		return false
	case strings.HasPrefix(mt, "text/"):
		return true
	case strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return true // image/svg+xml, application/manifest+json, application/ld+json, …
	}
	switch mt {
	case "application/json", "application/javascript", "application/x-javascript", "application/ecmascript",
		"application/xml", "application/wasm", "application/x-ndjson", "application/graphql-response+json",
		"application/vnd.ms-fontobject", "font/ttf", "font/otf", "font/collection", "application/x-font-ttf",
		"image/x-icon", "image/vnd.microsoft.icon", "image/bmp":
		return true
	}
	return false
}

// passes reports whether r is never compressed: a WebSocket (or other)
// upgrade, an event stream, HEAD or a Range request. Their writer is the
// server's own, so Hijack, Flush and the upgrade's 101 reach it untouched.
func passes(r *http.Request) bool {
	return r.Method == http.MethodHead || r.Header.Get("Range") != "" ||
		r.Header.Get("Upgrade") != "" || headerHasToken(r.Header, "Connection", "upgrade") ||
		strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// wireStats is what the compression did to a response, for the toolbar.
type wireStats struct {
	mu       sync.Mutex
	encoding string
	body     int64 // bytes the handler wrote
	wire     int64 // bytes sent: body, or its encoding
}

type wireKey struct{}

func (s *wireStats) get() (enc string, body, wire int64) {
	if s == nil {
		return "", 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encoding, s.body, s.wire
}

func (s *wireStats) add(body, wire int64) {
	if s != nil {
		s.mu.Lock()
		s.body += body
		s.wire += wire
		s.mu.Unlock()
	}
}

func (s *wireStats) setEncoding(enc string) {
	if s != nil {
		s.mu.Lock()
		s.encoding = enc
		s.mu.Unlock()
	}
}

// compress wraps h in the app's compression.
func (a *App) compress(h http.Handler) http.Handler {
	c := a.compressor
	if c == nil {
		return h
	}
	stats := a.tb != nil
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if passes(r) {
			h.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{w: w, c: c, enc: acceptEncoding(r)}
		if stats {
			cw.stats = &wireStats{}
			r = r.WithContext(context.WithValue(r.Context(), wireKey{}, cw.stats))
		}
		done := false
		defer func() {
			if !done {
				cw.panicked()
			}
		}()
		h.ServeHTTP(cw, r)
		done = true
		cw.close()
	})
}

// compressWriter holds a response's first bytes until it knows whether to
// compress it: when they reach compressMin, when the handler flushes, or when
// it returns.
type compressWriter struct {
	w     http.ResponseWriter
	c     *compressor
	enc   string // what the request takes; "" for identity
	stats *wireStats

	status  int
	buf     []byte
	decided bool
	zw      encoder
	counter *countWriter
}

// countWriter counts the bytes the encoder sends.
type countWriter struct {
	w http.ResponseWriter
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

func (cw *compressWriter) Header() http.Header         { return cw.w.Header() }
func (cw *compressWriter) Unwrap() http.ResponseWriter { return cw.w }

func (cw *compressWriter) WriteHeader(code int) {
	if cw.decided || cw.status != 0 {
		if cw.decided {
			cw.w.WriteHeader(code) // net/http says it is superfluous
		}
		return
	}
	if code < 200 { // 103 Early Hints and the like go out at once
		cw.w.WriteHeader(code)
		return
	}
	cw.status = code
	h := cw.w.Header()
	if code == http.StatusNoContent || code == http.StatusNotModified || code == http.StatusPartialContent ||
		h.Get("Content-Encoding") != "" || cw.enc == "" && h.Get("Content-Type") != "" {
		cw.decide(false)
		return
	}
	if cl := h.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n < compressMin {
			cw.decide(false)
		}
	}
}

func (cw *compressWriter) Write(b []byte) (int, error) {
	if cw.status == 0 {
		cw.WriteHeader(http.StatusOK)
	}
	if !cw.decided {
		cw.buf = append(cw.buf, b...)
		if len(cw.buf) >= compressMin {
			cw.decide(false)
		}
		return len(b), nil
	}
	if cw.zw != nil {
		n, err := cw.zw.Write(b)
		cw.stats.add(int64(n), 0)
		return n, err
	}
	n, err := cw.w.Write(b)
	cw.stats.add(int64(n), int64(n))
	return n, err
}

// Flush sends what was written so far, compressed when the response is: a
// streamed page reaches the browser as it is flushed.
func (cw *compressWriter) Flush() {
	if !cw.decided {
		if cw.status == 0 {
			cw.WriteHeader(http.StatusOK)
		}
		if !cw.decided {
			cw.decide(true)
		}
	}
	if cw.zw != nil {
		cw.zw.Flush()
	}
	http.NewResponseController(cw.w).Flush()
}

// Hijack is the server's, for a handler taking over a connection the
// compression didn't recognise as an upgrade: nothing was sent yet.
func (cw *compressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if cw.decided || len(cw.buf) > 0 {
		return nil, nil, http.ErrNotSupported
	}
	cw.decided = true
	return http.NewResponseController(cw.w).Hijack()
}

// decide sends the header, compressing when the response is worth it: a
// compressible type, not encoded already, and at least compressMin bytes,
// or flushed (streamed).
func (cw *compressWriter) decide(flushing bool) {
	cw.decided = true
	h := cw.w.Header()
	ctype := h.Get("Content-Type")
	if ctype == "" && len(cw.buf) > 0 && h["Content-Type"] == nil {
		ctype = http.DetectContentType(cw.buf)
		h.Set("Content-Type", ctype)
	}
	ok := ctype != "" && compressible(ctype)
	if ok && cw.status != http.StatusNoContent && cw.status != http.StatusNotModified {
		addVary(h)
	}
	if ok && cw.enc != "" && h.Get("Content-Encoding") == "" && !headerHasToken(h, "Cache-Control", noTransformDir) &&
		cw.status >= 200 && cw.status != http.StatusNoContent && cw.status != http.StatusNotModified && cw.status != http.StatusPartialContent &&
		(len(cw.buf) >= compressMin || flushing) {
		h.Del("Content-Length")
		h.Del("Accept-Ranges")
		h.Set("Content-Encoding", cw.enc)
		if et := h.Get("ETag"); et != "" && !strings.HasPrefix(et, "W/") {
			h.Set("ETag", "W/"+et) // another representation of the same content
		}
		cw.stats.setEncoding(cw.enc)
		cw.w.WriteHeader(cw.status)
		cw.counter = &countWriter{w: cw.w}
		cw.zw = cw.c.encoder(cw.enc, cw.counter)
		if len(cw.buf) > 0 {
			cw.zw.Write(cw.buf)
			cw.stats.add(int64(len(cw.buf)), 0)
		}
		cw.buf = nil
		return
	}
	cw.w.WriteHeader(cw.status)
	if len(cw.buf) > 0 {
		cw.w.Write(cw.buf)
		cw.stats.add(int64(len(cw.buf)), int64(len(cw.buf)))
	}
	cw.buf = nil
}

// close ends the response once the handler returned.
func (cw *compressWriter) close() {
	if !cw.decided {
		if cw.status == 0 && len(cw.buf) == 0 {
			return // nothing written: net/http's empty 200
		}
		if cw.status == 0 {
			cw.status = http.StatusOK
		}
		cw.decide(false)
	}
	if cw.zw != nil {
		cw.zw.Close()
		cw.stats.add(0, cw.counter.n)
		cw.c.release(cw.zw)
		cw.zw = nil
	}
}

// panicked sends what the handler wrote before it panicked, as written:
// the response is committed, as it would be without compression, and
// recovery aborts it rather than answering twice.
func (cw *compressWriter) panicked() {
	if !cw.decided && len(cw.buf) > 0 {
		cw.enc = ""
		cw.decide(false)
	}
}

// addVary adds Accept-Encoding to the response's Vary, once.
func addVary(h http.Header) {
	if !headerHasToken(h, "Vary", varyEncoding) && !headerHasToken(h, "Vary", "*") {
		h.Add("Vary", varyEncoding)
	}
}

// Push is the server's HTTP/2 push.
func (cw *compressWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := cw.w.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
