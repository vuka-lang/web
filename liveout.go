package web

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// liveOutFrames is how many encoded frames wait for a connection's socket.
const liveOutFrames = 32

// closeTimeout bounds a close the runtime is told of: the frames ahead of it
// finishing, and the close frame's write.
var closeTimeout = 2 * time.Second

// outbox is a connection's outbound frames, in the order they were made. One
// goroutine, started with the first frame and ended when none is left,
// writes them, so an idle page holds none; frames are never dropped or
// reordered, so what the browser holds stays what the session's shadows say.
type outbox struct {
	mu     sync.Mutex
	q      [][]byte
	run    bool          // the writer goroutine is running
	dead   bool          // the connection is ending: no more frames
	space  chan struct{} // signalled when the writer takes a frame
	idleCh chan struct{} // closed when the writer ends
}

func (o *outbox) isDead() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.dead
}

// kill refuses further frames and forgets those queued.
func (o *outbox) kill() {
	o.mu.Lock()
	o.dead = true
	o.q = nil
	o.mu.Unlock()
}

// send queues b for the writer; false when the connection is ending or the
// queue stayed full: with wait, until the writer takes a frame (at most
// writeTimeout), else not at all.
func (c *conn) send(ctx context.Context, b []byte, wait bool) bool {
	o := &c.out
	var timer *time.Timer
	for {
		o.mu.Lock()
		if o.dead {
			o.mu.Unlock()
			return false
		}
		if len(o.q) < liveOutFrames {
			o.q = append(o.q, b)
			start := !o.run
			o.run = true
			o.mu.Unlock()
			if start {
				go c.flushOut()
			}
			if timer != nil {
				timer.Stop()
			}
			return true
		}
		o.mu.Unlock()
		if !wait {
			c.overflow("a broadcast found the browser's queue of frames full")
			return false
		}
		if timer == nil {
			timer = time.NewTimer(writeTimeout)
			defer timer.Stop()
		}
		select {
		case <-o.space:
		case <-timer.C:
			c.overflow("the browser took no frame in " + writeTimeout.String())
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// overflow ends a connection whose browser doesn't take what it is sent: it
// loads the page again.
func (c *conn) overflow(cause string) {
	c.out.mu.Lock()
	c.out.q = nil // the page loads again: the frames waiting are of no use
	c.out.mu.Unlock()
	c.noteClose(closeReload, "live: the page must load again", cause)
	c.closeConn(closeReload, "live: the page must load again")
}

// flushOut writes the queued frames until none is left.
func (c *conn) flushOut() {
	o := &c.out
	for {
		o.mu.Lock()
		if len(o.q) == 0 || o.dead {
			o.run = false
			if o.idleCh != nil {
				close(o.idleCh)
				o.idleCh = nil
			}
			o.mu.Unlock()
			return
		}
		b := o.q[0]
		o.q[0] = nil
		o.q = o.q[1:]
		o.mu.Unlock()
		select {
		case o.space <- struct{}{}:
		default:
		}
		wctx, done := context.WithTimeout(context.Background(), writeTimeout)
		err := c.ws.Write(wctx, websocket.MessageText, b)
		done()
		if err != nil {
			o.kill()
			c.noteConn("drop", 0, "a write to the browser failed", err.Error())
			c.ws.CloseNow()
		}
	}
}

// settle waits for the writer to finish what is queued, up to d, and
// refuses more frames.
func (o *outbox) settle(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		o.mu.Lock()
		if !o.run {
			o.dead = true
			o.mu.Unlock()
			return
		}
		if o.idleCh == nil {
			o.idleCh = make(chan struct{})
		}
		ch := o.idleCh
		o.mu.Unlock()
		select {
		case <-ch:
		case <-t.C:
			o.kill()
			return
		}
	}
}

// closeConn sends the frames queued, then closes with code and reason, each
// under closeTimeout: a close frame goes out whenever the socket can take
// one, and the socket is dropped when it can't. It reports whether this call
// was the one that closed.
func (c *conn) closeConn(code websocket.StatusCode, reason string) bool {
	if !c.closing.CompareAndSwap(false, true) {
		return false
	}
	c.out.settle(closeTimeout)
	done := make(chan struct{})
	go func() {
		c.ws.Close(code, reason)
		close(done)
	}()
	t := time.NewTimer(closeTimeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		c.ws.CloseNow()
	}
	return true
}

// write queues m for the browser, waiting for room in the queue.
func (c *conn) write(ctx context.Context, m any) bool { return c.queue(ctx, m, true) }

// push queues m without waiting: a browser whose queue is full is closed
// and loads the page again.
func (c *conn) push(ctx context.Context, m any) bool { return c.queue(ctx, m, false) }

func (c *conn) queue(ctx context.Context, m any, wait bool) bool {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false) // markup as markup, not <…
	e.Encode(m)
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if c.tb != nil {
		f := newFrame("server", b)
		f.Report = c.wire.apply(b)
		c.frames = append(c.frames, f)
	}
	return c.send(ctx, b, wait)
}
