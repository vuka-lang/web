package web

import (
	"context"
	"fmt"
	"html"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2"
)

// bookRow is a row of the paged books list.
type bookRow struct {
	ID    int
	Title string
}

// bookSource is a Source over a fixed catalogue filtered by a query.
type bookSource struct{ q string }

func (s bookSource) all() []bookRow {
	var out []bookRow
	for i := 1; i <= 12; i++ {
		r := bookRow{ID: i, Title: fmt.Sprintf("Book %02d", i)}
		if s.q == "" || strings.Contains(strings.ToLower(r.Title), strings.ToLower(s.q)) {
			out = append(out, r)
		}
	}
	return out
}
func (s bookSource) Count(ctx context.Context) (int, error) { return len(s.all()), nil }
func (s bookSource) Slice(ctx context.Context, limit, offset int) ([]bookRow, error) {
	rows := s.all()
	if offset > len(rows) {
		offset = len(rows)
	}
	rows = rows[offset:]
	if limit < len(rows) {
		rows = rows[:limit]
	}
	return rows, nil
}

// booksLive is a live page that pages a books list with web.Pager and renders
// it with ui.DataTable.
type booksLive struct {
	ui.Live
	pager *Pager[bookRow]
	query string
}

func (b *booksLive) LiveSchema(l *ui.LiveSchema) { l.Mount(b.mount) }
func (b *booksLive) mount(ctx context.Context) error {
	b.pager = NewPager(5, func(ctx context.Context, page, size int) (ui.Page[bookRow], error) {
		return PaginateSource[bookRow](ctx, bookSource{q: b.query}, page, size)
	})
	return b.pager.Load(ctx)
}

func (b *booksLive) Search(ctx context.Context, q string) error {
	b.query = q
	return b.pager.Reload(ctx)
}

func (b *booksLive) Render() ui.Node {
	return ui.DataTable[bookRow]{
		Columns:  []ui.Column[bookRow]{{Header: ui.Text("Title"), Cell: func(r bookRow) ui.Node { return ui.Text(r.Title) }}},
		Page:     b.pager.Result(),
		Search:   b.query,
		OnPage:   b.pager.GoTo,
		OnSearch: b.Search,
		RowKey:   func(r bookRow) any { return r.ID },
	}
}

func pushAfter(t *testing.T, page, marker string) (id, value string) {
	t.Helper()
	u := html.UnescapeString(page)
	i := strings.Index(u, marker)
	if i < 0 {
		t.Fatalf("marker %q not in %s", marker, page)
	}
	m := regexp.MustCompile(`"h":"(c\d+:\d+)"(?:,"value":"([^"]*)")?`).FindStringSubmatch(u[i:])
	if m == nil {
		t.Fatalf("no push after %q in %s", marker, u[i:min(i+200, len(u))])
	}
	return m[1], m[2]
}

func TestLiveDataTablePaging(t *testing.T) {
	app := New(Only("none"))
	app.Live("/books", &booksLive{})
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	c, firstHTML := joined(t, srv, "/books")
	if !strings.Contains(firstHTML, "Book 01") || !strings.Contains(firstHTML, "Book 05") || strings.Contains(firstHTML, "Book 06") {
		t.Fatalf("page 1 should show the first five: %s", firstHTML)
	}
	if !strings.Contains(firstHTML, "of 12") {
		t.Fatalf("count missing: %s", firstHTML)
	}

	// Click the pager's Next button (a server push through web.Pager.GoTo).
	id, value := pushAfter(t, firstHTML, `aria-label="Next page"`)
	send(t, c, map[string]any{"type": "event", "ref": 2, "target": id, "event": "click", "value": value})
	m := must(t, c)
	if len(m.Updates) != 1 {
		t.Fatalf("paging patch: %+v", m)
	}
	if !strings.Contains(m.Updates[0].HTML, "Book 06") || strings.Contains(m.Updates[0].HTML, "Book 01") {
		t.Fatalf("page 2 should show the next five: %s", m.Updates[0].HTML)
	}

	// Search filters the list and resets to page 1.
	sid := handlerOf(t, firstHTML, "input", 0)
	send(t, c, map[string]any{"type": "event", "ref": 3, "target": sid, "event": "input", "value": "Book 1"})
	m = must(t, c)
	// "Book 1" matches Book 10, 11, 12 — three rows.
	if !strings.Contains(m.Updates[0].HTML, "of 3") || !strings.Contains(m.Updates[0].HTML, "Book 10") {
		t.Fatalf("search did not filter to 3: %s", m.Updates[0].HTML)
	}
}

// toastLive is a live page that pushes toasts from server events.
type toastLive struct {
	ui.Live
	msgs []string
}

func (p *toastLive) Add(ctx context.Context) error {
	p.msgs = append(p.msgs, fmt.Sprintf("Saved %d", len(p.msgs)+1))
	return nil
}
func (p *toastLive) Clear(ctx context.Context) error { p.msgs = nil; return nil }

func (p *toastLive) Render() ui.Node {
	var toasts []ui.Node
	for i, m := range p.msgs {
		toasts = append(toasts, ui.Toast{
			ID: fmt.Sprintf("t%d", i), Variant: ui.AlertSuccess, Title: m, Timeout: 4 * time.Second,
		})
	}
	return ui.El("div", nil,
		ui.El("button", []ui.Attr{{Name: "id", Value: "add"}, {Name: "onClick", Value: ui.On(p.Add)}}, ui.Text("Notify")),
		ui.El("button", []ui.Attr{{Name: "id", Value: "clear"}, {Name: "onClick", Value: ui.On(p.Clear)}}, ui.Text("Clear")),
		ui.ToastContainer{Children: ui.Fragment(toasts...)},
	)
}

func TestLiveToastPush(t *testing.T) {
	app := New(Only("none"))
	app.Live("/toasts", &toastLive{})
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	c, firstHTML := joined(t, srv, "/toasts")
	if strings.Contains(firstHTML, "vk-toast ") {
		t.Fatalf("no toasts before any event: %s", firstHTML)
	}
	add := handlerOf(t, firstHTML, "click", 0)
	clear := handlerOf(t, firstHTML, "click", 1)

	send(t, c, map[string]any{"type": "event", "ref": 2, "target": add, "event": "click"})
	m := must(t, c)
	if !strings.Contains(m.Updates[0].HTML, "vk-toast--success") || !strings.Contains(m.Updates[0].HTML, "Saved 1") {
		t.Fatalf("add did not push a toast: %s", m.Updates[0].HTML)
	}
	if !strings.Contains(m.Updates[0].HTML, "--vk-toast-life:4000ms") {
		t.Fatalf("toast missing auto-dismiss life: %s", m.Updates[0].HTML)
	}

	send(t, c, map[string]any{"type": "event", "ref": 3, "target": add, "event": "click"})
	m = must(t, c)
	if strings.Count(m.Updates[0].HTML, "vk-toast ") != 2 {
		t.Fatalf("second add should stack two toasts: %s", m.Updates[0].HTML)
	}

	send(t, c, map[string]any{"type": "event", "ref": 4, "target": clear, "event": "click"})
	m = must(t, c)
	if strings.Contains(m.Updates[0].HTML, "vk-toast ") {
		t.Fatalf("clear should remove the toasts: %s", m.Updates[0].HTML)
	}
}
