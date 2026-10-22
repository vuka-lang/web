package web

import (
	"context"
	"html"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
)

type courseLevel struct {
	Code string `json:"code" validate:"required,len=3"`
}

type courseFilter struct {
	Years int         `json:"years" validate:"min=1,max=40"`
	Level courseLevel `json:"level"`
}

// courses is a stateless page whose bound Q and whose method arguments are
// validated.
type courses struct {
	Q       string `json:"q" bind:"q" validate:"max=5"`
	Years   int    `json:"years"`
	Changes int    `json:"changes"`
	Calls   int    `json:"calls"`
}

func (c *courses) LiveSchema(l *ui.LiveSchema)                    { l.OnChange(c.onChange) }
func (c *courses) onChange(_ context.Context, _ ui.Changed) error { c.Changes++; return nil }
func (c *courses) Filter(f courseFilter) error                    { c.Calls++; c.Years = f.Years; return nil }
func (c *courses) Pick(levels []*courseLevel) error               { c.Calls++; return nil }

type liveCourses struct {
	ui.Live
	Q       string `json:"q" bind:"q" validate:"max=5"`
	Years   int    `json:"years"`
	Changes int    `json:"changes"`
	Calls   int    `json:"calls"`
}

func (c *liveCourses) Render() ui.Node                                { return ui.IslandPage(c) }
func (c *liveCourses) LiveSchema(l *ui.LiveSchema)                    { l.OnChange(c.onChange) }
func (c *liveCourses) onChange(_ context.Context, _ ui.Changed) error { c.Changes++; return nil }
func (c *liveCourses) Filter(f *courseFilter) error                   { c.Calls++; c.Years = f.Years; return nil }

type badRule struct {
	Q string `json:"q" bind:"q" validate:"max=five"`
}

func init() {
	ui.Island("components/Courses.vue")(vuka.TypeOf[courses]("web.courses"))
	ui.Island("components/Courses.vue")(vuka.TypeOf[liveCourses]("web.liveCourses"))
	ui.Island("components/Courses.vue")(vuka.TypeOf[badRule]("web.badRule"))
}

func TestIslandPageValidatesStateless(t *testing.T) {
	h := New(Only("none")).Page("/courses", &courses{}).Handler()
	_, body, _ := do(t, h, "GET", "/courses", "")
	_, raw, sig := rendered(t, body)
	json := []string{"Content-Type", "application/json"}

	expect(t, h, "POST", "/courses", setBody(map[string]any{"q": "toolong"}, "", raw, sig), 422,
		`{"error":{"status":422,"message":"invalid: q: at most 5 characters","fields":{"q":"at most 5 characters"}}}`, json...)
	expect(t, h, "POST", "/courses", callBody("filter", []any{map[string]any{"years": 0, "level": map[string]any{"code": "x"}}}, raw, sig), 422,
		`"fields":{"level.code":"exactly 3 characters","years":"at least 1"}`, json...)
	expect(t, h, "POST", "/courses", callBody("pick", []any{[]any{map[string]any{"code": "abc"}, map[string]any{}}}, raw, sig), 422,
		`"fields":{"1.code":"required"}`, json...)
	// A set waiting before a method: an invalid set runs neither.
	expect(t, h, "POST", "/courses", setBody(map[string]any{"q": "toolong"}, "filter", raw, sig), 422, `"fields":{"q":`, json...)

	code, resp, _ := do(t, h, "POST", "/courses", setBody(map[string]any{"q": "ok"}, "", raw, sig), json...)
	if code != 200 || !strings.Contains(resp, `\"q\":\"ok\"`) || !strings.Contains(resp, `\"changes\":1`) {
		t.Errorf("a valid set: %d %s", code, resp)
	}
	code, resp, _ = do(t, h, "POST", "/courses", callBody("filter", []any{map[string]any{"years": 3, "level": map[string]any{"code": "abc"}}}, raw, sig), json...)
	if code != 200 || !strings.Contains(resp, `\"years\":3`) || !strings.Contains(resp, `\"calls\":1`) {
		t.Errorf("a valid call: %d %s", code, resp)
	}
}

func TestIslandPageValidatesLive(t *testing.T) {
	srv := httptest.NewServer(New(Only("none")).Page("/live/courses", &liveCourses{}).Handler())
	defer srv.Close()
	c, page := joined(t, srv, "/live/courses")
	set := handlerOf(t, page, "m-_set", 0)
	filter := handlerOf(t, page, "m-filter", 0)

	send(t, c, map[string]any{"type": "event", "ref": 2, "target": set, "event": "emit", "data": map[string]any{"q": "toolong"}})
	m := must(t, c)
	if len(m.Updates) != 1 {
		t.Fatalf("set: %+v", m)
	}
	out := html.UnescapeString(m.Updates[0].HTML)
	if !strings.Contains(out, `data-vk-err='{"status":422,"message":"invalid: q: at most 5 characters","fields":{"q":"at most 5 characters"}}'`) &&
		!strings.Contains(out, `data-vk-err="{"status":422,"message":"invalid: q: at most 5 characters","fields":{"q":"at most 5 characters"}}"`) {
		t.Errorf("an invalid set's error:\n%s", out)
	}
	if props, _, _ := rendered(t, m.Updates[0].HTML); props["q"] != "" || props["changes"] != 0.0 {
		t.Errorf("an invalid set changed the page: %v", props)
	}

	send(t, c, map[string]any{"type": "event", "ref": 3, "target": filter, "event": "emit", "data": []any{map[string]any{"years": 50, "level": map[string]any{}}}})
	m = must(t, c)
	out = html.UnescapeString(m.Updates[0].HTML)
	if !strings.Contains(out, `"fields":{"level.code":"required","years":"at most 40"}`) {
		t.Errorf("an invalid call's error:\n%s", out)
	}
	if props, _, _ := rendered(t, m.Updates[0].HTML); props["calls"] != 0.0 {
		t.Errorf("an invalid call ran: %v", props)
	}

	send(t, c, map[string]any{"type": "event", "ref": 4, "target": filter, "event": "emit", "data": []any{map[string]any{"years": 4, "level": map[string]any{"code": "abc"}}}})
	m = must(t, c)
	if props, _, _ := rendered(t, m.Updates[0].HTML); props["calls"] != 1.0 || props["years"] != 4.0 || strings.Contains(m.Updates[0].HTML, "data-vk-err") {
		t.Errorf("a valid call: %v", props)
	}
}

func TestIslandPageBadRule(t *testing.T) {
	err := New(Only("none")).Page("/b", &badRule{}).Build()
	if err == nil || !strings.Contains(err.Error(), "max takes a number") {
		t.Errorf("got %v", err)
	}
}
