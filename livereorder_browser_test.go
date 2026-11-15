package web

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/web/v2/internal/chrome"
)

// reorderDOM is what a patch did to the rows of the reorder page.
type reorderDOM struct {
	Rows     int     // rows after it
	Kept     int     // rows that are the elements they were
	Inserted int     // rows put in the container (a move is a removal and an insert)
	Removed  int     // rows taken out of it
	Inner    int     // childList changes inside the rows
	Scoped   int     // the container's row-by-row morphs so far (verify mode only)
	Verify   bool    // verify mode is on
	Ms       float64 // the patch's apply, from its parse to the end of the socket's handler
}

// reorderOpen opens the reorder page, its rows mapped by a first patch.
func reorderOpen(b *chrome.Browser, url string) (*chrome.Page, *reorderPage) {
	p := b.Open(url + "/reorder")
	p.Wait(`document.documentElement.dataset.vkProtocol === "2"`)
	clickJS(p, "#tick")
	p.WaitText("#ticks", "1")
	return p, newReorderPage()
}

// reorderClick marks every row, clicks button and waits for the rows of
// model, op applied.
func reorderClick(t *testing.T, p *chrome.Page, model *reorderPage, button string, op func(*reorderPage)) reorderDOM {
	t.Helper()
	op(model)
	var want []string
	for _, r := range model.rows {
		want = append(want, fmt.Sprintf("%d=%s", r.ID, r.Name))
	}
	p.Eval(fmt.Sprintf(`(() => {
	const ul = document.getElementById("rows");
	ul.querySelectorAll("li").forEach((li) => { li.__marker = li.dataset.id; });
	if (window.__reorderWatch) __reorderWatch.disconnect();
	const w = window.__reorder = { ins: 0, rem: 0, inner: 0, want: %q };
	(window.__reorderWatch = new MutationObserver((rs) => rs.forEach((r) => {
		if (r.target === ul) { w.ins += r.addedNodes.length; w.rem += r.removedNodes.length; } else if (r.type === "childList") w.inner++;
	}))).observe(ul, { childList: true, subtree: true });
	const s = __wire.sockets[__wire.sockets.length - 1], parse = JSON.parse;
	JSON.parse = (x, ...a) => { if (w.at === undefined && typeof x === "string" && x.includes('"patch"')) w.at = performance.now(); return parse(x, ...a); };
	s.addEventListener("message", () => { JSON.parse = parse; w.end = performance.now(); }, { once: true });
	document.getElementById(%q).click();
	return 0;
})()`, strings.Join(want, "|"), button), nil)
	shown := `Array.from(document.querySelectorAll("#rows > li"), (li) => li.dataset.id + "=" + li.querySelector(".name").textContent).join("|")`
	p.Wait(shown + ` === __reorder.want`)
	var d reorderDOM
	p.Eval(`(() => {
	const lis = Array.from(document.querySelectorAll("#rows > li")), w = __reorder;
	return { rows: lis.length, kept: lis.filter((li) => li.__marker === li.dataset.id).length, inserted: w.ins, removed: w.rem, inner: w.inner,
		ms: w.end - w.at, scoped: (window.__vkScoped || {}).rows || 0, verify: !!window.__vkVerify };
})()`, &d)
	return d
}

// TestBrowserKeyedReorderMoves: live.js applies a reorder of a keyed loop
// by moving its rows, each the element it was (its expando survives), the
// fewest moves (the rows of a longest run in order stay), row by row (the
// unmoved rows' markup isn't parsed and morphed again); a row changed after
// is patched row by row too.
func TestBrowserKeyedReorderMoves(t *testing.T) {
	srv := httptest.NewServer(reorderApp().Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	for _, tc := range reorderCases {
		t.Run(tc.name, func(t *testing.T) {
			p, model := reorderOpen(b, srv.URL)
			d := reorderClick(t, p, model, tc.button, tc.model)
			t.Logf("%s: %d/%d rows kept, %d moved, %.1fms", tc.name, d.Kept, d.Rows, d.Inserted, d.Ms)
			if d.Rows != reorderRows || d.Kept != reorderRows {
				t.Fatalf("rows rebuilt: %+v", d)
			}
			if d.Inserted != tc.moves || d.Removed != tc.moves || d.Inner != 0 {
				t.Fatalf("%d moves, want %d: %+v", d.Inserted, tc.moves, d)
			}
			if d.Verify && d.Scoped != 1 {
				t.Fatalf("not morphed row by row: %+v", d)
			}
			if tc.moves == 0 {
				return
			}
			d = reorderClick(t, p, model, "edit", (*reorderPage).Edit)
			if d.Kept != reorderRows || d.Inserted != 0 || d.Verify && d.Scoped != 2 {
				t.Fatalf("an edit after the reorder: %+v", d)
			}
		})
	}
}

// TestBrowserKeyedReorderEdgeCases: a moved row whose content changed is
// moved and patched; a reorder moving the row holding the focus morphs the
// container whole, the focus kept.
func TestBrowserKeyedReorderEdgeCases(t *testing.T) {
	srv := httptest.NewServer(reorderApp().Handler())
	defer srv.Close()
	b := chrome.Launch(t)

	p, model := reorderOpen(b, srv.URL)
	d := reorderClick(t, p, model, "moveedit", (*reorderPage).MoveEdit)
	if d.Kept != reorderRows || d.Inserted != 1 || d.Verify && d.Scoped != 1 {
		t.Fatalf("move and edit: %+v", d)
	}

	p, model = reorderOpen(b, srv.URL)
	p.Eval(`document.querySelector('#rows > li:last-child .note').focus()`, nil)
	p.Type(`#rows > li:last-child .note`, "typed")
	d = reorderClick(t, p, model, "reverse", (*reorderPage).Reverse)
	var focus any
	p.Eval(`(() => { const a = document.activeElement; return a.classList.contains("note") && a.value === "typed" && a.closest("li") === document.querySelector("#rows > li:first-child"); })()`, &focus)
	if d.Kept != reorderRows || focus != true {
		t.Fatalf("the focused row moved: %+v, focus %v", d, focus)
	}
}
