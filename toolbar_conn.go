package web

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// The toolbar's Connection panel: the life of a page's live connections —
// each socket opened, its joins (the session the page's render parked
// adopted, or the page mounted fresh), every close the server sent with its
// code, its reason and why, the socket dropping, and the connection moving
// between pages — and the state the page is in now, from those.

// tbConn is an event of a live connection on a page's record.
type tbConn struct {
	At   time.Time
	Conn int // the connection's number: one per socket opened
	// Kind is open, reconnect (a socket opened on a page whose record had
	// one before), join, rejoin (a join on a socket joined already),
	// close (the server closed it), drop (it ended without the server
	// closing it), refused (closed as it opened), leave and arrive (the
	// connection navigated to another page).
	Kind   string
	Code   int    // a close's code
	Text   string // what happened, in words
	Detail string // why: a close's cause, a drop's error, a join's protocol
}

var tbConnSeq atomic.Int64

func (p *tbEntry) addConn(ev tbConn) {
	p.mu.Lock()
	if ev.Kind == "open" && len(p.Conns) > 0 {
		ev.Kind = "reconnect"
		ev.Text = fmt.Sprintf("the browser opened a socket again (connection #%d after #%d)", ev.Conn, p.Conns[len(p.Conns)-1].Conn)
	}
	if len(p.Conns) >= toolbarConns {
		p.Conns = slices.Delete(p.Conns, 0, 1)
	}
	p.Conns = append(p.Conns, ev)
	p.mu.Unlock()
}

// connEvent notes an event of the sink's connection on the page it shows.
// The first close or drop ends the connection's record: the socket's read
// failing once the server closed it says nothing more.
func (s *tbSink) connEvent(kind string, code int, text, detail string) {
	if kind == "close" || kind == "drop" || kind == "refused" {
		if !s.ended.CompareAndSwap(false, true) {
			return
		}
	}
	if p := s.at.Load(); p != nil {
		p.addConn(tbConn{At: time.Now(), Conn: s.conn, Kind: kind, Code: code, Text: text, Detail: detail})
	}
}

// moved notes the connection leaving the page old for p, at uri.
func (s *tbSink) moved(old, p *tbEntry, uri string) {
	old.mu.Lock()
	from := old.URI
	old.mu.Unlock()
	now := time.Now()
	old.addConn(tbConn{At: now, Conn: s.conn, Kind: "leave", Text: "the connection navigated to " + uri})
	p.addConn(tbConn{At: now, Conn: s.conn, Kind: "arrive", Text: "the connection arrived from " + from})
}

// refusedConn notes on the page's record, id, a socket closed as it opened.
func (tb *toolbar) refusedConn(id string, code websocket.StatusCode, reason string) {
	if s := tb.liveSink(id, ""); s != nil {
		s.connEvent("refused", int(code), "the server closed the socket as it opened: "+reason, closeConsequence(code))
	}
}

// noteConn records an event of the connection's life on the toolbar.
func (c *conn) noteConn(kind string, code websocket.StatusCode, text, detail string) {
	if c.tb != nil {
		c.tb.connEvent(kind, int(code), text, detail)
	}
}

// noteClose records a close the server is about to send: its code, the
// reason the browser gets and why.
func (c *conn) noteClose(code websocket.StatusCode, reason, cause string) {
	if c.tb != nil {
		text := "the server closed it: " + reason
		if n := closeConsequence(code); n != "" {
			cause += "; " + n
		}
		c.tb.connEvent("close", int(code), text, cause)
	}
}

// noteOpen records the socket opening.
func (c *conn) noteOpen() {
	if c.tb == nil {
		return
	}
	comp := "compressed (no context takeover)"
	switch c.app.liveDeflate {
	case ContextTakeover:
		comp = "compressed (context takeover)"
	case Off:
		comp = "not compressed"
	}
	if c.r.Header.Get("Sec-WebSocket-Extensions") == "" && c.app.liveDeflate != Off {
		comp = "not compressed (the browser offered no compression)"
	}
	c.tb.connEvent("open", 0, fmt.Sprintf("the browser opened a socket (connection #%d)", c.tb.conn), "WebSocket, "+comp)
}

// noteJoin records a join the connection answered in protocol version v.
func (c *conn) noteJoin(adopted bool, v int) {
	if c.tb == nil {
		return
	}
	kind, text := "join", "the page mounted fresh"
	if adopted {
		text = "the session the page's render parked was adopted"
	}
	if c.tb.joins++; c.tb.joins > 1 {
		kind, text = "rejoin", "again on the same socket: "+text
	}
	c.tb.connEvent(kind, 0, text, "protocol v"+strconv.Itoa(max(v, 1)))
}

// noteDrop records the socket ending without the server closing it: the
// browser closing it, or the network failing.
func (c *conn) noteDrop(err error) {
	if c.tb == nil {
		return
	}
	if code := websocket.CloseStatus(err); code != -1 {
		text := "the browser closed it"
		var ce websocket.CloseError
		if errors.As(err, &ce) && ce.Reason != "" {
			text += ": " + ce.Reason
		}
		detail := ""
		if code == websocket.StatusGoingAway {
			detail = "the page went away: closed, reloaded or left"
		}
		c.tb.connEvent("drop", int(code), text, detail)
		return
	}
	detail := err.Error()
	if errors.Is(err, context.Canceled) {
		detail = "the request's context ended"
	}
	c.tb.connEvent("drop", 0, "the connection dropped", detail)
}

// closeConsequence is what the browser does after a close with code.
func closeConsequence(code websocket.StatusCode) string {
	switch code {
	case closeReload:
		return "the browser loads the page again (in place when it can)"
	case closeIdle:
		return "the browser connects again once the page is shown"
	case closeRefused:
		return "the browser doesn't reconnect: the page can't be live"
	case websocket.StatusGoingAway:
		return "the browser reconnects"
	case websocket.StatusNormalClosure:
		return "the browser doesn't reconnect"
	}
	return "the browser reconnects"
}

// connView is the Connection panel of a record, and the state the handle
// shows.
type connView struct {
	State, Since, Text, Tone string
	Conns                    int
	Rows                     []connRow
}

type connRow struct {
	Clock, Kind, Label, Text, Detail, Tone string
	Conn, Code                             int
}

var connLabels = map[string]string{"open": "opened", "reconnect": "reconnected", "join": "joined", "rejoin": "rejoined",
	"close": "closed", "drop": "dropped", "refused": "refused", "leave": "moved", "arrive": "arrived"}

// connOf lays a page's connection events out, the latest first, and finds
// the page's state from them: connected while a connection that joined (or
// arrived) hasn't ended; connecting while one opened hasn't joined; else
// from how the last one ended — recovering (closed to load the page again),
// idle, moved (navigated to another page) or disconnected.
func connOf(evs []tbConn) connView {
	var v connView
	if len(evs) == 0 {
		return v
	}
	live := map[int]tbConn{} // each connection's last event, while it lasts
	seen := map[int]bool{}
	var last tbConn
	for _, e := range evs {
		seen[e.Conn] = true
		switch e.Kind {
		case "open", "reconnect", "join", "rejoin", "arrive":
			live[e.Conn] = e
		default:
			delete(live, e.Conn)
			last = e
		}
	}
	v.Conns = len(seen)
	var connected, connecting *tbConn
	for _, e := range live {
		switch e.Kind {
		case "join", "rejoin", "arrive":
			if connected == nil || e.At.After(connected.At) {
				connected = &e
			}
		default:
			if connecting == nil || e.At.After(connecting.At) {
				connecting = &e
			}
		}
	}
	switch {
	case connected != nil:
		v.State, v.Since = "connected", connected.At.Format("15:04:05.000")
	case connecting != nil:
		v.State, v.Since = "connecting", connecting.At.Format("15:04:05.000")
	default:
		v.Since = last.At.Format("15:04:05.000")
		switch {
		case last.Kind == "leave":
			v.State = "moved"
		case last.Code == int(closeReload):
			v.State, v.Tone = "recovering", "warn"
		case last.Code == int(closeIdle):
			v.State = "idle"
		default:
			v.State, v.Tone = "disconnected", "warn"
		}
	}
	v.Text = v.State + " since " + v.Since
	if v.State != "connected" && v.State != "connecting" {
		v.Text += " — " + last.Text
	}
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		r := connRow{Clock: e.At.Format("15:04:05.000"), Kind: e.Kind, Label: connLabels[e.Kind], Text: e.Text, Detail: e.Detail, Conn: e.Conn, Code: e.Code}
		switch e.Kind {
		case "drop", "refused":
			r.Tone = "err"
		case "close":
			if e.Code != int(closeIdle) {
				r.Tone = "warn"
			}
		}
		v.Rows = append(v.Rows, r)
	}
	return v
}
