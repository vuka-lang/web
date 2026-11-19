package components

import (
	"fmt"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2/examples/loadtest/feed"
)

func init() { ui.RegisterShared(&SharedFeed{}) }

// SharedFeed is the feed of the database-free scenario rendered once for every
// page: the last tick broadcast to it (ui.Broadcast) and a row for each of
// the default number of lines, each an instance, so the fragment has 21 like a
// feed page. It holds nothing of a viewer, and its rows have no button.
type SharedFeed struct {
	ui.SharedLive
	tick feed.Tick
}

func (f *SharedFeed) Update(t feed.Tick) { f.tick = t }

func (f *SharedFeed) Render() ui.Node {
	lines := feed.Lines(0)
	rows := make([]ui.Node, len(lines))
	for i, n := range lines {
		rows[i] = ui.Key(n, &SharedLine{N: n, Seq: f.tick.Seq})
	}
	return ui.El("main", nil,
		ui.El("p", []ui.Attr{{Name: "class", Value: "tag"}}, ui.Text(f.tick.Tag())),
		ui.El("ul", nil, rows...))
}

// SharedLine is a row of SharedFeed: an instance whose render depends only on
// its props.
type SharedLine struct {
	ui.Live
	N, Seq int
}

func (l *SharedLine) Render() ui.Node {
	return ui.El("li", []ui.Attr{{Name: "class", Value: "line"}},
		ui.El("span", nil, ui.Text(fmt.Sprint(l.N))),
		ui.El("span", []ui.Attr{{Name: "class", Value: "seq"}}, ui.Text(fmt.Sprint(l.Seq*(l.N+1)))))
}
