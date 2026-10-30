package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/auth"
	"github.com/vuka-lang/web/v2/auth/authtest"
)

// A handler that signs in can be tested with authtest.As: the identity it
// gives keeps the request's response, so SignIn sets its cookie
// (vuka#231).
func TestAuthtestSignsIn(t *testing.T) {
	for _, who := range []*auth.Identity{nil, {ID: "1"}} {
		app := web.New(web.Only("none"), web.NoDotEnv(),
			auth.Module(auth.Config{Users: users(), Passwords: authtest.Cheap, FailedLoginTime: -1}), authtest.As(who))
		type creds struct{ Login, Password string }
		app.Post("/signin", func(ctx context.Context, in creds) (string, error) {
			id, err := auth.Login(ctx, in.Login, in.Password)
			if err != nil {
				return "", err
			}
			return "signed in as " + id.ID, nil
		}, web.Attrs(auth.Public()))
		req := httptest.NewRequest("POST", "/signin", strings.NewReader(url.Values{"login": {"bob@example.com"}, "password": {"hunter22"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		app.Handler().ServeHTTP(w, req)
		var session bool
		for _, c := range (&http.Response{Header: w.Header()}).Cookies() {
			session = session || c.Name == "vk_session" && c.Value != ""
		}
		if w.Code != 200 || !strings.Contains(w.Body.String(), "signed in as 2") || !session {
			t.Errorf("as %v: %d %s %v", who, w.Code, w.Body, w.Header().Values("Set-Cookie"))
		}
	}
}
