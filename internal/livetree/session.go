package livetree

import (
	"context"

	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/web/v2/internal/liveproto"
)

// Install makes the web framework answer v2 joins with this stand-in, unless
// the Vuka live session speaks v2 itself.
func Install() {
	if !liveproto.Native() {
		liveproto.V2 = New
	}
}

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
	Type  string        `json:"type"`
	Ref   int           `json:"ref,omitempty"`
	V     int           `json:"v,omitempty"`
	Trees []Entry       `json:"trees,omitempty"`
	JS    []live.JSPush `json:"js,omitempty"`
	Error string        `json:"error,omitempty"`
	URL   string        `json:"url,omitempty"`
}

func (x *session) Join(ref int) any {
	html, err := x.s.Render()
	if err != nil {
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: x.s.ErrorText(err)}
	}
	return Message{Type: live.Render, Ref: ref, V: 2, Trees: x.enc.Render(html), JS: x.s.JS()}
}

func (x *session) Event(m live.ClientMessage) any {
	if m.Type != live.EventMsg {
		return live.ServerMessage{Type: live.Error, Ref: m.Ref, Error: "live: unknown message type " + m.Type}
	}
	p, err := x.s.Event(m.Event)
	if err != nil {
		return live.ServerMessage{Type: live.Error, Ref: m.Ref, Error: x.s.ErrorText(err)}
	}
	return x.patch(m.Ref, p)
}

func (x *session) Navigate(ref int, url string, rt live.Route, patch bool) (any, bool) {
	p, err := x.s.Navigate(rt, patch)
	if err != nil {
		return live.ServerMessage{Type: live.Error, Ref: ref, Error: x.s.ErrorText(err)}, false
	}
	m := x.patch(ref, p)
	m.URL = url
	return m, true
}

func (x *session) Info(topic string, msg any) any {
	p, err := x.s.Info(topic, msg)
	switch {
	case err != nil:
		return live.ServerMessage{Type: live.Error, Error: x.s.ErrorText(err)}
	case len(p.Updates) > 0 || len(p.JS) > 0 || p.Error != "":
		return x.patch(0, p)
	}
	return nil
}

func (x *session) patch(ref int, p live.Patch) Message {
	ups := make([]Update, len(p.Updates))
	for i, u := range p.Updates {
		ups[i] = Update{u.ID, u.HTML}
	}
	return Message{Type: live.PatchMsg, Ref: ref, Trees: x.enc.Patch(ups), JS: p.JS, Error: p.Error}
}

// SetErrorText sets the text the browser gets for an error
// (live.Session.SetErrorText).
func (x *session) SetErrorText(text func(error) string) { x.s.SetErrorText(text) }

func (x *session) Topics() []string { return x.s.Topics() }
func (x *session) Close()           { x.s.Close() }
