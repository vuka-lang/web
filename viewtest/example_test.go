package viewtest_test

import (
	"fmt"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/viewtest"
)

// Clicks is an app's live page: a count and a button adding one.
type Clicks struct {
	ui.Live
	n int
}

func (c *Clicks) Inc() { c.n++ }

func (c *Clicks) Render() ui.Node {
	return ui.El("main", nil,
		ui.El("p", []ui.Attr{{Name: "id", Value: "clicks"}}, ui.Text(fmt.Sprint(c.n, " clicks"))),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(c.Inc)}}, ui.Text("Click me")))
}

func clicksApp() *web.App {
	app := web.New(web.Only("none"), web.NoDotEnv())
	app.Live("/clicks", func() *Clicks { return &Clicks{} })
	return app
}

// An app's test of its page, over the page's connection: open it, click
// its button by its text, and expect the count it shows.
func Example() {
	var t *testing.T // the test's

	p := viewtest.Open(t, clicksApp(), "/clicks")
	p.Click("Click me")
	p.Expect("#clicks").Text("1 clicks")
}

// The same test in Chrome, the page's script running.
func ExampleBrowse() {
	var t *testing.T // the test's

	p := viewtest.Browse(t, clicksApp(), "/clicks")
	p.Click("Click me")
	p.Expect("#clicks").Text("1 clicks")
}

// The examples' tests, run.
func TestExamples(t *testing.T) {
	p := viewtest.Open(t, clicksApp(), "/clicks")
	p.Click("Click me")
	p.Expect("#clicks").Text("1 clicks")

	b := viewtest.Browse(t, clicksApp(), "/clicks")
	b.Click("Click me")
	b.Expect("#clicks").Text("1 clicks")
}
