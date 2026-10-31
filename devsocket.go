package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// devSocketPath is the WebSocket a development page's tooling shares. A
// browser allows about six HTTP/1.1 connections per host, and a stream held
// open counts against them, so a few tabs of event streams leave no room for
// any other request. WebSockets don't count: each tab holds one, whatever it
// uses of the tooling.
const devSocketPath = livePrefix + "dev"

// devSocketJS is the page's side (window.__vukaDev), put before each script
// using it, whichever runs first opening the socket.
//
//go:embed devsocket.js
var devSocketJS string

// devChannel is what the socket sends under a name: current's value, when ok.
type devChannel struct {
	name    string
	current func() (string, bool)
}

// devSocket serves the development channels: each sends its value when a page
// connects, then each new value as it changes, read every 100ms on the
// server, so the page asks nothing. A connection ends with the page, on the
// app's Stop, and on Shutdown of any server that served one.
type devSocket struct {
	channels []devChannel
	stop     chan struct{}
	once     sync.Once
	servers  sync.Map // *http.Server → struct{}: those told to end the connections on Shutdown
}

// newDevSocket is the app's socket, nil when nothing in it needs one: never in
// production.
func (a *App) newDevSocket() *devSocket {
	var chans []devChannel
	if a.css != nil && a.css.dev {
		chans = append(chans, devChannel{"css", func() (string, bool) {
			_, tag, ok := a.css.current()
			return tag, ok
		}})
	}
	if a.assets != nil && a.assets.dir != "" {
		chans = append(chans, devChannel{"assets", a.assets.devManifest})
	}
	if a.tb != nil && a.tb.dir != "" {
		chans = append(chans, devChannel{"build", func() (string, bool) {
			st := readBuildState(a.tb.dir, a.assets)
			st.Elapsed = 0 // the page counts from Since
			b, _ := json.Marshal(st)
			return string(b), true
		}})
	}
	if chans == nil {
		return nil
	}
	return &devSocket{channels: chans, stop: make(chan struct{})}
}

func (d *devSocket) serve(w http.ResponseWriter, r *http.Request) {
	if srv, ok := r.Context().Value(http.ServerContextKey).(*http.Server); ok {
		if _, seen := d.servers.LoadOrStore(srv, struct{}{}); !seen {
			srv.RegisterOnShutdown(d.close)
		}
	}
	// No OriginPatterns, LiveOrigins' or any: a WebSocket ignores CORS, so
	// Accept's check (403 unless Origin is this host) is what keeps other
	// sites from reading this.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ctx := ws.CloseRead(r.Context())
	sent := make([]string, len(d.channels))
	seen := make([]bool, len(d.channels))
	check := time.NewTicker(100 * time.Millisecond)
	defer check.Stop()
	alive := time.NewTicker(15 * time.Second)
	defer alive.Stop()
	for {
		for i, c := range d.channels {
			v, ok := c.current()
			if !ok || seen[i] && v == sent[i] {
				continue
			}
			sent[i], seen[i] = v, true
			msg, _ := json.Marshal(struct {
				Channel string `json:"c"`
				Value   string `json:"v"`
			}{c.name, v})
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := ws.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		}
		select {
		case <-check.C:
		case <-alive.C: // a page gone without a word
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := ws.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		case <-d.stop:
			ws.Close(websocket.StatusGoingAway, "stopping")
			return
		}
	}
}

func (d *devSocket) close() {
	if d != nil {
		d.once.Do(func() { close(d.stop) })
	}
}
