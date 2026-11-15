package web

import (
	"fmt"
	"math/rand/v2"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
)

// A keyed loop reordered with its rows' content unchanged travels as the
// new order of the old rows: moves by index, no row markup.

const reorderRows = 200

type reorderRow struct {
	ID   int
	Name string
}

type reorderPage struct {
	ui.Live
	rows []reorderRow
	n    int
}

// Tick changes the page but not its rows: the first patch, which maps the
// rows to their items for the next.
func (p *reorderPage) Tick() { p.n++ }

func newReorderPage() *reorderPage {
	p := &reorderPage{}
	for i := range reorderRows {
		p.rows = append(p.rows, reorderRow{ID: i + 1, Name: fmt.Sprintf("pet number %d", i+1)})
	}
	return p
}

func (p *reorderPage) Reverse() { slices.Reverse(p.rows) }

// MoveLast moves the last row to the top.
func (p *reorderPage) MoveLast() {
	last := p.rows[len(p.rows)-1]
	p.rows = append([]reorderRow{last}, p.rows[:len(p.rows)-1]...)
}

// Shuffle reorders the rows the same way every time.
func (p *reorderPage) Shuffle() {
	r := rand.New(rand.NewPCG(1, 2))
	r.Shuffle(len(p.rows), func(i, j int) { p.rows[i], p.rows[j] = p.rows[j], p.rows[i] })
}

// MoveEdit moves the last row to the top and changes its text.
func (p *reorderPage) MoveEdit() {
	p.MoveLast()
	p.rows[0].Name += " (moved)"
}

// Edit changes one row's text where it is.
func (p *reorderPage) Edit() { p.rows[len(p.rows)/2].Name += " (edited)" }

func (p *reorderPage) Render() ui.Node {
	var rows []ui.Node
	for _, r := range p.rows {
		id := r.ID
		rows = append(rows, ui.HTML(`<li key={k} class="row" data-id={id}><div class="card"><span class="name">{name}</span><span class="meta">id <b>{n}</b> · owner <i>someone</i></span></div><input class="note" name={field}/><button class="hit" onClick={hit}>open</button></li>`,
			id, id, r.Name, id, "note"+strconv.Itoa(id), func() { _ = id }))
	}
	return ui.HTML(`<main>
<h1 id="ticks">{n}</h1>
<button id="tick" onClick={tick}>tick</button>
<button id="reverse" onClick={reverse}>reverse</button>
<button id="movelast" onClick={movelast}>move last</button>
<button id="shuffle" onClick={shuffle}>shuffle</button>
<button id="edit" onClick={edit}>edit</button>
<button id="moveedit" onClick={moveedit}>move and edit</button>
<ul id="rows">{rows}</ul>
</main>`, p.n, p.Tick, p.Reverse, p.MoveLast, p.Shuffle, p.Edit, p.MoveEdit, rows)
}

func reorderApp() *App {
	a := New(Only("none"))
	a.Live("/reorder", newReorderPage)
	return a
}

// reorderCases are events of the reorder page and the moves each takes:
// the rows out of a longest run kept in order.
var reorderCases = []struct {
	name, button string
	moves        int
	model        func(*reorderPage)
}{
	{"reverse", "reverse", reorderRows - 1, (*reorderPage).Reverse},
	{"move last", "movelast", 1, (*reorderPage).MoveLast},
	{"shuffle", "shuffle", 173, (*reorderPage).Shuffle},
	{"edit", "edit", 0, (*reorderPage).Edit},
}

var movedChanged = regexp.MustCompile(`\{"m":\d+,"u"`)

var onClickRe = regexp.MustCompile(`<button id="([a-z]+)"[^>]*data-vk-on-click="([^"]+)"`)

// TestLiveKeyedReorderWire: a reorder's patch holds moves ({"m":i}), one per
// row out of the longest run kept in order, and none of the rows' markup;
// its size is a few bytes a move.
func TestLiveKeyedReorderWire(t *testing.T) {
	srv := httptest.NewServer(reorderApp().Handler())
	defer srv.Close()
	for _, tc := range reorderCases {
		t.Run(tc.name, func(t *testing.T) {
			body, tok := pageToken(t, srv, "/reorder")
			targets := map[string]string{}
			for _, m := range onClickRe.FindAllStringSubmatch(body, -1) {
				targets[m[1]] = m[2]
			}
			c, err := dial(t, srv, tok, nil)
			if err != nil {
				t.Fatal(err)
			}
			send(t, c, map[string]any{"type": "join", "ref": 1, "v": 2})
			_, join := recv2(t, c)
			send(t, c, map[string]any{"type": "event", "ref": 2, "target": targets[tc.button], "event": "click"})
			m, raw := recv2(t, c)
			if m.Type != "patch" || m.Error != "" {
				t.Fatalf("patch: %s", raw)
			}
			moves := strings.Count(raw, `{"m":`)
			t.Logf("%s: patch %d bytes (render %d), %d moves", tc.name, len(raw), len(join), moves)
			if moves != tc.moves {
				t.Fatalf("%d moves, want %d: %s", moves, tc.moves, raw)
			}
			if tc.moves == 0 {
				if !strings.Contains(raw, "(edited)") || len(raw) > 400 {
					t.Fatalf("edit: %s", raw)
				}
				return
			}
			if strings.Contains(raw, "pet number") || strings.Contains(raw, `class=\"row\"`) || movedChanged.MatchString(raw) {
				t.Fatalf("a reorder carries rows' markup: %s", raw)
			}
			if limit := 120 + 12*moves; len(raw) > limit {
				t.Fatalf("patch %d bytes for %d moves, want at most %d", len(raw), moves, limit)
			}
		})
	}
}
