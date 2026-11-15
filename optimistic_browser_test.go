package web

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// likeBox is a live page whose likes change at once in the browser, the
// handler answering later: Like fails, Save succeeds.
type likeBox struct {
	ui.Live
	likes int
}

func (c *likeBox) Like() error {
	time.Sleep(300 * time.Millisecond)
	return BadRequest("likes are closed")
}

func (c *likeBox) Save() error {
	time.Sleep(300 * time.Millisecond)
	c.likes++
	return nil
}

func (c *likeBox) Render() ui.Node {
	predict := func() ui.Commands {
		return ui.JS.AddClass(ui.This, "liked").SetAttr(ui.This, "aria-pressed", "true").SetText("#n", "?")
	}
	btn := func(id string, ch ui.Commands) ui.Node {
		return ui.El("button", []ui.Attr{attr("id", id), attr("class", "like"), attr("type", "button"), attr("onClick", ui.On(ch))}, ui.Text(id))
	}
	return ui.El("main", nil,
		ui.El("b", []ui.Attr{attr("id", "n")}, ui.Textf("%d", c.likes)),
		btn("bad", predict().Push(c.Like).Error(ui.JS.SetText("#err", ui.LoadError))),
		btn("keep", predict().Push(c.Like, ui.KeepOnError())),
		btn("ok", predict().Push(c.Save)),
		ui.El("p", []ui.Attr{attr("id", "err")}))
}

// TestBrowserOptimisticRollback pins optimistic updates on a real app: what
// a chain changes before its Push shows at once; when the handler fails the
// changes are taken back (the error reply renders nothing that would) before
// Error runs, and the button is marked failed; KeepOnError keeps them; a
// handler that succeeds keeps them where the server renders nothing else,
// and the server's render wins where it does.
func TestBrowserOptimisticRollback(t *testing.T) {
	a := New(Only("none"))
	a.Live("/likes", func() *likeBox { return &likeBox{} })
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	p := chrome.Launch(t).Open(srv.URL + "/likes")
	shown := func(id string) string {
		return `(() => { const b = document.querySelector("#` + id + `"); return b.className === "like liked" && b.getAttribute("aria-pressed") === "true" && document.querySelector("#n").textContent === "?" && b.hasAttribute("data-vk-pending"); })()`
	}

	p.Eval(`document.querySelector("#bad").click()`, nil)
	p.Wait(shown("bad"))
	p.Wait(`(() => { const b = document.querySelector("#bad"); return !b.hasAttribute("data-vk-pending") && b.className === "like" && !b.hasAttribute("aria-pressed") && document.querySelector("#n").textContent === "0" && b.hasAttribute("data-vk-failed") && document.querySelector("#err").textContent === "likes are closed"; })()`)

	p.Eval(`document.querySelector("#keep").click()`, nil)
	p.Wait(shown("keep"))
	p.Wait(`(() => { const b = document.querySelector("#keep"); return !b.hasAttribute("data-vk-pending") && b.hasAttribute("data-vk-failed"); })()`)
	time.Sleep(200 * time.Millisecond)
	var kept bool
	p.Eval(`(() => { const b = document.querySelector("#keep"); return b.className === "like liked" && b.getAttribute("aria-pressed") === "true" && document.querySelector("#n").textContent === "?"; })()`, &kept)
	if !kept {
		t.Fatal("KeepOnError: the predictions were taken back")
	}

	p.Eval(`document.querySelector("#ok").click()`, nil)
	p.Wait(shown("ok"))
	p.Wait(`(() => { const b = document.querySelector("#ok"); return !b.hasAttribute("data-vk-pending") && !b.hasAttribute("data-vk-failed") && document.querySelector("#n").textContent === "1"; })()`)
	time.Sleep(200 * time.Millisecond)
	var ok bool
	p.Eval(`(() => { const b = document.querySelector("#ok"); return b.className === "like liked" && b.getAttribute("aria-pressed") === "true"; })()`, &ok)
	if !ok {
		t.Fatal("a push that succeeded lost its predictions the server didn't render otherwise")
	}
}
