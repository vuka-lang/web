package web

import (
	"maps"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/web/internal/chrome"
)

// withCSS registers css as the app's stylesheet for the test.
func withCSS(t *testing.T, css string) {
	t.Helper()
	cssMu.Lock()
	saved := maps.Clone(cssReg)
	cssReg[cssName] = []byte(css)
	cssMu.Unlock()
	t.Cleanup(func() { cssMu.Lock(); cssReg = saved; cssMu.Unlock() })
}

func doc(body string) ui.Node {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("t"))), ui.El("body", nil, ui.Safe(body)))
}

func cssApp(opts ...Option) *App {
	app := liveApp(opts...)
	app.Get("/doc", func() ui.Node { return doc(`<p class="text-red-500">hi</p>`) })
	app.Get("/linked", func() ui.Node {
		return ui.El("html", nil, ui.El("head", nil, ui.Safe(`<link rel="stylesheet" href="/_vuka/app.css">`)), ui.El("body", nil))
	})
	app.Get("/bodyonly", func() ui.Node { return ui.El("body", nil, ui.Text("x")) })
	app.Get("/view", func() Page { return Page{Title: "T"} }, View(vuka.FileOf("css:p.html", "<html><head></head><body>{{.Title}}</body></html>")))
	app.Get("/viewfrag", func() Page { return Page{Title: "T"} }, View(vuka.FileOf("css:f.html", "<p>{{.Title}}</p>")))
	app.Post("/doc", func() ui.Node { return doc("posted") })
	return app
}

var linkRE = regexp.MustCompile(`<link rel="stylesheet" href="/_vuka/app\.css\?v=([\w-]*)">`)

func TestCSSLink(t *testing.T) {
	withCSS(t, ".text-red-500{color:red}")
	h := cssApp().Handler()
	tag := cssTag([]byte(".text-red-500{color:red}"))
	for _, path := range []string{"/doc", "/counter", "/view", "/bodyonly"} {
		_, body, _ := do(t, h, "GET", path, "")
		m := linkRE.FindAllStringSubmatch(body, -1)
		if len(m) != 1 || m[0][1] != tag {
			t.Errorf("GET %s: links %v, want one tagged %s: %s", path, m, tag, body)
		}
		if head := strings.Index(body, "</head>"); head >= 0 && strings.Index(body, "<link") > head {
			t.Errorf("GET %s: the link isn't in the head: %s", path, body)
		}
		if strings.Contains(body, "app-css.js") {
			t.Errorf("GET %s: the development script on a built stylesheet: %s", path, body)
		}
	}
	for _, path := range []string{"/plain", "/json", "/viewfrag"} {
		if _, body, _ := do(t, h, "GET", path, ""); strings.Contains(body, "app.css") {
			t.Errorf("GET %s: a fragment got the stylesheet: %s", path, body)
		}
	}
	if _, body, _ := do(t, h, "POST", "/doc", ""); strings.Contains(body, "app.css") {
		t.Errorf("POST /doc got the stylesheet: %s", body)
	}
	if _, body, _ := do(t, h, "GET", "/linked", ""); strings.Count(body, "app.css") != 1 {
		t.Errorf("a page linking the stylesheet got another link: %s", body)
	}
}

func TestCSSServe(t *testing.T) {
	withCSS(t, ".a{color:red}")
	cssMu.Lock()
	cssReg["print.css"] = []byte("@media print{}")
	cssMu.Unlock()
	h := cssApp().Handler()
	tag := cssTag([]byte(".a{color:red}"))
	code, body, hdr := do(t, h, "GET", "/_vuka/app.css?v="+tag, "")
	if code != 200 || body != ".a{color:red}" || hdr.Get("Content-Type") != "text/css; charset=utf-8" ||
		hdr.Get("ETag") != `"`+tag+`"` || !strings.Contains(hdr.Get("Cache-Control"), "immutable") {
		t.Errorf("tagged: %d %q %v", code, body, hdr)
	}
	if _, _, hdr := do(t, h, "GET", "/_vuka/app.css", ""); hdr.Get("Cache-Control") != "no-cache" {
		t.Errorf("untagged: %v", hdr)
	}
	if code, _, _ := do(t, h, "GET", "/_vuka/app.css", "", "If-None-Match", `"`+tag+`"`); code != 304 {
		t.Errorf("revalidated: %d, want 304", code)
	}
	if code, body, _ := do(t, h, "GET", "/_vuka/print.css", ""); code != 200 || body != "@media print{}" {
		t.Errorf("print.css: %d %q", code, body)
	}
	if code, _, _ := do(t, h, "GET", "/_vuka/app-css.js", ""); code != 404 {
		t.Errorf("the development script without development: %d", code)
	}
}

func TestNoTailwind(t *testing.T) {
	withCSS(t, ".a{}")
	h := cssApp(NoTailwind()).Handler()
	if _, body, _ := do(t, h, "GET", "/doc", ""); strings.Contains(body, "app.css") {
		t.Errorf("NoTailwind: %s", body)
	}
	if code, _, _ := do(t, h, "GET", "/_vuka/app.css", ""); code != 404 {
		t.Errorf("NoTailwind serves the stylesheet: %d", code)
	}
	t.Setenv(CSSEnv, filepath.Join(t.TempDir(), "app.css"))
	if _, body, _ := do(t, cssApp(NoTailwind()).Handler(), "GET", "/doc", ""); strings.Contains(body, "app.css") {
		t.Errorf("NoTailwind with %s: %s", CSSEnv, body)
	}
	if _, body, _ := do(t, cssApp(NoTailwind(), Tailwind("ui/site.css")).Handler(), "GET", "/doc", ""); !strings.Contains(body, "app.css") {
		t.Errorf("Tailwind after NoTailwind: %s", body)
	}
}

func TestNoStylesheet(t *testing.T) {
	cssMu.Lock()
	saved := cssReg
	cssReg = map[string][]byte{}
	cssMu.Unlock()
	t.Cleanup(func() { cssMu.Lock(); cssReg = saved; cssMu.Unlock() })
	h := cssApp().Handler()
	if _, body, _ := do(t, h, "GET", "/doc", ""); strings.Contains(body, "app.css") {
		t.Errorf("no stylesheet, yet a link: %s", body)
	}
}

func TestCSSDevFile(t *testing.T) {
	withCSS(t, ".built{}") // the file wins
	file := filepath.Join(t.TempDir(), "app.css")
	t.Setenv(CSSEnv, file)
	h := cssApp().Handler()
	if code, _, _ := do(t, h, "GET", "/_vuka/app.css", ""); code != 404 {
		t.Errorf("before the first build: %d", code)
	}
	_, body, _ := do(t, h, "GET", "/doc", "")
	if !linkRE.MatchString(body) || !strings.Contains(body, `<script src="/_vuka/app-css.js" defer></script></head>`) {
		t.Errorf("a page before the first build: %s", body)
	}
	tagOf := func() string {
		_, body, _ := do(t, h, "GET", "/doc", "")
		return linkRE.FindStringSubmatch(body)[1]
	}
	for _, css := range []string{".one{color:red}", ".two{color:blue}"} {
		os.WriteFile(file, []byte(css), 0o644)
		time.Sleep(150 * time.Millisecond)
		code, body, hdr := do(t, h, "HEAD", "/_vuka/app.css", "")
		if code != 200 || hdr.Get("ETag") != `"`+cssTag([]byte(css))+`"` || body != "" {
			t.Errorf("HEAD after writing %s: %d %v", css, code, hdr)
		}
		if _, body, _ := do(t, h, "GET", "/_vuka/app.css", ""); body != css {
			t.Errorf("GET: %q, want %q", body, css)
		}
		if got := tagOf(); got != cssTag([]byte(css)) {
			t.Errorf("the page links %s, want %s", got, cssTag([]byte(css)))
		}
	}
	if code, body, _ := do(t, h, "GET", "/_vuka/app-css.js", ""); code != 200 || !strings.Contains(body, "/_vuka/app.css") {
		t.Errorf("app-css.js: %d", code)
	}
}

// tailwind builds the CSS for the classes in src with the Tailwind CLI,
// skipping the test without one.
func tailwind(t *testing.T, src, out string) {
	t.Helper()
	bin, err := exec.LookPath("tailwindcss")
	if err != nil {
		t.Skip("no tailwindcss on PATH")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "page.html"), []byte(src), 0o644)
	os.WriteFile(filepath.Join(dir, "in.css"), []byte(`@import "tailwindcss" source(none); @source "./page.html";`), 0o644)
	cmd := exec.Command(bin, "-i", filepath.Join(dir, "in.css"), "-o", out+".tmp")
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tailwindcss: %v\n%s", err, b)
	}
	os.Rename(out+".tmp", out)
}

type swatch struct {
	ui.Live
	n int
}

func (s *swatch) Inc() { s.n++ }
func (s *swatch) Render() ui.Node {
	return ui.El("div", []ui.Attr{{Name: "id", Value: "box"}, {Name: "class", Value: "bg-emerald-500 p-4"}},
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(s.Inc)}}, ui.Text(s.n)))
}

// TestBrowserTailwind renders a live page styled by Tailwind, then rebuilds
// the stylesheet with another class, as vuka run does on a save: the page
// takes it without reloading.
func TestBrowserTailwind(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.css")
	tailwind(t, `<div class="bg-emerald-500 p-4">`, file)
	t.Setenv(CSSEnv, file)
	app := New(Only("none"))
	app.Live("/", &swatch{})
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/")
	bg := `getComputedStyle(document.getElementById("box")).backgroundColor`
	p.Wait(bg + ` !== "rgba(0, 0, 0, 0)"`)
	var green string
	p.Eval(bg, &green)
	p.Eval(`window.marker = 1`, nil)

	tailwind(t, `<div class="bg-rose-500 bg-emerald-500 p-4">`, file)
	os.WriteFile(file, []byte(strings.ReplaceAll(mustRead(t, file), ".bg-emerald-500", ".bg-unused")), 0o644)
	os.WriteFile(file, []byte(strings.ReplaceAll(mustRead(t, file), ".bg-rose-500", ".bg-emerald-500")), 0o644)
	p.Wait(bg + ` !== "` + green + `" && window.marker === 1`)
	p.Click("button")
	p.WaitText("#box button", "1")
	var links int
	p.Eval(`document.querySelectorAll('link[href^="/_vuka/app.css"]').length`, &links)
	if links != 1 {
		t.Errorf("%d stylesheet links after the swap, want 1", links)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
