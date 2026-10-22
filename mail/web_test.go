package mail_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/web"
	"github.com/vuka-lang/web/internal/chrome"
	"github.com/vuka-lang/web/mail"
	"github.com/vuka-lang/web/mail/mailtest"
)

type signup struct {
	Name  string `form:"name"`
	Email string `form:"email"`
}

func welcome(name string) ui.Node {
	return ui.El("div", nil, ui.El("h1", nil, ui.Text("Welcome, "+name)), ui.El("p", nil, ui.Text("Glad you're here.")))
}

func panels(t *testing.T, h http.Handler, id string) string {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/_vuka/toolbar/r/"+id+"?panels", nil))
	var out struct{ HTML string }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	return out.HTML
}

func TestProvideFromEnv(t *testing.T) {
	t.Setenv("MAIL_DRIVER", "smtp")
	t.Setenv("MAIL_HOST", "smtp.example.com")
	t.Setenv("MAIL_PORT", "2525")
	t.Setenv("MAIL_ENCRYPTION", "none")
	t.Setenv("MAIL_FROM", "hello@example.com")
	t.Setenv("MAIL_FROM_NAME", "Pets")
	app := web.New(web.Only("none"), web.NoDotEnv(), mail.Provide())
	m, err := web.Resolve[*mail.Mailer](app)
	if err != nil || m.Driver() != "smtp" {
		t.Fatalf("%v %v", m, err)
	}
	t.Setenv("MAIL_PORT", "lots")
	app = web.New(web.Only("none"), web.NoDotEnv(), mail.Provide())
	app.Get("/", func(m *mail.Mailer) string { return "" })
	if err := app.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "MAIL_PORT") {
		t.Errorf("a bad MAIL_PORT: %v", err)
	}
	t.Setenv("MAIL_DRIVER", "carrier-pigeon")
	t.Setenv("MAIL_PORT", "")
	app = web.New(web.Only("none"), web.NoDotEnv(), mail.Provide())
	app.Get("/", func(m *mail.Mailer) string { return "" })
	if err := app.Start(context.Background()); err == nil || !strings.Contains(err.Error(), `unknown driver "carrier-pigeon"`) {
		t.Errorf("an unknown driver: %v", err)
	}
}

func TestSignupAndToolbar(t *testing.T) {
	t.Setenv(web.DevDirEnv, t.TempDir()) // as vuka run: the toolbar is on
	m := mailtest.New(mail.Config{From: "Pets <hello@pets.example>"})
	app := web.New(web.Only("none"), web.NoDotEnv(), web.Provide(m))
	app.Post("/signup", func(ctx context.Context, in signup, m *mail.Mailer) error {
		if err := m.Send(ctx, mail.Message{To: []string{in.Name + " <" + in.Email + ">"}, Subject: "Welcome", HTML: welcome(in.Name)}); err != nil {
			return err
		}
		return m.SendLater(ctx, mail.Message{To: []string{"team@pets.example"}, Subject: "New sign-up", Text: in.Email})
	})
	app.Post("/broken", func(ctx context.Context, m *mail.Mailer) error {
		return m.Send(ctx, mail.Message{To: []string{"x@example.com"}, Subject: "Hi\r\nBcc: victim@example.com"})
	})
	h := app.Handler()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/signup", strings.NewReader("name=Ada&email=ada@example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("signup: %d %s", w.Code, w.Body)
	}

	msg := mailtest.Last(t, m)
	if msg.Subject != "New sign-up" || len(mailtest.To(t, m, "ada@example.com")) != 1 {
		t.Errorf("sent: %+v", mailtest.Sent(t, m))
	}
	first := mailtest.Sent(t, m)[0]
	if first.Text != "Welcome, Ada\n\nGlad you're here." || !strings.Contains(first.HTML, "<h1>Welcome, Ada</h1>") {
		t.Errorf("the welcome: %q %q", first.Text, first.HTML)
	}

	html := panels(t, h, w.Header().Get(web.ToolbarHeader))
	for _, want := range []string{
		`data-tab="Mail">Mail <span class="count">2</span>`,
		`<b>Welcome</b>`, `logged, not sent`, `<td>Ada &lt;ada@example.com&gt;</td>`,
		`<iframe class="preview mail-preview" sandbox="" title="Welcome" srcdoc="&lt;div&gt;&lt;h1&gt;Welcome, Ada&lt;/h1&gt;`,
		`<b>New sign-up</b>`, `<pre class="wrap">ada@example.com</pre>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the Mail panel lacks %q:\n%s", want, html)
		}
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/broken", nil))
	html = panels(t, h, w.Header().Get(web.ToolbarHeader))
	if w.Code != 500 || !strings.Contains(html, "refused") || !strings.Contains(html, "header injection refused") {
		t.Errorf("a refused message: %d %s", w.Code, html)
	}

	// A cron's mail is on its own record.
	app2 := web.New(web.Only("none"), web.NoDotEnv(), web.Provide(m))
	app2.Cron("digest", "@daily", func(ctx context.Context, m *mail.Mailer) error {
		return m.Send(ctx, mail.Message{To: []string{"ada@example.com"}, Subject: "Your digest", Text: "Nothing new."})
	})
	h2 := app2.Handler()
	if err := app2.RunCron(context.Background(), "digest"); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, httptest.NewRequest("GET", "/_vuka/toolbar/crons", nil))
	var list struct{ Runs []struct{ ID string } }
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Runs) != 1 || !strings.Contains(panels(t, h2, list.Runs[0].ID), "<b>Your digest</b>") {
		t.Errorf("the cron's record: %s", w.Body)
	}
}

func TestNoToolbarNoNotes(t *testing.T) {
	m := mailtest.New()
	app := web.New(web.Only("none"), web.NoDotEnv(), web.Provide(m))
	app.Post("/x", func(ctx context.Context, m *mail.Mailer) error {
		return m.Send(ctx, mail.Message{To: []string{"a@example.com"}, Text: "x"})
	})
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 204 || w.Header().Get(web.ToolbarHeader) != "" || len(m.Sent()) != 1 {
		t.Errorf("%d %v", w.Code, w.Header())
	}
}

// TestBrowserMailPanel opens the Mail panel of a page that sent a message:
// the HTML shows in its preview.
func TestBrowserMailPanel(t *testing.T) {
	t.Setenv(web.DevDirEnv, t.TempDir())
	m := mailtest.New()
	app := web.New(web.Only("none"), web.NoDotEnv(), web.Provide(m))
	app.Get("/", func(ctx context.Context, m *mail.Mailer) (ui.Node, error) {
		err := m.Send(ctx, mail.Message{To: []string{"Ada <ada@example.com>"}, Subject: "Welcome", HTML: welcome("Ada"),
			Attachments: []mail.Attachment{{Filename: "terms.txt", Data: []byte("be kind")}}})
		page := ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("t"))),
			ui.El("body", nil, ui.El("main", nil, ui.Text("signed up"), ui.Component("c#1", nil, &clicks{}))))
		return page, err
	})
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	b := chrome.Launch(t)
	p := b.Open(srv.URL + "/")
	p.Viewport(1400, 900, false)
	tb := `document.querySelector("vuka-toolbar").shadowRoot`
	p.Wait(tb + `.querySelector(".handle .num")`)
	p.ClickElement(tb + `.querySelector(".handle")`)
	p.Wait(tb + `.querySelector('.tab[data-tab="Mail"]')`)
	p.ClickElement(tb + `.querySelector('.tab[data-tab="Mail"]')`)
	p.Wait(tb + `.querySelector('.panel[data-panel="Mail"]:not([hidden]) iframe')`)
	var text string
	p.Eval(tb+`.querySelector('.panel[data-panel="Mail"]').textContent`, &text)
	for _, want := range []string{"Welcome", "Ada <ada@example.com>", "terms.txt (7 B)", "logged, not sent"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Mail panel lacks %q: %s", want, text)
		}
	}
	if dir := os.Getenv("VUKA_SCREENSHOTS"); dir != "" {
		p.Screenshot(filepath.Join(dir, "toolbar-mail.png"))
	}
}

// clicks makes the test's page live, as the browser helper waits for.
type clicks struct {
	ui.Live
	n int
}

func (c *clicks) Inc() { c.n++ }

func (c *clicks) Render() ui.Node {
	return ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(c.Inc)}}, ui.Text("+"))
}

// greeter is a stateful component: its page is live.
type greeter struct {
	ui.Live
	joins *[]bool // IsLiveJoin as each event saw it
}

func (g *greeter) Hello(ctx context.Context) { *g.joins = append(*g.joins, web.IsLiveJoin(ctx)) }

func (g *greeter) Render() ui.Node {
	return ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(g.Hello)}}, ui.Text("hello"))
}

// A live page's route runs again when its socket joins: a handler sending
// mail sends it once with the IsLiveJoin guard, twice without.
func TestLiveJoinSendsOnce(t *testing.T) {
	m := mailtest.New(mail.Config{From: "hello@pets.example"})
	app := web.New(web.Only("none"), web.NoDotEnv(), web.Provide(m))
	var runs, joins []bool
	app.Get("/welcome", func(ctx context.Context, m *mail.Mailer) (ui.Node, error) {
		runs = append(runs, web.IsLiveJoin(ctx))
		if !web.IsLiveJoin(ctx) {
			if err := m.Send(ctx, mail.Message{To: []string{"ada@example.com"}, Subject: "Welcome", Text: "hi"}); err != nil {
				return nil, err
			}
		}
		return ui.El("html", nil, ui.El("body", nil, ui.Component("greeter", 0, &greeter{joins: &joins}))), nil
	})
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	// A client can't claim to be a join.
	req, _ := http.NewRequest("GET", srv.URL+"/welcome", nil)
	req.Header.Set("X-Vuka-Live", "join")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	tok := regexp.MustCompile(`data-vk-token="([^"]+)"`).FindStringSubmatch(string(b))
	if tok == nil {
		t.Fatalf("no token: %s", b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/_vuka/live?t="+tok[1], &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	roundTrip := func(msg string) string {
		t.Helper()
		if err := ws.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatal(err)
		}
		_, b, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	join := roundTrip(`{"type":"join","ref":1,"v":1}`)
	target := regexp.MustCompile(`data-vk-on-click=\\?"(c\d+:\d+)`).FindStringSubmatch(join)
	if target == nil {
		t.Fatalf("join: %s", join)
	}
	roundTrip(`{"type":"event","ref":2,"target":"` + target[1] + `","event":"click"}`)
	// A navigation is a visit of its own: the route runs as one.
	roundTrip(`{"type":"navigate","ref":3,"url":"/welcome"}`)

	if fmt.Sprint(runs) != "[false true false]" {
		t.Errorf("IsLiveJoin in the route's runs (load, join, navigate): %v", runs)
	}
	if fmt.Sprint(joins) != "[false]" {
		t.Errorf("IsLiveJoin in an event after the join: %v", joins)
	}
	if n := len(mailtest.Sent(t, m)); n != 2 {
		t.Errorf("%d mails for a load and a navigation, want 2", n)
	}
}
