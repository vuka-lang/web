package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type routeCode string

func (c routeCode) MarshalText() ([]byte, error) { return []byte("c-" + string(c)), nil }

func namedApp(t *testing.T) *App {
	t.Helper()
	ok := func() string { return "ok" }
	a := New(Only("none"))
	a.Get("/users", ok, RouteName("users"))
	a.Get("/users/{user}/edit", func() string { return "edit" }, RouteName("users.edit"))
	a.Post("/users/{user}/edit", func() string { return "post" }, RouteName("users.edit"))
	a.Get("/files/{path...}", ok, RouteName("files"))
	a.Get("/codes/{code}", ok, RouteName("codes"))
	a.Get("/", ok, RouteName("index"))
	if err := a.Build(); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRoutePaths(t *testing.T) {
	a := namedApp(t)
	lastApp.Store(a)
	ctx := context.Background()
	cases := []struct {
		name string
		args []any
		want string
	}{
		{"users", nil, "/users"},
		{"index", nil, "/"},
		{"users.edit", []any{int64(7)}, "/users/7/edit"},
		{"users.edit", []any{uint8(7)}, "/users/7/edit"},
		{"users.edit", []any{"a b"}, "/users/a%20b/edit"},
		{"files", []any{Rest("a/b c")}, "/files/a/b%20c"},
		{"codes", []any{routeCode("x")}, "/codes/c-x"},
		{"users", []any{RouteQuery{"page": 2, "q": "", "tag": []string{"a", "b"}}}, "/users?page=2&tag=a&tag=b"},
		{"users", []any{RouteQuery{"q": ""}}, "/users"},
	}
	for _, c := range cases {
		if got := RoutePath(ctx, c.name, c.args...); got != c.want {
			t.Errorf("RoutePath(%q, %v) = %q, want %q", c.name, c.args, got, c.want)
		}
		if got := Route(ctx, c.name, c.args...); got != c.want {
			t.Errorf("Route(%q, %v) = %q without a request, want %q", c.name, c.args, got, c.want)
		}
	}
}

func TestRouteMountPrefix(t *testing.T) {
	a := New(Only("none"))
	var got []string
	a.Get("/users/{user}/edit", func(ctx context.Context) string {
		got = append(got, Route(ctx, "users.edit", 7), RouteURL(ctx, "users.edit", 7), RoutePath(ctx, "users.edit", 7), MountPath(ctx, "/users/7/edit"))
		return "ok"
	}, RouteName("users.edit"))
	if err := a.Build(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.StripPrefix("/gateway", a.Handler()))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/gateway/users/7/edit")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	want := []string{"/gateway/users/7/edit", "/gateway/users/7/edit", "/users/7/edit", "/gateway/users/7/edit"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("under http.StripPrefix: %q, want %q", got, want)
	}
}

func TestRouteMistakes(t *testing.T) {
	a := namedApp(t)
	lastApp.Store(a)
	ctx := context.Background()
	panics := func(f func()) (msg string) {
		defer func() {
			if r := recover(); r != nil {
				msg = r.(error).Error()
			}
		}()
		f()
		return ""
	}
	if msg := panics(func() { Route(ctx, "user.edit", 7) }); !strings.Contains(msg, `did you mean "users.edit"`) {
		t.Errorf("an unknown name in a test: %q, want a panic naming users.edit", msg)
	}
	if msg := panics(func() { Route(ctx, "users.edit") }); !strings.Contains(msg, "takes 1 path parameters (user), given 0") {
		t.Errorf("a missing parameter in a test: %q", msg)
	}
	if got := RouteURL(ctx, "user.edit", 7); got != "#" {
		t.Errorf("RouteURL of an unknown name = %q, want #", got)
	}
}

func TestRouteNameTwice(t *testing.T) {
	a := New(Only("none"))
	a.Get("/a", func() string { return "" }, RouteName("x"))
	a.Get("/b", func() string { return "" }, RouteName("x"))
	if err := a.Build(); err == nil || !strings.Contains(err.Error(), `the route name "x"`) {
		t.Errorf("one name for two paths: %v", err)
	}
}

// TestRegisteredRouteNames: with no app built, the names RegisterRouteNames
// gives link as the app's would, with the same mistakes; a built app's
// own name wins.
func TestRegisteredRouteNames(t *testing.T) {
	prev := lastApp.Swap(nil)
	t.Cleanup(func() {
		lastApp.Store(prev)
		fallbackMu.Lock()
		fallbackNames = nil
		fallbackMu.Unlock()
	})
	ctx := context.Background()
	if got := RouteURL(ctx, "orders.view", 3); got != "#" {
		t.Errorf("RouteURL with nothing registered = %q, want #", got)
	}
	RegisterRouteNames(map[string]string{"orders.view": "/orders/{order}", "users": "/people"})
	if got, want := RouteURL(ctx, "orders.view", 3, RouteQuery{"tab": "items"}), "/orders/3?tab=items"; got != want {
		t.Errorf("RouteURL = %q, want %q", got, want)
	}
	if got, want := Route(ctx, "users"), "/people"; got != want {
		t.Errorf("Route = %q, want %q", got, want)
	}
	panics := func(f func()) (msg string) {
		defer func() {
			if r := recover(); r != nil {
				msg = r.(error).Error()
			}
		}()
		f()
		return ""
	}
	if msg := panics(func() { Route(ctx, "order.view", 3) }); !strings.Contains(msg, `did you mean "orders.view"`) {
		t.Errorf("an unknown name: %q, want a panic naming orders.view", msg)
	}
	if msg := panics(func() { Route(ctx, "orders.view") }); !strings.Contains(msg, "takes 1 path parameters (order), given 0") {
		t.Errorf("a missing parameter: %q", msg)
	}
	lastApp.Store(namedApp(t))
	if got, want := Route(ctx, "users"), "/users"; got != want {
		t.Errorf("Route with an app built = %q, want the app's %q", got, want)
	}
	if got, want := Route(ctx, "orders.view", 3), "/orders/3"; got != want {
		t.Errorf("Route of a name the app lacks = %q, want %q", got, want)
	}
}
