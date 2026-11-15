package viewtest

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/vuka-lang/ui/v2/live"
)

// conn is a page's live connection, speaking the protocol's first version
// (each changed instance's whole markup): it holds the page as the
// messages so far left it — replies and what the server sends unasked, a
// deferred group's patch or a broadcast's — and signals each change.
type conn struct {
	ws *websocket.Conn

	mu      sync.Mutex
	html    string
	changed chan struct{} // closed, and replaced, on each message applied
	waits   map[int]chan live.ServerMessage
	ref     int
	err     error // why the connection ended
}

// dialLive connects to the page whose join token is token, with the
// cookies jar holds for base.
func dialLive(base string, jar http.CookieJar, token string) (*conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	u, _ := url.Parse(base)
	h := http.Header{"Origin": {base}}
	for _, c := range jar.Cookies(u) {
		h.Add("Cookie", c.Name+"="+c.Value)
	}
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/_vuka/live?t="+url.QueryEscape(html.UnescapeString(token)), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(-1)
	c := &conn{ws: ws, changed: make(chan struct{}), waits: map[int]chan live.ServerMessage{}}
	go c.read()
	return c, nil
}

func (c *conn) read() {
	for {
		_, data, err := c.ws.Read(context.Background())
		var m live.ServerMessage
		if err == nil {
			err = json.Unmarshal(data, &m)
		}
		c.mu.Lock()
		if err != nil {
			c.err = err
			for ref, ch := range c.waits {
				close(ch)
				delete(c.waits, ref)
			}
			close(c.changed)
			c.changed = make(chan struct{})
			c.mu.Unlock()
			return
		}
		switch m.Type {
		case live.Render:
			c.html = m.HTML
		case live.PatchMsg:
			for _, u := range m.Updates {
				c.html = replaceInstance(c.html, u.ID, u.HTML)
			}
		}
		if ch := c.waits[m.Ref]; m.Ref != 0 && ch != nil {
			delete(c.waits, m.Ref)
			ch <- m
		}
		close(c.changed)
		c.changed = make(chan struct{})
		c.mu.Unlock()
	}
}

// state is the page as the connection holds it, and a channel closed on
// its next change.
func (c *conn) state() (string, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.html, c.changed
}

// roundTrip sends m and waits for its reply, applied to the page.
func (c *conn) roundTrip(m live.ClientMessage) (live.ServerMessage, error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return live.ServerMessage{}, c.err
	}
	c.ref++
	m.Ref = c.ref
	ch := make(chan live.ServerMessage, 1)
	c.waits[m.Ref] = ch
	c.mu.Unlock()
	if err := c.write(m); err != nil {
		return live.ServerMessage{}, err
	}
	select {
	case reply, ok := <-ch:
		if !ok {
			return reply, c.closedErr()
		}
		return reply, nil
	case <-time.After(Timeout):
		c.mu.Lock()
		delete(c.waits, m.Ref)
		c.mu.Unlock()
		return live.ServerMessage{}, errors.New("no reply within " + Timeout.String())
	}
}

func (c *conn) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// write sends a message no reply answers (prefetch, loads).
func (c *conn) write(m any) error {
	b, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

func (c *conn) close() { c.ws.CloseNow() }
