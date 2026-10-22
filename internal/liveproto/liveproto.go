// Package liveproto is the seam between the web framework's live transport
// and the Vuka live session it drives: a Session speaks the protocol version
// its page joined with (v1: HTML, v2: render trees) and answers each client
// message with the message to send back.
package liveproto

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
)

// Session is one connected page's live session.
type Session interface {
	Join(ref int) any               // the page's render, or an error message
	Event(m live.ClientMessage) any // the reply to an event
	Info(topic string, msg any) any // what a broadcast changed; nil when nothing
	Topics() []string               // the topics its instances subscribe to
	Close()
	// Navigate makes rt's page the session's (live.Session.Navigate): the
	// reply to the navigate message ref, carrying url, and whether it is a
	// patch (not an error).
	Navigate(ref int, url string, rt live.Route, patch bool) (any, bool)
}

// Render is a page's render function.
type Render = func(context.Context) ui.Node

// V2 replaces the Vuka session for v2 joins when set (a test's stand-in).
var V2 func(ctx context.Context, render Render) Session

// New is the session of a page that joined asking for protocol version v.
// The Vuka session answers in the newest version both speak: a session that
// predates v2 ignores the request and renders in v1, which the page follows.
func New(ctx context.Context, render Render, v int) Session {
	if v >= 2 && V2 != nil {
		return V2(ctx, render)
	}
	return native{live.NewSession(ctx, render), v}
}

// Adopt is the session of a page whose HTTP render's live session was kept
// for its connection (live.Session.Adopt): its instances, mounted by the
// render, serve the join without mounting again, speaking protocol version
// v. ctx and render replace the finished render request's.
func Adopt(s *live.Session, ctx context.Context, render Render, v int) (Session, error) {
	if err := s.Adopt(ctx, render); err != nil {
		return nil, err
	}
	return native{s, v}, nil
}

type native struct {
	s *live.Session
	v int
}

// Join goes through the session's JSON so that the version travels with the
// join whether or not this Vuka's ClientMessage has a field for it.
func (x native) Join(ref int) any {
	b, _ := json.Marshal(map[string]any{"type": live.Join, "ref": ref, "v": max(x.v, 1)})
	var m live.ServerMessage
	if err := json.Unmarshal(x.s.HandleJSON(b), &m); err != nil {
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: "live: " + err.Error()}
	}
	return m
}

// Native reports whether the Vuka live session speaks protocol v2.
func Native() bool {
	x := native{live.NewSession(context.Background(), func(context.Context) ui.Node { return ui.Text("") }), 2}
	defer x.Close()
	b, _ := json.Marshal(x.Join(1))
	return bytes.Contains(b, []byte(`"trees"`))
}

func (x native) Event(m live.ClientMessage) any { return x.s.Handle(m) }

// Handler is the function an event's target names, when this Vuka's
// session says (live.Session.Handler); nil otherwise.
func (x native) Handler(target string) any {
	if h, ok := any(x.s).(interface{ Handler(string) any }); ok {
		return h.Handler(target)
	}
	return nil
}

// Component is the component an event's target belongs to, when this
// Vuka's session says (live.Session.Component); nil otherwise.
func (x native) Component(target string) ui.Stateful {
	if c, ok := any(x.s).(interface{ Component(string) ui.Stateful }); ok {
		return c.Component(target)
	}
	return nil
}

// SetErrorText sets the text the browser gets for an error
// (live.Session.SetErrorText).
func (x native) SetErrorText(text func(error) string) { x.s.SetErrorText(text) }

// Deferred starts the deferred loads of the session's pages backed by their
// islands (live.Session.Deferred).
func (x native) Deferred() []*ui.DeferredLoad { return x.s.Deferred() }

// Loaded is what a deferred load changed (live.Session.Loaded); nil when
// nothing.
func (x native) Loaded(d *ui.DeferredLoad) any {
	p, err := x.s.Loaded(d)
	switch {
	case err != nil:
		return live.ServerMessage{Type: live.Error, Error: x.s.ErrorText(err)}
	case !empty(p):
		return live.PatchMessage(0, p)
	}
	return nil
}

func (x native) Topics() []string { return x.s.Topics() }
func (x native) Close()           { x.s.Close() }

func (x native) Navigate(ref int, url string, rt live.Route, patch bool) (any, bool) {
	p, err := x.s.Navigate(rt, patch)
	if err != nil {
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: x.s.ErrorText(err)}, false
	}
	m := live.PatchMessage(ref, p)
	m.URL = url
	return m, true
}

func (x native) Info(topic string, msg any) any {
	p, err := x.s.Info(topic, msg)
	switch {
	case err != nil:
		return live.ServerMessage{Type: live.Error, Error: x.s.ErrorText(err)}
	case !empty(p):
		return live.PatchMessage(0, p)
	}
	return nil
}

// empty reports whether a patch carries nothing: no update, no tree, no
// error (whatever fields this Vuka's Patch has).
func empty(p live.Patch) bool {
	b, _ := json.Marshal(p)
	return string(b) == "{}"
}
