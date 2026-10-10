module github.com/vuka-lang/web

go 1.25.0

require (
	github.com/a-h/templ v0.3.1020
	github.com/coder/websocket v1.8.15
	github.com/vuka-lang/vuka v0.10.0
)

require github.com/vuka-lang/ui v0.1.0

// The ui js branch (JS commands), until it is released: remove at release.
replace github.com/vuka-lang/ui => ../vuka-ui
