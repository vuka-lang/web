//go:build vuka_release

package web

import "net/http"

// A release build has no toolbar (toolbar_serve.go): no app records
// requests or serves the toolbar, VUKA_ENV=development or not, and the live
// runtime is the minified one only.

const toolbarLinked = false

const toolbarTag = ""

var liveJS = liveMinJS

func (a *App) routeToolbar() {}

func (a *App) graphiqlPage(w http.ResponseWriter, r *http.Request, path string) {}
