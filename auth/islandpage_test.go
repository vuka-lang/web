package auth_test

import (
	"context"
	"encoding/json"
	stdhtml "html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/web"
	"github.com/vuka-lang/web/auth"
	"github.com/vuka-lang/web/auth/authtest"
)

type roster struct {
	Names []string `json:"names"`
}

func (r *roster) LiveSchema(l *ui.LiveSchema)   { l.Mount(r.mount) }
func (r *roster) mount(_ context.Context) error { r.Names = []string{"Ada"}; return nil }
func (r *roster) Add(name string)               { r.Names = append(r.Names, name) }

// tally defers its counts: their load is a call, gated as the page is.
type tally struct {
	Counts map[string]int `json:"counts" defer:""`
}

func (b *tally) LiveSchema(l *ui.LiveSchema) { l.Loads("counts", b.loadCounts) }
func (b *tally) loadCounts(_ context.Context) error {
	b.Counts = map[string]int{"open": 2}
	return nil
}

func init() {
	ui.Island("components/Roster.vue")(vuka.TypeOf[roster]("auth_test.roster"))
	ui.Island("components/Tally.vue")(vuka.TypeOf[tally]("auth_test.tally"))
}

func TestIslandPageDeferredGates(t *testing.T) {
	users := authtest.NewUsers()
	app := func(id *auth.Identity) http.Handler {
		return web.New(web.Only("none"), auth.Module(auth.Config{Users: users}), authtest.As(id)).
			Page("/tally", &tally{}, auth.Requires("staff")).Handler()
	}
	serve := func(h http.Handler, method, body string) (int, string) {
		r := httptest.NewRequest(method, "/tally", strings.NewReader(body))
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	staff := app(&auth.Identity{ID: "1", Perms: []string{"staff"}})
	_, page := serve(staff, "GET", "")
	props := stdhtml.UnescapeString(regexp.MustCompile(`data-vk-props="([^"]*)"`).FindStringSubmatch(page)[1])
	sig := stdhtml.UnescapeString(regexp.MustCompile(`data-vk-sig="([^"]*)"`).FindStringSubmatch(page)[1])
	if props != "{}" {
		t.Errorf("first render's props: %s", props)
	}
	load, _ := json.Marshal(map[string]any{"load": true, "props": props, "sig": sig})
	if code, body := serve(staff, "POST", string(load)); code != 200 || !strings.Contains(body, `\"open\":2`) {
		t.Errorf("staff's load: %d %s", code, body)
	}
	if code, body := serve(app(&auth.Identity{ID: "2"}), "POST", string(load)); code != http.StatusForbidden || strings.Contains(body, "open") {
		t.Errorf("a load without the perm: %d %s", code, body)
	}
	if code, body := serve(app(nil), "POST", string(load)); code == 200 || strings.Contains(body, "open") {
		t.Errorf("an anonymous load: %d %s", code, body)
	}
}

// A page backed by its island takes its type's gates: its page and every
// call; its component knows the visitor ($vuka).
func TestIslandPageGates(t *testing.T) {
	users := authtest.NewUsers()
	app := func(id *auth.Identity) http.Handler {
		return web.New(web.Only("none"), auth.Module(auth.Config{Users: users}), authtest.As(id)).
			Page("/roster", &roster{}, auth.Requires("staff")).Handler()
	}
	serve := func(h http.Handler, method, body string) (int, string) {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, "/roster", nil)
		} else {
			r = httptest.NewRequest(method, "/roster", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	staff := app(&auth.Identity{ID: "1", Perms: []string{"staff"}})
	code, page := serve(staff, "GET", "")
	if code != 200 || !strings.Contains(page, `data-vk-viewer="{&#34;perms&#34;:[&#34;staff&#34;],&#34;roles&#34;:[],&#34;user&#34;:{&#34;id&#34;:&#34;1&#34;}}"`) {
		t.Fatalf("%d: the viewer isn't the identity: %s", code, page)
	}
	props := stdhtml.UnescapeString(regexp.MustCompile(`data-vk-props="([^"]*)"`).FindStringSubmatch(page)[1])
	sig := stdhtml.UnescapeString(regexp.MustCompile(`data-vk-sig="([^"]*)"`).FindStringSubmatch(page)[1])
	call, _ := json.Marshal(map[string]any{"method": "add", "args": []any{"Bo"}, "props": props, "sig": sig})
	if code, body := serve(staff, "POST", string(call)); code != 200 || !strings.Contains(body, `Bo`) {
		t.Errorf("staff's call: %d %s", code, body)
	}
	if code, _ := serve(app(&auth.Identity{ID: "2"}), "POST", string(call)); code != http.StatusForbidden {
		t.Errorf("a call without the perm: %d", code)
	}
	if code, _ := serve(app(nil), "POST", string(call)); code == 200 {
		t.Errorf("an anonymous call: %d", code)
	}
	if code, _ := serve(app(&auth.Identity{ID: "2"}), "GET", ""); code != http.StatusForbidden {
		t.Errorf("the page without the perm: %d", code)
	}
}

// A stateless page's signed props are the visitor's: replayed by another
// user, anonymously, or after a sign-out and in again, they don't verify.
func TestIslandPagePropsAreTheVisitors(t *testing.T) {
	app := newApp(t, users(), nil)
	app.Page("/roster", &roster{}, auth.Public())
	ada1, bob, anon := serve(t, app), serve(t, app), serve(t, app)
	ada1.login("ada@example.com", "correct horse", "")
	bob.login("bob@example.com", "hunter22", "")
	page := ada1.page("/roster").body
	props := stdhtml.UnescapeString(regexp.MustCompile(`data-vk-props="([^"]*)"`).FindStringSubmatch(page)[1])
	sig := stdhtml.UnescapeString(regexp.MustCompile(`data-vk-sig="([^"]*)"`).FindStringSubmatch(page)[1])
	body, _ := json.Marshal(map[string]any{"method": "add", "args": []any{"Bo"}, "props": props, "sig": sig})
	call := func(c *client) answer {
		return c.do("POST", "/roster", string(body), "Content-Type", "application/json", "Origin", c.srv.URL)
	}
	if r := call(ada1); r.status != 200 || !strings.Contains(r.body, "Bo") {
		t.Errorf("ada's own call: %d %s", r.status, r.body)
	}
	for name, c := range map[string]*client{"bob": bob, "anonymous": anon} {
		if r := call(c); r.status != http.StatusForbidden || strings.Contains(r.body, "Bo") {
			t.Errorf("%s replaying ada's props: %d %s", name, r.status, r.body)
		}
	}
	ada1.do("POST", "/auth/logout", "", "Origin", ada1.srv.URL)
	if r := call(ada1); r.status != http.StatusForbidden {
		t.Errorf("after ada signed out: %d %s", r.status, r.body)
	}
	ada1.login("ada@example.com", "correct horse", "")
	if r := call(ada1); r.status != http.StatusForbidden {
		t.Errorf("ada signed in again: %d %s", r.status, r.body)
	}
}
