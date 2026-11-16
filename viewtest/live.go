package viewtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/web/v2"
)

// Live is a page a test drives over its connection, as the runtime does,
// without a browser: each action sends the message a browser's would —
// an element's event to its handler, a form's fields on submit, a
// navigation — and waits for its reply, and the page as the test reads it
// is the markup the server sent, patched as it arrives. Patches the
// server sends unasked (a deferred group loading, a broadcast) apply too,
// and an Expect waits for them.
//
//	p := viewtest.Open(t, app, "/todos")
//	p.Fill("title", "Buy milk").Submit("form")
//	p.Expect("#todos li").Text("Buy milk")
//
// It is fast and needs nothing installed; what it doesn't do is run the
// page's script. JS commands (ui.JS) other than navigations, islands,
// hooks and what the runtime keeps on the page across a patch (a
// ShowOnce's value, focus) need a browser: Browse.
//
// A page that isn't live is held as served: its links and forms load the
// next page as a browser without script does.
//
// A locator names an element: a CSS selector (tag, #id, .class, [attr],
// descendant and > combinators), a field's name, or the text of a button,
// link or label. Actions on fields (Fill, Check, Select, Value) read it as
// a field's name first; the others as a selector first.
type Live struct {
	t      testing.TB
	srv    *httptest.Server
	client *http.Client
	conn   *conn           // nil while the page isn't live
	static string          // the page, when it isn't live
	at     string          // its URL's path and query
	err    string          // the last reply's error
	fills  map[string]fill // what the test filled, by field
	asked  map[string]bool // the seen groups asked for on this page
	seq    int
}

// fill is a field's value as the test set it, kept, as a browser keeps
// what was typed, until the server renders the field with a value other
// than the one it had then.
type fill struct {
	value  string // a field's value; a radio group's chosen value
	on     bool   // a checkbox's
	server string // what the server rendered then
}

// Open serves app and opens the page at path (a path, with its query) as
// a new visitor, its cookies kept across the requests of the test, joining
// its connection when it is live. The server and the connection close as
// the test ends.
//
// To test as a signed-in user, build app with auth/authtest's As.
func Open(t testing.TB, app *web.App, path string) *Live {
	t.Helper()
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	l := &Live{t: t, srv: srv, client: &http.Client{Jar: jar}}
	t.Cleanup(func() {
		if l.conn != nil {
			l.conn.close()
		}
	})
	l.request(http.MethodGet, path, nil)
	return l
}

// request loads a page over HTTP, following redirects, and joins it when
// it is live.
func (l *Live) request(method, path string, form url.Values) {
	l.t.Helper()
	var body io.Reader
	if form != nil && method == http.MethodPost {
		body = strings.NewReader(form.Encode())
	} else if form != nil {
		u, _ := url.Parse(path)
		u.RawQuery = form.Encode()
		path = u.String()
	}
	req, err := http.NewRequest(method, l.srv.URL+path, body)
	if err != nil {
		l.t.Fatalf("viewtest: %s %s: %v", method, path, err)
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Dest", "document")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", l.srv.URL)
	}
	res, err := l.client.Do(req)
	if err != nil {
		l.t.Fatalf("viewtest: %s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if l.conn != nil {
		l.conn.close()
		l.conn = nil
	}
	l.at, l.static, l.err = res.Request.URL.RequestURI(), string(b), ""
	l.fills, l.asked = map[string]fill{}, map[string]bool{}
	m := tokenAttr.FindStringSubmatch(l.static)
	if m == nil {
		return
	}
	c, err := dialLive(l.srv.URL, l.client.Jar, m[1])
	if err != nil {
		l.t.Fatalf("viewtest: connecting %s: %v", l.at, err)
	}
	reply, err := c.roundTrip(live.ClientMessage{Type: live.Join})
	if err != nil || reply.Type != live.Render {
		c.close()
		l.t.Fatalf("viewtest: joining %s: %s %s %v", l.at, reply.Type, reply.Error, err)
	}
	l.conn = c
	l.run(reply.JS)
}

// HTML is the page as it stands: the document the connection holds, or
// the one served.
func (l *Live) HTML() string {
	if l.conn == nil {
		return l.static
	}
	h, _ := l.conn.state()
	return h
}

// URL is the page's URL, its path and query: where navigations and
// redirects moved it.
func (l *Live) URL() string { return l.at }

// Err is the error the last action's reply carried: what an event handler
// returned for the visitor to see (a flash); "" when none.
func (l *Live) Err() string { return l.err }

// doc is the page parsed, with what the test filled where the server
// still renders the field as it did then.
func (l *Live) doc() *node {
	root := parse(l.HTML())
	for k, f := range l.fills {
		if l.server(root, k) != f.server {
			delete(l.fills, k)
		}
	}
	return root
}

// key names a field across renders: a radio group by its name, a
// checkbox by its name and value.
func key(e *node) string {
	switch {
	case checkable(e) && strings.EqualFold(e.get("type"), "radio"):
		return "radio\x00" + e.get("name")
	case checkable(e):
		return "check\x00" + e.get("name") + "\x00" + e.get("value")
	}
	return "field\x00" + e.get("name")
}

// server is what the server renders of the field k names.
func (l *Live) server(root *node, k string) string {
	out := "\x00absent"
	root.walk(func(e *node) bool {
		if !isField(e) || key(e) != k {
			return true
		}
		switch {
		case checkable(e) && strings.EqualFold(e.get("type"), "radio"):
			if out == "\x00absent" {
				out = ""
			}
			if e.has("checked") {
				out = e.get("value")
			}
			return true
		case checkable(e):
			out = strconv.FormatBool(e.has("checked"))
		default:
			out = renderedValue(e)
		}
		return false
	})
	return out
}

func renderedValue(e *node) string {
	switch e.tag {
	case "textarea":
		return e.textContent()
	case "select":
		var first, chosen *node
		e.walk(func(o *node) bool {
			if o.tag == "option" {
				if first == nil {
					first = o
				}
				if o.has("selected") && chosen == nil {
					chosen = o
				}
			}
			return true
		})
		if chosen == nil {
			chosen = first
		}
		if chosen == nil {
			return ""
		}
		return optionValue(chosen)
	}
	return e.get("value")
}

func optionValue(o *node) string {
	if v, ok := o.attr("value"); ok {
		return v
	}
	return o.textContent()
}

// value is a field's value as the page holds it.
func (l *Live) value(e *node) string {
	if checkable(e) {
		if l.checked(e) {
			return cmpOr(e.get("value"), "on")
		}
		return ""
	}
	if f, ok := l.fills[key(e)]; ok {
		return f.value
	}
	return renderedValue(e)
}

func (l *Live) checked(e *node) bool {
	f, ok := l.fills[key(e)]
	switch {
	case !ok:
		return e.has("checked")
	case strings.EqualFold(e.get("type"), "radio"):
		return f.value == e.get("value")
	}
	return f.on
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// formValues are the fields form sends: its controls', and the button
// submitting it.
func (l *Live) formValues(form, submitter *node) url.Values {
	v := url.Values{}
	form.walk(func(e *node) bool {
		name := e.get("name")
		if e == form || name == "" || e.has("disabled") || !isField(e) {
			return true
		}
		switch strings.ToLower(e.get("type")) {
		case "submit", "button", "reset", "image", "file":
			return true
		}
		if e.tag == "select" && e.has("multiple") {
			e.walk(func(o *node) bool {
				if o.tag == "option" && o.has("selected") {
					v.Add(name, optionValue(o))
				}
				return true
			})
			return true
		}
		if checkable(e) && !l.checked(e) {
			return true
		}
		v.Add(name, l.value(e))
		return true
	})
	if submitter != nil && submitter.get("name") != "" {
		v.Add(submitter.get("name"), submitter.get("value"))
	}
	return v
}

func (l *Live) mustFind(root *node, what, loc string, kind int) *node {
	l.t.Helper()
	e := find(root, loc, kind)
	if e == nil {
		l.t.Fatalf("viewtest: %s: no element matches %q on %s:\n%s", what, loc, l.at, clip(l.HTML()))
	}
	return e
}

// Click clicks the element loc names, as a person does: its click handler
// gets the event (a struct form's first, when the element is in one); a
// link moves the page — over the connection when it is a ui.Link — and a
// form's submit button submits it. A click nothing answers fails the
// test, as does one only a JS command answers (that needs Browse).
func (l *Live) Click(loc string) *Live {
	l.t.Helper()
	root := l.doc()
	el := l.mustFind(root, "Click", loc, clickKind)
	if checkable(el) {
		return l.Check(loc, strings.EqualFold(el.get("type"), "radio") || !l.checked(el))
	}
	answered := l.dispatch(root, el, "click")
	if a := el.closest(func(e *node) bool { return e.tag == "a" && e.has("href") }); a != nil && !answered {
		l.follow(a)
		return l
	}
	if el.tag == "button" || el.tag == "input" {
		t := strings.ToLower(cmpOr(el.get("type"), "submit"))
		if form := el.closest(func(e *node) bool { return e.tag == "form" }); form != nil && (t == "submit" || t == "image") {
			if !answered || form.has("data-vk-on-submit") {
				l.submit(l.doc(), form, el)
			}
			return l
		}
	}
	if !answered {
		if owner(el, "click", "data-vk-js-") != nil {
			l.t.Fatalf("viewtest: Click(%q): only JS commands answer it; drive the page in a browser (Browse)", loc)
		}
		l.t.Fatalf("viewtest: Click(%q): nothing answers a click on %s", loc, clip(outer(el)))
	}
	return l
}

// follow moves the page to a link's href.
func (l *Live) follow(a *node) {
	l.t.Helper()
	href := a.get("href")
	if mode, ok := a.attr("data-vk-link"); ok && l.conn != nil && !a.has("data-vk-reload") {
		l.navigate(href, mode == "patch")
		return
	}
	l.request(http.MethodGet, l.resolve(href), nil)
}

// resolve is href as a path on the app's server.
func (l *Live) resolve(href string) string {
	base, _ := url.Parse(l.srv.URL + l.at)
	u, err := base.Parse(href)
	if err != nil {
		l.t.Fatalf("viewtest: %q: %v", href, err)
	}
	return u.RequestURI()
}

// Fill sets a field to value, as typing it and moving on does: the
// field's (and its struct form's) input, change and focusout handlers get
// their events.
func (l *Live) Fill(loc, value string) *Live {
	l.t.Helper()
	root := l.doc()
	el := l.mustFind(root, "Fill", loc, fieldKind)
	if !isField(el) || checkable(el) {
		l.t.Fatalf("viewtest: Fill(%q): %s isn't a text field", loc, clip(outer(el)))
	}
	l.set(root, el, fill{value: value})
	for _, typ := range []string{"input", "change", "focusout"} {
		l.dispatch(l.doc(), l.refind(el), typ)
	}
	return l
}

// Check sets a checkbox, or chooses a radio button (on is then true), as
// clicking it does.
func (l *Live) Check(loc string, on bool) *Live {
	l.t.Helper()
	root := l.doc()
	el := l.mustFind(root, "Check", loc, fieldKind)
	if !checkable(el) {
		l.t.Fatalf("viewtest: Check(%q): %s isn't a checkbox or radio button", loc, clip(outer(el)))
	}
	if l.checked(el) == on {
		return l
	}
	if strings.EqualFold(el.get("type"), "radio") {
		l.set(root, el, fill{value: el.get("value")})
	} else {
		l.set(root, el, fill{on: on})
	}
	for _, typ := range []string{"click", "input", "change"} {
		l.dispatch(l.doc(), l.refind(el), typ)
	}
	return l
}

// Select chooses the option of a select whose value, or else text, is
// option.
func (l *Live) Select(loc, option string) *Live {
	l.t.Helper()
	root := l.doc()
	el := l.mustFind(root, "Select", loc, fieldKind)
	if el.tag != "select" {
		l.t.Fatalf("viewtest: Select(%q): %s isn't a select", loc, clip(outer(el)))
	}
	var chosen *node
	el.walk(func(o *node) bool {
		if o.tag == "option" && (optionValue(o) == option || o.textContent() == squash(option)) {
			chosen = o
		}
		return chosen == nil
	})
	if chosen == nil {
		l.t.Fatalf("viewtest: Select(%q): no option %q in %s", loc, option, clip(outer(el)))
	}
	l.set(root, el, fill{value: optionValue(chosen)})
	for _, typ := range []string{"input", "change"} {
		l.dispatch(l.doc(), l.refind(el), typ)
	}
	return l
}

func (l *Live) set(root, el *node, f fill) {
	k := key(el)
	f.server = l.server(root, k)
	l.fills[k] = f
}

// refind is el in the page as it stands: the element of the same field
// (or at the same place) after a reply re-rendered it.
func (l *Live) refind(el *node) *node {
	root := l.doc()
	if isField(el) && el.get("name") != "" {
		var found *node
		root.walk(func(e *node) bool {
			if isField(e) && key(e) == key(el) && (!checkable(e) || e.get("value") == el.get("value")) {
				found = e
			}
			return found == nil
		})
		if found != nil {
			return found
		}
	}
	return el
}

// Submit submits the form loc names — the form, a button submitting it
// or any element in it — with its fields as they stand: over the
// connection when the form has a submit handler, else posted (or got) to
// its action as a browser without script does. A button named sends its
// own name and value too.
func (l *Live) Submit(loc string) *Live {
	l.t.Helper()
	root := l.doc()
	el := l.mustFind(root, "Submit", loc, clickKind)
	form := el.closest(func(e *node) bool { return e.tag == "form" })
	if form == nil {
		l.t.Fatalf("viewtest: Submit(%q): %s is in no form", loc, clip(outer(el)))
	}
	var submitter *node
	if el.tag == "button" || el.tag == "input" && strings.EqualFold(el.get("type"), "submit") {
		submitter = el
	}
	l.submit(root, form, submitter)
	return l
}

func (l *Live) submit(root, form, submitter *node) {
	l.t.Helper()
	if l.conn != nil && owner(form, "submit", "data-vk-on-") != nil {
		l.dispatchFrom(root, form, form, "submit", submitter)
		return
	}
	method := strings.ToUpper(cmpOr(form.get("method"), "GET"))
	action := l.resolve(cmpOr(form.get("action"), l.at))
	if method != http.MethodPost {
		method = http.MethodGet
	}
	l.request(method, action, l.formValues(form, submitter))
}

var nobubble = map[string]bool{"focus": true, "blur": true, "mouseenter": true, "mouseleave": true, "load": true, "error": true, "scroll": true}

// owner is the element answering an event of typ at el: el or its nearest
// ancestor with the attribute prefix+typ.
func owner(el *node, typ, prefix string) *node {
	if nobubble[typ] {
		if el.has(prefix + typ) {
			return el
		}
		return nil
	}
	return el.closest(func(e *node) bool { return e.has(prefix + typ) })
}

// dispatch sends an event of typ at el to whatever answers it, as the
// runtime does; it reports whether anything did.
func (l *Live) dispatch(root, el *node, typ string) bool {
	l.t.Helper()
	return l.dispatchFrom(root, el, el, typ, nil)
}

func (l *Live) dispatchFrom(root, target, at *node, typ string, submitter *node) bool {
	l.t.Helper()
	if l.conn == nil {
		return false
	}
	el := owner(at, typ, "data-vk-on-")
	if el == nil {
		return false
	}
	if el.tag != "form" && typ != "submit" {
		if vf := el.closest(func(e *node) bool { return e.tag == "form" && e.has("data-vk-form") }); vf != nil && vf.has("data-vk-on-"+typ) {
			l.send(root, vf, target, typ, nil)
			root = l.doc()
		}
	}
	l.send(root, el, target, typ, submitter)
	return true
}

// send sends el's handler of typ its event, t the element it happened at,
// and applies the reply.
func (l *Live) send(root, el, t *node, typ string, submitter *node) {
	l.t.Helper()
	ev := ui.Event{Target: el.get("data-vk-on-" + typ), Type: typ}
	switch {
	case el.tag == "input" && strings.EqualFold(el.get("type"), "checkbox"):
		ev.Value = strconv.FormatBool(l.checked(el))
	case isField(el):
		ev.Value = l.value(el)
	case el.tag == "button" || el.tag == "option":
		ev.Value = el.get("value")
	}
	if el.tag == "form" && el.has("data-vk-form") {
		l.markUsed(root, el, t, typ)
		ev.Form = l.formValues(el, submitter)
		l.seq++
		ev.Form.Set(ui.FormSeqField, strconv.Itoa(l.seq))
	} else if typ == "submit" {
		form := el
		if f := t.closest(func(e *node) bool { return e.tag == "form" }); f != nil {
			form = f
		}
		ev.Form = l.formValues(form, submitter)
	}
	reply, err := l.conn.roundTrip(live.ClientMessage{Type: live.EventMsg, Event: ev})
	if err != nil {
		l.t.Fatalf("viewtest: %s at %s: %v", typ, clip(outer(t)), err)
	}
	l.settle(reply, typ)
}

// markUsed marks the field t in its struct form's used fields as the
// runtime does: left with something in it, changed while in error, a
// choice made.
func (l *Live) markUsed(root, form, t *node, typ string) {
	if t == nil || !isField(t) || t.get("name") == "" {
		return
	}
	filled := l.value(t) != ""
	if !(typ == "focusout" && filled || t.get("aria-invalid") == "true" || typ == "change" && checkable(t)) {
		return
	}
	used := query(form, `input[name="`+ui.FormUsedField+`"]`)
	if used == nil {
		return
	}
	names := strings.Split(l.value(used), ",")
	if names[0] == "" {
		names = nil
	}
	for _, n := range names {
		if n == t.get("name") {
			return
		}
	}
	l.set(root, used, fill{value: strings.Join(append(names, t.get("name")), ",")})
}

// settle applies what a reply asks of the page beyond its markup: a load,
// a navigation, the error it carries.
func (l *Live) settle(reply live.ServerMessage, what string) {
	l.t.Helper()
	switch reply.Type {
	case live.Error:
		l.t.Fatalf("viewtest: %s on %s: %s", what, l.at, reply.Error)
	case live.Redirect:
		l.request(http.MethodGet, l.resolve(reply.URL), nil)
		return
	}
	l.err = reply.Error
	l.run(reply.JS)
}

// run follows the navigations among commands a reply pushed: what
// web.Redirect from an event, or ui.JS.Navigate pushed, becomes.
func (l *Live) run(pushes []live.JSPush) {
	l.t.Helper()
	for _, p := range pushes {
		var ops []json.RawMessage
		json.Unmarshal(p.Ops, &ops)
		for _, op := range ops {
			var pair []json.RawMessage
			var name string
			var args struct{ Href string }
			if json.Unmarshal(op, &pair) != nil || len(pair) < 2 || json.Unmarshal(pair[0], &name) != nil {
				continue
			}
			json.Unmarshal(pair[1], &args)
			if (name == "navigate" || name == "patch") && args.Href != "" {
				l.navigate(args.Href, name == "patch")
				return
			}
		}
	}
}

// Navigate moves the page to url as a link does: over the connection to
// another page (or HTTP when the server says it isn't one there), or, on
// a page that isn't live, by loading it.
func (l *Live) Navigate(url string) *Live {
	l.t.Helper()
	if l.conn == nil {
		l.request(http.MethodGet, l.resolve(url), nil)
		return l
	}
	l.navigate(url, false)
	return l
}

func (l *Live) navigate(href string, patch bool) {
	l.t.Helper()
	reply, err := l.conn.roundTrip(live.ClientMessage{Type: live.Navigate, URL: l.resolve(href), Patch: patch})
	if err != nil {
		l.t.Fatalf("viewtest: navigating to %s: %v", href, err)
	}
	if reply.Type == live.PatchMsg {
		if reply.URL != "" {
			l.at = reply.URL
		}
		if !patch {
			l.asked = map[string]bool{}
		}
	}
	l.settle(reply, "navigating to "+href)
}

// Hover moves the pointer onto the element loc names: on a ui.Link to
// another page the connection renders that page ahead (a prefetch), so
// the click that follows takes it.
func (l *Live) Hover(loc string) *Live {
	l.t.Helper()
	root := l.doc()
	el := l.mustFind(root, "Hover", loc, clickKind)
	a := el.closest(func(e *node) bool { return e.tag == "a" && e.has("href") })
	if l.conn == nil || a == nil || a.get("data-vk-link") != "" || !a.has("data-vk-link") || a.has("data-vk-reload") || a.has("download") {
		return l
	}
	if mode := a.get("data-vk-prefetch"); mode == "off" || mode == "false" {
		return l
	}
	href := a.get("href")
	if !strings.HasPrefix(href, "/") || strings.HasPrefix(href, "//") || l.resolve(href) == l.at {
		return l
	}
	if err := l.conn.write(map[string]any{"type": "prefetch", "url": l.resolve(href)}); err != nil {
		l.t.Fatalf("viewtest: Hover(%q): %v", loc, err)
	}
	return l
}

// ScrollTo brings the element loc names into view: the deferred groups
// loading when seen (ui.Defer("x").WhenSeen()) whose fallbacks are it or
// inside it start loading, and the lazy regions loading when seen
// (ui.Lazy's seen) that are it or inside it, shown, ask for their content;
// Expect waits for what they render.
func (l *Live) ScrollTo(loc string) *Live {
	l.t.Helper()
	root := l.doc()
	el := l.mustFind(root, "ScrollTo", loc, anyKind)
	var groups, inits []string
	el.walk(func(e *node) bool {
		if g := e.get("data-vk-seen"); g != "" && !l.asked[g] {
			l.asked[g] = true
			groups = append(groups, g)
		}
		if e.has("data-vk-when-seen") && e.has("data-vk-init") && e.get("data-vk-on-vkinit") != "" && shown(e) {
			inits = append(inits, e.get("data-vk-on-vkinit"))
		}
		return true
	})
	if len(groups) > 0 && l.conn != nil {
		if err := l.conn.write(map[string]any{"type": "loads", "groups": groups}); err != nil {
			l.t.Fatalf("viewtest: ScrollTo(%q): %v", loc, err)
		}
	}
	for _, target := range inits {
		if l.conn == nil {
			break
		}
		reply, err := l.conn.roundTrip(live.ClientMessage{Type: live.EventMsg, Event: ui.Event{Target: target, Type: "vkinit"}})
		if err != nil {
			l.t.Fatalf("viewtest: ScrollTo(%q): %v", loc, err)
		}
		l.settle(reply, "ScrollTo("+loc+")")
	}
	return l
}

// Text is the text of the element loc names now, white space squashed;
// "" when none matches.
func (l *Live) Text(loc string) string {
	if e := find(l.doc(), loc, anyKind); e != nil {
		return e.textContent()
	}
	return ""
}

// Expect is the element loc names, to check as the page changes.
func (l *Live) Expect(loc string) *Expectation { return &Expectation{s: l, loc: loc} }

// Wait waits for cond to hold, checking it as each message arrives (and
// every few milliseconds between, for a cond reading more than the page),
// and fails the test naming what it waited for once Timeout passes.
//
//	p.Wait("two rows", func() bool { return strings.Count(p.HTML(), "<tr") == 2 })
func (l *Live) Wait(what string, cond func() bool) {
	l.t.Helper()
	l.until(what, cond, func() string { return "the page:\n" + clip(l.HTML()) })
}

func (l *Live) tb() testing.TB { return l.t }

func (l *Live) look(loc string, kind int) look {
	root := l.doc()
	e := find(root, loc, kind)
	if e == nil {
		return look{}
	}
	return look{Found: true, Text: e.textContent(), Value: l.value(e), Visible: shown(e), Markup: outer(e)}
}

func (l *Live) until(what string, ok func() bool, last func() string) {
	l.t.Helper()
	end := time.Now().Add(Timeout)
	for {
		var changed <-chan struct{}
		if l.conn != nil {
			_, changed = l.conn.state()
		}
		if ok() {
			return
		}
		left := time.Until(end)
		if left <= 0 {
			l.t.Fatalf("viewtest: waiting for %s on %s: %s", what, l.at, last())
		}
		select { // a message changing the page, or a while: a condition may read more than the page
		case <-changed:
		case <-time.After(min(left, 20*time.Millisecond)):
		}
	}
}

// outer is an element's markup, rebuilt, for a message.
func outer(e *node) string {
	var b strings.Builder
	var rec func(*node)
	rec = func(n *node) {
		if n.tag == "" {
			b.WriteString(n.text)
			return
		}
		b.WriteString("<" + n.tag)
		for _, a := range n.attrs {
			b.WriteString(" " + a[0] + `="` + a[1] + `"`)
		}
		b.WriteString(">")
		for _, k := range n.kids {
			rec(k)
		}
		if !voids[n.tag] {
			b.WriteString("</" + n.tag + ">")
		}
	}
	rec(e)
	return b.String()
}
