package livetree

import (
	"context"

	"github.com/vuka-lang/vuka/live"
	"github.com/vuka-lang/web/internal/liveproto"
)

// Install makes the web framework answer v2 joins with this stand-in.
func Install() { liveproto.V2 = New }

// New is a v2 session rendering what render returns.
func New(ctx context.Context, render liveproto.Render) liveproto.Session {
	return &session{s: live.NewSession(ctx, render)}
}

type session struct {
	s   *live.Session
	enc Encoder
}

// Message is a v2 server message.
type Message struct {
	Type  string  `json:"type"`
	Ref   int     `json:"ref,omitempty"`
	V     int     `json:"v,omitempty"`
	Trees []Entry `json:"trees,omitempty"`
	Error string  `json:"error,omitempty"`
}

func (x *session) Join(ref int) any {
	html, err := x.s.Render()
	if err != nil {
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: err.Error()}
	}
	return Message{Type: live.Render, Ref: ref, V: 2, Trees: x.enc.Render(html)}
}

func (x *session) Event(m live.ClientMessage) any {
	if m.Type != live.EventMsg {
		return live.ServerMessage{Type: live.Error, Ref: m.Ref, Error: "live: unknown message type " + m.Type}
	}
	p, err := x.s.Event(m.Event)
	if err != nil {
		return live.ServerMessage{Type: live.Error, Ref: m.Ref, Error: err.Error()}
	}
	return x.patch(m.Ref, p)
}

func (x *session) Info(topic string, msg any) any {
	p, err := x.s.Info(topic, msg)
	switch {
	case err != nil:
		return live.ServerMessage{Type: live.Error, Error: err.Error()}
	case len(p.Updates) > 0 || p.Error != "":
		return x.patch(0, p)
	}
	return nil
}

func (x *session) patch(ref int, p live.Patch) Message {
	ups := make([]Update, len(p.Updates))
	for i, u := range p.Updates {
		ups[i] = Update{u.ID, u.HTML}
	}
	return Message{Type: live.PatchMsg, Ref: ref, Trees: x.enc.Patch(ups), Error: p.Error}
}

func (x *session) Topics() []string { return x.s.Topics() }
func (x *session) Close()           { x.s.Close() }
