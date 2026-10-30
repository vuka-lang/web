package web

import (
	"net/http"
	"strings"
)

// CSRF refuses cross-origin requests that change state: a POST, PUT, PATCH
// or DELETE a browser sends from another site's page (a form posted to the
// app, a fetch) answers 403, so a signed-in user's cookie can't be used by
// a page they happen to visit. Browsers say where a request comes from
// (Sec-Fetch-Site, else Origin), and net/http's CrossOriginProtection reads
// that: no token to put in every form. GET, HEAD and OPTIONS are never
// refused, so keep them free of side effects.
//
// trusted are other origins allowed to send such requests
// ("https://admin.example.com"). A request carrying an Authorization:
// Bearer header is let through: a browser never adds one by itself, so it
// can't be forged by another site. Requests that say nothing of where they
// come from (curl, servers) are let through too, and so are those of a
// TokenOnly route, whose cookies are taken off, and those from an origin a
// CORS rule with Credentials lists. Live connections check their origin
// themselves (LiveOrigins).
func CSRF(trusted ...string) Option {
	return OptionFunc(func(a *App) {
		a.mutable("CSRF")
		if a.csrf == nil {
			a.csrf = http.NewCrossOriginProtection()
		}
		for _, o := range trusted {
			if err := a.csrf.AddTrustedOrigin(o); err != nil {
				a.c.errs = append(a.c.errs, err)
			}
		}
	})
}

var errCrossOrigin = &Error{Status: http.StatusForbidden, Message: "cross-origin request refused"}

func (a *App) csrfProtect(next http.Handler) http.Handler {
	cop := a.csrf
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.writeError(w, r, errCrossOrigin, acceptsHTML(r))
	}))
	guarded := cop.Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) || crossOriginExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
}

func bearer(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	return len(h) > 7 && strings.EqualFold(h[:7], "bearer ")
}

// acceptsHTML reports whether r is a browser's navigation or form post,
// which takes an HTML page, rather than a script's call.
func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// AcceptsHTML reports whether r asks for an HTML page (its Accept header
// names text/html): a browser's navigation or form post, not a script's
// fetch.
func AcceptsHTML(r *http.Request) bool { return acceptsHTML(r) }
