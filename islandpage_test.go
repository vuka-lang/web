package web

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
)

// applicantDB is a dependency of a page backed by its island.
type applicantDB struct{ names []string }

func (d *applicantDB) find(q string) []string {
	var out []string
	for _, n := range d.names {
		if strings.Contains(strings.ToLower(n), strings.ToLower(q)) {
			out = append(out, n)
		}
	}
	return out
}

// applicants is a stateless page backed by its island: Q bound to ?q=, Rows
// a prop, db the server's own.
type applicants struct {
	Q      string   `json:"q" url:"q"`
	Rows   []string `json:"rows"`
	Mounts int      `json:"mounts"`
	db     *applicantDB
	secret string
}

func (a *applicants) Mount(ctx context.Context) error {
	a.Mounts++
	a.secret = "never sent"
	a.Rows = a.db.find(a.Q)
	return nil
}

func (a *applicants) Search(ctx context.Context, q string) error {
	if len(q) == 1 {
		return Invalid{"q": "type at least 2 letters"}
	}
	a.Q, a.Rows = q, a.db.find(q)
	return nil
}

func (a *applicants) Clear() { a.Q, a.Rows = "", nil }

func (a *applicants) Away() error { return Redirect("/elsewhere") }

func (a *applicants) Boom() error { return http.ErrAbortHandler }

// liveApplicants is the live variant: ui.Live embedded; its Render is
// the compiler's, written here.
type liveApplicants struct {
	ui.Live
	Q    string   `json:"q" url:"q"`
	Rows []string `json:"rows"`
	db   *applicantDB
}

func (a *liveApplicants) Render() ui.Node { return ui.IslandPage(a) }

func (a *liveApplicants) Mount() { a.Rows = a.db.find(a.Q) }

func (a *liveApplicants) Search(q string) { a.Q, a.Rows = q, a.db.find(q) }

func init() {
	ui.Island("components/Applicants.vue")(vuka.TypeOf[applicants]("web.applicants"))
	ui.Island("components/Applicants.vue")(vuka.TypeOf[liveApplicants]("web.liveApplicants"))
}

func pageApp(opts ...Option) *App {
	db := &applicantDB{names: []string{"Ada", "Alan", "Grace"}}
	return New(append([]Option{Only("none"), Provide(func() *applicantDB { return db })}, opts...)...).
		Page("/applicants", &applicants{}).
		Page("/live/applicants", &liveApplicants{})
}

var (
	propsAttr = regexp.MustCompile(`data-vk-props="([^"]*)"`)
	sigAttr   = regexp.MustCompile(`data-vk-sig="([^"]*)"`)
)

// rendered is a page's island props and signature.
func rendered(t *testing.T, body string) (props map[string]any, raw, sig string) {
	t.Helper()
	m := propsAttr.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no data-vk-props in %s", body)
	}
	raw = html.UnescapeString(m[1])
	if err := json.Unmarshal([]byte(raw), &props); err != nil {
		t.Fatal(err)
	}
	if s := sigAttr.FindStringSubmatch(body); s != nil {
		sig = html.UnescapeString(s[1])
	}
	return props, raw, sig
}

func callBody(method string, args []any, props, sig string) string {
	b, _ := json.Marshal(map[string]any{"method": method, "args": args, "props": props, "sig": sig})
	return string(b)
}

func TestIslandPageStateless(t *testing.T) {
	h := pageApp().Handler()
	code, body, _ := do(t, h, "GET", "/applicants?q=a", "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	props, raw, sig := rendered(t, body)
	if props["q"] != "a" || props["mounts"] != 1.0 || len(props["rows"].([]any)) != 3 {
		t.Errorf("props %v", props)
	}
	for _, no := range []string{"never sent", "secret", "db"} {
		if strings.Contains(raw, no) {
			t.Errorf("an unexported field reached the props: %s", raw)
		}
	}
	for _, want := range []string{`data-vk-island="applicants"`, `data-vk-client="load"`, `data-vk-call="/applicants"`,
		`data-vk-methods="[&#34;away&#34;,&#34;boom&#34;,&#34;clear&#34;,&#34;search&#34;]"`, `data-vk-url="{&#34;q&#34;:&#34;q&#34;}"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "data-vk-on-m-") {
		t.Errorf("a stateless page registers no live handlers: %s", body)
	}

	// A call: rebuilt from the signed props, the dependency injected again,
	// Mount and the method run, the new props signed.
	code, resp, _ := do(t, h, "POST", "/applicants", callBody("search", []any{"gr"}, raw, sig), "Content-Type", "application/json")
	var out struct{ Props, Sig string }
	if code != 200 || json.Unmarshal([]byte(resp), &out) != nil {
		t.Fatalf("call: %d %s", code, resp)
	}
	var np map[string]any
	json.Unmarshal([]byte(out.Props), &np)
	if np["q"] != "gr" || np["mounts"] != 2.0 || len(np["rows"].([]any)) != 1 {
		t.Errorf("after search: %v", np)
	}
	// The new props and signature make the next call.
	code, resp, _ = do(t, h, "POST", "/applicants", callBody("clear", nil, out.Props, out.Sig), "Content-Type", "application/json")
	if code != 200 || !strings.Contains(resp, `\"q\":\"\"`) {
		t.Errorf("clear: %d %s", code, resp)
	}

	// Field errors, a redirect, a hidden error.
	expect(t, h, "POST", "/applicants", callBody("search", []any{"g"}, raw, sig), 422, `"fields":{"q":"type at least 2 letters"}`, "Content-Type", "application/json")
	expect(t, h, "POST", "/applicants", callBody("away", nil, raw, sig), 200, `"redirect":"/elsewhere"`, "Content-Type", "application/json")
	code, resp, _ = do(t, h, "POST", "/applicants", callBody("boom", nil, raw, sig), "Content-Type", "application/json")
	if code != 500 || strings.Contains(resp, "abort") {
		t.Errorf("boom: %d %s", code, resp)
	}
	expect(t, h, "POST", "/applicants", callBody("nope", nil, raw, sig), 400, `has no method`, "Content-Type", "application/json")
}

func TestIslandPageTampering(t *testing.T) {
	h := pageApp().Handler()
	_, body, _ := do(t, h, "GET", "/applicants?q=a", "")
	_, raw, sig := rendered(t, body)
	forged := strings.Replace(raw, `"mounts":1`, `"mounts":99`, 1)
	expect(t, h, "POST", "/applicants", callBody("clear", nil, forged, sig), 403, "aren't the server's", "Content-Type", "application/json")
	expect(t, h, "POST", "/applicants", callBody("clear", nil, raw, sig+"x"), 403, "aren't the server's", "Content-Type", "application/json")
	expect(t, h, "POST", "/applicants", callBody("clear", nil, raw, ""), 403, "aren't the server's", "Content-Type", "application/json")
	// Another app's key signs nothing this one takes.
	expect(t, pageApp().Handler(), "POST", "/applicants", callBody("clear", nil, raw, sig), 403, "aren't the server's", "Content-Type", "application/json")
	// A cross-site call is refused.
	expect(t, h, "POST", "/applicants", callBody("clear", nil, raw, sig), 403, "cross-origin", "Content-Type", "application/json", "Sec-Fetch-Site", "cross-site")
}

func TestIslandPageLive(t *testing.T) {
	h := pageApp().Handler()
	code, body, _ := do(t, h, "GET", "/live/applicants?q=gr", "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	props, raw, _ := rendered(t, body)
	if props["q"] != "gr" || len(props["rows"].([]any)) != 1 || strings.Contains(raw, "db") {
		t.Errorf("props %v", props)
	}
	for _, want := range []string{`data-vk-methods="[&#34;search&#34;]"`, `data-vk-on-m-search="c1:`, `data-vk-token=`} {
		if !strings.Contains(body, want) {
			t.Errorf("live page lacks %s:\n%s", want, body)
		}
	}
}

func TestIslandPageNeedsIsland(t *testing.T) {
	type plain struct{ X int }
	if err := New(Only("none")).Page("/p", &plain{}).Build(); err == nil || !strings.Contains(err.Error(), "add @ui.Island") {
		t.Errorf("got %v", err)
	}
}
