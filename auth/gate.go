package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"

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

// strictGates is Config.StrictGates of an app built with it: a gated call
// outside any request has no app to ask, so it holds for the process.
var strictGates atomic.Bool

// errNoIdentity refuses, under StrictGates, a gated call nothing says
// whose it is.
var errNoIdentity = fmt.Errorf("auth: a gated call with no identity (Config.StrictGates): pass it a request's context, or one from auth.WithIdentity or auth.System: %w", web.Unauthorized)

type systemKey struct{}

// System is ctx marked as the app's own work — a cron, a job, a migration,
// main — which gated methods given it let through, whoever ctx's identity
// is. Under Config.StrictGates a gated call is refused without an identity
// or this mark; without it, a call outside any request runs anyway, and
// the mark only says so.
func System(ctx context.Context) context.Context { return context.WithValue(ctx, systemKey{}, true) }

func isSystem(ctx context.Context) bool { v, _ := ctx.Value(systemKey{}).(bool); return v }

// guardEvent decides a live event before its handler runs (web's
// GuardLiveEvents): the gates of the component's type, of the handler's
// receiver type, then of the handler — an @auth.Public on the handler
// opening it — checked against the identity the page joined with.
func (a *Auth) guardEvent(ctx context.Context, h web.LiveHandler) error {
	attrs := append([]any(nil), vuka.TypeAttrs(h.Component)...)
	if h.Recv != nil && h.Recv != h.Component {
		attrs = append(attrs, vuka.TypeAttrs(h.Recv)...)
	}
	attrs = append(attrs, h.Attrs...)
	gs, _ := gatesOf(attrs)
	if len(gs) == 0 {
		return nil
	}
	st := stateOf(ctx)
	if st == nil {
		return errNoAuthEvent
	}
	for _, g := range gs {
		if err := g.allow(st.id); err != nil {
			web.NoteRequest(ctx, web.RequestNote{Section: "Auth", Name: "Event gate", Value: describe(gs) + " on " + h.Name + ": refused, " + err.Error(), Refused: true})
			return err
		}
	}
	return nil
}

// call is a gate's call decorator. The identity it checks is, in order:
//
//   - the one the call's context.Context carries — a request's, a live
//     page's (what an event's handler is given), auth.WithIdentity's — on
//     whatever goroutine; auth.System's lets it through;
//   - on a live connection's goroutine, the page's while an event's handler
//     runs (the handler itself, or anything it calls synchronously, a
//     function literal's calls included) — not the render after it, which
//     isn't an action;
//   - on a goroutine a live connection's goroutine started (go b.Clear()
//     in a handler, a decorator moving the call), that page's.
//
// A call none of these attributes runs — the app's own code: a cron, a job,
// main — unless Config.StrictGates, which refuses it. A gate on a type
// advises its methods but leaves alone those a live component's session
// calls itself (Render, Mount, Update) and those marked @auth.Public.
func (g Gate) call(c *vuka.Call) {
	own := c.Attrs()
	if !hasGate(own, g) {
		if isPublic(own) || c.Receiver != nil && lifecycle(c.Name) {
			c.Next()
			return
		}
	}
	err, where := g.decide(c.Context())
	if err == nil {
		c.Next()
		return
	}
	ev := web.CurrentLiveEvent()
	if where == nil && ev != nil {
		where = ev.Context()
	}
	if where != nil {
		web.NoteRequest(where, web.RequestNote{Section: "Auth", Name: "Gate", Value: g.String() + " on " + c.Name + ": refused, " + err.Error(), Refused: true})
	}
	if ev != nil && ev.Handling() {
		ev.Refuse(err)
	}
	refuse(c, err)
}

// decide is nil when the caller ctx (or the live page the call comes from)
// names may pass g; where is the context the identity came from.
func (g Gate) decide(ctx context.Context) (err error, where context.Context) {
	if isSystem(ctx) {
		return nil, ctx
	}
	if st := stateOf(ctx); st != nil {
		return g.allow(st.id), ctx
	}
	if ev := web.CurrentLiveEvent(); ev != nil {
		if ev.Handling() {
			st := stateOf(ev.Context())
			if st == nil {
				return errNoAuthEvent, ev.Context()
			}
			return g.allow(st.id), ev.Context()
		}
	} else if o := web.LiveOrigin(); o != nil {
		if st := stateOf(o); st != nil {
			return g.allow(st.id), o
		}
		return errNoAuthEvent, o
	}
	if strictGates.Load() {
		return errNoIdentity, nil
	}
	return nil, nil
}

// hasGate reports whether attrs hold g: the declaration's own gate, not its
// type's.
func hasGate(attrs []any, g Gate) bool {
	for _, x := range attrs {
		if o, ok := x.(Gate); ok && o.kind == g.kind && slices.Equal(o.perms, g.perms) {
			return true
		}
	}
	return false
}

func isPublic(attrs []any) bool {
	for _, x := range attrs {
		if o, ok := x.(Gate); ok && o.kind == public {
			return true
		}
	}
	return false
}

// lifecycle reports whether a method ("Board.Render") is one a live
// component's session calls itself, never an event.
func lifecycle(name string) bool {
	m := name[strings.LastIndexByte(name, '.')+1:]
	return m == "Render" || m == "Mount" || m == "Update" || strings.HasPrefix(m, "Update__")
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
