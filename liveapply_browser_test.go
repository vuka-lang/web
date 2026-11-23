package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/ui/v2/uigen"
	"github.com/vuka-lang/web/v2/internal/chrome"
)

// What a small patch to a long keyed loop costs the browser: the rows it
// doesn't change are left as they are, so applying it costs about the
// rows it changes, not the rows on the page. The rows bind a class to a
// browser state (a selection), which puts the binding back after every
// patch.

type applyRow struct {
	ID    int
	Label string
}

type applyPage struct {
	ui.Live
	rows     []applyRow
	selected ui.State[int]
}

func newApplyPage(n int) *applyPage {
	p := &applyPage{}
	for i := range n {
		p.rows = append(p.rows, applyRow{i + 1, fmt.Sprintf("row %d", i+1)})
	}
	return p
}

func (p *applyPage) at(id int) int {
	return slices.IndexFunc(p.rows, func(r applyRow) bool { return r.ID == id })
}

func (p *applyPage) Edit(id int) {
	if i := p.at(id); i >= 0 {
		p.rows[i].Label += " #"
	}
}

func (p *applyPage) Remove(id int) {
	if i := p.at(id); i >= 0 {
		p.rows = slices.Delete(p.rows, i, i+1)
	}
}

// Thin removes every third row.
func (p *applyPage) Thin() {
	var keep []applyRow
	for i, r := range p.rows {
		if i%3 != 1 {
			keep = append(keep, r)
		}
	}
	p.rows = keep
}

// SwapDrop swaps the second and the second to last row and removes the last.
func (p *applyPage) SwapDrop() {
	n := len(p.rows)
	p.rows[1], p.rows[n-2] = p.rows[n-2], p.rows[1]
	p.rows = p.rows[:n-1]
}

// MoveEditDrop moves the last row to the top, edits it and removes the
// third.
func (p *applyPage) MoveEditDrop() {
	n := len(p.rows)
	last := p.rows[n-1]
	last.Label += " (moved)"
	p.rows = append([]applyRow{last}, p.rows[:n-1]...)
	p.rows = slices.Delete(p.rows, 2, 3)
}

func (p *applyPage) Update() {
	for i := 0; i < len(p.rows); i += 10 {
		p.rows[i].Label += " !!!"
	}
}

func (p *applyPage) Swap() {
	if n := len(p.rows); n > 3 {
		p.rows[1], p.rows[n-2] = p.rows[n-2], p.rows[1]
	}
}

func (p *applyPage) Render() ui.Node {
	var rows []ui.Node
	for _, r := range p.rows {
		rows = append(rows, ui.HTML(`<tr key={k} class:danger={sel}><td class="id">{id}</td><td><a class="lbl" onClick={pick}>{label}</a></td><td><a class="edit" onClick={edit}>edit</a></td><td><a class="remove" onClick={remove}>x</a></td></tr>`,
			r.ID, uigen.Eq(p.selected, r.ID), r.ID, ui.JS.Set(p.selected, r.ID), r.Label, ui.JS.Push(p.Edit, r.ID), ui.JS.Push(p.Remove, r.ID)))
	}
	return ui.HTML(`<main>
<button id="update" onClick={update}>update</button>
<button id="swap" onClick={swap}>swap</button>
<button id="thin" onClick={thin}>thin</button>
<button id="swapdrop" onClick={swapdrop}>swap and drop</button>
<button id="moveeditdrop" onClick={moveeditdrop}>move, edit and drop</button>
<table><tbody id="rows">{rows}</tbody></table>
</main>`, p.Update, p.Swap, p.Thin, p.SwapDrop, p.MoveEditDrop, rows)
}

func applyServer(t *testing.T, n int) string {
	t.Helper()
	a := New(Only("none"))
	a.Live("/rows", func() *applyPage { return newApplyPage(n) })
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv.URL + "/rows"
}

// applyStep is what a step left: the rows' markup, which rows are the
// elements they were (their mark, from when the page loaded), and whether
// the page morphed the rows row by row.
type applyStep struct {
	HTML   string
	Marks  []string
	Scoped int
}

// applySteps runs the scenario on a fresh page; whole morphs every patch
// as a whole container (live.js without its frame-scoped morph).
func applySteps(t *testing.T, b *chrome.Browser, url string, whole bool) []applyStep {
	t.Helper()
	p := b.Open(url)
	p.Wait(`document.documentElement.dataset.vkProtocol === "2"`)
	if whole {
		p.Eval(`window.__vkNoScope = true`, nil)
	}
	p.Eval(`document.querySelectorAll("#rows tr").forEach((tr) => { tr.__mark = "r" + tr.querySelector(".id").textContent; }); 0`, nil)
	state := func() applyStep {
		var s applyStep
		p.Eval(`(() => ({ html: document.getElementById("rows").outerHTML,
			marks: Array.from(document.querySelectorAll("#rows tr"), (tr) => tr.__mark || "new"),
			scoped: (window.__vkScoped || {}).rows || 0 }))()`, &s)
		return s
	}
	var out []applyStep
	// step clicks and waits for the patch (or, for a browser-only state
	// change, the class) to be applied: the rows changed, none waiting for
	// its reply
	step := func(click, done string) {
		before := state().HTML
		p.Eval(click+`; 0`, nil)
		if done == "" {
			done = fmt.Sprintf(`!document.querySelector("#rows [data-vk-pending]") && document.getElementById("rows").outerHTML !== %s`, mustJSON(before))
		}
		p.Wait(done)
		out = append(out, state())
	}
	row := func(n int, a string) string {
		return fmt.Sprintf(`document.querySelector("#rows tr:nth-child(%d) a.%s").click()`, n, a)
	}
	step(row(3, "lbl"), `document.querySelector("#rows tr:nth-child(3)").classList.contains("danger")`) // select row 3
	step(row(5, "edit"), "")
	step(row(4, "remove"), "")
	step(row(3, "remove"), "") // the selected row
	step(`document.getElementById("swap").click()`, "")
	step(row(7, "lbl"), `document.querySelector("#rows tr:nth-child(7)").classList.contains("danger")`)
	step(`document.getElementById("thin").click()`, "")
	step(row(1, "remove"), "")
	step(`document.getElementById("swapdrop").click()`, "")
	step(`document.getElementById("moveeditdrop").click()`, "")
	step(`document.getElementById("update").click()`, "")
	step(`document.querySelector("#rows tr:last-child a.remove").click()`, "")
	step(row(2, "edit"), "")
	var diverged []string
	p.Eval(`window.__vkDiverged || []`, &diverged)
	if len(diverged) > 0 {
		t.Fatalf("the frame-scoped morph differs from the whole one: %v", diverged)
	}
	return out
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestBrowserApplySmallPatchSame: removals, swaps, edits and a selection
// of a long keyed loop leave the page as the whole-container morph does,
// step by step: the same markup, the same elements kept — and they are
// applied row by row (verify mode checks each against a whole morph too).
func TestBrowserApplySmallPatchSame(t *testing.T) {
	url := applyServer(t, 60)
	b := chrome.Launch(t)
	fast, whole := applySteps(t, b, url, false), applySteps(t, b, url, true)
	for i := range whole {
		if fast[i].HTML != whole[i].HTML || !slices.Equal(fast[i].Marks, whole[i].Marks) {
			t.Fatalf("step %d differs from the whole morph:\n got  %s %v\n want %s %v", i, fast[i].HTML, fast[i].Marks, whole[i].HTML, whole[i].Marks)
		}
	}
	// every patch after the first is applied row by row
	if last := fast[len(fast)-1].Scoped; last < 9 {
		t.Fatalf("%d of the patches applied row by row", last)
	}
}

// TestBrowserApplySmallPatchFlat: a patch changing one row of a long
// keyed loop costs the browser about that row, not the rows on the page:
// on 4,000 rows an edit, a removal and a swap each apply in under a tenth
// of what morphing the container whole (live.js without its frame-scoped
// morph) takes for the same patch. What is left grows with the rows only
// through the patch's bookkeeping (the loop's items, a reorder's order),
// a fraction of a microsecond a row; the growth from 500 rows is logged.
// The apply is the patch's parse to vk:patched (seen first, on window):
// the morph and the bindings, not vk:patched's page-wide listeners.
func TestBrowserApplySmallPatchFlat(t *testing.T) {
	b := chrome.Launch(t)
	type took struct{ Apply, All float64 }
	// measure is the best of runs
	measure := func(p *chrome.Page, click string, runs int) took {
		best := took{-1, -1}
		for range runs {
			var ms took
			if err := p.Eval(fmt.Sprintf(`new Promise((done) => {
	const s = __wire.sockets[__wire.sockets.length - 1], parse = JSON.parse;
	let at, patched;
	JSON.parse = (x, ...a) => { if (at === undefined && typeof x === "string" && x.includes('"patch"')) at = performance.now(); return parse(x, ...a); };
	window.addEventListener("vk:patched", () => { patched = performance.now(); }, { capture: true, once: true });
	s.addEventListener("message", () => { JSON.parse = parse; const end = performance.now(); done({ Apply: (patched || end) - at, All: end - at }); }, { once: true });
	%s;
})`, click), &ms); err != nil {
				t.Fatal(err)
			}
			if best.Apply < 0 || ms.Apply < best.Apply {
				best.Apply = ms.Apply
			}
			if best.All < 0 || ms.All < best.All {
				best.All = ms.All
			}
		}
		return best
	}
	ops := []struct{ name, click string }{
		{"edit", `document.querySelector("#rows tr:nth-child(5) a.edit").click()`},
		{"remove", `document.querySelector("#rows tr:nth-child(4) a.remove").click()`},
		{"swap", `document.getElementById("swap").click()`},
	}
	open := func(n int) *chrome.Page {
		p := b.Open(applyServer(t, n))
		p.Wait(`document.documentElement.dataset.vkProtocol === "2"`)
		p.Eval(`window.__vkVerify = false`, nil) // verify mode morphs the whole page again: what this measures not to happen
		p.Eval(`document.querySelector("#rows tr:nth-child(2) a.lbl").click()`, nil)
		measure(p, `document.querySelector("#rows tr:nth-child(1) a.edit").click()`, 1) // maps the rows to their items
		return p
	}
	small, large := map[string]took{}, map[string]took{}
	p := open(500)
	for _, op := range ops {
		small[op.name] = measure(p, op.click, 5)
	}
	p = open(4000)
	for _, op := range ops {
		large[op.name] = measure(p, op.click, 5)
	}
	p.Eval(`window.__vkNoScope = true`, nil)
	for _, op := range ops {
		whole := measure(p, op.click, 3)
		s, l := small[op.name], large[op.name]
		t.Logf("%s: apply 500 rows %.1fms, 4,000 rows %.1fms (%.1f×), whole on 4,000 %.1fms; with vk:patched's listeners %.1fms, %.1fms",
			op.name, s.Apply, l.Apply, l.Apply/max(s.Apply, 0.1), whole.Apply, s.All, l.All)
		if l.Apply > whole.Apply/10 {
			t.Errorf("%s of one row on 4,000 rows took %.1fms to apply, whole %.1fms: it costs the rows on the page", op.name, l.Apply, whole.Apply)
		}
	}
}
