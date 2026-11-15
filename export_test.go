package web

// CaptureSlog is captureSlog for the package's external tests.
var CaptureSlog = captureSlog

// StatsApp is statsApp for the package's external tests: its rows load
// once release is called.
func StatsApp() (app *App, release func()) {
	ctl := &deferCtl{release: make(chan struct{}), ended: make(chan error, 1)}
	return statsApp(ctl), func() { close(ctl.release) }
}

// NavHTTPApp is navHTTPApp for the package's external tests.
var NavHTTPApp = navHTTPApp
