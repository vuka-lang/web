package web

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
)

// An event assigning a @ui.URL field of the page writes the page's URL: its
// reply is already the render with the new state, and carries a patch
// command that replaces the history entry with the new address. The browser
// runs that command as a navigation to the address, which would run the
// route, mount the page's components again and start their loads again,
// for a change the page made itself.
//
// noteSelfURL keeps the address of such a command, and the navigate that
// arrives for it is answered by selfNavigate: the address, with nothing to
// render again.

// selfHref is the address of the one replacing patch command in an event's
// reply, a change of the page's own query, or "" when the reply has another
// command that moves the page, the event edited the URL by other means
// (web.Query) or its path changes.
func (c *conn) selfHref(ev *LiveEvent, pushes []live.JSPush) string {
	if ev == nil || ev.foreignURL.Load() {
		return ""
	}
	href := ""
	for _, p := range pushes {
		var ops [][]json.RawMessage
		if json.Unmarshal(p.Ops, &ops) != nil {
			return ""
		}
		for _, op := range ops {
			var name string
			if len(op) != 2 || json.Unmarshal(op[0], &name) != nil {
				return ""
			}
			if name != "patch" && name != "navigate" && name != "reload" {
				continue
			}
			var args struct {
				Href    string `json:"href"`
				Replace bool   `json:"replace"`
			}
			if name != "patch" || href != "" || json.Unmarshal(op[1], &args) != nil || !args.Replace {
				return ""
			}
			href = args.Href
		}
	}
	cur, _, _ := strings.Cut(c.uri, "?")
	next, _, _ := strings.Cut(href, "?")
	if href == "" || strings.Contains(href, "#") || cur != next {
		return ""
	}
	return href
}

// noteSelfURL records the address an event's reply moves the page to by a
// change of its own URL state, for the navigate that follows it.
func (c *conn) noteSelfURL(ev *LiveEvent, reply any) {
	href := ""
	if sm, ok := reply.(live.ServerMessage); ok {
		href = c.selfHref(ev, sm.JS)
	}
	if href == "" {
		c.selfURL.Store(nil)
		return
	}
	c.selfURL.Store(&href)
}

// selfNavigates reports whether a navigate message to rawURL is the one
// selfNavigate answers.
func (c *conn) selfNavigates(rawURL string) bool {
	uri, _, _ := strings.Cut(rawURL, "#")
	want := c.selfURL.Load()
	return want != nil && *want == uri
}

// selfNavigate answers a patch navigate to the address the page's own event
// just moved it to: the page has rendered it already, with its components
// mounted and their loads running or done, so it carries only the page's
// token and title for the address. It reports false for any other
// navigation, which runs as ever.
func (c *conn) selfNavigate(m live.ClientMessage, uri string) (any, bool) {
	if !m.Patch || !c.selfNavigates(uri) {
		return nil, false
	}
	c.selfURL.Store(nil)
	if u, err := url.ParseRequestURI(uri); err == nil && c.page != nil {
		page := c.page
		c.page = ui.WithPageURL(page, u, func(name string) string { return ui.PageParam(page, name) })
		c.shared.Store(&c.page)
	}
	c.uri = uri
	reply := live.ServerMessage{Type: live.PatchMsg, Ref: m.Ref, URL: m.URL}
	return withToken{reply, c.app.signToken(uri), c.docTitle(ui.PageTitle(c.page))}, true
}
