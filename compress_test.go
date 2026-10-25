package web

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
)

func TestAcceptEncoding(t *testing.T) {
	for _, c := range []struct{ header, want string }{
		{"", ""},
		{"br", "br"},
		{"gzip", "gzip"},
		{"gzip, deflate, br, zstd", "br"},
		{"gzip;q=1.0, br;q=0.5", "gzip"},
		{"br;q=0.8, gzip;q=0.8", "br"},
		{"br;q=0, gzip", "gzip"},
		{"br;q=0, gzip;q=0", ""},
		{"identity", ""},
		{"deflate", ""},
		{"*", "br"},
		{"*;q=0.5, br;q=0", "gzip"},
		{"x-gzip", "gzip"},
		{"BR ; Q=0.9", "br"},
		{"br;q=nope, gzip", "gzip"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if c.header != "" {
			r.Header.Set("Accept-Encoding", c.header)
		}
		if got := acceptEncoding(r); got != c.want {
			t.Errorf("Accept-Encoding %q: %q, want %q", c.header, got, c.want)
		}
	}
}

func decode(t *testing.T, enc string, body []byte) string {
	t.Helper()
	var rd io.Reader = bytes.NewReader(body)
	switch enc {
	case "br":
		rd = brotli.NewReader(rd)
	case "gzip":
		zr, err := gzip.NewReader(rd)
		if err != nil {
			t.Fatalf("gzip: %v", err)
		}
		rd = zr
	}
	out, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("decoding %s: %v", enc, err)
	}
	return string(out)
}

var bigText = strings.Repeat("<p>a pet named Rex, available</p>\n", 200)

func compressApp(opts ...Option) http.Handler {
	a := New(append([]Option{Only("none")}, opts...)...)
	a.Get("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, bigText)
	})
	a.Get("/sniffed", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<!doctype html>"+bigText) })
	a.Get("/json", func() []Pet {
		var ps []Pet
		for i := range 100 {
			ps = append(ps, Pet{ID: i, Name: "Rex"})
		}
		return ps
	})
	a.Get("/small", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "small")
	})
	a.Get("/png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(bytes.Repeat([]byte{0}, 4096))
	})
	a.Get("/encoded", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		io.WriteString(zw, bigText)
		zw.Close()
	})
	a.Get("/notransform", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "no-transform")
		io.WriteString(w, bigText)
	})
	a.Get("/nocontent", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	a.Get("/tagged", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(bigText))
	})
	return a.Handler()
}

func get(h http.Handler, method, target string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCompressNegotiates(t *testing.T) {
	h := compressApp()
	for _, c := range []struct{ accept, want string }{{"gzip, deflate, br", "br"}, {"gzip", "gzip"}, {"", ""}, {"br;q=0, gzip;q=0", ""}} {
		for _, path := range []string{"/big", "/sniffed", "/json"} {
			rec := get(h, "GET", path, "Accept-Encoding", c.accept)
			if got := rec.Header().Get("Content-Encoding"); got != c.want {
				t.Errorf("%s with %q: Content-Encoding %q, want %q", path, c.accept, got, c.want)
			}
			if v := rec.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
				t.Errorf("%s with %q: Vary %q", path, c.accept, v)
			}
			if rec.Header().Get("Content-Length") != "" && c.want != "" {
				t.Errorf("%s with %q: a Content-Length on an encoded body", path, c.accept)
			}
			body := decode(t, c.want, rec.Body.Bytes())
			if path == "/big" && body != bigText {
				t.Errorf("%s with %q: decoded body differs", path, c.accept)
			}
			if path == "/json" && !strings.HasPrefix(body, `[{"id":0,"name":"Rex"}`) {
				t.Errorf("%s with %q: %.40q", path, c.accept, body)
			}
			if c.want != "" && rec.Body.Len()*5 > len(body) {
				t.Errorf("%s with %q: %d bytes for %d", path, c.accept, rec.Body.Len(), len(body))
			}
		}
	}
	if ct := get(h, "GET", "/sniffed", "Accept-Encoding", "br").Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("a sniffed type: %q", ct)
	}
}

func TestCompressSkips(t *testing.T) {
	h := compressApp()
	for _, c := range []struct {
		name, method, path string
		header             []string
	}{
		{"a small body", "GET", "/small", nil},
		{"an image", "GET", "/png", nil},
		{"a Range request", "GET", "/big", []string{"Range", "bytes=0-99"}},
		{"HEAD", "HEAD", "/big", nil},
		{"no-transform", "GET", "/notransform", nil},
		{"a 204", "GET", "/nocontent", nil},
	} {
		rec := get(h, c.method, c.path, append([]string{"Accept-Encoding", "br, gzip"}, c.header...)...)
		if enc := rec.Header().Get("Content-Encoding"); enc != "" {
			t.Errorf("%s: Content-Encoding %q", c.name, enc)
		}
	}
	if rec := get(h, "GET", "/small", "Accept-Encoding", "br"); rec.Body.String() != "small" {
		t.Errorf("a small body: %q", rec.Body.String())
	}
	// A handler's own encoding isn't encoded again.
	rec := get(h, "GET", "/encoded", "Accept-Encoding", "br, gzip")
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" || decode(t, "gzip", rec.Body.Bytes()) != bigText {
		t.Errorf("a handler's gzip: Content-Encoding %q, %d bytes", enc, rec.Body.Len())
	}
	// A 304 stays one; the encoded response's ETag is weak, and still matches.
	rec = get(h, "GET", "/tagged", "Accept-Encoding", "br")
	if et := rec.Header().Get("ETag"); et != `W/"v1"` || rec.Header().Get("Content-Encoding") != "br" {
		t.Errorf("an encoded response's ETag %q, encoding %q", et, rec.Header().Get("Content-Encoding"))
	}
	rec = get(h, "GET", "/tagged", "Accept-Encoding", "br", "If-None-Match", `W/"v1"`)
	if rec.Code != http.StatusNotModified || rec.Header().Get("Content-Encoding") != "" || rec.Body.Len() != 0 {
		t.Errorf("a revalidation: %d, encoding %q, %d bytes", rec.Code, rec.Header().Get("Content-Encoding"), rec.Body.Len())
	}
}

func TestCompressionOff(t *testing.T) {
	h := compressApp(Compression(Off))
	for _, path := range []string{"/big", "/json", "/_vuka/live.js"} {
		rec := get(h, "GET", path, "Accept-Encoding", "br, gzip")
		if enc := rec.Header().Get("Content-Encoding"); enc != "" {
			t.Errorf("%s with Compression(Off): Content-Encoding %q", path, enc)
		}
	}
	rec := get(compressApp(Compression(BestSpeed)), "GET", "/big", "Accept-Encoding", "br")
	if rec.Header().Get("Content-Encoding") != "br" || decode(t, "br", rec.Body.Bytes()) != bigText {
		t.Errorf("Compression(BestSpeed): %q", rec.Header().Get("Content-Encoding"))
	}
}

// A streamed response reaches the browser as it is flushed, compressed.
func TestCompressStreamsFlushes(t *testing.T) {
	for _, enc := range []string{"br", "gzip"} {
		next := make(chan struct{})
		a := New(Only("none"))
		a.Get("/stream", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<html><head><title>first</title></head>")
			http.NewResponseController(w).Flush()
			<-next
			io.WriteString(w, "<body>"+bigText+"</body></html>")
		})
		srv := httptest.NewServer(a.Handler())
		req, _ := http.NewRequest("GET", srv.URL+"/stream", nil)
		req.Header.Set("Accept-Encoding", enc)
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Header.Get("Content-Encoding") != enc {
			t.Errorf("%s: a streamed page's encoding: %q", enc, res.Header.Get("Content-Encoding"))
		}
		var rd io.Reader = res.Body
		if enc == "br" {
			rd = brotli.NewReader(res.Body)
		} else if rd, err = gzip.NewReader(res.Body); err != nil {
			t.Fatal(err)
		}
		first := make(chan string, 1)
		go func() {
			b := make([]byte, 64)
			n, _ := io.ReadAtLeast(rd, b, len("<html><head><title>first</title></head>"))
			first <- string(b[:n])
		}()
		select {
		case got := <-first:
			if got != "<html><head><title>first</title></head>" {
				t.Errorf("%s: the first flush: %q", enc, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the flushed head never arrived", enc)
		}
		close(next)
		rest, _ := io.ReadAll(rd)
		if !strings.HasSuffix(string(rest), "</body></html>") {
			t.Errorf("%s: the rest: %.60q", enc, rest)
		}
		res.Body.Close()
		srv.Close()
	}
}

// WebSocket upgrades and event streams never go through the compression:
// their handler gets the server's writer, which hijacks and flushes.
func TestCompressLeavesUpgradesAndStreams(t *testing.T) {
	a := New(Only("none"))
	wrapped := make(chan bool, 4)
	a.Get("/ws", func(w http.ResponseWriter, r *http.Request) {
		wrapped <- compressWriterOf(w) != nil
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		typ, b, err := c.Read(r.Context())
		if err == nil {
			c.Write(r.Context(), typ, b)
		}
	})
	a.Get("/events", func(w http.ResponseWriter, r *http.Request) {
		wrapped <- compressWriterOf(w) != nil
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: "+strings.Repeat("x", 2000)+"\n\n")
		http.NewResponseController(w).Flush()
	})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Accept-Encoding": {"br, gzip"}},
	})
	if err != nil {
		t.Fatalf("a WebSocket through the app: %v", err)
	}
	c.Write(ctx, websocket.MessageText, []byte("ping"))
	if _, b, err := c.Read(ctx); err != nil || string(b) != "ping" {
		t.Errorf("the echo: %q %v", b, err)
	}
	c.Close(websocket.StatusNormalClosure, "")
	if <-wrapped {
		t.Error("the upgrade's handler got the compression's writer")
	}

	req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "br, gzip")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(res.Body).ReadString('\n')
	res.Body.Close()
	if res.Header.Get("Content-Encoding") != "" || !strings.HasPrefix(line, "data: xxx") {
		t.Errorf("an event stream: encoding %q, %.20q", res.Header.Get("Content-Encoding"), line)
	}
	if <-wrapped {
		t.Error("an event stream's handler got the compression's writer")
	}

	// The live socket's path, asked as an upgrade, isn't wrapped either.
	r := httptest.NewRequest("GET", liveSocket, nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	if !passes(r) {
		t.Error("an upgrade of the live socket would be compressed")
	}
}

// Fixed content is sent in an encoding made once, its own ETag answering
// revalidations with a 304.
func TestFixedAssetsEncoded(t *testing.T) {
	h := compressApp()
	path := liveScript + "?v=" + liveJSTag
	tags := map[string]string{}
	for _, enc := range []string{"br", "gzip", ""} {
		rec := get(h, "GET", path, "Accept-Encoding", enc)
		if got := rec.Header().Get("Content-Encoding"); got != enc {
			t.Fatalf("live.js with %q: Content-Encoding %q", enc, got)
		}
		if decode(t, enc, rec.Body.Bytes()) != string(liveJS) {
			t.Errorf("live.js in %q differs from the source", enc)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("live.js: Cache-Control %q", cc)
		}
		if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
			t.Errorf("live.js with %q: no Vary", enc)
		}
		if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(rec.Body.Len()) {
			t.Errorf("live.js with %q: Content-Length %q for %d bytes", enc, cl, rec.Body.Len())
		}
		tags[enc] = rec.Header().Get("ETag")
	}
	if tags["br"] == tags["gzip"] || tags["br"] == tags[""] || tags[""] != `"`+liveJSTag+`"` {
		t.Errorf("one ETag per encoding: %v", tags)
	}
	for enc, tag := range tags {
		rec := get(h, "GET", path, "Accept-Encoding", enc, "If-None-Match", tag)
		if rec.Code != http.StatusNotModified {
			t.Errorf("live.js in %q revalidated: %d", enc, rec.Code)
		}
	}
	if rec := get(h, "GET", path, "Accept-Encoding", "gzip", "If-None-Match", tags["br"]); rec.Code != http.StatusOK {
		t.Errorf("the brotli ETag matched a gzip request: %d", rec.Code)
	}
	if rec := get(h, "GET", path, "Accept-Encoding", "br", "Range", "bytes=0-9"); rec.Code != http.StatusPartialContent || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != string(liveJS[:10]) {
		t.Errorf("a Range of live.js: %d %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
}

// An encoding vuka build registered is sent as it is.
func TestRegisteredEncoding(t *testing.T) {
	css := []byte(strings.Repeat(".pet{color:red}\n", 200))
	var b bytes.Buffer
	zw := brotli.NewWriterLevel(&b, 11)
	zw.Write(css)
	zw.Close()
	RegisterCSS("registered.css", css)
	RegisterEncoded(livePrefix+"registered.css", "br", b.Bytes())
	t.Cleanup(func() {
		cssMu.Lock()
		delete(cssReg, "registered.css")
		cssMu.Unlock()
		encodedMu.Lock()
		delete(encodedReg, livePrefix+"registered.css")
		encodedMu.Unlock()
	})
	h := compressApp()
	rec := get(h, "GET", livePrefix+"registered.css", "Accept-Encoding", "br")
	if !bytes.Equal(rec.Body.Bytes(), b.Bytes()) || rec.Header().Get("Content-Encoding") != "br" {
		t.Errorf("the registered encoding wasn't sent: %q, %d bytes", rec.Header().Get("Content-Encoding"), rec.Body.Len())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type %q", ct)
	}
}

func TestStaticCaching(t *testing.T) {
	css := []byte(strings.Repeat(".pet{color:red}\n", 200))
	var br bytes.Buffer
	zw := brotli.NewWriterLevel(&br, 11)
	zw.Write(css)
	zw.Close()
	files := fstest.MapFS{
		"forms.js":            {Data: []byte(strings.Repeat("console.log('forms');\n", 100)), ModTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		"logo.svg":            {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)},
		"app.3f9a1c2b.css":    {Data: css},
		"app.3f9a1c2b.css.br": {Data: br.Bytes()},
		"photo.png":           {Data: bytes.Repeat([]byte{1}, 4096)},
		"index.html":          {Data: []byte("<!doctype html><title>static</title>")},
	}
	a := New(Only("none"))
	a.Static("/static", files)
	h := a.Handler()

	rec := get(h, "GET", "/static/logo.svg")
	tag := rec.Header().Get("ETag")
	if tag == "" || rec.Header().Get("Cache-Control") != staticRevalidate {
		t.Fatalf("an unhashed file: ETag %q, Cache-Control %q", tag, rec.Header().Get("Cache-Control"))
	}
	if rec := get(h, "GET", "/static/logo.svg", "If-None-Match", tag); rec.Code != http.StatusNotModified {
		t.Errorf("revalidating an unhashed file: %d", rec.Code)
	}
	rec = get(h, "GET", "/static/forms.js", "Accept-Encoding", "gzip")
	if rec.Header().Get("Last-Modified") == "" || rec.Header().Get("Content-Encoding") != "gzip" ||
		decode(t, "gzip", rec.Body.Bytes()) != string(files["forms.js"].Data) {
		t.Errorf("forms.js: Last-Modified %q, Content-Encoding %q", rec.Header().Get("Last-Modified"), rec.Header().Get("Content-Encoding"))
	}
	if rec := get(h, "GET", "/static/forms.js", "Accept-Encoding", "gzip", "If-Modified-Since", rec.Header().Get("Last-Modified")); rec.Code != http.StatusNotModified {
		t.Errorf("If-Modified-Since: %d", rec.Code)
	}
	if rec := get(h, "GET", "/static/forms.js", "Accept-Encoding", "gzip", "If-None-Match", rec.Header().Get("ETag")); rec.Code != http.StatusNotModified {
		t.Errorf("revalidating the gzip of forms.js: %d", rec.Code)
	}
	rec = get(h, "GET", "/static/app.3f9a1c2b.css", "Accept-Encoding", "br, gzip")
	if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || rec.Header().Get("Content-Encoding") != "br" ||
		!bytes.Equal(rec.Body.Bytes(), br.Bytes()) || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") {
		t.Errorf("a hashed file with its .br: Cache-Control %q, Content-Encoding %q, type %q, same bytes %v",
			rec.Header().Get("Cache-Control"), rec.Header().Get("Content-Encoding"), rec.Header().Get("Content-Type"), bytes.Equal(rec.Body.Bytes(), br.Bytes()))
	}
	rec = get(h, "GET", "/static/photo.png", "Accept-Encoding", "br, gzip")
	if rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("ETag") == "" || rec.Body.Len() != 4096 {
		t.Errorf("an image: Content-Encoding %q, ETag %q", rec.Header().Get("Content-Encoding"), rec.Header().Get("ETag"))
	}
	if rec := get(h, "GET", "/static/", "Accept-Encoding", "br"); !strings.Contains(rec.Body.String(), "static") {
		t.Errorf("a directory's index.html: %d %q", rec.Code, rec.Body.String())
	}
	if rec := get(h, "GET", "/static/missing.css"); rec.Code != http.StatusNotFound {
		t.Errorf("a missing file: %d", rec.Code)
	}
}

func TestStaticHashed(t *testing.T) {
	for name, want := range map[string]bool{
		"app.3f9a1c2b.css": true, "index-BkL2x9aQ.js": true, "chunk.deadbeef01.js": true,
		"logo.svg": false, "forms.js": false, "jquery-3.7.1.min.js": false, "jquery-ui-1-12-1.js": false,
		"report-20260101.pdf": false, "bootstrap-datepicker.js": false, "phosphor.woff2": false,
	} {
		if got := staticHashed(name); got != want {
			t.Errorf("staticHashed(%q) = %v", name, got)
		}
	}
}

// listPage is a page of 200 rows, as a list page of a vuka new app renders
// them: the measure of what compression saves on the wire.
func listPage() ui.Node {
	rows := make([]ui.Node, 0, 200)
	for i := range 200 {
		rows = append(rows, ui.El("tr", []ui.Attr{{Name: "class", Value: "border-b transition-colors hover:bg-muted/50 data-[state=selected]:bg-muted"}},
			ui.El("td", []ui.Attr{{Name: "class", Value: "p-2 align-middle font-medium"}}, ui.Text(fmt.Sprint(i+1))),
			ui.El("td", []ui.Attr{{Name: "class", Value: "p-2 align-middle"}}, ui.Text(fmt.Sprintf("Pet number %d", (i*7919)%1000))),
			ui.El("td", []ui.Attr{{Name: "class", Value: "p-2 align-middle"}}, ui.El("span", []ui.Attr{{Name: "class", Value: "inline-flex items-center rounded-md border px-2.5 py-0.5 text-xs font-semibold"}}, ui.Text([]string{"available", "pending", "sold"}[i%3]))),
			ui.El("td", []ui.Attr{{Name: "class", Value: "p-2 align-middle text-right"}},
				ui.El("a", []ui.Attr{{Name: "href", Value: fmt.Sprintf("/pets/%d/edit", i+1)}, {Name: "class", Value: "inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-medium h-8 px-3 hover:bg-accent"}}, ui.Text("Edit")),
				ui.El("button", []ui.Attr{{Name: "type", Value: "submit"}, {Name: "class", Value: "inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-medium h-8 px-3 text-destructive hover:bg-destructive/10"}}, ui.Text("Delete")))))
	}
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Pets"))),
		ui.El("body", nil, ui.El("table", []ui.Attr{{Name: "class", Value: "w-full caption-bottom text-sm"}}, ui.El("tbody", nil, rows...))))
}

// TestListPageWireSize reports a 200-row page's bytes on the wire: as
// written, in gzip and in brotli.
func TestListPageWireSize(t *testing.T) {
	a := New(Only("none"))
	a.Get("/pets", listPage)
	h := a.Handler()
	sizes := map[string]int{}
	var page string
	for _, enc := range []string{"", "gzip", "br"} {
		rec := get(h, "GET", "/pets", "Accept-Encoding", enc)
		if rec.Header().Get("Content-Encoding") != enc {
			t.Fatalf("the list page with %q: Content-Encoding %q", enc, rec.Header().Get("Content-Encoding"))
		}
		sizes[enc] = rec.Body.Len()
		got := decode(t, enc, rec.Body.Bytes())
		if page == "" {
			page = got
		} else if got != page {
			t.Errorf("the list page in %q differs", enc)
		}
	}
	t.Logf("a 200-row page on the wire: %d B as written, %d B gzip, %d B brotli", sizes[""], sizes["gzip"], sizes["br"])
	if sizes["br"]*10 > sizes[""] || sizes["gzip"]*8 > sizes[""] || sizes["br"] > sizes["gzip"] {
		t.Errorf("the list page compresses poorly: %v", sizes)
	}
}

func BenchmarkListPage(b *testing.B) {
	a := New(Only("none"))
	a.Get("/pets", listPage)
	h := a.Handler()
	for _, enc := range []string{"identity", "gzip", "br"} {
		b.Run(enc, func(b *testing.B) {
			req := httptest.NewRequest("GET", "/pets", nil)
			req.Header.Set("Accept-Encoding", enc)
			var wire int
			for b.Loop() {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				wire = rec.Body.Len()
			}
			b.ReportMetric(float64(wire), "wire-B")
		})
	}
}

// The toolbar's Request panel shows the body's size and its encoding's.
func TestToolbarSentSize(t *testing.T) {
	if got := requestOf(tbRecord{Body: 276_000, Wire: 9_400, Encoding: "br"}).SentSize; got != "276.0 kB → 9400 B br (3%)" {
		t.Errorf("an encoded response: %q", got)
	}
	if got := requestOf(tbRecord{Body: 500, Wire: 500}).SentSize; got != "500 B" {
		t.Errorf("a response sent as written: %q", got)
	}
	var s *wireStats
	if enc, body, wire := s.get(); enc != "" || body != 0 || wire != 0 {
		t.Error("no stats without compression")
	}
	a := &App{compressor: newCompressor(DefaultCompression), tb: &toolbar{}} // the toolbar asks for the stats
	h := a.compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s = r.Context().Value(wireKey{}).(*wireStats)
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, bigText)
	}))
	rec := get(h, "GET", "/", "Accept-Encoding", "br")
	if enc, body, wire := s.get(); enc != "br" || body != int64(len(bigText)) || wire != int64(rec.Body.Len()) {
		t.Errorf("the stats: %q %d %d; sent %d of %d", enc, body, wire, rec.Body.Len(), len(bigText))
	}
}
