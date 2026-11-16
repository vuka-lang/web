package web

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/vuka-lang/ui/v2"
)

// devError carries, for the development 500 log, where a request's error
// came from: the route serving it (routeSpec.where, "main.ShowPets at
// pets.vuka:12") and the deepest app-owned frame of the stack where the
// framework first saw the error — "" when the error came back as a plain
// return value, whose stack is unwound by then. writeError and callError
// peel it off before answering, so responses, OnError and production log
// lines never change.
type devError struct {
	err   error
	where string // the route's handler, as registered
	frame string // app file:line the error surfaced through; "" when unknown
}

func (e *devError) Error() string { return e.err.Error() }
func (e *devError) Unwrap() error { return e.err }

// devErr wraps err for the development 500 log, naming s as the route
// serving the request. In production it returns err as it is.
func (a *App) devErr(err error, s *routeSpec) error {
	if err == nil || !a.dev {
		return err
	}
	if d, ok := err.(*devError); ok {
		if d.where == "" && s != nil {
			d.where = s.where
		}
		return d
	}
	d := &devError{err: err, frame: appFrame(2)}
	if s != nil {
		d.where = s.where
	}
	return d
}

// devLog is the development form of the 500 log line: the production line
// with the query kept and, when known, the route's handler and the app
// frame the error surfaced through.
func (d *devError) devLog(status int, method, uri, kind string, err error) string {
	line := fmt.Sprintf("web: %d %s %s%s: %v", status, method, uri, kind, err)
	if d.where != "" {
		line += " | handler " + d.where
	}
	if d.frame != "" {
		line += " | at " + d.frame
	}
	return line
}

// catchFailure, deferred around a render, stops a panic of ui.Must as
// ui.Catch does, and — the stack being intact inside the recover — records
// the app frame the failing call sits in, which the development 500 log
// shows. Any other panic goes on.
func catchFailure(err *error) {
	p := recover()
	if p == nil {
		return
	}
	f, ok := ui.Failed(p)
	if !ok {
		panic(p)
	}
	if frame := appFrame(2); frame != "" {
		f = &devError{err: f, frame: frame}
	}
	*err = f
}

// appFrame is the deepest app-owned frame on the calling stack, as
// "file.go:12 (pkg.Func)": the first frame that is neither the Go runtime
// and standard library nor vuka's own modules. Called where an error is
// first seen it names the app code the error came through; "" when only
// framework frames remain (an error returned from a handler has its stack
// unwound already).
func appFrame(skip int) string {
	pc := make([]uintptr, 64)
	n := runtime.Callers(skip+1, pc)
	frames := runtime.CallersFrames(pc[:n])
	for {
		f, more := frames.Next()
		if strings.HasPrefix(f.Function, "net/http.") {
			// The walk reached the server's dispatch: everything beyond
			// called into the framework and isn't on the error's path.
			return ""
		}
		if f.Function != "" && isAppFrame(f.Function, f.File) {
			return fmt.Sprintf("%s:%d (%s)", shortFile(f.File), f.Line, shortName(f.Function))
		}
		if !more {
			return ""
		}
	}
}

// appModule is the main module's path, which isAppFrame takes a frame's
// package under as the app's own.
var appModule = func() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.Main.Path
	}
	return ""
}()

// isAppFrame reports whether a stack frame — function as runtime.Frame
// names it, file its source file — is the app's own code: not the Go
// runtime or standard library, and not vuka's modules. A _test.go frame is
// the app's (a test binary plays the app); package main and the main
// module's packages are the app's; with no build info, anything whose
// module path has a domain passes.
func isAppFrame(function, file string) bool {
	if strings.HasSuffix(file, "_test.go") {
		return true
	}
	if strings.HasPrefix(function, "github.com/vuka-lang/") {
		return false
	}
	if strings.HasPrefix(function, "main.") {
		return true
	}
	if appModule != "" && appModule != "command-line-arguments" {
		return strings.HasPrefix(function, appModule+".") || strings.HasPrefix(function, appModule+"/")
	}
	first, _, cut := strings.Cut(function, "/")
	if !cut {
		first, _, _ = strings.Cut(first, ".")
	}
	return strings.Contains(first, ".")
}
