package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// counting starts a count from its mount hook by pushing a command that
// calls a method its markup never names; so does the generic table it
// renders.
type counting struct {
	ui.Live
	started, clicks int
}

func (c *counting) LiveSchema(l *ui.LiveSchema) { l.Mount(c.mount) }
func (c *counting) mount(context.Context) error {
	c.PushJS(ui.JS.Push(c.start))
	return nil
}
func (c *counting) start() { c.started++ }
func (c *counting) Inc()   { c.clicks++ }
func (c *counting) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("button", []ui.Attr{{Name: "id", Value: "inc"}, {Name: "onClick", Value: ui.On(c.Inc)}}, ui.Text("+")),
		ui.El("p", []ui.Attr{{Name: "id", Value: "page"}}, ui.Text(fmt.Sprintf("started %d clicks %d", c.started, c.clicks))),
		ui.Component("rows", nil, &countingTable[string]{Rows: []string{"a", "b"}}))
}

type countingTable[T any] struct {
	ui.Live
	Rows  []T
	total int
}

func (c *countingTable[T]) LiveSchema(l *ui.LiveSchema) { l.Mount(c.mount) }
func (c *countingTable[T]) mount(context.Context) error {
	c.PushJS(ui.JS.Push(c.startCount))
	return nil
}
func (c *countingTable[T]) startCount(context.Context) error { c.total = len(c.Rows); return nil }
func (c *countingTable[T]) Render() ui.Node {
	return ui.El("div", nil,
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(func() {})}}, ui.Text("noop")),
		ui.El("p", []ui.Attr{{Name: "id", Value: "table"}}, ui.Text(fmt.Sprintf("of %d entries", c.total))))
}

// A real browser: the commands the page's components pushed from their
// mount hooks reach handlers that answer, whether the join adopted the
// session the GET mounted (whose render ran again before the commands
// arrived) or mounted afresh.
func TestBrowserPushFromMount(t *testing.T) {
	b := chrome.Launch(t)
	for _, adopt := range []bool{true, false} {
		var opts []Option
		if !adopt {
			opts = append(opts, LiveAdopt(0))
		}
		app := New(append([]Option{Only("none")}, opts...)...)
		app.Live("/counting", &counting{})
		srv := httptest.NewServer(app.Handler())
		p := b.Open(srv.URL + "/counting")
		p.WaitText("#page", "started 1 clicks 0")
		p.WaitText("#table", "of 2 entries")
		p.Click("#inc")
		p.WaitText("#page", "started 1 clicks 1")
		srv.Close()
	}
}
