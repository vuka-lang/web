package auth

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/web"
)

// routeEndpoints mounts the login, logout, me and token endpoints under
// prefix.
func (a *Auth) routeEndpoints(app *web.App, prefix string) {
	prefix = "/" + strings.Trim(prefix, "/")
	open := web.Attrs(Public())
	app.Post(prefix+"/login", a.handleLogin, open)
	app.Post(prefix+"/logout", a.handleLogout, open)
	app.Get(prefix+"/me", a.handleMe)
	app.Post(prefix+"/token", a.handleToken, open)
}

type credentials struct {
	Login    string `json:"login"`
	Password string `json:"password"`
	Next     string `json:"next"`
	Name     string `json:"name"` // a token's
	Remember bool   `json:"remember"`
}

// readCredentials reads a JSON body, else a form.
func readCredentials(w http.ResponseWriter, r *http.Request) (credentials, bool, error) {
	var c credentials
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" {
		err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&c)
		if err != nil {
			return c, true, web.BadRequest("invalid JSON")
		}
		return c, true, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		return c, false, web.BadRequest("invalid form")
	}
	c.Login, c.Password, c.Next, c.Name = r.PostFormValue("login"), r.PostFormValue("password"), r.PostFormValue("next"), r.PostFormValue("name")
	switch strings.ToLower(r.PostFormValue("remember")) {
	case "on", "1", "true", "yes":
		c.Remember = true
	}
	return c, false, nil
}

// fail answers a login's or token's error: JSON, with Retry-After when
// throttled.
func (a *Auth) fail(w http.ResponseWriter, r *http.Request, err error) {
	var te *ThrottledError
	if errors.As(err, &te) {
		w.Header().Set("Retry-After", strconv.Itoa(int(te.RetryAfter.Seconds())+1))
	}
	a.app.WriteError(w, r, err, false)
}

// handleLogin is POST <prefix>/login: login and password (a form, or
// JSON). A form is sent on to next (or Home) once signed in, and back to
// the login page with ?error= otherwise; JSON gets the identity, or the
// error.
func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	c, isJSON, err := readCredentials(w, r)
	if st := stateOf(r.Context()); st != nil && a.cfg.RememberMe && !c.Remember {
		st.brief = true
	}
	if err == nil {
		_, err = Login(r.Context(), c.Login, c.Password)
	}
	if isJSON {
		if err != nil {
			a.fail(w, r, err)
			return
		}
		a.writeMe(w, r)
		return
	}
	if err != nil {
		code := "invalid"
		var te *ThrottledError
		switch {
		case errors.As(err, &te):
			code = "throttled"
		case !errors.Is(err, ErrInvalidLogin):
			var we *web.Error
			if !errors.As(err, &we) || we.Status >= 500 {
				a.app.WriteError(w, r, err, true)
				return
			}
			code = "refused"
		}
		q := url.Values{"error": {code}}
		if SafeNext(c.Next) {
			q.Set("next", c.Next)
		}
		http.Redirect(w, r, a.cfg.LoginPath+"?"+q.Encode(), http.StatusSeeOther)
		return
	}
	to := a.cfg.Home
	if SafeNext(c.Next) {
		to = c.Next
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// handleLogout is POST <prefix>/logout: the session ends (or the bearer
// token is revoked); a form goes to the login page, JSON gets 204.
func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := SignOut(r.Context()); err != nil {
		a.app.WriteError(w, r, err, web.AcceptsHTML(r))
		return
	}
	if web.AcceptsHTML(r) {
		http.Redirect(w, r, a.cfg.LoginPath, http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMe is GET <prefix>/me: who the request comes from.
func (a *Auth) handleMe(w http.ResponseWriter, r *http.Request) { a.writeMe(w, r) }

// Me is what GET /auth/me answers: the identity's id, roles and perms, and
// what Users.Public returns of the user, when Users has that method.
type Me struct {
	ID    string   `json:"id"`
	Roles []string `json:"roles,omitempty"`
	Perms []string `json:"perms,omitempty"`
	User  any      `json:"user,omitempty"`
}

func (a *Auth) writeMe(w http.ResponseWriter, r *http.Request) {
	id := Current(r.Context())
	if id == nil {
		a.app.WriteError(w, r, web.Unauthorized, false)
		return
	}
	me := Me{ID: id.ID, Roles: id.Roles, Perms: id.Perms}
	if p, ok := a.users.(publicUser); ok {
		me.User = p.Public(id)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(me)
}

// TokenResponse is what POST /auth/token answers, as OAuth 2's token
// endpoint does.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// handleToken is POST <prefix>/token: login and password (JSON or a form;
// an optional name says what the token is for) give a bearer token, shown
// once.
func (a *Auth) handleToken(w http.ResponseWriter, r *http.Request) {
	c, _, err := readCredentials(w, r)
	var id *Identity
	if err == nil {
		id, err = a.Verify(r.Context(), r, c.Login, c.Password)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	tok, t, err := a.Issue(r.Context(), id.ID, c.Name, 0)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(TokenResponse{AccessToken: tok, TokenType: "Bearer", ExpiresIn: int64(time.Until(t.Expires) / time.Second)})
}

// LoginError is the message for the login page's ?error= code, which
// POST /auth/login sends back with: "" when there is none.
func LoginError(r *http.Request) string {
	switch r.URL.Query().Get("error") {
	case "invalid":
		return msgInvalid
	case "throttled":
		return msgThrottled
	case "refused":
		return "This account can't sign in."
	}
	return ""
}

const (
	msgInvalid   = "Invalid login or password."
	msgThrottled = "Too many failed logins. Try again in a few minutes."
)

// LoginMessage is the message a login page shows for Login's error, for an
// app's own login handler re-rendering its form: the same for an unknown
// login and a wrong password, the throttle's, a refusal's own message (a
// *web.Error CheckLogin returned), and a general one for anything else
// (the error itself is the app's to log). "" for nil.
func LoginMessage(err error) string {
	var te *ThrottledError
	var we *web.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrInvalidLogin):
		return msgInvalid
	case errors.As(err, &te):
		return msgThrottled
	case errors.As(err, &we) && we.Status < 500 && we.Message != "":
		return we.Message
	}
	return "Signing in failed. Try again."
}

// LoginFormProps are a LoginForm's attributes.
type LoginFormProps struct {
	Action string // where it posts; "/auth/login" by default
	Next   string // where to go once signed in (SafeNext), else Config.Home
	Error  string // a message to show above the fields
	Login  string // the login field's value: what was typed, on a re-render
	// Remember adds a "Remember me" checkbox, the remember field
	// Config.RememberMe reads.
	Remember bool
	Class    string
}

// LoginForm is a sign-in form posting login and password to the login
// endpoint (Config.Endpoints):
//
//	<auth.LoginForm next={r.URL.Query().Get("next")} error={auth.LoginError(r)} />
//
// Replace it with your own markup when you like: the endpoint reads the
// fields login, password and next.
func LoginForm(p LoginFormProps) ui.Node {
	action := p.Action
	if action == "" {
		action = "/auth/login"
	}
	var msg ui.Node
	if p.Error != "" {
		msg = ui.El("p", []ui.Attr{{Name: "role", Value: "alert"}, {Name: "class", Value: "vk-login-error"}}, ui.Text(p.Error))
	}
	var remember ui.Node
	if p.Remember {
		remember = ui.El("label", []ui.Attr{{Name: "class", Value: "vk-login-remember"}},
			ui.El("input", []ui.Attr{{Name: "type", Value: "checkbox"}, {Name: "name", Value: "remember"}, {Name: "value", Value: "1"}}),
			ui.Text(" Remember me"))
	}
	var next ui.Node
	if SafeNext(p.Next) {
		next = ui.El("input", []ui.Attr{{Name: "type", Value: "hidden"}, {Name: "name", Value: "next"}, {Name: "value", Value: p.Next}})
	}
	return ui.El("form", []ui.Attr{{Name: "method", Value: "post"}, {Name: "action", Value: action}, {Name: "class", Value: "vk-login " + p.Class}},
		msg,
		ui.El("label", nil, ui.Text("Login"),
			ui.El("input", []ui.Attr{{Name: "name", Value: "login"}, {Name: "value", Value: p.Login}, {Name: "autocomplete", Value: "username"}, {Name: "required", Value: true}, {Name: "autofocus", Value: p.Login == ""}})),
		ui.El("label", nil, ui.Text("Password"),
			ui.El("input", []ui.Attr{{Name: "name", Value: "password"}, {Name: "type", Value: "password"}, {Name: "autocomplete", Value: "current-password"}, {Name: "required", Value: true}, {Name: "autofocus", Value: p.Login != ""}})),
		remember,
		next,
		ui.El("button", []ui.Attr{{Name: "type", Value: "submit"}}, ui.Text("Sign in")),
	)
}

// LoginPageProps are a LoginPage's attributes.
type LoginPageProps struct {
	// Request fills the form: on a GET its ?next= and ?error=; on a form
	// post (an app's own login handler re-rendering the page) the posted
	// login and next, so what was typed stays.
	Request *http.Request
	Title   string // "Sign in" by default
	Action  string // LoginForm's
	Error   string // the message to show, over Request's ?error= (LoginMessage)
	// Remember is LoginForm's: a "Remember me" checkbox.
	Remember bool
}

// LoginPage is a whole sign-in page around a LoginForm, for an app's
// login route:
//
//	@web.Get("/login")
//	@auth.Public
//	func Login(r *http.Request) ui.Node { return <auth.LoginPage request={r} /> }
func LoginPage(p LoginPageProps) ui.Node {
	title := p.Title
	if title == "" {
		title = "Sign in"
	}
	var next, msg, login string
	if r := p.Request; r != nil {
		next, msg = r.URL.Query().Get("next"), LoginError(r)
		if r.Method == http.MethodPost {
			next, login = r.PostFormValue("next"), r.PostFormValue("login")
		}
	}
	if p.Error != "" {
		msg = p.Error
	}
	return ui.El("html", []ui.Attr{{Name: "lang", Value: "en"}},
		ui.El("head", nil,
			ui.El("meta", []ui.Attr{{Name: "charset", Value: "utf-8"}}),
			ui.El("meta", []ui.Attr{{Name: "name", Value: "viewport"}, {Name: "content", Value: "width=device-width, initial-scale=1"}}),
			ui.El("title", nil, ui.Text(title)),
			ui.El("style", nil, ui.Text(loginCSS))),
		ui.El("body", nil, ui.El("main", nil,
			ui.El("h1", nil, ui.Text(title)),
			LoginForm(LoginFormProps{Action: p.Action, Next: next, Error: msg, Login: login, Remember: p.Remember}))))
}

const loginCSS = `body{font:16px/1.5 system-ui,sans-serif;margin:0;min-height:100vh;display:grid;place-items:center;background:#f6f7f9;color:#111}
main{width:min(22rem,100% - 2rem);background:#fff;padding:2rem;border-radius:.75rem;box-shadow:0 1px 3px #0002}
h1{margin:0 0 1rem;font-size:1.4rem}.vk-login{display:grid;gap:.9rem}.vk-login label{display:grid;gap:.25rem;font-size:.9rem}
.vk-login input{font:inherit;padding:.5rem .6rem;border:1px solid #ccd;border-radius:.4rem}
.vk-login button{font:inherit;padding:.55rem;border:0;border-radius:.4rem;background:#111;color:#fff;cursor:pointer}
.vk-login-error{margin:0;color:#b00020}.vk-login .vk-login-remember{display:flex;gap:.4rem;align-items:center}
@media (prefers-color-scheme:dark){body{background:#111;color:#eee}main{background:#1c1c1f}.vk-login input{background:#111;color:#eee;border-color:#444}.vk-login button{background:#eee;color:#111}.vk-login-error{color:#ff6b81}}`
