package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/web"
)

const wrapperName = "auth.Module"

type gateKind int

const (
	public gateKind = iota
	required
	requiresAll
	requiresAny
)

// Gate is what a route, a live page or a live event needs of its caller:
// the typed attribute Public, Required, Requires and RequiresAny put on a
// declaration. Without one, a route needs a sign-in (unless
// Config.PublicByDefault).
type Gate struct {
	kind  gateKind
	perms []string
}

// RouteWrapper makes a route carrying a gate fail Build when the app has no
// auth.Module.
func (Gate) RouteWrapper() string { return wrapperName }

func (g Gate) String() string {
	switch g.kind {
	case public:
		return "Public"
	case required:
		return "Required"
	case requiresAll:
		return "Requires " + strings.Join(g.perms, ", ")
	}
	return "RequiresAny " + strings.Join(g.perms, ", ")
}

// Public opens a route, a live page or a controller's route to everyone,
// signed in or not: a login page, a landing page, a health check. On a
// route of a guarded controller it opens that route only.
//
//	@web.Get("/login")
//	@auth.Public
//	func Login(r *http.Request) ui.Node { … }
//
// From Go: app.Get("/login", h, web.Attrs(auth.Public())).
func Public() vuka.Bundle { return vuka.Compose(Gate{kind: public}) }

// Required needs a sign-in: what every route needs already unless
// Config.PublicByDefault, written to say so. On a live event method of a
// public page, the event is refused to anonymous visitors.
func Required() vuka.Bundle { return Gate{kind: required}.bundle() }

// Requires needs a sign-in with every one of perms (wildcards in the
// identity's perms count: "orders.*" grants "orders.write"). A visitor
// signed in without them gets 403. On a live event method it refuses the
// event; on a controller or a live page's type, every route or event of it.
func Requires(perms ...string) vuka.Bundle {
	if len(perms) == 0 {
		return Required()
	}
	return Gate{kind: requiresAll, perms: perms}.bundle()
}

// RequiresAny needs a sign-in with at least one of perms.
func RequiresAny(perms ...string) vuka.Bundle {
	if len(perms) == 0 {
		return Required()
	}
	return Gate{kind: requiresAny, perms: perms}.bundle()
}

// bundle is the gate as an attribute (routes and pages read it) and a call
// decorator (live events: it runs when the method handles one).
func (g Gate) bundle() vuka.Bundle { return vuka.Compose(g, vuka.Decorator(g.call)) }

// allow is nil when id may pass g, else web.Unauthorized or web.Forbidden.
func (g Gate) allow(id *Identity) error {
	if g.kind == public {
		return nil
	}
	if id == nil {
		return web.Unauthorized
	}
	switch g.kind {
	case requiresAll:
		for _, p := range g.perms {
			if !id.Can(p) {
				return web.Forbidden
			}
		}
	case requiresAny:
		for _, p := range g.perms {
			if id.Can(p) {
				return nil
			}
		}
		return web.Forbidden
	}
	return nil
}

// gates is what applies to a route: the gates after the last Public among
// its attributes (a controller's first, then its own), every one of them;
// none, when it is public.
func gatesOf(attrs []any) (gs []Gate, marked bool) {
	for _, x := range attrs {
		g, ok := x.(Gate)
		if !ok {
			continue
		}
		marked = true
		if g.kind == public {
			gs = gs[:0]
			continue
		}
		gs = append(gs, g)
	}
	return gs, marked
}

func describe(gs []Gate) string {
	if len(gs) == 0 {
		return "Public"
	}
	parts := make([]string, len(gs))
	for i, g := range gs {
		parts[i] = g.String()
	}
	return strings.Join(parts, " and ")
}

// wrapRoute is the gate of a route, from its attributes.
func (a *Auth) wrapRoute(info web.RouteInfo) web.Middleware {
	gs, marked := gatesOf(info.Attrs)
	if !marked && !a.cfg.PublicByDefault {
		gs = []Gate{{kind: required}}
	}
	if (info.Method == http.MethodGet || info.Live) && info.Path == a.cfg.LoginPath && len(gs) > 0 {
		a.problems = append(a.problems, fmt.Errorf("auth: GET %s (%s) is the login page, and needs a sign-in (%s): mark it @auth.Public", info.Path, info.Where, describe(gs)))
	}
	what := describe(gs)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var id *Identity
			if st := stateOf(r.Context()); st != nil {
				id = st.id
			}
			var err error
			for _, g := range gs {
				if err = g.allow(id); err != nil {
					break
				}
			}
			ctx := r.Context()
			if err == nil {
				web.NoteRequest(ctx, web.RequestNote{Section: "Auth", Name: "Gate", Value: what + ": allowed"})
				next.ServeHTTP(w, r)
				return
			}
			web.NoteRequest(ctx, web.RequestNote{Section: "Auth", Name: "Gate", Value: what + ": refused, " + err.Error(), Refused: true})
			html := info.HTML || web.AcceptsHTML(r)
			if errors.Is(err, web.Unauthorized) {
				if html && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
					http.Redirect(w, r, a.loginURL(r.URL.RequestURI()), http.StatusFound)
					return
				}
				if html {
					http.Redirect(w, r, a.loginURL(""), http.StatusSeeOther)
					return
				}
				w.Header().Set("WWW-Authenticate", "Bearer")
			}
			a.app.WriteError(w, r, err, html)
		})
	}
}

// loginURL is the login page, asked to come back to next.
func (a *Auth) loginURL(next string) string {
	if next == "" || !SafeNext(next) {
		return a.cfg.LoginPath
	}
	sep := "?"
	if strings.Contains(a.cfg.LoginPath, "?") {
		sep = "&"
	}
	return a.cfg.LoginPath + sep + "next=" + url.QueryEscape(next)
}

// SafeNext reports whether next may be where a sign-in sends the browser:
// a path of this site ("/orders?page=2"), never another site — no scheme or
// host, no "//" or "/\" a browser reads as one, no backslash or control
// character anywhere, before or after decoding.
func SafeNext(next string) bool {
	if next == "" || len(next) > 2048 || next[0] != '/' {
		return false
	}
	for i := 0; i < len(next); i++ {
		if c := next[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return false
		}
	}
	if len(next) > 1 && next[1] == '/' {
		return false
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return false
	}
	if strings.HasPrefix(u.Path, "//") || strings.ContainsAny(u.Path, "\\\x00") {
		return false
	}
	for i := 0; i < len(u.Path); i++ {
		if c := u.Path[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// Next is where a sign-in from r goes: its next parameter (a form field, else
// the query's) when SafeNext, else home.
func Next(r *http.Request, home string) string {
	next := r.FormValue("next")
	if SafeNext(next) {
		return next
	}
	return home
}

// errNoAuthEvent refuses a gated live event of an app without auth.
var errNoAuthEvent = errors.New("auth: this event is gated, and the app has no auth.Module")

// call is a gate's call decorator: when the method is called while a live
// event's handler runs — as the handler, or from it, a function literal
// handler (onClick={() => b.Clear()}) included — it refuses the call unless
// the page's identity passes the gate. Otherwise it is not the gate's
// business: routes and pages are gated by their middleware, and a call from
// Go outside an event, or the render after one (Render, Mount, a helper), is
// the app's own. On a type, it advises every method, and gates those an
// event's handler calls.
func (g Gate) call(c *vuka.Call) {
	ev := web.CurrentLiveEvent()
	if ev == nil || !ev.Handling() {
		c.Next()
		return
	}
	st := stateOf(ev.Context())
	var err error
	if st == nil {
		err = errNoAuthEvent
	} else {
		err = g.allow(st.id)
	}
	if err == nil {
		c.Next()
		return
	}
	web.NoteRequest(ev.Context(), web.RequestNote{Section: "Auth", Name: "Event gate", Value: g.String() + " on " + c.Name + ": refused, " + err.Error(), Refused: true})
	ev.Refuse(err)
	refuse(c, err)
}

// refuse ends a call without running the function: with err when it
// returns an error, else with no results.
func refuse(c *vuka.Call, err error) {
	defer func() {
		if recover() != nil {
			c.Return()
		}
	}()
	c.SetErr(err)
}
