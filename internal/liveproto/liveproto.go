// Package liveproto is the seam between the web framework's live transport
// and the Vuka live session it drives: a Session speaks one protocol version
// (v1: HTML, v2: render trees) and answers each client message with the
// message to send back.
package liveproto

import (
	"context"

	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/vuka/live"
)

// Session is one connected page's live session.
type Session interface {
	Join(ref int) any               // the page's render, or an error message
	Event(m live.ClientMessage) any // the reply to an event
	Info(topic string, msg any) any // what a broadcast changed; nil when nothing
	Topics() []string               // the topics its instances subscribe to
	Close()
}

// Render is a page's render function.
type Render = func(context.Context) vuka.Node

// V2 starts a protocol v2 session; nil while no v2 implementation is linked,
// and the transport answers v2 joins in v1.
var V2 func(ctx context.Context, render Render) Session

// V1 is a protocol v1 session over s.
func V1(s *live.Session) Session { return v1{s} }

type v1 struct{ s *live.Session }

func (x v1) Join(ref int) any               { return x.s.Handle(live.ClientMessage{Type: live.Join, Ref: ref}) }
func (x v1) Event(m live.ClientMessage) any { return x.s.Handle(m) }
func (x v1) Topics() []string               { return x.s.Topics() }
func (x v1) Close()                         { x.s.Close() }

func (x v1) Info(topic string, msg any) any {
	p, err := x.s.Info(topic, msg)
	switch {
	case err != nil:
		return live.ServerMessage{Type: live.Error, Error: err.Error()}
	case len(p.Updates) > 0 || p.Error != "":
		return live.PatchMessage(0, p)
	}
	return nil
}
