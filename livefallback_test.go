package web

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/ui/live"
	"github.com/vuka-lang/web/internal/testenv"
)

// notes is a hand-written component long enough for its changes to come
// as token patches.
type notes struct {
	ui.Live
	n int
}

func (s *notes) Add() { s.n++ }
func (s *notes) Render() ui.Node {
	var lis []ui.Node
	for i := range s.n + 100 {
		lis = append(lis, ui.El("li", nil, ui.Text("note "+strconv.Itoa(i))))
	}
	return ui.El("section", nil, ui.El("ul", nil, lis...),
		ui.El("button", []ui.Attr{{Name: "onClick", Value: ui.On(s.Add)}}, ui.Text("add")))
}

var clickTarget = regexp.MustCompile(`data-vk-on-click="([^"]+)"`)

// TestLiveJSWholeComponents: components written by hand, with no render
// tracking, patch as whole components; the browser runtime's tree code
// turns every reply into the page the server holds.
func TestLiveJSWholeComponents(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		testenv.Needs(t, "no node")
	}
	page := func(context.Context) ui.Node {
		counters := ui.Nodes(func(add func(ui.Node)) {
			for i := range 4 {
				add(ui.Component("whole#c", i, &counter{Label: "c" + strconv.Itoa(i), Start: i}))
			}
		})
		opaque := ui.NodeFunc(func(ctx context.Context, w io.Writer) error {
			return ui.El("aside", nil, ui.Component("whole#o", nil, &counter{Label: "inside"})).Render(ctx, w)
		})
		return ui.El("main", nil, counters, opaque, ui.Component("whole#n", nil, &notes{}))
	}
	s := live.NewSession(context.Background(), page)
	type step struct {
		Msg  json.RawMessage `json:"msg"`
		Page string          `json:"page"`
	}
	var steps []step
	send := func(m string) {
		steps = append(steps, step{Msg: s.HandleJSON([]byte(m)), Page: s.HTML()})
	}
	send(`{"type":"join","v":2}`)
	for i := range 30 {
		targets := clickTarget.FindAllStringSubmatch(s.HTML(), -1)
		send(`{"type":"event","ref":` + strconv.Itoa(i+1) + `,"target":"` + targets[i*7%len(targets)][1] + `","event":"click"}`)
	}
	fx, _ := json.Marshal([]map[string]any{{"name": "whole components", "steps": steps}})
	file := filepath.Join(t.TempDir(), "whole.json")
	if err := os.WriteFile(file, fx, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "testdata/livejs/run.js", "live.js", file).CombinedOutput()
	if err != nil {
		t.Fatalf("%s", out)
	}
	if !strings.Contains(string(fx), `"p":`) {
		t.Errorf("no token patch among the replies")
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}
