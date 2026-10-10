package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/vuka-lang/vuka"
)

var line atomic.Int64

// decl describes fn the way the generated code of a decorated function does.
func decl(pkg, name string, fn any, params ...string) *vuka.Decl {
	return vuka.FuncDecl(vuka.Decl{Name: "main." + name, Pkg: pkg, File: "t.vuka", Line: int(line.Add(1)), Func: fn}, params...)
}

func pkgOf(t *testing.T) string { return "test/" + t.Name() }

func do(t *testing.T, h http.Handler, method, target, body string, header ...string) (int, string, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), rec.Header()
}

func expect(t *testing.T, h http.Handler, method, target, body string, code int, want string, header ...string) {
	t.Helper()
	got, b, _ := do(t, h, method, target, body, header...)
	if got != code || !strings.Contains(b, want) {
		t.Errorf("%s %s: %d %q; want %d containing %q", method, target, got, b, code, want)
	}
}

type Pet struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type Store struct{ pets map[int]Pet }

func NewStore() *Store { return &Store{pets: map[int]Pet{1: {1, "Rex"}, 2: {2, "Tom"}}} }

func (s *Store) Find(id int) vuka.Result[Pet] {
	if p, ok := s.pets[id]; ok {
		return vuka.Ok(p)
	}
	return vuka.Err[Pet](NotFound)
}

type Level int

func (l *Level) UnmarshalText(b []byte) error {
	switch string(b) {
	case "low":
		*l = 1
	case "high":
		*l = 2
	default:
		return fmt.Errorf("unknown level %q", b)
	}
	return nil
}

func TestBindScalars(t *testing.T) {
	p := pkgOf(t)
	Get("/pets/{id}")(decl(p, "show", func(id int, verbose bool, tags []string, level Level, limit *int) string {
		l := "nil"
		if limit != nil {
			l = fmt.Sprint(*limit)
		}
		return fmt.Sprintln(id, verbose, tags, level, l)
	}, "id", "verbose", "tags", "level", "limit"))
	Get("/files/{path...}")(decl(p, "file", func(path string) string { return path }, "path"))
	h := New(Only(p)).Handler()
	expect(t, h, "GET", "/pets/7?verbose=true&tags=a&tags=b&level=high&limit=3", "", 200, `"7 true [a b] 2 3\n"`)
	expect(t, h, "GET", "/pets/7", "", 200, `"7 false [] 0 nil\n"`)
	expect(t, h, "GET", "/pets/x", "", 400, `id: \"x\" is not an integer`)
	expect(t, h, "GET", "/pets/1?level=mid", "", 400, `unknown level`)
	expect(t, h, "GET", "/files/a/b.txt", "", 200, `"a/b.txt"`)
}

type NewPet struct {
	Name  string   `json:"name" validate:"required"`
	Age   int      `form:"years"`
	Tags  []string `json:"tags"`
	Owner string
}

func TestBindStruct(t *testing.T) {
	p := pkgOf(t)
	Post("/pets")(decl(p, "create", func(in NewPet) NewPet { return in }, "in"))
	Put("/owners/{owner}/pets")(decl(p, "put", func(in *NewPet, r *http.Request, ctx context.Context) string {
		return fmt.Sprint(in.Name, " ", in.Owner, " ", r.Method, " ", Request(ctx) == r)
	}, "in", "r", "ctx"))
	Get("/search")(decl(p, "search", func(q NewPet) NewPet { return q }, "q"))
	h := New(Only(p)).Handler()
	expect(t, h, "POST", "/pets", `{"name":"Rex","tags":["a"]}`, 200, `"name":"Rex","Age":0,"tags":["a"]`, "Content-Type", "application/json")
	expect(t, h, "POST", "/pets", `name=Tom&years=3&tags=x&tags=y&owner=ann`, 200, `"name":"Tom","Age":3,"tags":["x","y"],"Owner":"ann"`,
		"Content-Type", "application/x-www-form-urlencoded")
	expect(t, h, "POST", "/pets", `{"name":`, 400, "invalid JSON", "Content-Type", "application/json")
	expect(t, h, "POST", "/pets", `{}`, 400, "name is required", "Content-Type", "application/json")
	expect(t, h, "POST", "/pets", `name=a&years=old`, 400, `years: \"old\" is not an integer`, "Content-Type", "application/x-www-form-urlencoded")
	expect(t, h, "PUT", "/owners/bob/pets", `{"name":"Rex","Owner":"evil"}`, 200, `"Rex bob PUT true"`, "Content-Type", "application/json")
	expect(t, h, "GET", "/search?NAME=z", "", 200, `"name":"z"`)
}

type Page struct {
	Title string
	Pets  []Pet
}

func TestResults(t *testing.T) {
	p := pkgOf(t)
	s := NewStore()
	Get("/none")(decl(p, "none", func() {}))
	Get("/err")(decl(p, "err", func(fail bool) error {
		if fail {
			return BadRequest("nope")
		}
		return nil
	}, "fail"))
	Get("/pair/{id}")(decl(p, "pair", func(id int) (Pet, error) { return s.Find(id).Get() }, "id"))
	Get("/result/{id}")(decl(p, "result", func(id int) vuka.Result[Pet] { return s.Find(id) }, "id"))
	Get("/node")(decl(p, "node", func(r *http.Request) vuka.Node { return vuka.El("h1", nil, vuka.Text("Hi "+r.URL.Query().Get("n"))) }, "r"))
	Get("/node-err")(decl(p, "nodeErr", func() (vuka.Node, error) { return nil, Forbidden }))
	Get("/go")(decl(p, "goto", func() Redirect { return "/there" }))
	Post("/go")(decl(p, "post", func() (Pet, error) { return Pet{}, Redirect("/pets/1") }))
	Get("/maybe")(decl(p, "maybe", func(away bool) vuka.Node {
		if away {
			return Redirect("/away")
		}
		return vuka.Text("here")
	}, "away"))
	Get("/boom")(decl(p, "boom", func() (int, error) { return 0, errors.New("db password leaked") }))
	Get("/panic")(decl(p, "panic", func() int { panic("x") }))
	Get("/wrapped")(decl(p, "wrapped", func() error { return fmt.Errorf("loading: %w", NotFound) }))
	Get("/custom")(decl(p, "custom", func() error { return Status(http.StatusTeapot, "short and %s", "stout") }))
	h := New(Only(p)).Handler()
	expect(t, h, "GET", "/none", "", 204, "")
	expect(t, h, "GET", "/err", "", 204, "")
	expect(t, h, "GET", "/err?fail=1", "", 400, `{"error":"nope","status":400}`)
	expect(t, h, "GET", "/pair/1", "", 200, `{"id":1,"name":"Rex"}`)
	expect(t, h, "GET", "/pair/9", "", 404, `"error":"not found"`)
	expect(t, h, "GET", "/result/2", "", 200, `{"id":2,"name":"Tom"}`)
	expect(t, h, "GET", "/result/9", "", 404, `"status":404`)
	expect(t, h, "GET", "/node?n=%3Cb%3E", "", 200, `<h1>Hi &lt;b&gt;</h1>`)
	expect(t, h, "GET", "/node-err", "", 403, `<h1>Forbidden</h1>`)
	expect(t, h, "GET", "/maybe", "", 200, "here")
	expect(t, h, "GET", "/boom", "", 500, `"error":"Internal Server Error"`)
	expect(t, h, "GET", "/panic", "", 500, `Internal Server Error`)
	expect(t, h, "GET", "/wrapped", "", 404, `"error":"not found"`)
	expect(t, h, "GET", "/custom", "", 418, `short and stout`)
	for _, c := range []struct {
		method, target, to string
		code               int
	}{{"GET", "/go", "/there", 302}, {"POST", "/go", "/pets/1", 303}, {"GET", "/maybe?away=1", "/away", 302}} {
		code, _, hd := do(t, h, c.method, c.target, "")
		if code != c.code || hd.Get("Location") != c.to {
			t.Errorf("%s %s: %d to %q; want %d to %q", c.method, c.target, code, hd.Get("Location"), c.code, c.to)
		}
	}
	dev := New(Only(p), Dev()).Handler()
	expect(t, dev, "GET", "/boom", "", 500, "db password leaked")
	expect(t, dev, "GET", "/panic", "", 500, "panic: x")
}

func TestHTMLView(t *testing.T) {
	p := pkgOf(t)
	f := vuka.FileOf(p+":pets.html", `<h1>{{.Title}}</h1>{{range .Pets}}<li>{{.Name}}</li>{{end}}{{define "count"}}{{len .Pets}} pets{{end}}`)
	pets := func() Page { return Page{"All <pets>", []Pet{{1, "Rex"}, {2, "Tom"}}} }
	d := decl(p, "list", pets)
	Template(f)(d) // before the route: order doesn't matter
	Get("/pets")(d)
	d = decl(p, "count", pets)
	Get("/count")(d)
	Template(f, "count")(d)
	d = decl(p, "missing", func() (Page, error) { return Page{}, NotFound })
	Get("/missing")(d)
	Template(f)(d)
	h := New(Only(p)).Handler()
	expect(t, h, "GET", "/pets", "", 200, `<h1>All &lt;pets&gt;</h1><li>Rex</li><li>Tom</li>`)
	expect(t, h, "GET", "/count", "", 200, `2 pets`)
	expect(t, h, "GET", "/missing", "", 404, `<h1>Not Found</h1>`)
}

func templ(name string) func(...any) vuka.Node {
	return func(args ...any) vuka.Node {
		return vuka.NodeFunc(func(ctx context.Context, w io.Writer) error {
			_, err := fmt.Fprint(w, name, args)
			return err
		})
	}
}

func TestTemplView(t *testing.T) {
	p := pkgOf(t)
	key := p + ":pets.templ"
	f := vuka.FileOf(key, `package main

templ PetList(title string, pets []Pet) {
	<h1>{ title }</h1>
}

templ Card(pet Pet) { <b>{ pet.Name }</b> }

templ (x T) Ignored() {}

templ footer() {}
`)
	vuka.RegisterTempl(key, map[string]any{
		"PetList": func(title string, pets []Pet) vuka.Node { return templ("list")(title, len(pets)) },
		"Card":    func(pet Pet) vuka.Node { return templ("card")(pet.Name) },
		"footer":  func() vuka.Node { return templ("footer")() },
	})
	page := Page{"Pets", []Pet{{1, "Rex"}}}
	route := func(path string, fn any, name ...string) {
		d := decl(p, path, fn)
		Get(path)(d)
		Template(f, name...)(d)
	}
	route("/list", func() Page { return page }, "PetList")
	route("/list-ptr", func() *Page { return &page }, "PetList")
	route("/card", func() vuka.Result[Pet] { return vuka.Ok(Pet{1, "Rex"}) }, "Card")
	route("/footer", func() int { return 1 }, "footer")
	h := New(Only(p)).Handler()
	expect(t, h, "GET", "/list", "", 200, "list[Pets 1]")
	expect(t, h, "GET", "/list-ptr", "", 200, "list[Pets 1]")
	expect(t, h, "GET", "/card", "", 200, "card[Rex]")
	expect(t, h, "GET", "/footer", "", 200, "footer[]")
	_, _, hd := do(t, h, "GET", "/card", "")
	if ct := hd.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type %q", ct)
	}

	bad := pkgOf(t) + "/bad"
	for path, c := range map[string]struct {
		fn   any
		name []string
	}{
		"/ambiguous": {func() Page { return page }, nil},
		"/nofield":   {func() Pet { return Pet{} }, []string{"PetList"}},
		"/wrongtype": {func() struct{ Title, Pets string } { return struct{ Title, Pets string }{} }, []string{"PetList"}},
		"/noresult":  {func() {}, []string{"Card"}},
		"/nocomp":    {func() Pet { return Pet{} }, []string{"Nope"}},
	} {
		d := decl(bad, path, c.fn)
		Get(path)(d)
		Template(f, c.name...)(d)
	}
	err := New(Only(bad)).Build()
	for _, want := range []string{
		`/ambiguous (main./ambiguous at t.vuka:`,
		`has 2 exported components [Card PetList]; name one`,
		`component PetList: web.Pet has no field for parameter title`,
		`field .Pets is string; parameter pets is []web.Pet`,
		`the view pets.templ needs a result to render`,
		`pets.templ has no component Nope`,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Build error %v; want %q", err, want)
		}
	}
}

type DB struct{ name string }
type Opaque struct{ conn int }
type Repo struct{ db *DB }
type Cache interface{ Get(string) string }
type A struct{ b *B }
type B struct{ a *A }

func TestDI(t *testing.T) {
	p := pkgOf(t)
	built := 0
	Get("/repo")(decl(p, "repo", func(r *Repo, again *Repo) string { return fmt.Sprintln(r.db.name, r == again, built) }, "r", "again"))
	a := New(Only(p), Provide(func(db *DB) *Repo { built++; return &Repo{db} }))
	a.Provide(&DB{"main"})
	h := a.Handler()
	expect(t, h, "GET", "/repo", "", 200, `"main true 1\n"`)
	expect(t, h, "GET", "/repo", "", 200, `"main true 1\n"`)
	if r, err := Resolve[*Repo](a); err != nil || r.db.name != "main" {
		t.Errorf("Resolve: %v %v", r, err)
	}

	failing := New(Only(p), Provide(func() (*DB, error) { return nil, errors.New("no network") }, func(db *DB) *Repo { return &Repo{db} }))
	expect(t, failing.Handler(), "GET", "/repo", "", 500, "Internal Server Error")
	if err := failing.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "no network") {
		t.Errorf("Start: %v", err)
	}

	errOf := func(opts ...Option) string {
		err := New(append([]Option{Only(p)}, opts...)...).Build()
		if err == nil {
			return ""
		}
		return err.Error()
	}
	if e := errOf(Provide(func(db *DB) *Repo { return nil })); !strings.Contains(e, "no provider for *web.DB, needed by ") ||
		!strings.Contains(e, "needed by main.repo at t.vuka:") {
		t.Errorf("missing: %s", e)
	}
	if e := errOf(Provide(func(a *A) *DB { return nil }, func(b *B) *A { return nil }, func(a *A) *B { return nil }, func(db *DB) *Repo { return nil })); !strings.Contains(e, "dependency cycle: *web.A -> *web.B -> *web.A") {
		t.Errorf("cycle: %s", e)
	}
	if e := errOf(Provide(&DB{}, &DB{}, func(db *DB) *Repo { return nil })); !strings.Contains(e, "*web.DB is provided twice") {
		t.Errorf("twice: %s", e)
	}
	if e := errOf(Provide(func() {})); !strings.Contains(e, "must be func(deps…) T") {
		t.Errorf("bad ctor: %s", e)
	}
}

type Life struct {
	name string
	log  *[]string
}

func (l *Life) Start(context.Context) error { *l.log = append(*l.log, "start "+l.name); return nil }
func (l *Life) Stop(context.Context) error  { *l.log = append(*l.log, "stop "+l.name); return nil }

type First struct{ *Life }
type Second struct{ *Life }

func TestLifecycle(t *testing.T) {
	p := pkgOf(t)
	var log []string
	Get("/")(decl(p, "home", func(s *Second) string { return s.name }, "s"))
	a := New(Only(p), Provide(
		func(f *First) *Second { return &Second{&Life{"second", &log}} },
		func() *First { return &First{&Life{"first", &log}} },
	))
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ", "); got != "start first, start second, stop second, stop first" {
		t.Errorf("lifecycle: %s", got)
	}
}

type PetService struct{ db *DB }
type Admin struct{ svc *PetService }

func (a *Admin) Index() string               { return "admin " + a.svc.db.name }
func (a *Admin) Show(id int) (string, error) { return fmt.Sprint("pet ", id), nil }
func (a Admin) Value() string                { return "value " + a.svc.db.name }

func TestServicesAndControllers(t *testing.T) {
	const p = "github.com/vuka-lang/web" // the types' package
	Service(vuka.TypeWith[PetService]("web.PetService", func(db *DB) *PetService { return &PetService{db} }))
	Controller("/admin/pets")(vuka.TypeWith[Admin]("web.Admin", func(svc *PetService) *Admin { return &Admin{svc} }))
	method := func(name string, fn any, params ...string) *vuka.Decl {
		return vuka.MethodDecl(vuka.Decl{Name: "Admin." + name, Pkg: p, File: "t.vuka", Line: int(line.Add(1)), Func: fn}, params...)
	}
	Get("/")(method("Index", (*Admin).Index))
	Get("/{id}")(method("Show", (*Admin).Show, "id"))
	Get("/value")(method("Value", Admin.Value))
	Get("/svc")(decl(p, "svc", func(s *PetService) string { return s.db.name }, "s"))
	h := New(Only(p), Provide(&DB{"pg"})).Handler()
	expect(t, h, "GET", "/admin/pets", "", 200, `"admin pg"`)
	expect(t, h, "GET", "/admin/pets/3", "", 200, `"pet 3"`)
	expect(t, h, "GET", "/admin/pets/value", "", 200, `"value pg"`)
	expect(t, h, "GET", "/svc", "", 200, `"pg"`)
	expect(t, h, "GET", "/admin/pets/", "", 404, "")

	err := New(Only(p)).Build()
	if err == nil || !strings.Contains(err.Error(), "no provider for *web.DB, needed by service web.PetService, needed by controller web.Admin, needed by Admin.Index at t.vuka:") {
		t.Errorf("Build: %v", err)
	}
}

func TestMiddleware(t *testing.T) {
	p := pkgOf(t)
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	d := decl(p, "x", func() string { order = append(order, "handler"); return "ok" })
	Use(mw("route1"), mw("route2"))(d)
	Get("/x")(d)
	Use(mw("route3"))(d)
	Get("/y")(decl(p, "y", func() string { return "y" }))
	a := New(Only(p), Use(mw("app1")))
	a.Use(mw("app2"))
	h := a.Handler()
	expect(t, h, "GET", "/x", "", 200, "ok")
	if got := strings.Join(order, " "); got != "app1 app2 route1 route2 route3 handler" {
		t.Errorf("order: %s", got)
	}
	order = nil
	expect(t, h, "GET", "/y", "", 200, "y")
	expect(t, h, "GET", "/nowhere", "", 404, "")
	if got := strings.Join(order, " "); got != "app1 app2 app1 app2" {
		t.Errorf("order: %s", got)
	}
}

// segRouter is a minimal Router, matching whole segments, to prove the seam.
type segRouter struct{ routes []segRoute }

type segRoute struct {
	method string
	segs   []string
	h      http.Handler
}

type paramsKey struct{}

func (s *segRouter) Handle(method, pattern string, h http.Handler) {
	s.routes = append(s.routes, segRoute{method, strings.Split(strings.Trim(pattern, "/"), "/"), h})
}

func (s *segRouter) Param(r *http.Request, name string) string {
	m, _ := r.Context().Value(paramsKey{}).(map[string]string)
	return m[name]
}

func (s *segRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
next:
	for _, rt := range s.routes {
		if rt.method != r.Method {
			continue
		}
		params := map[string]string{}
		for i, seg := range rt.segs {
			switch {
			case strings.HasSuffix(seg, "...}"):
				params[seg[1:len(seg)-4]] = strings.Join(segs[i:], "/")
				rt.h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), paramsKey{}, params)))
				return
			case i >= len(segs):
				continue next
			case strings.HasPrefix(seg, "{"):
				params[seg[1:len(seg)-1]] = segs[i]
			case seg != segs[i]:
				continue next
			}
		}
		if len(segs) == len(rt.segs) {
			rt.h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), paramsKey{}, params)))
			return
		}
	}
	http.NotFound(w, r)
}

func TestRouterSeam(t *testing.T) {
	p := pkgOf(t)
	Get("/pets/{id}")(decl(p, "show", func(id int, in struct{ ID int }) string { return fmt.Sprint(id, in.ID) }, "id", "in"))
	a := New(Only(p), WithRouter(&segRouter{}))
	a.Static("/assets", fstest.MapFS{"app.css": {Data: []byte("body{}")}})
	h := a.Handler()
	expect(t, h, "GET", "/pets/5", "", 200, `"5 5"`)
	expect(t, h, "GET", "/assets/app.css", "", 200, "body{}")
	expect(t, h, "GET", "/pets", "", 404, "")
}

func TestStatic(t *testing.T) {
	a := New(Only(pkgOf(t)))
	a.Static("assets/", fstest.MapFS{"css/app.css": {Data: []byte("body{}")}})
	h := a.Handler()
	expect(t, h, "GET", "/assets/css/app.css", "", 200, "body{}")
	expect(t, h, "GET", "/assets/none.css", "", 404, "")
}

type Query struct {
	Q    string
	Page int
}

func TestPlainGo(t *testing.T) {
	a := New(Only(pkgOf(t)), Provide(NewStore))
	a.Get("/pets/{id}", func(s *Store, id int) (Pet, error) { return s.Find(id).Get() }, Params("", "id"))
	a.Get("/search", func(q Query, r *http.Request) string { return fmt.Sprint(q.Q, q.Page, r.Method) })
	a.Post("/pets", func(in NewPet) (Pet, error) { return Pet{3, in.Name}, nil })
	a.Get("/page", func() Page { return Page{Title: "T"} }, View(vuka.FileOf("plain:p.html", "<p>{{.Title}}</p>")))
	a.Delete("/pets/{id}", func(id int) error { return nil }, Params("id"), Wrap(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Wrapped", "1"); h.ServeHTTP(w, r) })
	}))
	h := a.Handler()
	expect(t, h, "GET", "/pets/2", "", 200, `"name":"Tom"`)
	expect(t, h, "GET", "/search?q=cat&page=2", "", 200, `"cat2GET"`)
	expect(t, h, "POST", "/pets", `{"name":"Bo"}`, 200, `{"id":3,"name":"Bo"}`, "Content-Type", "application/json")
	expect(t, h, "GET", "/page", "", 200, "<p>T</p>")
	if code, _, hd := do(t, h, "DELETE", "/pets/1", ""); code != 204 || hd.Get("X-Wrapped") != "1" {
		t.Errorf("DELETE: %d %v", code, hd)
	}

	err := New(Only(pkgOf(t))).Get("/x/{id}", func(id int) int { return id }).Build()
	if err == nil || !strings.Contains(err.Error(), "parameter #1 int: a scalar parameter binds by name") {
		t.Errorf("unnamed scalar: %v", err)
	}
}

func TestStartupErrors(t *testing.T) {
	p := pkgOf(t)
	Get("/a")(decl(p, "a", func(n int) int { return n }))
	Get("/b")(decl(p, "b", func(x, y NewPet) int { return 0 }, "x", "y"))
	Get("/c")(decl(p, "c", func(ch chan int) int { return 0 }, "ch"))
	Get("/d")(decl(p, "d", func() (int, int) { return 0, 0 }))
	Get("/e")(decl(p, "e", func(c Cache) int { return 0 }, "c"))
	Get("/a")(decl(p, "a2", func() int { return 0 }))
	Get("/g")(decl(p, "g", func(m *Opaque) int { return 0 }, "m"))
	Get("/f")(vuka.MethodDecl(vuka.Decl{Name: "Repo.F", Pkg: p, File: "t.vuka", Line: 99, Func: func(*Repo) int { return 0 }}))
	err := New(Only(p)).Build()
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"GET /a (main.a at t.vuka:",
		"parameter #1 int: a scalar parameter binds by name",
		"parameter y web.NewPet: a handler binds one struct from the request, and this is the second",
		"parameter ch chan int: can't be bound from a request",
		"results func() (int, int): a handler returns nothing, error, T, (T, error) or Result[T]",
		"parameter c web.Cache: no provider for web.Cache",
		"GET /a is declared twice: by main.a at t.vuka:",
		"parameter m *web.Opaque: no provider for *web.Opaque",
		"the receiver *web.Repo is not a @web.Controller or a provided type",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("Handler didn't panic")
		}
	}()
	New(Only(p)).Handler()
}

func TestSnapshot(t *testing.T) {
	p := pkgOf(t)
	Get("/one")(decl(p, "one", func() int { return 1 }))
	first := New(Only(p))
	Get("/two")(decl(p, "two", func() int { return 2 }))
	expect(t, first.Handler(), "GET", "/two", "", 404, "")
	expect(t, New(Only(p)).Handler(), "GET", "/two", "", 200, "2")
}

func TestDecoratorSeesRequest(t *testing.T) {
	p := pkgOf(t)
	guard := func(c *vuka.Call) {
		if Request(c.Context()).Header.Get("X-User") == "" {
			c.SetErr(Unauthorized)
			return
		}
		c.Next()
	}
	info := &vuka.Func{Name: "main.secret", ErrIndex: 1, CtxIndex: 0, Zero: func() []any { return []any{"", error(nil)} }}
	inner := func(ctx context.Context) (string, error) {
		return "secret for " + Request(ctx).Header.Get("X-User"), nil
	}
	decorated := func(ctx context.Context) (string, error) {
		c := vuka.NewCall(info, []vuka.Decorator{guard}, nil, []any{ctx}, func(c *vuka.Call) []any {
			s, err := inner(vuka.As[context.Context](c.Args[0]))
			return []any{s, err}
		})
		c.Run()
		return vuka.As[string](c.Results[0]), vuka.As[error](c.Results[1])
	}
	Get("/secret")(decl(p, "secret", decorated, "ctx"))
	h := New(Only(p)).Handler()
	expect(t, h, "GET", "/secret", "", 401, "unauthorized")
	expect(t, h, "GET", "/secret", "", 200, "secret for ann", "X-User", "ann")
}
