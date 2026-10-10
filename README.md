# web

A web framework for [Vuka](https://github.com/vuka-lang/vuka): routes, views,
services and controllers are declared with decorators, parameters bind by
name, and `web.New()` gathers everything into an app.

```vuka
package main

import "github.com/vuka-lang/web"

@web.Service
type PetStore struct{ db *DB }            // built from its fields, a lazy singleton

@web.Get("/pets")
@web.Template("pets.templ")               // a templ component renders the result
func ListPets(store *PetStore) Result[PetList] { … }

@web.Get("/pets/{id}")
@web.Template("views/pet.html")           // html/template, the result as data
func ShowPet(id int, store *PetStore) Result[Pet] { return store.Find(id) }

@web.Get("/")
func Home(r *http.Request) vuka.Node {    // JSX
	return <Layout title="Home"><h1>Hi</h1></Layout>
}

@web.Post("/api/pets")
func CreatePet(in NewPet, store *PetStore) Result[Pet] { … }   // JSON in, JSON out

@web.Controller("/admin/pets")
type PetAdmin struct{ store *PetStore }

@web.Get("/")
func (a *PetAdmin) Index() Result[[]Pet] { return a.store.All() }

func main() {
	app := web.New()       // every route, service and controller declared at init
	app.Provide(OpenDB)    // more constructors, fx-style
	app.Run(":8080")       // graceful shutdown on SIGINT/SIGTERM
}
```

A complete app is in [`examples/pets`](examples/pets).

```
go get github.com/vuka-lang/web
```

## Declaring

| | |
|---|---|
| `@web.Get(path)`, `Post`, `Put`, `Patch`, `Delete`, `Handle(method, path)` | a route; works on functions and controller methods |
| `@web.Template(file [, name])` | render the result with a view (below); before or after the route decorator |
| `@web.Use(mw…)` | HTTP middleware for this route, `func(http.Handler) http.Handler` |
| `@web.Service` | a struct becomes a dependency |
| `@web.Controller(prefix)` | a struct becomes a dependency whose route methods are served under `prefix` |

Paths use `{name}` for a segment and `{name...}` for the rest of the path, and
match exactly (`/pets` doesn't match `/pets/`). Under a controller, `"/"` is
the prefix itself.

Route decorators are Vuka *declarers* (`func(*vuka.Decl)`): they run once at
init and record the function with its parameter names. Other decorators on
the same function still wrap it — `@audited` below runs on every call.

## The app

`web.New(opts…)` takes a snapshot of what has been declared, so several apps
(in tests, say) each get their own container and singletons.

| Option | |
|---|---|
| `web.Provide(ctors…)` | constructors for the container (also `app.Provide`) |
| `web.Use(mw…)` | app-wide middleware (also `app.Use`); runs for every request, 404s included |
| `web.WithRouter(r)` | another router (below) |
| `web.Only(pkgs…)` | take only the declarations of these packages (import paths; `"main"`) |
| `web.Dev()` | show internal error messages; also `VUKA_ENV=development` |
| `web.OnError(f)` | replace the error page / JSON error body |

`app.Build()` resolves everything — every parameter of every route, every
result and view, the whole dependency graph — and returns all problems at
once, each naming the route, its source position and the parameter:

```
web: GET /pets (main.ListPets at pages.vuka:24): component PetsPage: main.PetList has no field for parameter title
web: GET /admin (main.Admin at admin.vuka:9): parameter db *sql.DB: no provider for *sql.DB, needed by main.Admin at admin.vuka:9
web: dependency cycle: *main.A -> *main.B -> *main.A
```

`app.Handler()` (an `http.Handler` to mount anywhere; it panics when Build
fails), `app.Start(ctx)` and `app.Run(addr)` call Build first, so nothing is
served from a broken app. Register everything before the first of them.
`app.Run("")` listens on `:$PORT`, else `:8080`. `web.Resolve[T](app)` returns
a dependency, for tests and `main`.

## Parameters

Each parameter binds by its type and name:

| parameter | from |
|---|---|
| `*http.Request`, `http.ResponseWriter`, `context.Context` | the request (the context is the request's) |
| a provided type | the container |
| a scalar: `string`, ints, floats, `bool`, a `encoding.TextUnmarshaler`; `*T` of one (nil when absent), `[]T` (repeated) | the path segment of the same name, else the query parameter `?name=`; absent → zero value |
| one struct, or pointer to a struct that isn't provided (with exported fields; else it is a missing dependency) | the body: JSON for `application/json`, else the form (urlencoded, multipart) or query; then path segments into fields of the same name |

Struct fields bind by their `form` tag, else `json` tag, else name, matched
case-insensitively; `validate:"required"` rejects a zero value. A malformed
value answers 400 with a message (`id: "x" is not an integer`).

A call decorator reaches the request through the handler's `context.Context`
argument: `web.Request(c.Context())`, `web.ResponseWriter(c.Context())`.

```vuka
decorator audited(c) {
	r := web.Request(c.Context())
	c.Next()
	log.Println(r.Method, r.URL.Path, c.Err())
}

@web.Delete("/{id}")
@web.Use(requireToken)
@audited
func (a *PetAdmin) Remove(ctx context.Context, id int) error { … }
```

## Results

A handler returns nothing, `error`, `T`, `(T, error)` or `Result[T]`.

| result | response |
|---|---|
| nothing, a nil error | 204 |
| `vuka.Node` (JSX, templ) | HTML |
| `T` with `@web.Template` | the view, HTML |
| `T` without | JSON, 200 |
| `web.Redirect(url)` (as a value or an error) | 302 for GET/HEAD, else 303 |
| `*web.Error`, wrapped or not | its status and message |
| any other error | 500; its message only in dev |

Errors are an HTML page on HTML routes (a view, a `vuka.Node` or a redirect
result) and `{"error": …, "status": …}` elsewhere. `web.NotFound`,
`Unauthorized`, `Forbidden`, `Conflict`, `web.BadRequest(msg)`,
`web.Status(code, format, args…)`; `errors.Is(err, web.NotFound)` matches by
status. `web.Redirect` is also a `vuka.Node`, so a page handler can return it.
Panics answer 500.

## Views

**templ.** `@web.Template("pets.templ")` uses the file's one exported
component; with several, name one: `@web.Template("cards.templ", "Card")`. The
result fills the component's parameters: whole, when it has one parameter the
result is assignable to; otherwise the result is a struct (or pointer to one)
and each parameter takes the field of its name (exactly, or with the first
letter's case changed: `title` ↔ `Title`). Layouts are templ's own
composition (`@Layout(title) { … }`).

```templ
templ PetsPage(title string, pets []Pet) { @Layout(title) { … } }
```
```vuka
type PetList struct { Title string; Pets []Pet }
```

**html/template.** Any other file is parsed once at Build and executed with the
result; a second argument names a `{{define}}`d template to execute.

**JSX / render functions.** Return a `vuka.Node`; no view needed.

Views are embedded at compile time (`vuka.File`), so the binary carries them.
A `.templ` file must sit in the handler's package directory (it is compiled
into that package); `.html` files may be anywhere below it.

## Dependency injection

The container holds constructors — `func(deps…) T` or `func(deps…) (T, error)`
— and ready values (anything that isn't a function). `@web.Service` and
`@web.Controller` structs are constructed from their fields (Vuka's
`vuka.Type.New`; tag a field `inject:"-"` to leave it out). Every value is a
lazy singleton per app; dependencies match by exact type (provide a
constructor returning the interface to inject an interface). Missing
providers, cycles and duplicates are Build errors.

A provided value implementing `Start(context.Context) error` is started by
`app.Start`/`Run` in dependency order; `Stop(context.Context) error` runs on
shutdown in reverse order.

## Router

The default router is `net/http`'s `ServeMux` with Go 1.22 patterns. Any
router plugs in through

```go
type Router interface {
	http.Handler
	Handle(method, pattern string, h http.Handler)   // canonical {name} / {name...} patterns
	Param(r *http.Request, name string) string
}
```

with `web.New(web.WithRouter(r))`. A chi adapter is a dozen lines (`Handle` →
`r.Method(method, pattern, h)`, `Param` → `chi.URLParam`); the tests carry a
tiny one.

## Static files

`app.Static("/assets", fsys)` serves an `fs.FS` (an `embed.FS` sub-tree,
`os.DirFS(…)`).

## Plain Go

Everything works from Go without decorators. Reflection can't see parameter
names, so scalar parameters need `web.Params`; structs, request values and
dependencies bind as usual.

```go
app := web.New(web.Provide(NewStore))
app.Get("/pets/{id}", func(s *Store, id int) (Pet, error) { … }, web.Params("s", "id"))
app.Post("/pets", func(in NewPet, s *Store) (Pet, error) { … })
app.Get("/page", page, web.View(vuka.File("views/page.html")), web.Wrap(mw))
```

From Go, `vuka.File("…")` reads from disk relative to the working directory.

## Testing

```
go test ./...                     # the package
vuka test ./...                   # also examples/ and internal/vukatest, written in Vuka
```

## Limits

- One struct parameter per handler; scalars bind from the path or query only
  (use `*http.Request` for headers and cookies).
- A `.templ` view must be in the handler's package directory.
- A `vuka.File` literal is embedded only where a decorator or attribute takes
  one, so there is no `web.Layout(file)` option; compose layouts in templ, or
  `{{define}}` them in one html file.
- Dependencies match by exact type; no named values or groups.
- Status codes for success are fixed (200/204); write through
  `http.ResponseWriter` for anything else.

## License

MIT
