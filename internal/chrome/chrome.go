// Package chrome drives a headless Chrome over the DevTools protocol, for the
// tests of live pages: open pages, click, type, press keys and evaluate
// JavaScript. It is a small hand-written client over the WebSocket library
// the framework already uses.
package chrome

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Timeout bounds every call and wait.
var Timeout = 10 * time.Second

// Find returns a Chrome or Chromium binary: $VUKA_CHROME, then the usual
// install locations and PATH names; "" when there is none.
func Find() string {
	if p := os.Getenv("VUKA_CHROME"); p != "" {
		return p
	}
	if runtime.GOOS == "darwin" {
		for _, p := range []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium"} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// Browser is a running headless Chrome.
type Browser struct {
	t    testing.TB
	cmd  *exec.Cmd
	dir  string
	ws   *websocket.Conn
	ctx  context.Context
	stop context.CancelFunc

	mu      sync.Mutex
	next    int
	pending map[int]chan message
}

type message struct {
	ID        int             `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    any             `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Launch starts Chrome for the test, closed when it ends; it skips the test
// when there is no Chrome.
func Launch(t testing.TB) *Browser {
	t.Helper()
	bin := Find()
	if bin == "" {
		t.Skip("no Chrome found (set VUKA_CHROME)")
	}
	dir, err := os.MkdirTemp("", "vuka-chrome-")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--disable-background-networking", "--remote-debugging-port=0",
		"--user-data-dir=" + dir, "about:blank"}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		args = append(args, "--no-sandbox")
	}
	ctx, stop := context.WithCancel(context.Background())
	b := &Browser{t: t, cmd: exec.Command(bin, args...), dir: dir, ctx: ctx, stop: stop, pending: map[int]chan message{}}
	if err := b.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.close)
	addr, err := devtools(dir)
	if err != nil {
		t.Fatal(err)
	}
	dctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	if b.ws, _, err = websocket.Dial(dctx, addr, nil); err != nil {
		t.Fatal(err)
	}
	b.ws.SetReadLimit(64 << 20)
	go b.read()
	return b
}

// devtools waits for Chrome to write its DevTools port and path.
func devtools(dir string) (string, error) {
	for end := time.Now().Add(Timeout); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		f, err := os.Open(filepath.Join(dir, "DevToolsActivePort"))
		if err != nil {
			continue
		}
		var lines []string
		for sc := bufio.NewScanner(f); sc.Scan(); {
			lines = append(lines, strings.TrimSpace(sc.Text()))
		}
		f.Close()
		if len(lines) >= 2 && lines[1] != "" {
			return "ws://127.0.0.1:" + lines[0] + lines[1], nil
		}
	}
	return "", errors.New("chrome: no DevTools port")
}

func (b *Browser) read() {
	for {
		_, data, err := b.ws.Read(b.ctx)
		if err != nil {
			b.mu.Lock()
			for id, ch := range b.pending {
				close(ch)
				delete(b.pending, id)
			}
			b.mu.Unlock()
			return
		}
		var m message
		if json.Unmarshal(data, &m) == nil && m.ID != 0 {
			b.mu.Lock()
			if ch := b.pending[m.ID]; ch != nil {
				delete(b.pending, m.ID)
				ch <- m
			}
			b.mu.Unlock()
		}
	}
}

func (b *Browser) call(session, method string, params, out any) error {
	b.mu.Lock()
	b.next++
	id, ch := b.next, make(chan message, 1)
	b.pending[id] = ch
	b.mu.Unlock()
	data, _ := json.Marshal(message{ID: id, Method: method, Params: params, SessionID: session})
	ctx, cancel := context.WithTimeout(b.ctx, Timeout)
	defer cancel()
	if err := b.ws.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}
	select {
	case m, ok := <-ch:
		switch {
		case !ok:
			return errors.New("chrome: closed")
		case m.Error != nil:
			return fmt.Errorf("%s: %s", method, m.Error.Message)
		case out != nil:
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

func (b *Browser) close() {
	if b.ws != nil {
		b.call("", "Browser.close", nil, nil)
		b.ws.CloseNow()
	}
	b.stop()
	done := make(chan struct{})
	go func() { b.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		b.cmd.Process.Kill()
		<-done
	}
	os.RemoveAll(b.dir)
}

// Page is a tab.
type Page struct {
	b       *Browser
	session string
}

// Open opens url in a new tab and waits for the page to be live.
func (b *Browser) Open(url string) *Page {
	b.t.Helper()
	var target struct {
		TargetID string `json:"targetId"`
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.call("", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		b.t.Fatal(err)
	}
	if err := b.call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		b.t.Fatal(err)
	}
	p := &Page{b, attached.SessionID}
	p.must("Page.enable", nil, nil)
	p.must("Page.navigate", map[string]any{"url": url}, nil)
	p.Wait(`document.documentElement.dataset.vkState === "connected"`)
	return p
}

func (p *Page) must(method string, params, out any) {
	p.b.t.Helper()
	if err := p.b.call(p.session, method, params, out); err != nil {
		p.b.t.Fatal(err)
	}
}

// Eval evaluates a JavaScript expression (awaiting a promise) and decodes its
// value into out, when not nil.
func (p *Page) Eval(js string, out any) error {
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := p.b.call(p.session, "Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true, "awaitPromise": true}, &r); err != nil {
		return err
	}
	if r.Exception != nil {
		return fmt.Errorf("%s: %s %s", js, r.Exception.Text, r.Exception.Exception.Description)
	}
	if out != nil && len(r.Result.Value) > 0 {
		return json.Unmarshal(r.Result.Value, out)
	}
	return nil
}

// Wait waits for a JavaScript expression to be true.
func (p *Page) Wait(js string) {
	p.b.t.Helper()
	var last error
	for end := time.Now().Add(Timeout); time.Now().Before(end); time.Sleep(25 * time.Millisecond) {
		var ok bool
		if last = p.Eval("!!("+js+")", &ok); last == nil && ok {
			return
		}
	}
	var html string
	p.Eval("document.body.outerHTML", &html)
	p.b.t.Fatalf("chrome: waiting for %s: %v\n%s", js, last, html)
}

// Text is the text of the first element matching a CSS selector.
func (p *Page) Text(sel string) string {
	p.b.t.Helper()
	var s string
	if err := p.Eval(fmt.Sprintf(`(document.querySelector(%q) || {}).textContent || ""`, sel), &s); err != nil {
		p.b.t.Fatal(err)
	}
	return s
}

// WaitText waits for the element matching sel to have the text want.
func (p *Page) WaitText(sel, want string) {
	p.b.t.Helper()
	p.Wait(fmt.Sprintf(`(document.querySelector(%q) || {}).textContent === %q`, sel, want))
}

// Click clicks the middle of the element matching sel with the mouse.
func (p *Page) Click(sel string) {
	p.b.t.Helper()
	var at struct{ X, Y float64 }
	p.Wait(fmt.Sprintf(`document.querySelector(%q)`, sel))
	js := fmt.Sprintf(`(() => { const el = document.querySelector(%q); el.scrollIntoView({block: "center"});
		const r = el.getBoundingClientRect(); return {X: r.x + r.width / 2, Y: r.y + r.height / 2}; })()`, sel)
	if err := p.Eval(js, &at); err != nil {
		p.b.t.Fatal(err)
	}
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		p.must("Input.dispatchMouseEvent", map[string]any{"type": typ, "x": at.X, "y": at.Y, "button": "left", "clickCount": 1}, nil)
	}
}

// Type focuses the element matching sel and types text into it.
func (p *Page) Type(sel, text string) {
	p.b.t.Helper()
	p.Click(sel)
	for _, r := range text {
		p.must("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "text": string(r)}, nil)
		p.must("Input.dispatchKeyEvent", map[string]any{"type": "keyUp"}, nil)
	}
}

// Press presses a key: "Enter", "Escape", …
func (p *Page) Press(key string) {
	p.b.t.Helper()
	down := map[string]any{"type": "keyDown", "key": key, "code": key}
	if key == "Enter" {
		down["text"], down["windowsVirtualKeyCode"] = "\r", 13
	}
	p.must("Input.dispatchKeyEvent", down, nil)
	p.must("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": key, "code": key}, nil)
}
