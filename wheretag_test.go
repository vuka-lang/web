package web

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestWhereTag(t *testing.T) {
	r := httptest.NewRequest("GET", "/ajira/adverts/view", nil)
	rq := &request{route: &routeSpec{where: "main.AdvertsView at modules/adverts/view.vuka:12"}}
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, rq))

	// Dev names the route; production stays terse.
	if got := (&App{dev: true}).whereTag(r); got != " [main.AdvertsView at modules/adverts/view.vuka:12]" {
		t.Errorf("dev whereTag = %q", got)
	}
	if got := (&App{dev: false}).whereTag(r); got != "" {
		t.Errorf("prod whereTag = %q, want empty", got)
	}

	// No route in context (an error before dispatch) is empty even in dev.
	bare := httptest.NewRequest("GET", "/", nil)
	if got := (&App{dev: true}).whereTag(bare); got != "" {
		t.Errorf("routeless whereTag = %q, want empty", got)
	}
}
