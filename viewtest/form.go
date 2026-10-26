package viewtest

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/web/v2"
)

// Page drives a page's struct form (<ui.Form for>) as a visitor does: it
// fills fields, leaves them and submits the form, and checks what the page
// shows of each field and where a submit sends the visitor.
//
//	p := viewtest.Mount[*pages.Join](t, app, "/join")
//	p.Fill(&p.Page.Form.Email, "pa@").Blur(&p.Page.Form.Email)
//	p.Expect(&p.Page.Form.Email).Error("validate.email")
//	p.Fill(&p.Page.Form.Email, "pa@example.com").Submit()
//	p.ExpectRedirect("/welcome")
//
// A field is named by a pointer into Page — a value of the page's type the
// test holds only to point at its fields — or by its path ("email",
// "address.city"). Filling a field the form doesn't render fails the test.
//
// By default the page runs live, over its connection, as in a browser with
// script; Plain drives it as a browser without script, each submit a plain
// POST. A form test reads the same either way, but for the live checks a
// Blur runs: without script they happen on submit.
type Page[T any] struct {
	// Page is a value of the page's type, for its fields' addresses: what
	// Fill, Blur and Expect take.
	Page T

	t      testing.TB
	srv    *httptest.Server
	client *http.Client
	path   string
	tr     transport
	html   string     // the page as it stands
	values url.Values // the form's fields as filled
	used   []string
	seq    int
	redir  string // where the last submit sent the visitor
	status int    // the last plain post's status
}

// transport is how the page talks to the app: over its connection, or as
// plain HTTP.
type transport interface {
	// event sends the form's event (input, focusout, submit) with its
	// values, and returns the page after it and where it sent the visitor.
	event(p *pageState, typ string, values url.Values) (string, string)
	close()
}

// pageState is what a transport reads of the page.
type pageState struct {
	t    testing.TB
	html string
}

// Option configures Mount.
type Option func(*options)

type options struct{ plain bool }

// Plain drives the page as a browser without script does: its form posts
// to the page on submit, the page answering 422 with the errors or 303.
func Plain() Option { return func(o *options) { o.plain = true } }

// Mount opens the page app serves at path, a live page of type T (a
// pointer, *pages.Join), with a fresh visitor.
func Mount[T any](t testing.TB, app *web.App, path string, opts ...Option) *Page[T] {
	t.Helper()
	var o options
	for _, f := range opts {
		f(&o)
	}
	p := &Page[T]{t: t, path: path}
	if rt := reflect.TypeFor[T](); rt.Kind() == reflect.Pointer {
		p.Page = reflect.New(rt.Elem()).Interface().(T)
	}
	p.srv = httptest.NewServer(app.Handler())
	t.Cleanup(p.srv.Close)
	jar, _ := cookiejar.New(nil)
	p.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, body := p.get(path)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("viewtest: GET %s: %d", path, res.StatusCode)
	}
	p.html = body
	if !strings.Contains(body, "data-vk-form=") {
		t.Fatalf("viewtest: GET %s has no struct form (<ui.Form for>)", path)
	}
	if o.plain {
		p.tr = &plainPost{p: p.post}
	} else {
		lt, html := joinLive(t, p.srv, p.client, body)
		p.tr, p.html = lt, html
	}
	t.Cleanup(p.tr.close)
	p.values = formValues(p.html)
	return p
}

func (p *Page[T]) get(path string) (*http.Response, string) {
	p.t.Helper()
	res, err := p.client.Get(p.srv.URL + path)
	if err != nil {
		p.t.Fatalf("viewtest: GET %s: %v", path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func (p *Page[T]) post(values url.Values) (int, string, string) {
	p.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, p.srv.URL+p.path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Origin", p.srv.URL)
	res, err := p.client.Do(req)
	if err != nil {
		p.t.Fatalf("viewtest: POST %s: %v", p.path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header.Get("Location")
}

// path is the path of the field f names: a path, or a pointer into Page.
func (p *Page[T]) fieldPath(f any) string {
	p.t.Helper()
	if s, ok := f.(string); ok {
		return s
	}
	path, err := ui.FieldPath(f, p.Page)
	if err != nil {
		p.t.Fatalf("viewtest: %v", err)
	}
	return path
}

// control is the field's first control in the page; ok is false when the
// form doesn't render one.
func (p *Page[T]) control(path string) (tag string, ok bool) {
	m := regexp.MustCompile(`<(input|textarea|select)\b[^>]*\sname="` + regexp.QuoteMeta(path) + `"`).FindStringSubmatch(p.html)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Fill sets the field to v, as typing it (or choosing it) does: over the
// connection the form's checks run on it, showing errors only of fields
// already showing one.
func (p *Page[T]) Fill(field any, v string) *Page[T] {
	p.t.Helper()
	path := p.fieldPath(field)
	if _, ok := p.control(path); !ok {
		p.t.Fatalf("viewtest: Fill: the form renders no field %q; it renders %s", path, strings.Join(rendered(p.html), ", "))
	}
	p.values.Set(path, v)
	if _, live := p.tr.(*liveConn); live {
		p.send("input")
	}
	return p
}

// Blur leaves the field, as moving focus out of it does: a field left with
// something in it shows its error.
func (p *Page[T]) Blur(field any) *Page[T] {
	p.t.Helper()
	path := p.fieldPath(field)
	if p.values.Get(path) != "" && !slices.Contains(p.used, path) {
		p.used = append(p.used, path)
	}
	if _, live := p.tr.(*liveConn); live {
		p.send("focusout")
	}
	return p
}

// Submit submits the form.
func (p *Page[T]) Submit() *Page[T] {
	p.t.Helper()
	p.send("submit")
	return p
}

func (p *Page[T]) send(typ string) {
	p.t.Helper()
	v := url.Values{}
	for k, vs := range formValues(p.html) {
		if strings.HasPrefix(k, "_vk_") {
			v[k] = vs
		}
	}
	for k, vs := range p.values {
		if !strings.HasPrefix(k, "_vk_") {
			v[k] = vs
		}
	}
	v.Set(ui.FormUsedField, strings.Join(p.used, ","))
	p.seq++
	v.Set(ui.FormSeqField, strconv.Itoa(p.seq))
	st := &pageState{t: p.t, html: p.html}
	p.html, p.redir = p.tr.event(st, typ, v)
	if pp, ok := p.tr.(*plainPost); ok {
		p.status = pp.status
	}
}

// HTML is the page as it stands.
func (p *Page[T]) HTML() string { return p.html }

// Field is what the page shows of one field.
type Field struct {
	t    testing.TB
	path string
	html string
}

// Expect is what the page shows of the field, to check.
func (p *Page[T]) Expect(field any) *Field {
	p.t.Helper()
	path := p.fieldPath(field)
	if _, ok := p.control(path); !ok {
		p.t.Fatalf("viewtest: Expect: the form renders no field %q", path)
	}
	return &Field{t: p.t, path: path, html: p.html}
}

// errorKey is the message key of the error the field shows and its text;
// "" when it shows none. A field's error is the element whose id is its
// first control's with -error after it.
func (f *Field) errorKey() (key, text string) {
	m := regexp.MustCompile(`<(?:input|textarea|select)\b[^>]*\sname="` + regexp.QuoteMeta(f.path) + `"[^>]*>`).FindString(f.html)
	id := attrs(m)["id"]
	if id == "" {
		return "", ""
	}
	e := regexp.MustCompile(`<p class="vk-field__error[^"]*" id="` + regexp.QuoteMeta(id) + `-error" data-vk-error="([^"]*)">(.*?)</p>`).FindStringSubmatch(f.html)
	if e == nil {
		return "", ""
	}
	text = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(e[2], "")
	return html.UnescapeString(e[1]), strings.TrimSpace(strings.TrimPrefix(html.UnescapeString(text), "Error:"))
}

// Error checks that the field shows an error, its message key key
// ("validate.email", or the key a Clean method's forms.Errorf gave) or its
// text.
func (f *Field) Error(key string) {
	f.t.Helper()
	got, text := f.errorKey()
	switch {
	case got == "":
		f.t.Errorf("viewtest: %s shows no error, want %q", f.path, key)
	case got != key && text != key:
		f.t.Errorf("viewtest: %s shows %q (%s), want %q", f.path, got, text, key)
	}
}

// NoError checks that the field shows no error.
func (f *Field) NoError() {
	f.t.Helper()
	if got, text := f.errorKey(); got != "" {
		f.t.Errorf("viewtest: %s shows %q (%s), want no error", f.path, got, text)
	}
}

// ExpectRedirect checks that the last submit sent the visitor to url.
func (p *Page[T]) ExpectRedirect(url string) {
	p.t.Helper()
	if p.redir != url {
		p.t.Errorf("viewtest: the submit sent the visitor to %q, want %q", p.redir, url)
	}
}

// Status is the status of the last submit's answer, as a plain post's: 422
// when the form had errors, 303 when it was handled; 0 over a connection.
func (p *Page[T]) Status() int { return p.status }

// plainPost is the transport of a page without script: each submit posts.
type plainPost struct {
	p      func(url.Values) (int, string, string)
	status int
}

func (t *plainPost) event(st *pageState, typ string, v url.Values) (string, string) {
	if typ != "submit" {
		return st.html, ""
	}
	code, body, loc := t.p(v)
	t.status = code
	switch code {
	case http.StatusSeeOther:
		return st.html, loc
	case http.StatusUnprocessableEntity:
		return body, ""
	}
	st.t.Fatalf("viewtest: the post answered %d:\n%s", code, body)
	return st.html, ""
}

func (t *plainPost) close() {}

// liveConn is the transport of a page with script: its connection.
type liveConn struct {
	c   *websocket.Conn
	ref int
}

var tokenAttr = regexp.MustCompile(`data-vk-token="([^"]+)"`)

func joinLive(t testing.TB, srv *httptest.Server, client *http.Client, page string) (*liveConn, string) {
	t.Helper()
	m := tokenAttr.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("viewtest: the page isn't live (no join token)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u, _ := url.Parse(srv.URL)
	h := http.Header{"Origin": {srv.URL}}
	for _, c := range client.Jar.Cookies(u) {
		h.Add("Cookie", c.Name+"="+c.Value)
	}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/_vuka/live?t="+url.QueryEscape(html.UnescapeString(m[1])), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("viewtest: connecting the page: %v", err)
	}
	c.SetReadLimit(-1)
	lc := &liveConn{c: c}
	msg := lc.roundTrip(t, live.ClientMessage{Type: live.Join})
	if msg.Type != live.Render {
		t.Fatalf("viewtest: join: %s %s", msg.Type, msg.Error)
	}
	return lc, msg.HTML
}

func (l *liveConn) roundTrip(t testing.TB, m live.ClientMessage) live.ServerMessage {
	t.Helper()
	l.ref++
	m.Ref = l.ref
	b, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("viewtest: %v", err)
	}
	for {
		_, data, err := l.c.Read(ctx)
		if err != nil {
			t.Fatalf("viewtest: %v", err)
		}
		var reply live.ServerMessage
		if err := json.Unmarshal(data, &reply); err != nil {
			t.Fatalf("viewtest: %v", err)
		}
		if reply.Ref == m.Ref {
			return reply
		}
	}
}

var (
	formHandler = regexp.MustCompile(`<form\b[^>]*\sdata-vk-form="[^"]*"[^>]*>`)
	navigate    = regexp.MustCompile(`"navigate",\{"href":"([^"]+)"`)
)

func (l *liveConn) event(st *pageState, typ string, v url.Values) (string, string) {
	st.t.Helper()
	tag := formHandler.FindString(st.html)
	m := regexp.MustCompile(`\sdata-vk-on-` + typ + `="([^"]+)"`).FindStringSubmatch(tag)
	if m == nil {
		if typ == "submit" {
			st.t.Fatalf("viewtest: the form has no submit handler")
		}
		return st.html, "" // a form checking only on submit
	}
	reply := l.roundTrip(st.t, live.ClientMessage{Type: live.EventMsg, Event: ui.Event{Target: m[1], Type: typ, Form: v}})
	switch {
	case reply.Type == live.Error:
		st.t.Fatalf("viewtest: %s: %s", typ, reply.Error)
	case reply.Type == live.Redirect:
		return st.html, reply.URL
	case reply.Error != "":
		st.t.Errorf("viewtest: %s: %s", typ, reply.Error)
	}
	page := st.html
	for _, u := range reply.Updates {
		page = replaceInstance(page, u.ID, u.HTML)
	}
	js, _ := json.Marshal(reply.JS)
	if n := navigate.FindSubmatch(js); n != nil {
		var href string
		json.Unmarshal([]byte(`"`+string(n[1])+`"`), &href)
		return page, href
	}
	return page, ""
}

func (l *liveConn) close() { l.c.CloseNow() }

// replaceInstance is page with the element of instance id (its data-vk-id)
// replaced by html; c0 is the whole page.
func replaceInstance(page, id, html string) string {
	if id == live.RootID {
		return html
	}
	at := strings.Index(page, ` data-vk-id="`+id+`"`)
	if at < 0 {
		return page
	}
	start := strings.LastIndexByte(page[:at], '<')
	tag := page[start+1:]
	tag = tag[:strings.IndexAny(tag, " \t\n>/")]
	end := elementEnd(page, start, tag)
	return page[:start] + html + page[end:]
}

// elementEnd is where the element opening at start ends, its children
// counted.
func elementEnd(s string, start int, tag string) int {
	depth := 0
	re := regexp.MustCompile(`(?i)<(/?)` + regexp.QuoteMeta(tag) + `[\s>/]`)
	for _, m := range re.FindAllStringSubmatchIndex(s[start:], -1) {
		if s[start+m[2]:start+m[3]] == "/" {
			depth--
		} else {
			depth++
		}
		if depth == 0 {
			close := strings.IndexByte(s[start+m[0]:], '>')
			return start + m[0] + close + 1
		}
	}
	return len(s)
}

var (
	inputTag    = regexp.MustCompile(`<input\b[^>]*>`)
	textareaTag = regexp.MustCompile(`(?s)<textarea\b([^>]*)>(.*?)</textarea>`)
	selectTag   = regexp.MustCompile(`(?s)<select\b([^>]*)>(.*?)</select>`)
	optionTag   = regexp.MustCompile(`<option\b[^>]*>`)
	attrRe      = regexp.MustCompile(`\s([\w:-]+)(?:="([^"]*)")?`)
)

func attrs(tag string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
		out[m[1]] = html.UnescapeString(m[2])
	}
	return out
}

// formValues are the values the page's struct form sends as it stands.
func formValues(page string) url.Values {
	v := url.Values{}
	loc := formHandler.FindStringIndex(page)
	if loc == nil {
		return v
	}
	form := page[loc[0]:]
	if end := strings.Index(form, "</form>"); end >= 0 {
		form = form[:end]
	}
	for _, tag := range inputTag.FindAllString(form, -1) {
		a := attrs(tag)
		name := a["name"]
		if name == "" {
			continue
		}
		switch a["type"] {
		case "checkbox", "radio":
			if _, on := a["checked"]; on {
				v.Add(name, a["value"])
			}
		default:
			v.Add(name, a["value"])
		}
	}
	for _, m := range textareaTag.FindAllStringSubmatch(form, -1) {
		if name := attrs(m[1])["name"]; name != "" {
			v.Add(name, html.UnescapeString(m[2]))
		}
	}
	for _, m := range selectTag.FindAllStringSubmatch(form, -1) {
		name := attrs(m[1])["name"]
		if name == "" {
			continue
		}
		for _, o := range optionTag.FindAllString(m[2], -1) {
			if a := attrs(o); a["selected"] != "" || strings.Contains(o, " selected") {
				v.Add(name, a["value"])
			}
		}
	}
	return v
}

// rendered are the names of the form's fields, for a message.
func rendered(page string) []string {
	var out []string
	for k := range formValues(page) {
		if !strings.HasPrefix(k, "_vk_") {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
