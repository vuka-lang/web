module github.com/vuka-lang/web

go 1.25.0

require (
	github.com/a-h/templ v0.3.1020 // indirect
	github.com/coder/websocket v1.8.15
	github.com/vuka-lang/vuka v0.10.0
)

require github.com/vuka-lang/ui v0.2.0

// Until ui's optimistic pushes and navigation are released: ui's navigation
// branch, in the worktree beside this one.
replace github.com/vuka-lang/ui => ../vuka-ui-wt-nav
