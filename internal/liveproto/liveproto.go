// Package liveproto is the seam between the web framework's live transport
// and the Vuka live session it drives: a Session speaks the protocol version
// its page joined with (v1: HTML, v2: render trees) and answers each client
// message with the message to send back.
package liveproto

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/ui/live"
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
func (x native) Topics() []string               { return x.s.Topics() }
func (x native) Close()                         { x.s.Close() }

func (x native) Navigate(ref int, url string, rt live.Route, patch bool) (any, bool) {
	p, err := x.s.Navigate(rt, patch)
	if err != nil {
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: err.Error()}, false
	}
	m := live.PatchMessage(ref, p)
	m.URL = url
	return m, true
}

func (x native) Info(topic string, msg any) any {
	p, err := x.s.Info(topic, msg)
	switch {
	case err != nil:
		return live.ServerMessage{Type: live.Error, Error: err.Error()}
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
