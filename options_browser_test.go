package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// geoDB is the cities the shop's pages offer: a dependency of the methods
// loading them.
type geoDB struct{ calls atomic.Int32 }

func (g *geoDB) cities(country string) []ui.Option {
	g.calls.Add(1)
	switch country {
	case "tz":
		return []ui.Option{{Value: "dar", Label: "Dar es Salaam"}, {Value: "aru", Label: "Arusha"}}
	case "ke":
		return []ui.Option{{Value: "nbo", Label: "Nairobi"}, {Value: "aru", Label: "Arusha (KE)"}}
	}
	return nil
}

// checkoutPage is a checkout page that isn't live: its country select loads the
// city select's options from CitiesOf; its broken select from Broken, which
// fails.
type checkoutPage struct{}

func (p *checkoutPage) CitiesOf(ctx context.Context, country string, g *geoDB) ([]ui.Option, error) {
	return g.cities(country), nil
}

func (p *checkoutPage) Broken(ctx context.Context) ([]ui.Option, error) {
	return nil, errors.New("down")
}

func (p *checkoutPage) Sizes(kind string) []ui.OptionGroup {
	return []ui.OptionGroup{{Label: kind, Options: []ui.Option{{Value: kind + "-s", Label: "S"}, {Value: kind + "-m", Label: "M"}}}}
}

func init() {
	ui.RegisterOptions((*checkoutPage).CitiesOf)
	ui.RegisterOptions((*checkoutPage).Broken)
	ui.RegisterOptions((*checkoutPage).Sizes)
	ui.RegisterOptions((*regionPage).Towns)
	ui.RegisterOptions((*liveShop).CitiesOf)
}

func attr(name string, v any) ui.Attr { return ui.Attr{Name: name, Value: v} }

func opt(v, label string, selected bool) ui.Node {
	return ui.El("option", []ui.Attr{attr("value", v), attr("selected", selected)}, ui.Text(label))
}

// enhancer is a script watching the city select's options, as a library
// enhancing a select does: it mirrors them into #mirror.
const enhancer = `<script>
new MutationObserver(() => {
  document.getElementById("mirror").textContent = Array.from(document.getElementById("city").options, (o) => o.value).join(",");
}).observe(document.getElementById("city"), { childList: true, subtree: true });
document.getElementById("city").addEventListener("change", () => { window.cityChanges = (window.cityChanges || 0) + 1; });
</script>`

func (p *checkoutPage) body() ui.Node {
	return ui.Fragment(
		ui.El("select", []ui.Attr{attr("id", "country"), attr("name", "country"), attr("onChange", ui.On(ui.JS.Options(ui.Sel("#city"), p.CitiesOf, ui.This.Value())))},
			opt("", "Country", true), opt("tz", "Tanzania", false), opt("ke", "Kenya", false), opt("ug", "Uganda", false)),
		ui.El("select", []ui.Attr{attr("id", "city"), attr("name", "city")}, opt("", "City", false), opt("aru", "Arusha", true)),
		ui.El("p", []ui.Attr{attr("id", "mirror")}),
		ui.El("button", []ui.Attr{attr("id", "broken"), attr("onClick", ui.On(ui.JS.Options(ui.Sel("#city"), p.Broken).OnError(ui.JS.Show(ui.Sel("#err")))))}, ui.Text("broken")),
		ui.El("button", []ui.Attr{attr("id", "broken-quiet"), attr("onClick", ui.On(ui.JS.Options(ui.Sel("#city"), p.Broken)))}, ui.Text("broken quietly")),
		ui.El("p", []ui.Attr{attr("id", "err"), attr("hidden", true)}, ui.Text("Couldn't load the cities")),
		ui.El("select", []ui.Attr{attr("id", "size")}),
		ui.El("button", []ui.Attr{attr("id", "sizes"), attr("onClick", ui.On(ui.JS.On(ui.Sel("#size")).Options(p.Sizes, "shirt")))}, ui.Text("sizes")),
		ui.Safe(enhancer))
}

func (p *checkoutPage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Shop"))), ui.El("body", nil, ui.El("main", nil, p.body()))).Render(ctx, w)
}

// liveShop is the same page, live: a click re-renders it.
type liveShop struct {
	ui.Live
	clicks int
}

func (p *liveShop) CitiesOf(ctx context.Context, country string, g *geoDB) ([]ui.Option, error) {
	return g.cities(country), nil
}

func (p *liveShop) Inc() { p.clicks++ }

func (p *liveShop) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("select", []ui.Attr{attr("id", "country"), attr("name", "country"), attr("onChange", ui.On(ui.JS.Options(ui.Sel("#city"), p.CitiesOf, ui.This.Value())))},
			opt("", "Country", true), opt("tz", "Tanzania", false), opt("ke", "Kenya", false)),
		ui.El("select", []ui.Attr{attr("id", "city"), attr("name", "city"), attr("data-clicks", p.clicks)}, opt("", "City", false), opt("aru", "Arusha", true)),
		ui.El("p", []ui.Attr{attr("id", "mirror")}),
		ui.El("button", []ui.Attr{attr("id", "inc"), attr("onClick", ui.On(p.Inc))}, ui.Text("+")),
		ui.El("p", []ui.Attr{attr("id", "clicks")}, ui.Textf("clicks %d", p.clicks)),
		ui.Safe(enhancer))
}

// regionPage is a folder page: its path field bound for its Towns, its
// guard refusing a request with ?closed, its mount hook (its Load) noted.
type regionPage struct {
	Region string
	log    *accessLog
}

func newRegionPage(l *accessLog) *regionPage { return &regionPage{log: l} }

func (p *regionPage) LiveSchema(l *ui.LiveSchema) { l.Mount(p.mount) }
func (p *regionPage) mount(context.Context) error { p.log.note("mount"); return nil }

func (p *regionPage) Towns(ctx context.Context, n int) []ui.Option {
	p.log.note("towns " + p.Region)
	return []ui.Option{{Value: fmt.Sprint(n), Label: p.Region + " town " + fmt.Sprint(n)}}
}

func (p *regionPage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("select", []ui.Attr{attr("id", "town"), attr("onChange", ui.On(ui.JS.Options(ui.This, p.Towns, ui.This.Value())))}).Render(ctx, w)
}

type regionGuard struct{ Region string }

func (g *regionGuard) Load(ctx context.Context, l *accessLog) error {
	l.note("guard " + g.Region)
	if Request(ctx).URL.Query().Get("closed") != "" {
		return Forbidden
	}
	return nil
}

func checkoutApp() (*App, *geoDB, *accessLog) {
	g, l := &geoDB{}, &accessLog{errs: map[int]error{}}
	a := New(Only("none"), Provide(func() *geoDB { return g }), Provide(func() *accessLog { return l }))
	a.Get("/shop", func() *checkoutPage { return &checkoutPage{} })
	a.Live("/live-shop", &liveShop{})
	a.Get("/regions/{region}", newRegionPage, PageRoute(), GuardOf[regionGuard]())
	return a, g, l
}

var optionsRefRe = regexp.MustCompile(`&#34;m&#34;:&#34;([^&]+)&#34;`)

// optionsCall asks path's page for the options of the Options command its
// render carries (the first, else the i-th).
func optionsCall(t *testing.T, srv *httptest.Server, path string, i int, args string, header ...string) (int, string) {
	t.Helper()
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	refs := optionsRefRe.FindAllStringSubmatch(string(b), -1)
	if len(refs) <= i {
		t.Fatalf("no Options command %d in %s: %s", i, path, b)
	}
	return optionsCallRef(t, srv, path, refs[i][1], args, header...)
}

func optionsCallRef(t *testing.T, srv *httptest.Server, path, ref, args string, header ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	q := req.URL.Query()
	q.Set("_vk_args", args)
	req.URL.RawQuery = q.Encode()
	req.Header.Set(OptionsHeader, ref)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("an options reply is cached: %q", res.Header.Get("Cache-Control"))
	}
	return res.StatusCode, string(b)
}

// TestOptionsCall: a page answers the Options calls its render carries:
// the method's options as JSON, its dependencies injected, its path fields
// bound when it is the page's, its guards run and its Load not; a
// reference another page rendered, or a cross-site call, is refused.
func TestOptionsCall(t *testing.T) {
	a, g, l := checkoutApp()
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	code, body := optionsCall(t, srv, "/shop", 0, `["tz"]`)
	if code != 200 || body != `{"options":[{"value":"dar","label":"Dar es Salaam"},{"value":"aru","label":"Arusha"}]}`+"\n" || g.calls.Load() != 1 {
		t.Errorf("CitiesOf: %d %s (%d calls)", code, body, g.calls.Load())
	}
	code, body = optionsCall(t, srv, "/shop", 1, `[]`)
	if code != 500 || !strings.Contains(body, `"error"`) {
		t.Errorf("a method failing: %d %s", code, body)
	}
	code, body = optionsCall(t, srv, "/shop", 0, `["tz", "x"]`)
	if code != 400 || !strings.Contains(body, "takes 1 arguments, not 2") {
		t.Errorf("too many arguments: %d %s", code, body)
	}
	code, body = optionsCall(t, srv, "/shop", 3, `["shirt"]`)
	if code != 200 || !strings.Contains(body, `{"group":"shirt","options":[{"value":"shirt-s","label":"S"}`) {
		t.Errorf("Sizes: %d %s", code, body)
	}
	code, _ = optionsCall(t, srv, "/shop", 0, `["tz"]`, "Sec-Fetch-Site", "cross-site")
	if code != 403 {
		t.Errorf("a cross-site call: %d", code)
	}

	// the folder page's own method: bound from the path, its guard run, its
	// mount hook not
	l.take()
	code, body = optionsCall(t, srv, "/regions/north", 0, `["3"]`)
	if code != 200 || body != `{"options":[{"value":"3","label":"north town 3"}]}`+"\n" {
		t.Errorf("Towns: %d %s", code, body)
	}
	if got := l.take(); got != "guard north,mount,guard north,towns north" { // the GET, then the call
		t.Errorf("ran %s", got)
	}
	// a reference is the route's: good on its other paths, not another route's
	res, _ := http.Get(srv.URL + "/regions/north")
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	ref := optionsRefRe.FindStringSubmatch(string(b))[1]
	if code, body := optionsCallRef(t, srv, "/regions/south", ref, `["3"]`); code != 200 || !strings.Contains(body, "south town 3") {
		t.Errorf("another path of the route: %d %s", code, body)
	}
	if code, body := optionsCallRef(t, srv, "/shop", ref, `["3"]`); code != 400 || !strings.Contains(body, "names no method this page calls") {
		t.Errorf("another route's reference: %d %s", code, body)
	}
	l.take()
	if code, _ := optionsCallRef(t, srv, "/regions/north?closed=1", ref, `["1"]`); code != 403 {
		t.Errorf("a refusing guard: %d", code)
	}
	if got := l.take(); strings.Contains(got, "towns") {
		t.Errorf("ran %s behind a refusing guard", got)
	}
	// without the header, the page as it is
	res, _ = http.Get(srv.URL + "/shop?_vk_args=x")
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Errorf("a GET without Vk-Options: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res.Body.Close()
}

// TestBrowserOptions: a select's options follow another's on a page that
// isn't live and on a live one: replaced (the placeholder kept), the value
// kept while offered and cleared once it isn't, change fired, a script
// watching the options seeing them; a failure runs OnError or is logged.
func TestBrowserOptions(t *testing.T) {
	a, _, _ := checkoutApp()
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	for _, path := range []string{"/shop", "/live-shop"} {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			var p *chrome.Page
			if path == "/shop" {
				p = b.Visit(srv.URL + path)
				var state any
				p.Eval(`document.documentElement.dataset.vkState || ""`, &state)
				if state == "connected" {
					t.Fatalf("the page connected")
				}
			} else {
				p = b.Open(srv.URL + path)
			}
			cities := `[document.querySelector("#city").value, Array.from(document.querySelector("#city").options, (o) => o.value + ":" + o.text).join(","), window.cityChanges || 0, document.querySelector("#mirror").textContent]`
			var got any
			if path == "/shop" {
				// Arusha is chosen, and Tanzania offers it: kept
				p.Eval(`{ const c = document.querySelector("#country"); c.value = "tz"; c.dispatchEvent(new Event("change", { bubbles: true })) }`, nil)
				p.Wait(`document.querySelector("#city").options.length === 3`)
				p.Eval(cities, &got)
				if g, _ := got.([]any); fmt.Sprint(g) != "[aru :City,dar:Dar es Salaam,aru:Arusha 1 ,dar,aru]" {
					t.Errorf("after Tanzania: %v", got)
				}
			} else {
				p.Eval(`{ const c = document.querySelector("#country"); c.value = "tz"; c.dispatchEvent(new Event("change", { bubbles: true })) }`, nil)
				p.Wait(`document.querySelector("#city").options[1].value === "dar"`)
				// a re-render of the page keeps the options the command loaded,
				// while the server renders the ones it rendered before
				p.Click("#inc")
				p.WaitText("#clicks", "clicks 1")
				p.Eval(cities, &got)
				if g, _ := got.([]any); fmt.Sprint(g) != "[aru :City,dar:Dar es Salaam,aru:Arusha 1 ,dar,aru]" {
					t.Errorf("after Tanzania and a re-render: %v", got)
				}
			}
			// Kenya offers Arusha too; Uganda nothing: cleared to the placeholder
			p.Eval(`{ const c = document.querySelector("#country"); c.value = "ke"; c.dispatchEvent(new Event("change", { bubbles: true })) }`, nil)
			p.Wait(`document.querySelector("#city").options[1].value === "nbo"`)
			p.Eval(cities, &got)
			if g, _ := got.([]any); fmt.Sprint(g) != "[aru :City,nbo:Nairobi,aru:Arusha (KE) 2 ,nbo,aru]" {
				t.Errorf("after Kenya: %v", got)
			}
			p.Eval(`{ const c = document.querySelector("#country"); c.value = ""; c.dispatchEvent(new Event("change", { bubbles: true })) }`, nil)
			p.Wait(`document.querySelector("#city").options.length === 1`)
			p.Eval(cities, &got)
			if g, _ := got.([]any); fmt.Sprint(g) != "[ :City 3 ]" {
				t.Errorf("after no country: %v", got)
			}
			if path != "/shop" {
				return
			}
			// a failure: OnError's chain, or logged with the options left
			p.Click("#broken")
			p.Wait(`!document.querySelector("#err").hidden`)
			p.Eval(`window.vkErrors = []; document.addEventListener("vk:error", (e) => vkErrors.push(e.detail.error))`, nil)
			p.Click("#broken-quiet")
			p.Wait(`vkErrors.length === 1`)
			p.Eval(`[vkErrors[0], document.querySelector("#city").options.length]`, &got)
			if g, _ := got.([]any); len(g) != 2 || !strings.Contains(fmt.Sprint(g[0]), "error") || g[1] != float64(1) {
				t.Errorf("a failure without OnError: %v", got)
			}
			// groups: <optgroup>s
			p.Click("#sizes")
			p.Wait(`document.querySelector("#size optgroup") !== null`)
			p.Eval(`[document.querySelector("#size optgroup").label, Array.from(document.querySelector("#size").options, (o) => o.value).join(",")]`, &got)
			if g, _ := got.([]any); fmt.Sprint(g) != "[shirt shirt-s,shirt-m]" {
				t.Errorf("groups: %v", got)
			}
		})
	}
}

// billingPage copies the billing address into the shipping one when a box
// is ticked: SetValue with reads of other fields.
type billingPage struct{}

func (billingPage) Render(ctx context.Context, w io.Writer) error {
	same := ui.JS.If(ui.This.Checked()).
		SetValue(ui.Sel("#ship-city"), ui.Sel("#bill-city").Value()).
		SetValue(ui.Sel("#ship-country"), ui.Sel("#bill-country").Value()).
		SetValue(ui.Sel("#ship-gift"), ui.Sel("#bill-gift").Checked()).
		Else().SetValue(ui.Sel("#ship-city"), "").SetValue(ui.Sel("#ship-gift"), false).End()
	return ui.El("main", nil,
		ui.El("input", []ui.Attr{attr("id", "bill-city"), attr("value", "Arusha")}),
		ui.El("select", []ui.Attr{attr("id", "bill-country")}, opt("tz", "Tanzania", false), opt("ke", "Kenya", true)),
		ui.El("input", []ui.Attr{attr("id", "bill-gift"), attr("type", "checkbox"), attr("checked", true)}),
		ui.El("input", []ui.Attr{attr("id", "same"), attr("type", "checkbox"), attr("onChange", ui.On(same))}),
		ui.El("input", []ui.Attr{attr("id", "ship-city")}),
		ui.El("select", []ui.Attr{attr("id", "ship-country")}, opt("tz", "Tanzania", true), opt("ke", "Kenya", false)),
		ui.El("input", []ui.Attr{attr("id", "ship-gift"), attr("type", "checkbox")}),
		ui.Safe(`<script>window.seen = []; for (const id of ["ship-city", "ship-country", "ship-gift"]) for (const ev of ["input", "change"]) document.getElementById(id).addEventListener(ev, () => seen.push(id + ":" + ev));</script>`),
	).Render(ctx, w)
}

// TestBrowserSetValue: SetValue sets an input, a select and a checkbox from
// reads of other fields, firing input and change on each.
func TestBrowserSetValue(t *testing.T) {
	a := New(Only("none"))
	a.Get("/billing", func() billingPage { return billingPage{} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	p := b.Visit(srv.URL + "/billing")
	p.Click("#same")
	p.Wait(`document.querySelector("#ship-city").value !== ""`)
	var got any
	fields := `[document.querySelector("#ship-city").value, document.querySelector("#ship-country").value, document.querySelector("#ship-gift").checked, seen.join(" ")]`
	p.Eval(fields, &got)
	if fmt.Sprint(got) != "[Arusha ke true ship-city:input ship-city:change ship-country:input ship-country:change ship-gift:input ship-gift:change]" {
		t.Errorf("ticked: %v", got)
	}
	p.Eval(`seen = []`, nil)
	p.Click("#same")
	p.Wait(`document.querySelector("#ship-city").value === ""`)
	p.Eval(fields, &got)
	if fmt.Sprint(got) != "[ ke false ship-city:input ship-city:change ship-gift:input ship-gift:change]" {
		t.Errorf("unticked: %v", got)
	}
}

// deliveryForm's city choices depend on its country.
type deliveryForm struct {
	ui.FormState
	Country string `validate:"required"`
	City    string `validate:"required" depends:"Country"`
}

func (f *deliveryForm) CountryChoices() []ui.Option {
	return []ui.Option{{Value: "tz", Label: "Tanzania"}, {Value: "ke", Label: "Kenya"}}
}

func (f *deliveryForm) CityChoices(ctx context.Context) []ui.Option {
	return (&geoDB{}).cities(f.Country)
}

type deliveryPage struct{ Form deliveryForm }

func (p *deliveryPage) Save(ctx context.Context, f deliveryForm) error { return Redirect("/shop") }

func (p *deliveryPage) Render() ui.Node {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Delivery"))),
		ui.El("body", nil, ui.Form{For: ui.For(&p.Form), OnSubmit: p.Save}))
}

// TestBrowserFormChoicesHTTP: on a page that isn't live, a field's choices
// follow the field they depend on: the form's check carries them.
func TestBrowserFormChoicesHTTP(t *testing.T) {
	a := New(Only("none"))
	a.Get("/delivery", func() ui.Node { return (&deliveryPage{}).Render() })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	// the check's reply
	vals := url.Values{"_vk_form": {"delivery-form"}, "_vk_check": {"1"}, "country": {"ke"}}
	req, _ := http.NewRequest("POST", srv.URL+"/delivery", strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(FormHeader, "check")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Choices map[string][]map[string]string `json:"choices"`
	}
	json.NewDecoder(res.Body).Decode(&rep)
	res.Body.Close()
	if fmt.Sprint(rep.Choices) != "map[city:[map[label:Nairobi value:nbo] map[label:Arusha (KE) value:aru]]]" {
		t.Errorf("the check's choices: %d %v", res.StatusCode, rep.Choices)
	}

	b := chrome.Launch(t)
	p := b.Visit(srv.URL + "/delivery")
	var got any
	p.Eval(`[document.documentElement.dataset.vkState || "", !!document.querySelector("form[data-vk-http]")]`, &got)
	if g, _ := got.([]any); len(g) != 2 || g[0] == "connected" || g[1] != true {
		t.Fatalf("the page connected, or its form isn't sent over HTTP: %v", got)
	}
	p.Eval(`{ const c = document.querySelector('select[name="country"]'); c.value = "tz"; c.dispatchEvent(new Event("change", { bubbles: true })) }`, nil)
	p.Wait(`Array.from(document.querySelector('select[name="city"]').options, (o) => o.value).includes("dar")`)
	p.Eval(`document.querySelector('select[name="city"]').value = "aru"`, nil)
	p.Eval(`{ const c = document.querySelector('select[name="country"]'); c.value = "ke"; c.dispatchEvent(new Event("change", { bubbles: true })) }`, nil)
	p.Wait(`Array.from(document.querySelector('select[name="city"]').options, (o) => o.value).includes("nbo")`)
	p.Eval(`[document.querySelector('select[name="city"]').value, Array.from(document.querySelector('select[name="city"]').options, (o) => o.value).join(",")]`, &got)
	if fmt.Sprint(got) != "[aru ,nbo,aru]" {
		t.Errorf("City after Kenya: %v", got)
	}
}
