package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
	"github.com/vuka-lang/web/v2/internal/liveproto"
)

// keyedLog is what a keyedPage's rows record, for the test to read.
type keyedLog struct {
	mu   sync.Mutex
	hits []string
}

func (l *keyedLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits = append(l.hits, s)
}

func (l *keyedLog) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.hits)
}

type keyedRow struct {
	Key   any
	Label string
}

// keyedPage is a live page of keyed rows with handlers: a button inserts a row
// at the top, and each row has a hit and a del button and a form.
type keyedPage struct {
	ui.Live
	Log  *keyedLog
	rows []keyedRow
}

func (p *keyedPage) Insert() {
	p.rows = append([]keyedRow{{"new", "new"}}, p.rows...)
}

func (p *keyedPage) remove(label string) {
	p.rows = slices.DeleteFunc(p.rows, func(r keyedRow) bool { return r.Label == label })
}

func (p *keyedPage) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "id", Value: "insert"}, {Name: "onClick", Value: ui.On(p.Insert)}}, ui.Text("insert")),
		ui.El("ul", nil, ui.Each(p.rows, func(r keyedRow) ui.Node {
			return ui.El("li", []ui.Attr{{Name: "key", Value: r.Key}, {Name: "data-label", Value: r.Label}},
				ui.El("button", []ui.Attr{{Name: "class", Value: "hit"}, {Name: "onClick", Value: ui.On(func() { p.Log.add(r.Label) })}}, ui.Text("hit")),
				ui.El("button", []ui.Attr{{Name: "class", Value: "del"}, {Name: "onClick", Value: ui.On(func() { p.remove(r.Label) })}}, ui.Text("del")),
				ui.El("button", []ui.Attr{{Name: "class", Value: "cmd"}, {Name: "onClick", Value: ui.JS.Push(func() { p.Log.add("cmd " + r.Label) })}}, ui.Text("cmd")))
		})))
}

func keyedServer(t *testing.T, rows []keyedRow) (*chrome.Page, *keyedLog) {
	t.Helper()
	log := &keyedLog{}
	a := New(Only("none"))
	a.Live("/rows", func() *keyedPage { return &keyedPage{Log: log, rows: slices.Clone(rows)} })
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return chrome.Launch(t).Open(srv.URL + "/rows"), log
}

func row(label string) string { return `document.querySelector('li[data-label="` + label + `"] ` }

// TestBrowserKeyedClickRacedByPatch: live.js, clicking twice in one tick, sends
// the second click on the row's id before the first's patch comes: it reaches
// that row though the insert moved it, and after a removal reaches none.
func TestBrowserKeyedClickRacedByPatch(t *testing.T) {
	p, log := keyedServer(t, []keyedRow{{1, "one"}, {2, "two"}, {3, "three"}})
	p.Eval(`document.querySelector("#insert").click(); `+row("two")+`.hit').click();`, nil)
	p.Wait(`document.querySelector('li[data-label="new"]') !== null`)
	p.Click(`li[data-label="three"] .hit`)
	waitHits(t, p, log, []string{"two", "three"})
	// its own row gone: nothing, not the row after it
	p.Eval(row("two")+`.del').click(); `+row("two")+`.hit').click();`, nil)
	p.Wait(`document.querySelector('li[data-label="two"]') === null`)
	p.Click(`li[data-label="three"] .hit`)
	waitHits(t, p, log, []string{"two", "three", "three"})
}

func waitHits(t *testing.T, p *chrome.Page, log *keyedLog, want []string) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got := log.got(); !slices.Equal(got, want) {
		t.Fatalf("hits %q, want %q", got, want)
	}
}

// TestBrowserKeyedHostileKeys: keys holding what an attribute, JSON or a
// selector minds reach their rows through the runtime, by data-vk-on and by a
// command's push.
func TestBrowserKeyedHostileKeys(t *testing.T) {
	keys := []any{"a:b", "a/b", `"><img src=x onerror=window.pwned=1>`, "'", "&amp;", "%41", "A", "日本語", "c0:0", "!0", strings.Repeat("k", 300), 7, "7", int64(7)}
	rows := make([]keyedRow, len(keys))
	for i, k := range keys {
		rows[i] = keyedRow{k, fmt.Sprintf("r%d", i)}
	}
	p, log := keyedServer(t, rows)
	var want []string
	for _, r := range rows {
		p.Click(`li[data-label="` + r.Label + `"] .hit`)
		p.Click(`li[data-label="` + r.Label + `"] .cmd`)
		want = append(want, r.Label, "cmd "+r.Label)
	}
	waitHits(t, p, log, want)
	var pwned any
	p.Eval(`window.pwned === undefined`, &pwned)
	if pwned != true {
		t.Fatal("a key ran as markup")
	}
}

// TestToolbarNamesKeyedInstance: an instance whose handlers are all under keys
// is named in the Live panel as any other.
func TestToolbarNamesKeyedInstance(t *testing.T) {
	s := liveproto.New(context.Background(), func(context.Context) ui.Node {
		return ui.Component("rows", nil, &keyedPage{Log: &keyedLog{}, rows: []keyedRow{{1, "one"}}})
	}, 2)
	s.Join(1)
	c := &conn{s: s}
	if got := c.instanceType("c1"); got != "keyedPage" {
		t.Fatalf("c1 is %q", got)
	}
	if got := c.instanceType("c9"); got != "" {
		t.Fatalf("c9 is %q", got)
	}
}

// TestToolbarWireMasksKeyedInput: a password input under a key is masked as
// any other: the mirror finds its target in its tag.
func TestToolbarWireMasksKeyedInput(t *testing.T) {
	m := newWireMirror(func(string) string { return "" })
	m.apply([]byte(`{"type":"render","html":"<form data-vk-key=\"a:b\"><input type=\"password\" name=\"pw\" data-vk-on-input=\"c0:a%3Ab:0\"></form>"}`))
	got := string(m.maskClient([]byte(`{"type":"event","ref":3,"target":"c0:a%3Ab:0","event":"input","value":"hunter2"}`)))
	if strings.Contains(got, "hunter2") {
		t.Fatalf("a keyed password input's value: %s", got)
	}
}

type keyedFormValues struct {
	ui.FormState
	Name string `validate:"required"`
}

// keyedForms is a form per keyed row.
type keyedForms struct {
	ui.Live
	Log   *keyedLog
	forms map[string]*keyedFormValues
	keys  []string
}

func (p *keyedForms) Insert() { p.keys = append([]string{"new"}, p.keys...) }

func (p *keyedForms) form(k string) *keyedFormValues {
	if p.forms == nil {
		p.forms = map[string]*keyedFormValues{}
	}
	if p.forms[k] == nil {
		p.forms[k] = &keyedFormValues{}
	}
	return p.forms[k]
}

func (p *keyedForms) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "id", Value: "insert"}, {Name: "onClick", Value: ui.On(p.Insert)}}, ui.Text("insert")),
		ui.Each(p.keys, func(k string) ui.Node {
			return ui.El("section", []ui.Attr{{Name: "key", Value: k}, {Name: "data-label", Value: k}},
				ui.Form{For: ui.For(p.form(k)), Key: k, OnSubmit: func(v keyedFormValues) error { p.Log.add(k + "=" + v.Name); return nil }})
		}))
}

// TestBrowserKeyedForm: a form of a keyed row keeps submitting to its row
// after a row is inserted before it.
func TestBrowserKeyedForm(t *testing.T) {
	log := &keyedLog{}
	a := New(Only("none"))
	a.Live("/forms", func() *keyedForms { return &keyedForms{Log: log, keys: []string{"a:1", `b"2`}} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/forms")
	var attr any
	p.Eval(`document.querySelectorAll('form')[1].getAttribute('data-vk-on-submit')`, &attr)
	if s, _ := attr.(string); strings.Count(s, ":") != 2 {
		t.Fatalf("the form's submit id: %v", attr)
	}
	p.Click("#insert")
	p.Wait(`document.querySelectorAll('form').length === 3`)
	sel := `section[data-label='b"2'] `
	p.Type(sel+`input[name="name"]`, "pets")
	p.Click(sel + `.vk-btn--submit`)
	waitHits(t, p, log, []string{`b"2=pets`})
}
