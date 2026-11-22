package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/uigen"
)

// searchStatic is a plain (non-live) page with one synced state (q, read by
// its Find method) and one browser-local state (open, never read by Go):
// one state over HTTP (§12).
type searchStatic struct {
	q    ui.State[string]
	open ui.State[bool]
}

func init() {
	uigen.SyncFields[searchStatic]("q")
	ui.RegisterPush((*searchStatic).Find)
	ui.RegisterPush((*searchStatic).Reset)
}

func (p *searchStatic) Find() string { return "found:" + p.q.Get() }

func (p *searchStatic) Reset() string { uigen.SrvSet(p.q, ""); return "reset" }

func (p *searchStatic) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Search"))), ui.El("body", nil,
		ui.El("input", []ui.Attr{attr("id", "q"), ui.BindAttr("value", p.q)}),
		ui.El("ul", []ui.Attr{ui.BindAttr("hidden", p.open)}),
		ui.El("button", []ui.Attr{attr("id", "set-q"), attr("onClick", ui.On(ui.JS.Set(p.q, "tea")))}, ui.Text("set q")),
		ui.El("button", []ui.Attr{attr("id", "toggle-open"), attr("onClick", ui.On(ui.JS.Set(p.open, true)))}, ui.Text("open")),
		ui.El("button", []ui.Attr{attr("id", "find"), attr("onClick", ui.On(ui.JS.Push(p.Find).Into(ui.Sel("#out"))))}, ui.Text("find")),
		ui.El("button", []ui.Attr{attr("id", "reset"), attr("onClick", ui.On(ui.JS.Push(p.Reset)))}, ui.Text("reset")),
		ui.El("span", []ui.Attr{attr("id", "out")}),
	)).Render(ctx, w)
}

var stateRefRe = regexp.MustCompile(`&#34;s&#34;:&#34;([^&]+)&#34;`)

// statePushCall posts a push call carrying states.
func statePushCall(t *testing.T, srv *httptest.Server, path, ref, states string) (*http.Response, string) {
	t.Helper()
	v := url.Values{"_vk_args": {"[]"}}
	if states != "" {
		v.Set("_vk_states", states)
	}
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set(PushHeader, ref)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(b)
}

// TestPushStatesHTTP: on a page that isn't live, the browser's dirty synced
// states ride the push request, the method reads them, and the reply
// carries the echoes and the method's own writes, versioned, for the
// browser to adopt as a patch's.
func TestPushStatesHTTP(t *testing.T) {
	a := New(Only("none"))
	a.Get("/search", func() *searchStatic {
		p := &searchStatic{}
		uigen.States(p) // what a .vuka.html page's generated code does
		return p
	}, PageRoute())
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/search")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	page := string(b)
	refs := pushRefRe.FindAllStringSubmatch(page, -1)
	if len(refs) != 2 {
		t.Fatalf("%d push refs in\n%s", len(refs), page)
	}
	find, reset := refs[0][1], refs[1][1]
	var keys []string
	for _, m := range stateRefRe.FindAllStringSubmatch(page, -1) {
		if !slices.Contains(keys, m[1]) {
			keys = append(keys, m[1])
		}
	}
	if len(keys) != 2 {
		t.Fatalf("%d state keys (%v) in\n%s", len(keys), keys, page)
	}
	qKey, openKey := keys[0], keys[1]
	// q's reference is marked synced, open's isn't.
	if !strings.Contains(page, qKey+"&#34;,&#34;sy&#34;:1") {
		t.Errorf("q isn't marked synced:\n%s", page)
	}
	if strings.Count(page, "&#34;sy&#34;:1") != 1 {
		t.Errorf("open is marked synced too:\n%s", page)
	}

	// A push with no states: the method reads the initial value, the reply
	// carries no states.
	if res, body := statePushCall(t, srv, "/search", find, ""); res.StatusCode != 200 || !strings.Contains(body, `"value":"found:"`) || strings.Contains(body, `"states"`) {
		t.Errorf("a push with no states: %d %s", res.StatusCode, body)
	}

	// A carried write lands before the method runs; the reply echoes it
	// under the carried key with its write number.
	res2, body := statePushCall(t, srv, "/search", find, `{"`+qKey+`":{"v":"tea","n":1}}`)
	if res2.StatusCode != 200 || !strings.Contains(body, `"value":"found:tea"`) {
		t.Fatalf("the method didn't read the carried state: %d %s", res2.StatusCode, body)
	}
	if !strings.Contains(body, `"`+qKey+`"`) || !strings.Contains(body, `"bn":1`) || !strings.Contains(body, `"v":"tea"`) || !strings.Contains(body, `"ver":`) {
		t.Errorf("the reply doesn't echo the write versioned: %s", body)
	}

	// The method's own write supersedes: marked sw with its value.
	if _, body := statePushCall(t, srv, "/search", reset, `{"`+qKey+`":{"v":"tea","n":2}}`); !strings.Contains(body, `"sw":1`) || !strings.Contains(body, `"v":""`) {
		t.Errorf("the server write isn't marked sw: %s", body)
	}

	// A write naming the browser-local state is refused.
	if res, body := statePushCall(t, srv, "/search", find, `{"`+openKey+`":{"v":true,"n":1}}`); res.StatusCode == 200 {
		t.Errorf("a write to a browser-local state was applied: %d %s", res.StatusCode, body)
	}

	// States that don't parse are a bad request.
	if res, _ := statePushCall(t, srv, "/search", find, `{`); res.StatusCode != 400 {
		t.Errorf("unparsed states: %d", res.StatusCode)
	}
}
