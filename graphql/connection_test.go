package graphql

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func names(c Connection[string]) string {
	var out []string
	for _, e := range c.Edges {
		out = append(out, e.Node)
	}
	return fmt.Sprintf("%v next=%v prev=%v", out, c.PageInfo.HasNextPage, c.PageInfo.HasPreviousPage)
}

func TestPaginateSlice(t *testing.T) {
	ctx := context.Background()
	rows := Slice([]string{"a", "b", "c", "d", "e"})
	n := func(i int) *int { return &i }
	page, err := Paginate(ctx, rows, Page{First: n(2)})
	if err != nil || names(page) != "[a b] next=true prev=false" {
		t.Fatalf("first 2: %s %v", names(page), err)
	}
	page, _ = Paginate(ctx, rows, Page{First: n(2), After: page.PageInfo.EndCursor})
	if names(page) != "[c d] next=true prev=true" {
		t.Errorf("next: %s", names(page))
	}
	before := page.PageInfo.StartCursor
	page, _ = Paginate(ctx, rows, Page{Last: n(5), Before: before})
	if names(page) != "[a b] next=true prev=false" {
		t.Errorf("last before c: %s", names(page))
	}
	page, _ = Paginate(ctx, rows, Page{})
	if names(page) != "[a b c d e] next=false prev=false" {
		t.Errorf("default size: %s", names(page))
	}
	page, _ = Paginate(ctx, rows, Page{First: n(0)})
	if names(page) != "[] next=false prev=false" || page.Edges == nil {
		t.Errorf("first 0: %s", names(page))
	}
	if n, _ := page.TotalCount(ctx); n != 5 {
		t.Errorf("totalCount %d", n)
	}
	if _, err := Paginate(ctx, rows, Page{First: n(MaxPageSize + 1)}); err == nil || !strings.Contains(err.Error(), "more than a page holds") {
		t.Errorf("over the max: %v", err)
	}
	if strings.Contains(*before, "slice") || page.PageInfo.EndCursor != nil {
		t.Error("cursors are opaque; an empty page has none")
	}
}
