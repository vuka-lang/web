# web

A web framework for [Vuka](https://github.com/vuka-lang/vuka): routes, views,
services and controllers are declared with decorators, parameters bind by
name, and `web.New()` gathers everything into an app.

```vuka
package main

import (
	"github.com/vuka-lang/ui"
	"github.com/vuka-lang/web"
)

@web.Service
type PetStore struct{ db *DB }            // built from its fields, a lazy singleton

@web.Get("/pets")
@web.Template("views/pets.templ")         // a templ component renders the result
func ListPets(store *PetStore) Result[PetList] { … }

@web.Get("/pets/{id}")
@web.Template("views/pet.html")           // html/template, the result as data
func ShowPet(id int, store *PetStore) Result[Pet] { return store.Find(id) }

@web.Get("/")
func Home(r *http.Request) ui.Node {    // JSX
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

A complete app is in [`examples/pets`](examples/pets); live components in
[`examples/live`](examples/live).

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
| `@web.Live(path, opts…)` | a stateful component becomes a live page (below) |

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
| `web.Eager(ctors…)` | constructors built and started even when nothing needs them (also `app.Eager`) |
| `web.OnStart(fn)`, `web.OnStop(fn)` | lifecycle hooks (also `app.OnStart`/`app.OnStop`; below) |
| `web.Check(fn)` | a check `Build` runs with its own (also `app.Check`) |
| `web.Options(opts…)`, `web.OptionFunc(f)` | several options as one; an option written as code (below) |
| `web.Use(mw…)` | app-wide middleware (also `app.Use`); runs for every request, 404s included |
| `web.WithRouter(r)` | another router (below) |
| `web.Only(pkgs…)` | take only the declarations of these packages (import paths; `"main"`) |
| `web.Dev()` | show internal error messages; also `VUKA_ENV=development` |
| `web.DotEnv(files…)`, `web.NoDotEnv()`, `web.Require(keys…)` | env files and required variables (below) |
| `web.OnError(f)` | replace the error page / JSON error body |
| `web.LiveLayout(f)`, `LiveKey(k)`, `LiveOrigins(p…)`, `LiveTokenTTL(d)`, `WithRelay(r)` | live pages (below) |

`app.Build()` resolves everything — every parameter of every route, every
result and view, the whole dependency graph — and returns all problems at
once, each naming the route, its source position and the parameter:

```
web: GET /pets (main.ListPets at pages.vuka:24): component PetsPage: main.PetList has no field for parameter title
web: GET /admin (main.Admin at admin.vuka:9): parameter db *sql.DB: no provider for *sql.DB, needed by main.Admin at admin.vuka:9
web: dependency cycle: *main.A -> *main.B -> *main.A
```

### Options from libraries

A library hands the app one option that does all it needs: `web.Options`
groups options, and `web.OptionFunc` runs code against the app inside `New`,
before `Build`, where every `App` method is open to it.

```go
func Cache(cfg Config) web.Option {
	return web.Options(
		web.Provide(cfg, NewCache),
		web.OptionFunc(func(a *web.App) {
			a.Use(cacheHeaders)
			a.Get("/cache/stats", (*Cache).Stats)
			a.OnStop(func(ctx context.Context) error { return flush(ctx) })
		}),
		web.Check(func(*web.App) error { return cfg.Validate() }),
	)
}

app := web.New(Cache(cfg), web.Dev())
```

A check's error is one of `Build`'s, reported with the routes' and the
container's.

`app.Handler()` (an `http.Handler` to mount anywhere; it panics when Build
fails), `app.Start(ctx)` and `app.Run(addr)` call Build first, so nothing is
served from a broken app. Register everything before the first of them.
`app.Run("")` listens on `:$PORT`, else `:8080`. `web.Resolve[T](app)` returns
a dependency, for tests and `main`.

## .env files

`web.New()` first reads env files into the process environment, so every
constructor, `Start` method and option sees their values through `os.Getenv`
or `web.Env`. By `VUKA_ENV`, the first file listed winning:

| `VUKA_ENV` | files |
|---|---|
| `development`, `staging`, … | `.env.<env>.local`, `.env.local`, `.env.<env>`, `.env` |
| `test` (also when unset under `go test`) | `.env.test`, `.env` |
| unset | `.env.local`, `.env` |

The real environment wins over every file: a key is set only when the
process doesn't have it yet. `.env.local` files hold one machine's settings
and are skipped in tests, so test runs don't depend on the developer's box.
`VUKA_ENV` picks the files, so it comes from the real environment; set in a
file, it still turns on `web.Dev()` behaviour.

Files are read from the working directory or, when it has none of them, from
the nearest parent up to the module root (the directory with `go.mod`), so
`go test ./pkg/...` finds the project's `.env`. Missing files are fine.
`web.DotEnv("a.env", "b.env")` replaces the list (first wins), `web.NoDotEnv()`
reads none, and `web.LoadEnv(files…)` loads them earlier — for options built
from the environment; New reading the same files afterwards changes nothing.
With `VUKA_ENV=development` the app logs which files it read.

Syntax:

```sh
# a comment
PORT=8080
export HOST=localhost              # export is allowed; " #" starts a comment
NAME=two words                     # unquoted values are trimmed
RAW='no $expansion, no \n escapes'  # single quotes (and `backticks`) are literal
KEY="-----BEGIN KEY-----
multi-line, with \n \t \" \\ and \$ escapes"
DATABASE_URL=postgres://${HOST}:5432/app
REPLICA_URL="${REPLICA_HOST:-$HOST}"   # :- gives a default for unset or empty
app.name=pets                      # keys: [A-Za-z_][A-Za-z0-9_.]*
```

`$VAR` and `${VAR}` expand in unquoted and double-quoted values from the real
environment, then the keys defined above (in this file or a lower-precedence
one); an undefined variable is empty. A malformed line is a Build error with
the file and line; nothing of that file is set.

Reading:

```go
port := web.Env("PORT", 8080)                   // int; the default when unset or empty
debug := web.Env[bool]("DEBUG")                 // true/false, 1/0, yes/no, on/off
ttl := web.Env("CACHE_TTL", 5*time.Minute)      // time.Duration
hosts := web.Env[[]string]("ALLOWED_HOSTS")     // comma-separated (any slice of these)
db := web.Env[*url.URL]("DATABASE_URL")         // absolute URLs; also encoding.TextUnmarshaler types
key := web.MustEnv[string]("SECRET_KEY")        // panics when unset or invalid

app := web.New(web.Require("DATABASE_URL", "SECRET_KEY"))
```

A value that doesn't convert gives the default, and the next `app.Build()` (or
`app.Start`, for constructors) fails naming the variable. `web.Require` fails
Build listing every missing variable, the files looked for and the keys of
`.env.example` that are unset too:

```
web: missing environment variables: SECRET_KEY (read .env; looked for .env.local, .env); also unset, listed in .env.example: SMTP_HOST
```

Errors and logs name variables and files, never values. Keep `.env` and
`.env*.local` out of git (this repository's `.gitignore` does) and commit an
`.env.example` with the keys and harmless values instead; in production, set
real environment variables.

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
| nothing, a nil error | 204 — unless the handler wrote the response itself through its `http.ResponseWriter` |
| `ui.Node` (JSX, templ) | HTML |
| `T` with `@web.Template` | the view, HTML |
| `T` without | JSON, 200 |
| `web.Redirect(url)` (as a value or an error) | 302 for GET/HEAD, else 303 |
| `*web.Error`, wrapped or not | its status and message |
| any other error | 500; its message only in dev |

Errors are an HTML page on HTML routes (a view, a `ui.Node` or a redirect
result) and `{"error": …, "status": …}` elsewhere. `web.NotFound`,
`Unauthorized`, `Forbidden`, `Conflict`, `web.BadRequest(msg)`,
`web.Status(code, format, args…)`; `errors.Is(err, web.NotFound)` matches by
status. `web.Redirect` is also a `ui.Node`, so a page handler can return it.
Panics answer 500. A handler that wrote the response and then returns an
error can't change it any more: the error is logged.

## Views

**templ.** `@web.Template("views/pets.templ")` uses the file's one exported
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

**JSX / render functions.** Return a `ui.Node`; no view needed.

Views are embedded at compile time (`vuka.File`), so the binary carries them,
from the handler's package directory or below it. A `.templ` file beside the
handler is part of its package; one in a subfolder is that folder's package,
as Go has it — `views/pets.templ` starts with `package views` — and Vuka
compiles it and imports it for you:

```
examples/pets/
  pages.vuka        @web.Template("views/pets.templ"), @web.Template("views/pet.html")
  model/pet.go      package model: Pet, shared by handlers and views
  views/layout.templ  package views: Layout
  views/pets.templ    package views: PetsPage, composed with @Layout
  views/pet.html
```

Only the subfolder's exported components can be views, and its types can't
come from the handler's package (`package main` can't be imported): put shared
types in a package of their own, like `model` here. A JSX page uses a
subfolder's component through its import, `<views.Layout title="Home">`.

## Live components

A [stateful component](https://github.com/vuka-lang/vuka/blob/main/docs/features/components.md#stateful-components)
(a struct embedding `ui.Live`) keeps its state on the server and answers
its elements' events over a WebSocket; the page changes in place.

```vuka
@web.Live("/counter")
type Counter struct {
	ui.Live
	Start int // ?start=3
	n     int
}

func (c *Counter) Mount() { c.n = c.Start }
func (c *Counter) Inc()   { c.n++ }

func (c *Counter) Render() ui.Node {
	return <div><output>{c.n}</output><button onClick={c.Inc}>+</button></div>
}
```

**How a page becomes live.** Two ways, one mechanism:

- `@web.Live(path)` on a stateful component serves it as a GET page. Its
  exported fields of provided types are injected; the others that hold text
  bind from the path segment or query parameter of their name (`form` tag,
  else the field name, ignoring case). The page is the component inside the
  layout `web.Layout(f)` names, else the app's `web.LiveLayout(f)`, else a bare
  document — `f` is `func(page ui.Node) ui.Node`. `web.Use(mw…)` adds
  middleware. From Go: `app.Live("/counter", &Counter{Start: 1})` (the
  value's fields are the defaults).
- Any GET route whose page — a `ui.Node` result, or a templ view —
  renders a stateful component or an event handler is live by itself:

  ```vuka
  @web.Get("/")
  func Home() ui.Node { return <Shell title="Home"><Counter Start={10} /></Shell> }
  ```

  A Vuka component rendered from a templ view is live too: templ passes the
  context on. html/template views and JSON are never live.

The first, HTTP render is complete HTML, and before `</body>` goes
`<script src="/_vuka/live.js" data-vk-token="…" defer>`. The script opens
`/_vuka/live?t=<token>` and sends `join`; the server runs the same route
again — a GET to the URI in the token, with the WebSocket request's headers
(cookies, auth), through the app's and the route's middleware, with its
dependencies — and the page it renders becomes the connection's
[`live.Session`](https://github.com/vuka-lang/vuka/tree/main/live). That
second run mounts the instances that keep state (LiveView's two-phase
mount). A join the route answers with a redirect sends `redirect` (the
browser follows it); any other non-page answer (a 404, JSON) is an `error`,
and the connection closes.

The route's handler runs once per connection: its result is the page, and the
page is rendered again after every event. State belongs in the components.

**Events.** `onClick={c.Inc}` renders `data-vk-on-click="c1:0"`; the runtime
listens on the document for each event type a page names and sends the
nearest handler's id, the element's value (a checkbox's `"true"`/`"false"`),
the key of a keyboard event, and a submitted form's fields. What a handler
receives follows its type (Vuka's rules: nothing, the value, `ui.Event`,
`url.Values`, a struct bound from the form). Two attributes tune it:

| attribute | |
|---|---|
| `data-vk-debounce="150"` | send the element's events (not submits) 150ms after the last one |
| `data-vk-key="Enter Escape"` | send a keyboard event only for these keys |

Submits are `preventDefault`ed; so are clicks on links and on buttons of a
form without a submit handler. A form whose submit handler succeeds is reset
to what the server rendered; one whose handler returns an error keeps what
the user typed. The element of a click or submit carries `aria-busy="true"`
until the reply.

**Patches.** The reply carries the HTML of each component whose output
changed (the page itself is `c0`). The runtime morphs the element in place:
elements are matched by `id`, else `data-vk-id`, else position and tag; a
focused field keeps its value and caret; another field takes the server's
value when it changed. Give list rows an `id` for stable identity across
inserts and removals. `data-vk-ignore` (with an `id`) leaves an element as the
browser has it. A whole-document `c0` patch morphs the `<body>` and sets the
title.

**Browser hooks.** `<html data-vk-state>` is `connecting`, `connected`,
`disconnected` or `closed` (style a banner on it). The document receives
`vk:patched` (`detail.ids`) after each render or patch and `vk:error`
(`detail.message`) for a handler's error or a failed message; errors are also
logged to the console.

**Broadcasts.** `web.Broadcast(ctx, topic, msg)` — from an event handler, an
`Update`, or any request — delivers `msg` to every component subscribed to
`topic` (`c.Subscribe(topic)` in `Mount`), in every connected page; each
takes it in the `Update` overload of its type and its page gets the patch.
`app.Broadcast(ctx, topic, msg)` does the same from outside a request.
Delivery is in-process; `web.WithRelay(r)` sends broadcasts through a `Relay`
(`Publish(ctx, topic, msg)` / `Subscribe(deliver)`) to reach every replica.

```vuka
@web.Live("/chat/{room}")
type Chat struct {
	ui.Live
	Room  string   // the path segment
	Log   *ChatLog // injected
	lines []string
}

func (c *Chat) Mount()          { c.Subscribe("chat:" + c.Room) }
func (c *Chat) Update(m Said)   { c.lines = append(c.lines, m.Who+": "+m.Text) }
func (c *Chat) Update(j Joined) { c.lines = append(c.lines, j.Who+" joined") }

func (c *Chat) Send(ctx context.Context, f ChatForm) error {
	return web.Broadcast(ctx, "chat:"+c.Room, Said{c.name, f.Text})
}
```

**Connections.** One session per connection; its events and broadcasts are
handled one at a time, in order, by the connection's goroutine. The server
pings every 25 seconds; a closed connection unmounts its components.
`app.Stop` (and `Run`'s shutdown) closes every connection as "going away".
The runtime reconnects with jittered exponential backoff (up to ~10s) and
joins again — **the route runs again and the state starts over**; what was
not saved elsewhere is lost.

**Security.** The token is the page's URI and an expiry (`LiveTokenTTL`,
12h by default), HMAC-SHA256-signed with the app's key: a connection can only
rebuild a page the app rendered, and it does so with the connecting
browser's own cookies, through the route's own middleware. Each app draws a
random key; replicas behind one hostname need a shared `web.LiveKey(key)`. A
bad or expired token closes the connection with code 4001 and the runtime
reloads the page (at most once in 10 seconds) for a fresh one. WebSocket
upgrades only from the page's own origin, unless `web.LiveOrigins(patterns…)`
allows more (`"*.example.com"`).

### JS commands

Opening a menu, showing a dialog, moving focus or copying a snippet needs no
server: a chain of [JS commands](https://github.com/vuka-lang/ui) as an `on…`
value runs in the browser, at once, and a `Push` step in it calls a handler
when the server should know too.

```vuka
func (c *Chores) Render() ui.Node {
	return <section>
		<button onClick={ui.JS.RemoveClass(ui.Ref("menu"), "hidden")}>Pick one</button>
		<ul ref="menu" class="menu hidden">
			{for _, ch := range c.chores {
				<li><button value={ch} onClick={ui.JS.AddClass(ui.Ref("menu"), "hidden").Push(c.Pick)}>{ch}</button></li>
			}}
		</ul>
		{for _, ch := range c.chores {
			<p>{ch} <button value={ch} onClick={ui.JS.Confirm("Delete " + ch + "?").Push(c.Delete)}>delete</button></p>
		}}
		<p ref="status">{len(c.chores)} left</p>
	</section>
}

func (c *Chores) Delete(e ui.Event) {
	// …
	c.PushJS(ui.JS.On(ui.Ref("status")).AddClass("flash")) // runs once the reply is applied
}
```

A chain renders as `data-vk-js-<event>="[[name, args], …]"` (the live
protocol's "JS commands"). The runtime runs it when the event reaches the
nearest element with commands or a handler for it: classes and attributes,
`Show`/`Hide`/`Toggle` (the `style` attribute's `display`, with optional
transition classes `during`/`from`/`to`), `Transition`, `Focus`,
`FocusFirst`, a focus stack (`PushFocus`/`PopFocus`, for dialogs),
`Dispatch` (a `CustomEvent`), `Exec` (the commands held in another
attribute), `Copy`/`CopyText` (the element gets `data-copied` for 1.5s),
`ScrollTo`, `SetValue` (fires `input` and `change`), and the steps that steer
the chain: `Key("Escape")` (stop for other keys), `Confirm(msg)` (stop on
cancel), `Debounce`/`Throttle` (the steps after them, per element and event;
a debounced step waiting in a form runs before the form's submit), and
`If`/`ElseIf`/`Else`/`End` over a target's checked state, value, attribute,
selector match or existence. Targets: the element itself, `ui.Ref(name)` (the
`ref`ed element of the same component instance, not of the instances inside
it), `ui.Closest(sel)`, `ui.Inner(sel)`, or a selector. A submit's commands
`preventDefault` it; a push also does what a handler would (links, a form's
buttons). An element with both commands and a handler for one event runs the
commands first, and a declined `Confirm` keeps the event from the server.

**Pushed commands.** `c.PushJS(chain)` in a handler, an `Update` or `Mount`
rides the reply (`"js":[{"id","ops"}]` on `render` and `patch`, both protocol
versions) and runs after it is applied, on the component's root element.

**Reconciliation.** What a command changed — a class, an attribute, the
`style` display — stays across the server's patches while the server keeps
rendering that attribute as it did when the command ran: an open menu stays
open while the list in it updates. When the server renders a different value,
the server wins and the browser's change is dropped. A running transition's
classes survive a patch until it ends.

**Without a connection.** Commands need no session: a page whose only
`data-vk-` attributes are commands gets the runtime script without a token,
runs its commands, and never opens a WebSocket — a static page can have menus
and dialogs. A `Push` on such a page does nothing (the console warns once). A
page is live when it renders a stateful component, a handler, or a command
that pushes to one.

### Protocol versions

The runtime joins asking for **protocol v2** (`{"type":"join","v":2}`) and
follows whatever the server answers:

- **v2, render trees.** Each component instance arrives as a tree: its
  *statics* (the markup the JSX fixes) around its *dynamics* (what Go
  expressions render), loops as item frames keyed by `key`, nested instances
  as references. Statics travel once per connection and are referred to by
  id; long strings are kept by the connection too. After an event or a
  broadcast the server sends, for each instance whose output changed, only
  the dynamics that changed: a new text or attribute, loop steps (keep,
  skip, insert, move a keyed item, change one in place), a token patch of a
  long string — or the instance's plain HTML when that is smaller. The
  runtime applies the changes to the trees it holds, renders the changed
  instances' HTML from them and morphs the page with it exactly as in v1,
  so focus, caret, typed values, busy states and `data-vk-ignore` behave the
  same. A counter's click is ~70 bytes instead of ~250; a todo list's
  events ~3–4× less than v1 (the examples' browser tests log both). A reply
  the runtime can't apply to its trees makes it join again.
- **v1, HTML.** Each changed instance's whole HTML (`updates`), morphed in.
  A Vuka session that doesn't speak v2 answers a v2 join in v1, and the page
  works as before.

`web.LiveProtocol(1)` makes the app answer in v1 whatever the page asks
(`LiveProtocol(2)`, the default, uses v2 when the Vuka session speaks it).
The format is Vuka's [protocol v2](https://github.com/vuka-lang/vuka/blob/main/docs/features/components.md#protocol-v2-render-trees);
`<html data-vk-protocol="2">` says which one a page got.

### The wire, beyond Vuka's protocol

Vuka's [wire protocol](https://github.com/vuka-lang/vuka/blob/main/docs/features/components.md#the-wire-protocol)
as is, plus:

| | |
|---|---|
| `GET /_vuka/live?t=<token>` | the WebSocket; the token in the URL, so a bad one is refused before any message |
| `GET /_vuka/live.js?v=<hash>` | the runtime, immutable for its hash (ETag otherwise) |
| `join` | runs the route; a second `join` on the same connection starts over; its `v` picks the protocol (above) |
| `redirect` | sent for a join the route redirects; the connection then closes normally (1000) |
| close 4001 | invalid or expired token: reload |
| close 4004 | the route isn't a live page: the error message precedes it; no retry |
| close 1001 | the server is stopping: reconnect |
| request header `X-Vuka-Live: join` | on the route's run for a join |

A broadcast arriving before `join` is dropped (the join renders fresh).

## Dependency injection

The container holds constructors — `func(deps…) T` or `func(deps…) (T, error)`
— and ready values (anything that isn't a function). `@web.Service` and
`@web.Controller` structs are constructed from their fields (Vuka's
`vuka.Type.New`; tag a field `inject:"-"` to leave it out). Every value is a
lazy singleton per app; dependencies match by exact type (provide a
constructor returning the interface to inject an interface). Missing
providers, cycles and duplicates are Build errors.

### Lifecycle

`app.Start` (and `Run`) builds what the routes need, then runs the start
sequence; `app.Stop` runs the stop sequence, its reverse. The sequence holds,
in order:

- ready values given to `Provide`, and hooks added with `OnStart`/`OnStop`,
  in the order they were added — a ready value was given explicitly, so it
  starts even when nothing needs it;
- values built from constructors, in dependency order — only those something
  needs (a route, a live page, another value), and those provided with
  `web.Eager`.

A value implementing `Start(context.Context) error` is started and one
implementing `Stop(context.Context) error` is stopped. The first failing
start ends `Start` with its error; every stop runs, and their errors are
joined.

```go
app := web.New(
	web.Provide(cfg),                 // cfg.Start, if any, runs first
	web.OnStart(migrate),
	web.Eager(NewMailer),             // started though no route takes it
)
```

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

The browser tests (`examples/live`, `TestBrowserReconnect`, `TestBrowserJS…`)
drive a headless Chrome over the DevTools protocol (`internal/chrome`) and
skip when none is found; `VUKA_CHROME` names the binary. The runtime's JS
commands also run in node over a small DOM (`testdata/livejs/js.js`,
`TestLiveJSCommands`), skipped without node.

## Limits

- One struct parameter per handler; scalars bind from the path or query only
  (use `*http.Request` for headers and cookies).
- Views live in the handler's package directory or below it (`go:embed`
  doesn't reach up); `../shared/x.templ` is an error.
- A `vuka.File` literal is embedded only where a decorator or attribute takes
  one, so there is no `web.Layout(file)` option; compose layouts in templ, or
  `{{define}}` them in one html file.
- Dependencies match by exact type; no named values or groups.
- Status codes for success are fixed (200/204); write through
  `http.ResponseWriter` for anything else.
- Live patches are whole components' HTML, morphed in the browser (Vuka's
  phase 1); a live connection's state starts over when it reconnects, and
  events sent while disconnected are dropped. The live endpoints are at
  `/_vuka/…` from the root: an app mounted under a path prefix needs them
  routed there.
- Event handlers of a live page get the join's context: `web.Request` is the
  join's request, and writing to `web.ResponseWriter` goes nowhere.

## License

MIT
