package web

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/vuka-lang/ui/v2"
)

// Parts of a page that isn't live (ui.Part, @ui.Defer on a method): a GET
// page renders each deferred part's fallback, each deferred method starting
// as the render meets it (ui.DefaultPartLimit at once), and is sent — its
// status and headers decided, its guards, layouts' Load and every method
// that isn't deferred run — up to its </body>, flushed through the
// compression: a deferred method's error never changes the status. Then
// each part is appended as it arrives, a <template data-vk-part> and the script moving
// it into place, flushed; </body></html> ends the response. A navigation
// over HTTP gets the same stream: the runtime patches the page in as soon
// as its start arrives (the parts script marks it) and each part as it
// follows.
//
// A part whose method fails shows its error fallback, else a short
// message (role="alert"); it is logged, and the toolbar shows it. The
// client going away cancels the methods still running. X-Accel-Buffering:
// no asks nginx not to buffer the response; a proxy that buffers anyway
// delivers it whole, still correct.

// In a test binary a page's parts render in place, its HTML whole: what a
// test reading it wants (viewtest). WithPendingParts leaves them pending
// instead.
var streamInTests bool // this package's tests stream, as an app does

type pendingKey struct{}

// WithPendingParts is ctx with the deferred parts of the page it renders
// left pending: the page shows their fallbacks and their methods never run.
// viewtest.Pending sets it, to test a page's first paint.
func WithPendingParts(ctx context.Context) context.Context {
	return context.WithValue(ctx, pendingKey{}, true)
}

func pendingParts(ctx context.Context) bool {
	v, _ := ctx.Value(pendingKey{}).(bool)
	return v
}

// streamPage writes a page whose parts stream.
func (a *App) streamPage(rq *request, html []byte, head, script string, parts *ui.Parts) {
	w := rq.w
	w.Header().Set("X-Accel-Buffering", "no")
	doc := pageBufs.Get().(*bytes.Buffer)
	defer putPageBuf(doc)
	writePage(doc, html, head, script)
	out := doc.Bytes()
	cut := indexTag(out, "</body>", true)
	if cut < 0 {
		cut = len(out)
	}
	rc := http.NewResponseController(w)
	flush := func() error {
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		return nil
	}
	if _, err := w.Write(out[:cut]); err != nil {
		return
	}
	// Stream flushes it with the parts script, then each part.
	err := parts.Stream(w, flush)
	if err != nil {
		return // the client went away
	}
	w.Write(out[cut:])
}

// partsOptions are how the deferred methods of r's page run: their
// spans in the toolbar, their errors' text and log lines.
func (a *App) partsOptions(r *http.Request, pending bool) ui.PartsOptions {
	return ui.PartsOptions{
		Pending:   pending,
		Observe:   a.observePart,
		ErrorText: a.partErrorText,
		Written: func(name string, err error) {
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("web: %s %s: part %s: %v", r.Method, r.URL.Path, name, err)
			}
		},
	}
}

// partErrorText is the message a failing part without an error fallback
// shows, as an error page would: a public message (*Error's), the error's
// text in development, else the default.
func (a *App) partErrorText(err error) string {
	var pub interface{ PublicMessage() string }
	var to Redirect
	switch {
	case errors.As(err, &to):
		return ""
	case errors.As(err, &pub):
		return pub.PublicMessage()
	case a.dev:
		return err.Error()
	}
	return ""
}

// observePart opens a deferred method's span on the toolbar, its
// statements marked as the part's.
func (a *App) observePart(ctx context.Context, name string) (context.Context, func(error)) {
	if a.tb == nil {
		return ctx, func(error) {}
	}
	e := entryOf(ctx)
	if e == nil {
		return ctx, func(error) {}
	}
	field := "part " + name
	return StatementOrigin(ctx, "", field), e.partSpan(name, field)
}
