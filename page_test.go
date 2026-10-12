package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/vuka-lang/ui"
)

// benchRow and benchPage are a 12 KB page as the compiler lowers its JSX: a
// layout and a 50-row table, each row's cells with a link and a dynamic class.
type benchRow struct {
	ID                        int
	Name, Email, Role, Status string
}

func benchRows() []benchRow {
	out := make([]benchRow, 50)
	roles := []string{"admin", "editor", "viewer"}
	for i := range out {
		s := strconv.Itoa(i + 1)
		st := "active"
		if i%4 == 0 {
			st = "invited"
		}
		out[i] = benchRow{ID: i + 1, Name: "User number " + s, Email: "user" + s + "@example.com", Role: roles[i%3], Status: st}
	}
	return out
}

// benchLayout and benchPage are the FT lowering the compiler now emits (see
// vuka's frameFT): each frame carries only its dynamics and a closure that
// rebuilds the F tree. The HTML stays byte-identical to the F lowering, which
// TestPagesGolden pins.
func benchLayout(title string, children ui.Node) ui.Node {
	const shape = "Es(E(Es()Ess()E(ht))Es(Es(E(Es(t)tEs(t)))Es(h)Es(t)))"
	return ui.FT(0x4b44aa8876d5918c, shape, func(__d []any) ui.Node {
		return ui.F(0x4b44aa8876d5918c, shape, ui.El("html", []ui.Attr{{Name: "lang", Value: "en"}}, ui.El("head", nil, ui.El("meta", []ui.Attr{{Name: "charset", Value: "utf-8"}}), ui.El("meta", []ui.Attr{{Name: "name", Value: "viewport"}, {Name: "content", Value: "width=device-width, initial-scale=1"}}), ui.El("title", nil, ui.Child(__d[0]), ui.Text(" · Bench"))), ui.El("body", []ui.Attr{{Name: "class", Value: "page"}}, ui.El("header", []ui.Attr{{Name: "class", Value: "top"}}, ui.El("nav", nil, ui.El("a", []ui.Attr{{Name: "href", Value: "/"}}, ui.Text("Home")), ui.Text(" "), ui.El("a", []ui.Attr{{Name: "href", Value: "/page"}}, ui.Text("Users")))), ui.El("main", []ui.Attr{{Name: "class", Value: "content"}}, ui.Child(__d[1])), ui.El("footer", []ui.Attr{{Name: "class", Value: "foot"}}, ui.Text("Benchmark page")))))
	}, title, children)
}

func benchPage() ui.Node {
	rs := benchRows()
	const contentShape = "Es(t)Es(E(E(E(t)E(t)E(t)E(t)E(t)))E(b))"
	const rowShape = "Es(Es(h)Es(h)Es(Ed(h))Es(h)Ed(h))"
	content := ui.FT(0xd553e1cee178f424, contentShape, func(__d []any) ui.Node {
		return ui.F(0xd553e1cee178f424, contentShape, ui.El("h1", []ui.Attr{{Name: "class", Value: "title"}}, ui.Text("Users")), ui.El("table", []ui.Attr{{Name: "class", Value: "table"}}, ui.El("thead", nil, ui.El("tr", nil, ui.El("th", nil, ui.Text("ID")), ui.El("th", nil, ui.Text("Name")), ui.El("th", nil, ui.Text("Email")), ui.El("th", nil, ui.Text("Role")), ui.El("th", nil, ui.Text("Status")))), ui.El("tbody", nil, ui.Child(__d[0]))))
	}, ui.Nodes(func(__add func(ui.Node)) {
		for _, r := range rs {
			__add(ui.FT(0x8b2b40ae54e58b1f, rowShape, func(__d []any) ui.Node {
				return ui.F(0x8b2b40ae54e58b1f, rowShape, ui.El("tr", []ui.Attr{{Name: "class", Value: "row"}}, ui.El("td", []ui.Attr{{Name: "class", Value: "num"}}, ui.Child(__d[0])), ui.El("td", []ui.Attr{{Name: "class", Value: "name"}}, ui.Child(__d[1])), ui.El("td", []ui.Attr{{Name: "class", Value: "email"}}, ui.El("a", []ui.Attr{{Name: "href", Value: __d[2]}}, ui.Child(__d[3]))), ui.El("td", []ui.Attr{{Name: "class", Value: "role"}}, ui.Child(__d[4])), ui.El("td", []ui.Attr{{Name: "class", Value: __d[5]}}, ui.Child(__d[6]))))
			}, strconv.Itoa(r.ID), r.Name, "mailto:"+r.Email, r.Email, r.Role, "status status-"+r.Status, r.Status))
		}
	}))
	return ui.FT(0xaf63de4c8601eff2, "c", func(__d []any) ui.Node {
		return ui.F(0xaf63de4c8601eff2, "c", ui.Child(__d[0]))
	}, benchLayout("Users", content))
}

func pagesApp(opts ...Option) *App {
	app := New(append([]Option{Only("none")}, opts...)...)
	doc := func(body ...ui.Node) ui.Node {
		return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("t"))), ui.El("BODY", nil, body...))
	}
	app.Get("/bench", benchPage)
	app.Get("/keyed", func() ui.Node {
		return doc(ui.El("ul", nil, ui.El("li", []ui.Attr{{Name: "key", Value: 1}}, ui.Text("one")), ui.El("li", []ui.Attr{{Name: "key", Value: "b&c"}}, ui.Text("two"))))
	})
	app.Get("/js", func() ui.Node {
		return doc(ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(ui.JS.Toggle(ui.Ref("menu")))}}, ui.Text("menu")), ui.El("nav", []ui.Attr{{Name: "ref", Value: "menu"}}))
	})
	app.Get("/push", func() ui.Node {
		return doc(ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(ui.JS.Toggle(ui.Ref("menu")).Push(func() {}))}}, ui.Text("menu")))
	})
	app.Get("/handler", func() ui.Node {
		return doc(ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(func() {})}}, ui.Text("+")))
	})
	app.Get("/component", func() ui.Node {
		return doc(ui.Text("before"), ui.Component("pages#1", nil, &counter{Label: "n="}), ui.Text("after"))
	})
	app.Get("/safe", func() ui.Node {
		return doc(ui.Safe(`<b data-vk-js-click="[]">raw</b>`))
	})
	app.Get("/nobody", func() ui.Node { return ui.El("p", nil, ui.Text("fragment")) })
	app.Post("/form", benchPage)
	return app
}

var pageVolatile = regexp.MustCompile(`data-vk-(token|toolbar)="[^"]*"`)

// TestPagesGolden pins the HTML of whole pages, live and not, with and
// without the dev toolbar: a faster page path must not change a byte.
func TestPagesGolden(t *testing.T) {
	var got strings.Builder
	for _, dev := range []bool{false, true} {
		var app *App
		if dev {
			toolbarInTests = true
			app = pagesApp(Dev(), NoTailwind())
			toolbarInTests = false
		} else {
			app = pagesApp()
		}
		h := app.Handler()
		for _, rq := range []struct{ method, path, dest string }{
			{"GET", "/bench", ""}, {"GET", "/keyed", ""}, {"GET", "/js", ""}, {"GET", "/push", ""},
			{"GET", "/handler", ""}, {"GET", "/component", ""}, {"GET", "/safe", ""}, {"GET", "/nobody", ""},
			{"HEAD", "/keyed", ""}, {"POST", "/form", "document"}, {"POST", "/form", "empty"},
		} {
			r := httptest.NewRequest(rq.method, rq.path, nil)
			if rq.dest != "" {
				r.Header.Set("Sec-Fetch-Dest", rq.dest)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			body := pageVolatile.ReplaceAllString(w.Body.String(), `data-vk-$1="…"`)
			got.WriteString("== dev=" + strconv.FormatBool(dev) + " " + rq.method + " " + rq.path + " " + rq.dest + " " + strconv.Itoa(w.Code) + " " + w.Header().Get("Content-Type") + "\n" + body + "\n")
		}
	}
	const golden = "testdata/pages.golden"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("pages render differently:\n%s", got.String())
	}
}

func benchServe(b *testing.B, app *App, path string) {
	h := app.Handler()
	b.ReportAllocs()
	for b.Loop() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusOK {
			b.Fatal(w.Code, w.Body)
		}
	}
}

func BenchmarkPage(b *testing.B)          { benchServe(b, pagesApp(NoTailwind()), "/bench") }
func BenchmarkPageComponent(b *testing.B) { benchServe(b, pagesApp(NoTailwind()), "/component") }
