package web

import (
	"context"
	"io"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

var adminLoads atomic.Int64

// adminShell is an admin area's document: a menu of links in collapsible
// groups, and a header with the page's title and breadcrumbs. It reads
// nothing of the request, so it stays across navigations.
type adminShell struct {
	Children ui.Node
}

func (l *adminShell) Load(ctx context.Context) error {
	adminLoads.Add(1)
	return nil
}

func (l *adminShell) Render(ctx context.Context, w io.Writer) error {
	link := func(id, href string) ui.Node {
		return ui.Link(ui.LinkProps{Href: href, ID: id, Active: ui.ActivePrefix, Children: ui.Text(id)})
	}
	group := func(id, label string, links ...ui.Node) ui.Node {
		return ui.El("details", attrs("id", id, "data-vk-active-within", ""),
			append([]ui.Node{ui.El("summary", nil, ui.Text(label))}, links...)...)
	}
	var crumbs []ui.Node
	for _, c := range ui.PageCrumbs(ctx) {
		if c.Href == "" {
			crumbs = append(crumbs, ui.El("li", attrs("aria-current", "page"), ui.Text(c.Label)))
		} else {
			crumbs = append(crumbs, ui.El("li", nil, ui.El("a", attrs("href", c.Href), ui.Text(c.Label))))
		}
	}
	return ui.El("html", nil,
		ui.El("head", nil, ui.El("title", nil, ui.Text(ui.PageTitle(ctx)))),
		ui.El("body", nil,
			ui.El("nav", attrs("id", "menu"),
				group("orders", "Orders", link("to-open", "/orders/open"), link("to-done", "/orders/done")),
				group("users", "Users", link("to-users", "/users/all"), link("to-invite", "/users/invite"))),
			ui.El("header", nil,
				ui.El("h1", attrs("id", "title"), ui.Text(ui.PageTitle(ctx))),
				ui.El("ol", attrs("id", "crumbs"), crumbs...)),
			ui.El("main", nil, l.Children))).Render(ctx, w)
}

// adminPage is a live page titled and crumbed by its section and name.
type adminPage struct {
	ui.Live
	section, name string
}

func (p *adminPage) Title() string { return p.name }
func (p *adminPage) Crumbs() []ui.Crumb {
	return []ui.Crumb{{Label: "Admin", Href: "/"}, {Label: p.section, Href: "/" + p.section}, {Label: p.name}}
}
func (p *adminPage) Render() ui.Node { return ui.El("div", attrs("id", "content"), ui.Text(p.name)) }

// invitePage is a stateless page under the same shell.
type invitePage struct{}

func (p *invitePage) Title() string { return "Invite" }
func (p *invitePage) Crumbs() []ui.Crumb {
	return []ui.Crumb{{Label: "Admin", Href: "/"}, {Label: "users", Href: "/users"}, {Label: "Invite"}}
}
func (p *invitePage) Render(ctx context.Context, w io.Writer) error {
	return ui.El("div", attrs("id", "content"), ui.Text("Invite")).Render(ctx, w)
}

// TestBrowserLayoutHeader: under a layout that stays, the header it
// renders from the page's title and breadcrumbs follows each page, its Load
// run once; a menu group holding the current link opens, on the first
// load and as navigation moves into it, and a group the visitor opens or
// closes stays so.
func TestBrowserLayoutHeader(t *testing.T) {
	adminLoads.Store(0)
	app := New(Only("none"))
	shell := LayoutOf[adminShell]()
	page := func(section, name string) func() *adminPage {
		return func() *adminPage { return &adminPage{section: section, name: name} }
	}
	app.Live("/orders/open", page("orders", "Open orders"), shell)
	app.Live("/orders/done", page("orders", "Done orders"), shell)
	app.Live("/users/all", page("users", "All users"), shell)
	app.Get("/users/invite", func() *invitePage { return &invitePage{} }, shell)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/orders/open")
	follow := func(sel string) { p.Eval(`document.querySelector(`+strconv.Quote(sel)+`).click()`, nil) }
	crumbs := func(want string) {
		p.Wait(`Array.from(document.querySelectorAll("#crumbs li")).map((li) => li.textContent).join(" / ") === ` + strconv.Quote(want))
	}
	open := func(id string) string { return `document.querySelector("#` + id + `").open` }

	p.WaitText("#title", "Open orders")
	crumbs("Admin / orders / Open orders")
	p.Wait(open("orders") + ` && !` + open("users"))
	p.Wait(`document.querySelector("#orders").getAttribute("data-vk-active-within") === "true" && document.querySelector("#users").getAttribute("data-vk-active-within") === ""`)
	loads := adminLoads.Load()

	// Within the group: the header follows, the group stays open, and so
	// does the group the visitor opened.
	p.Click("#users summary")
	p.Wait(open("users"))
	follow("#to-done")
	p.Wait(`location.pathname === "/orders/done" && document.title === "Done orders"`)
	p.WaitText("#title", "Done orders")
	crumbs("Admin / orders / Done orders")
	p.Wait(open("orders") + ` && ` + open("users"))

	// Into the users group: the orders group the visitor left stays open.
	follow("#to-users")
	p.Wait(`location.pathname === "/users/all"`)
	p.WaitText("#title", "All users")
	crumbs("Admin / users / All users")
	p.Wait(open("users") + ` && ` + open("orders"))
	p.Wait(`document.querySelector("#orders").getAttribute("data-vk-active-within") === "" && document.querySelector("#users").getAttribute("data-vk-active-within") === "true"`)

	// The visitor closes the orders group: a navigation, its layout
	// rendering again, keeps it closed. A stateless page's title and crumbs
	// reach the header too.
	p.Click("#orders summary")
	p.Wait(`!` + open("orders"))
	follow("#to-invite")
	p.Wait(`location.pathname === "/users/invite"`)
	p.WaitText("#title", "Invite")
	crumbs("Admin / users / Invite")
	p.Wait(`!` + open("orders") + ` && ` + open("users"))

	// Back into the orders group: it opens again.
	p.Eval(`vuka.navigate("/orders/open")`, nil)
	p.Wait(`location.pathname === "/orders/open"`)
	p.WaitText("#title", "Open orders")
	p.Wait(open("orders"))

	if n := adminLoads.Load(); n != loads {
		t.Errorf("the shell loaded again: %d loads, want %d", n, loads)
	}
}
