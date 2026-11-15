package web_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/viewtest"
)

// A page's deferred groups load concurrently after it renders, each
// patching on its own, and a component an event mounts loads its own.
func TestLiveDeferredTemplate(t *testing.T) {
	app, release := web.StatsApp()

	// The first render: every group loading, whatever the loaders do.
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/stats", nil))
	for _, want := range []string{"stats loading", "rows loading", "users loading"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the first render lacks %q: %s", want, rec.Body)
		}
	}

	// stats and users arrive while rows still wait.
	p := viewtest.Open(t, app, "/stats")
	p.Expect("#stats").Text("open 3")
	p.Expect("#users").Text("users Grace")
	if got := p.Text("#rows"); got != "rows loading" {
		t.Errorf("rows before their release: %q", got)
	}
	release()
	p.Expect("#rows").Text("rows Ada,Alan")

	// A component an event mounts loads its own group.
	p.Click("#more")
	p.Expect("#more-users").Text("users Grace")
}
