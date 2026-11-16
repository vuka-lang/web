package web_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/live"
	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/viewtest"
)

// replyDB is what the reply-watched table lists: a package variable, so
// the table has no props and the page's renders skip it.
var replyDB = struct {
	mu  sync.Mutex
	ids []int
}{}

type replyChanged struct{}

// replyWatched is a page whose watcher hears of new orders by a broadcast
// and has the table's region render in the broadcast's own patch
// (ui.Live.Reload) — no round trip of the browser's.
type replyWatched struct{ ui.Live }

func (p *replyWatched) Render() ui.Node {
	return ui.El("main", nil,
		ui.Component("reply.watcher", nil, &replyWatcher{}),
		ui.Component("reply.table", nil, &replyTable{}))
}

type replyWatcher struct{ ui.Live }

func (w *replyWatcher) LiveSchema(l *ui.LiveSchema) { l.Mount(w.mount) }
func (w *replyWatcher) mount(context.Context) error { w.Subscribe("orders"); return nil }
func (w *replyWatcher) Update(replyChanged)         { w.Reload("#orders") }
func (w *replyWatcher) Render() ui.Node             { return ui.El("span", nil) }

type replyTable struct{ ui.Live }

func (t *replyTable) Render() ui.Node {
	replyDB.mu.Lock()
	defer replyDB.mu.Unlock()
	rows := make([]ui.Node, 0, len(replyDB.ids))
	for _, id := range replyDB.ids {
		rows = append(rows, ui.El("tr", []ui.Attr{{Name: "key", Value: id}},
			ui.El("td", nil, ui.Text("order "+strconv.Itoa(id)))))
	}
	return ui.El("table", []ui.Attr{{Name: "id", Value: "orders"}}, ui.El("tbody", nil, rows...))
}

// A broadcast's Update renders a region in the broadcast's own patch
// (ui.Live.Reload): the page shows the rows added since with no event from
// the browser.
func TestReloadInReply(t *testing.T) {
	defer func(v bool) { live.Verify = v }(live.Verify)
	live.Verify = false // the table reads the database
	replyDB.ids = []int{1}
	a := web.New(web.Only("none"))
	a.Live("/watched", func() *replyWatched { return &replyWatched{} })
	p := viewtest.Open(t, a, "/watched")
	p.Expect("#orders td").Text("order 1")

	replyDB.mu.Lock()
	replyDB.ids = append(replyDB.ids, 2)
	replyDB.mu.Unlock()
	if err := a.Broadcast(context.Background(), "orders", replyChanged{}); err != nil {
		t.Fatal(err)
	}
	p.Wait("two rows", func() bool {
		return strings.Contains(p.HTML(), "order 1") && strings.Contains(p.HTML(), "order 2")
	})
}
