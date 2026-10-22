package mail

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2"
)

// The development toolbar's Mail panel: the messages a request (or a cron
// run) sent or queued, each with its headers, attachments and its HTML in
// a sandboxed preview. web.Note records nothing outside development.

const panel = "Mail"

func init() { web.AddPanel(panel, drawPanel) }

// noted is a message as the panel shows it; SendLater updates its state.
type noted struct {
	mu                        sync.Mutex
	Driver, From, To, Cc, Bcc string
	ReplyTo, Subject          string
	HTML, Text                string
	Attachments               []string
	At                        time.Time
	State, Err                string // sending, sent, queued, retrying, failed, refused
	Attempts                  int
}

func note(ctx context.Context, driver string, b *built, state string) *noted {
	n := &noted{Driver: driver, From: b.from.String(), To: strings.Join(b.msg.To, ", "), Cc: strings.Join(b.msg.Cc, ", "),
		Bcc: strings.Join(b.msg.Bcc, ", "), ReplyTo: b.msg.ReplyTo, Subject: b.msg.Subject, HTML: b.html, Text: b.text, At: b.at, State: state}
	for _, a := range b.msg.Attachments {
		n.Attachments = append(n.Attachments, fmt.Sprintf("%s (%s)", a.Filename, size(len(a.Data))))
	}
	web.Note(ctx, panel, n)
	return n
}

// noteFailed shows a message refused before it was built.
func noteFailed(ctx context.Context, driver string, msg Message, err error) {
	web.Note(ctx, panel, &noted{Driver: driver, From: msg.From, To: strings.Join(msg.To, ", "), Subject: msg.Subject, At: time.Now(), State: "refused", Err: err.Error()})
}

func (n *noted) done(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Attempts++
	n.State, n.Err = "sent", ""
	if n.Driver == "log" {
		n.State = "logged, not sent"
	}
	if err != nil {
		n.State, n.Err = "failed", err.Error()
	}
}

func (n *noted) retrying(err error, attempts int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.State, n.Err, n.Attempts = "retrying", err.Error(), attempts
}

func size(n int) string {
	if n >= 10_000 {
		return fmt.Sprintf("%.1f kB", float64(n)/1000)
	}
	return fmt.Sprintf("%d B", n)
}

func drawPanel(notes []any) ui.Node {
	var items []ui.Node
	for _, v := range notes {
		n, ok := v.(*noted)
		if !ok {
			items = append(items, ui.El("pre", nil, ui.Text(fmt.Sprintf("%+v", v))))
			continue
		}
		items = append(items, n.node())
	}
	if len(items) == 0 {
		return ui.El("p", []ui.Attr{{Name: "class", Value: "empty"}}, ui.Text("No mail sent."))
	}
	return ui.Fragment(items...)
}

func (n *noted) node() ui.Node {
	n.mu.Lock()
	defer n.mu.Unlock()
	row := func(k, v string) ui.Node {
		if v == "" {
			return nil
		}
		return ui.El("tr", nil, ui.El("th", nil, ui.Text(k)), ui.El("td", nil, ui.Text(v)))
	}
	state := n.State
	if n.Attempts > 1 {
		state += fmt.Sprintf(" after %d attempts", n.Attempts)
	}
	tone := ""
	if n.Err != "" {
		tone = "err"
	}
	var preview ui.Node
	switch {
	case n.HTML != "":
		// sandbox without allow-scripts: the message's scripts don't run,
		// and it can't reach the page.
		preview = ui.El("iframe", []ui.Attr{{Name: "class", Value: "preview mail-preview"}, {Name: "sandbox", Value: ""}, {Name: "title", Value: n.Subject},
			{Name: "srcdoc", Value: n.HTML}})
	case n.Text != "":
		preview = ui.El("pre", []ui.Attr{{Name: "class", Value: "wrap"}}, ui.Text(n.Text))
	}
	var text ui.Node
	if n.HTML != "" && n.Text != "" {
		text = ui.El("details", nil, ui.El("summary", nil, ui.Text("Text part")), ui.El("pre", []ui.Attr{{Name: "class", Value: "wrap"}}, ui.Text(n.Text)))
	}
	var errNode ui.Node
	if n.Err != "" {
		errNode = ui.El("div", []ui.Attr{{Name: "class", Value: "errtext"}}, ui.Text(n.Err))
	}
	return ui.El("div", []ui.Attr{{Name: "class", Value: "mail " + tone}, {Name: "style", Value: ui.Style{"margin-bottom": "18px"}}},
		ui.El("div", []ui.Attr{{Name: "class", Value: "line"}},
			ui.El("span", []ui.Attr{{Name: "class", Value: "status " + tone}}, ui.Text(state)),
			ui.El("b", nil, ui.Text(n.Subject)),
			ui.El("span", []ui.Attr{{Name: "class", Value: "dim"}}, ui.Text(n.Driver+" · "+n.At.Format("15:04:05.000")))),
		errNode,
		ui.El("table", []ui.Attr{{Name: "class", Value: "kv"}},
			row("From", n.From), row("To", n.To), row("Cc", n.Cc), row("Bcc", n.Bcc), row("Reply-To", n.ReplyTo),
			row("Attachments", strings.Join(n.Attachments, ", "))),
		preview, text)
}
