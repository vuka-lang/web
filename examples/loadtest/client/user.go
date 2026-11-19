package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

var (
	tokenRe = regexp.MustCompile(`data-vk-token="([^"]+)"`)
	onClick = regexp.MustCompile(`data-vk-on-click="([^"]+)"`)
	tagOpen = []byte("~S")
)

// class is what a user does with its page: normal users hold a feed page and
// only receive, interactive users send events to a page nothing is broadcast
// to, slow users hold a feed page and don't keep up with what it receives.
type class int

const (
	normal class = iota
	interactive
	slow
)

var classNames = [...]string{"normal", "interactive", "slow"}

func newHTTPClient(rcvbuf int) *http.Client {
	d := &net.Dialer{Timeout: 30 * time.Second}
	if rcvbuf > 0 {
		d.Control = func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvbuf)
			})
		}
	}
	tr := &http.Transport{DialContext: d.DialContext, MaxIdleConns: 256, MaxIdleConnsPerHost: 256, DisableCompression: true}
	return &http.Client{Transport: tr, Timeout: time.Minute}
}

// pageFor is the address a user loads, and the id of the button it clicks.
func (r *runner) pageFor(i int, cl class) (path, button string) {
	switch {
	case r.o.scenario == "pg":
		return "/?u=" + strconv.Itoa(1+i%r.o.owners), "refresh"
	case cl == interactive:
		return "/counter?rows=" + strconv.Itoa(r.o.rows), "bump"
	}
	return "/feed?rows=" + strconv.Itoa(r.o.rows) + "&topic=" + url.QueryEscape(r.o.topic), ""
}

// user is one browser tab: GET, join, then what its class does until the run
// ends or the server closes the socket.
func (r *runner) user(i int) {
	cl := r.classes[i]
	joined, err := r.session(i, cl)
	if r.ctx.Err() != nil {
		return
	}
	if !joined {
		r.failed.Add(1)
		r.note(&r.failures, cl, err)
		return
	}
	r.ended.Add(1)
	r.alive[cl].Add(-1)
	if err != nil {
		r.note(&r.closes, cl, err)
	}
}

func (r *runner) session(i int, cl class) (joined bool, err error) {
	c := r.httpc
	if cl == slow {
		c = r.slowc
	}
	base := r.targets[i%len(r.targets)]
	path, button := r.pageFor(i, cl)
	page, err := base.Parse(path)
	if err != nil {
		return false, err
	}
	token, target, cookie, err := load(r.ctx, c, page.String(), button)
	if err != nil {
		return false, stage("get", err)
	}
	ws := *base
	ws.Scheme = strings.Replace(base.Scheme, "http", "ws", 1)
	ws.Path = "/_vuka/live"
	ws.RawQuery = "t=" + url.QueryEscape(token)
	dialCtx, done := context.WithTimeout(r.ctx, 30*time.Second)
	defer done()
	dopts := &websocket.DialOptions{HTTPClient: c, HTTPHeader: http.Header{}}
	if cookie != "" {
		dopts.HTTPHeader.Set("Cookie", cookie)
	}
	if r.o.compress && cl != slow {
		dopts.CompressionMode = websocket.CompressionNoContextTakeover
	}
	conn, resp, err := websocket.Dial(dialCtx, ws.String(), dopts)
	if err != nil {
		if resp != nil {
			err = fmt.Errorf("http %d", resp.StatusCode)
		}
		return false, stage("dial", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 24)

	joinCtx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()
	if err := conn.Write(joinCtx, websocket.MessageText, []byte(`{"type":"join","ref":1,"v":2}`)); err != nil {
		return false, stage("join", err)
	}
	var rep reply
	if err := readReply(joinCtx, conn, &rep); err != nil {
		return false, stage("join", err)
	}
	if rep.Type != "render" {
		return false, stage("join", fmt.Errorf("answered %q: %s", rep.Type, rep.Error))
	}
	r.joined.Add(1)
	r.alive[cl].Add(1)

	switch cl {
	case interactive:
		return true, r.interact(i, conn, target)
	case slow:
		return true, r.lag(conn)
	}
	for {
		_, b, err := conn.Read(r.ctx)
		if err != nil {
			return true, err
		}
		r.frame(i, b, time.Now())
	}
}

// interact is an interactive user: a goroutine reads and times the replies
// by their refs, this one sends a click per period, its first at a random
// point of the period so the users spread out.
func (r *runner) interact(i int, conn *websocket.Conn, target string) error {
	var pending sync.Map // ref → time sent
	readErr := make(chan error, 1)
	go func() {
		for {
			var m reply
			if err := readReply(r.ctx, conn, &m); err != nil {
				readErr <- err
				return
			}
			got := time.Now()
			if m.Ref == 0 {
				continue
			}
			v, ok := pending.LoadAndDelete(m.Ref)
			if !ok {
				continue
			}
			if m.Error != "" || m.Type == "error" {
				r.replyErrs.Add(1)
				continue
			}
			if rec := r.rec.Load(); rec != nil {
				rec.add(i, got.Sub(v.(time.Time)))
			}
		}
	}()

	select {
	case <-time.After(time.Duration(rand.Int64N(int64(r.o.every)))):
	case err := <-readErr:
		return err
	case <-r.ctx.Done():
		return nil
	}
	tick := time.NewTicker(r.o.every)
	defer tick.Stop()
	ref := 1
	var msg bytes.Buffer
	for {
		if r.eventsOn.Load() {
			ref++
			msg.Reset()
			fmt.Fprintf(&msg, `{"type":"event","target":%q,"event":"click","ref":%d}`, target, ref)
			pending.Store(ref, time.Now())
			if err := conn.Write(r.ctx, websocket.MessageText, msg.Bytes()); err != nil {
				return err
			}
		}
		select {
		case <-tick.C:
		case err := <-readErr:
			return err
		case <-r.ctx.Done():
			return nil
		}
	}
}

// lag is a slow user. In delay mode it reads a frame per slowDelay, in stop
// mode nothing, until the run drains the slow users.
func (r *runner) lag(conn *websocket.Conn) error {
	if r.o.slowMode == "delay" {
		for {
			select {
			case <-r.resume:
				return r.drain(conn)
			default:
			}
			if _, _, err := conn.Read(r.ctx); err != nil {
				return err
			}
			r.slowFrames.Add(1)
			select {
			case <-time.After(r.o.slowDelay):
			case <-r.resume:
				return r.drain(conn)
			case <-r.ctx.Done():
				return nil
			}
		}
	}
	select {
	case <-r.resume:
		return r.drain(conn)
	case <-r.ctx.Done():
		return nil
	}
}

// drain reads what the server queued, as fast as it comes, and finds whether
// the server closed the socket and how.
func (r *runner) drain(conn *websocket.Conn) error {
	defer r.slowResolved.Add(1)
	for {
		rctx, cancel := context.WithTimeout(r.ctx, r.o.slowDrain)
		_, _, err := conn.Read(rctx)
		cancel()
		switch {
		case err == nil:
			r.slowFrames.Add(1)
		case errors.Is(err, context.DeadlineExceeded) && websocket.CloseStatus(err) < 0:
			r.slowOpen.Add(1)
			return nil
		default:
			return err
		}
	}
}

// frame is what a feed page does with a frame: finds the tick the page shows
// in it and records how long it took to arrive.
func (r *runner) frame(i int, b []byte, now time.Time) {
	k := bytes.Index(b, tagOpen)
	if k < 0 {
		return
	}
	rest := b[k+2:]
	colon := bytes.IndexByte(rest, ':')
	end := bytes.IndexByte(rest, '~')
	if colon < 0 || end < colon {
		return
	}
	seq, ok1 := atoi(rest[:colon])
	at, ok2 := atoi(rest[colon+1 : end])
	if !ok1 || !ok2 {
		return
	}
	lat := max(0, now.UnixNano()-at)
	r.subFrames.Add(1)
	r.subBytes.Add(int64(len(b)))
	r.lastSeq[i].Store(int32(seq))
	if h := r.sub.Load(); h != nil {
		h.observe(time.Duration(lat))
	}
	if t := r.bt.Load(); t != nil {
		t.arrive(int(seq), lat)
	}
}

func atoi(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 19 {
		return 0, false
	}
	var n int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

type reply struct {
	Type  string `json:"type"`
	Ref   int    `json:"ref"`
	Error string `json:"error"`
}

func readReply(ctx context.Context, c *websocket.Conn, into *reply) error {
	_, b, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// load GETs the page for its join token and, when button is named, that
// button's handler.
func load(ctx context.Context, c *http.Client, u, button string) (token, target, cookie string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", "", "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}
	if resp.StatusCode != 200 {
		return "", "", "", fmt.Errorf("http %d", resp.StatusCode)
	}
	m := tokenRe.FindSubmatch(body)
	if m == nil {
		return "", "", "", errors.New("the page has no data-vk-token")
	}
	if button != "" {
		b := regexp.MustCompile(`<button[^>]*id="` + button + `"[^>]*>`).Find(body)
		if b == nil {
			return "", "", "", fmt.Errorf(`the page has no <button id="%s">`, button)
		}
		h := onClick.FindSubmatch(b)
		if h == nil {
			return "", "", "", fmt.Errorf("the %s button has no handler", button)
		}
		target = string(h[1])
	}
	for _, ck := range resp.Cookies() {
		if cookie != "" {
			cookie += "; "
		}
		cookie += ck.Name + "=" + ck.Value
	}
	return string(m[1]), target, cookie, nil
}

func stage(name string, err error) error { return fmt.Errorf("%s: %w", name, err) }

// reason is how a failed or closed user shows in the result: the close
// status and reason when the server closed it, else the error's text.
func reason(err error) string {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return strings.TrimSpace(fmt.Sprintf("close %d %s", int(ce.Code), ce.Reason))
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "failed to write"):
		return "closed by the server without a close frame"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof without a close frame"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
