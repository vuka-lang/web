package viewtest_test

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/ui/v2/live/livetest"
	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/viewtest"
)

type userVars struct{ Me string }

func userContext(context.Context) (userVars, error) { return userVars{Me: "from the processor"}, nil }

func me() ui.Node {
	return ui.NodeFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "me="+web.Var[string](ctx, "me"))
		return err
	})
}

func TestWith(t *testing.T) {
	app := web.New(web.Only("none"), web.NoDotEnv(), web.ContextProcessors(userContext), viewtest.With(userVars{Me: "ada"}))
	app.Get("/", func() ui.Node { return me() })
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Body.String() != "me=ada" {
		t.Errorf("body %q", rec.Body.String())
	}
}

type mePanel struct{ ui.Live }

func (p *mePanel) Render() ui.Node { return ui.El("p", nil, me()) }

func TestContextWithLivetest(t *testing.T) {
	ctx := viewtest.Context(context.Background(), userVars{Me: "grace"})
	s := live.NewSession(ctx, func(context.Context) ui.Node { return ui.Component("me", nil, &mePanel{}) })
	defer s.Close()
	c, err := livetest.Join(s, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.HTML(), "me=grace") {
		t.Errorf("html %s", c.HTML())
	}
}
