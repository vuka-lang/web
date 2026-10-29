# web

A web framework for [Vuka](https://github.com/vuka-lang/vuka): routes, views
and controllers are declared with decorators, services registered with
`app.Service`, every hook's and handler's parameters filled by type, and
`web.New()` gathers everything into an app.

```vuka
package main

import (
	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2"
)

type PetStore struct{ db *DB }

func NewPetStore(db *DB) *PetStore { return &PetStore{db} } // a service: app.Service

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
	app := web.New()            // every route and controller declared at init
	app.Service(NewPetStore)    // the app's services: a lazy singleton each
	app.Provide(OpenDB)         // more constructors and values, fx-style
	app.Run(":8080")            // graceful shutdown on SIGINT/SIGTERM
}
```

A complete app is in [`examples/pets`](examples/pets); live components in
[`examples/live`](examples/live).

```
go get github.com/vuka-lang/web/v2
```

## Declaring

| | |
|---|---|
| `@web.Get(path)`, `Post`, `Put`, `Patch`, `Delete`, `Handle(method, path)` | a route; works on functions and controller methods — on an `@ui.Island` type, a stateless page backed by that component (below) |
| `@web.Template(file [, name])` | render the result with a view (below); before or after the route decorator |
| `@web.Use(mw…)` | HTTP middleware for this route, `func(http.Handler) http.Handler` |
| `@web.Controller(prefix)` | a struct becomes a dependency whose route methods are served under `prefix` |
| `@web.Live(path, opts…)` | a stateful component becomes a live page (below) — an `@ui.Island` type embedding `ui.Live`, a live page backed by that component; `web.Style`, `web.Script`, `web.CSS`, `web.JS` give it styles and scripts (below) |
| `@web.Cron(spec)` | the function runs on a schedule (below) |

Paths use `{name}` for a segment and `{name...}` for the rest of the path, and
match exactly (`/pets` doesn't match `/pets/`); a request whose path differs
from a route's only by a trailing slash is redirected to it (308, method and
query kept), unless something else the app serves answers that path
(`web.ExactPaths()` turns this off). Under a controller, `"/"` is the prefix
itself.

Route decorators are Vuka declarers for a function and type decorators for a
type (a `vuka.Bundle` made with `vuka.Dual`): they run once at init and record
the function with its parameter names, or the type. Other decorators on
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
| `web.Dev()` | development where the process runs in it (`VUKA_DEV=1`, `VUKA_ENV=development`) and in tests: internal error messages, Tailwind and Vite for `go run .` (below); nothing in production |
| `web.DotEnv(files…)`, `web.NoDotEnv()`, `web.Require(keys…)` | env files and required variables (below) |
| `web.OnError(f)` | replace the error page / JSON error body |
| `web.LiveLayout(f)`, `LiveKey(k)`, `LiveOrigins(p…)`, `LiveTokenTTL(d)`, `WithRelay(r)` | live pages (below) |
| `web.LiveJoinTimeout(d)`, `LiveIdleTimeout(d)`, `LiveMaxConns(n)`, `LiveMaxConnsPerIP(n)`, `LiveMaxConnsPerPage(n)`, `LiveCheckEvery(d)` | live connections' limits (below) |
| `web.NoTailwind()`, `web.Tailwind(path)` | no stylesheet; or the project's Tailwind stylesheet at another path (below) |
| `web.Style(paths…)`, `Script(paths…)`, `CSS(src)`, `JS(src)` | the app's own stylesheets and scripts, unscoped (below) |
| `web.NoToolbar()` | no development toolbar (below) |
| `web.NoCrons()`, `web.CronClock(c)` | don't schedule the crons; run them by another clock (below) |
| `web.CSRF(trusted…)` | refuse cross-origin form posts and the like (below) |
| `web.WrapRoutes`, `ParamFrom`, `LiveCheck`, `GuardLiveEvents` | hooks for libraries guarding routes and live events (below) |
| `web.Frontend(fsys, root, opts…)` | a single-page app built by Vite (or anything else) served from the binary (below) |
| `web.ExactPaths()` | no trailing-slash redirect (above) |
| `web.Headers(h)`, `web.NoSecurityHeaders()` | the security headers every response carries (below) |
| `web.TrustedProxies(cidrs…)` | the proxies whose `X-Forwarded-For`/`Forwarded`/`X-Forwarded-Proto` count (below) |
| `web.MaxRequestBody(n)` | the largest request body, 32 MiB by default; a route's own with `@web.MaxBody(n)` (below) |

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
Keys starting with `VUKA_` (`VUKA_ENV`, `VUKA_DEV_DIR`, `VUKA_DEV_LIVE_KEY`…)
come from the real environment only: an env file setting one is ignored
(and logged), so a file next to a deployed binary never switches it into
development. `web.IsDev()` says whether the process runs in development
(`VUKA_ENV=development`, `VUKA_DEV=1`, or `vuka run`).

Files are read from the working directory or, when it has none of them, from
the nearest parent up to the module root (the directory with `go.mod`), so
`go test ./pkg/...` finds the project's `.env`. Without a `go.mod` in the
working directory or above it — a deployed binary — only the working
directory is read. Missing files are fine.
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
case-insensitively. A malformed value answers 400 with a message (`id: "x"
is not an integer`). Then the struct's `validate:` tags are checked, nested
structs and slices of them included — `required`, `omitempty`,
`min=n`/`max=n`/`len=n`, `oneof=a b`, `email`, the rules of a page backed
by a component — and a failure answers 422 with each field's message, keyed
by its path as the request named it (json names for a JSON body, else form
names):

```json
{"error":"invalid: address.city: required; email: an email address","fields":{"address.city":"required","email":"an email address"},"status":422}
```

The error is a `web.Invalid`, so an `OnError` handler reads the fields with
`errors.As`. A rule that isn't one of these fails `Build`.

A request body is at most 32 MiB (`web.MaxRequestBody(n)`; negative for
none), uploads included — a multipart form spills to temporary files past
10 MiB, and never past the limit. A longer body answers 413, whoever reads
it: the binding, or a handler reading `r.Body`. One route sets its own:
`@web.MaxBody(512 << 20)`, or `web.Attrs(web.MaxBody(n))` from Go.

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
Panics in handlers, middleware, custom routers and page rendering are recovered
per request. Before the response is committed, they answer 500 through
`OnError` (HTML on page routes, JSON otherwise; middleware/router failures
use the request's `Accept` header). Details and a stack trace stay in the
server log; development responses may include the error message.
If `OnError` itself panics before writing, a generic plain-text 500 is used.

A panic after writing or flushing a response aborts that response instead of
appending an error body. `http.ErrAbortHandler` retains its normal abort
semantics. Streaming and WebSocket upgrades are not buffered; a panic during
live rendering or a live guard closes the affected socket with code 1011
and a generic reason, discarding its session. Other requests keep running.
This does not catch panics in application-created goroutines or turn startup
configuration errors into successful builds.

A handler that wrote the response and then returns an error can't change it
any more: the error is logged.

**Forms that fail.** A page answering a POST (or PUT, PATCH, DELETE) is the
fragment the handler rendered — for htmx, `fetch` and the like — unless the
browser shows it as its document: a form posted without script
(`Sec-Fetch-Dest: document`, or, from a browser that doesn't send it, an
`Accept: text/html` request without `HX-Request`). That one is a whole page,
as GET would serve it — stylesheet, scripts, toolbar — so a handler can
re-render the form with its errors and what was typed instead of
redirecting. Such a page isn't live (a reconnect would rebuild it with GET):
its JS commands run, its stateful components render but don't connect.

```vuka
@web.Post("/signup")
func SignUp(ctx context.Context, in Signup, users *Users) ui.Node {
	if err := users.Create(ctx, in); err != nil {
		return <SignupPage form={in} problem={err.Error()} />
	}
	return web.Redirect("/welcome")
}
```

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
  views.vuka        @web.Template("views/pets.templ"), @web.Template("views/pet.html")
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

@ui.Mount
func (c *Counter) mount(ctx context.Context) error { c.n = c.Start; return nil }

func (c *Counter) Inc() { c.n++ }

func (c *Counter) Render() ui.Node {
	return <div><output>{c.n}</output><button onClick={c.Inc}>+</button></div>
}
```

**How a page becomes live.** Two ways, one mechanism:

- `@web.Live(path)` on a stateful component serves it as a GET page. Its
  exported fields of provided types are injected; the others that hold text
  bind from the path segment or query parameter of their name (`form` tag,
  else the field name, ignoring case). The page is the component inside the
  layout `web.Layout(f, assets…)` names, else the app's `web.LiveLayout(f, assets…)`, else a bare
  document — `f` is `func(page ui.Node) ui.Node`. `web.Use(mw…)` adds
  middleware. From Go: `app.Live("/counter", NewCounter)` (or `&Counter{Start: 1}`, whose
  fields are the defaults).
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

**Side effects in a live page's handler.** The route runs twice for one
visit — the GET, then the join (and again on each reconnect) — so what it
does besides building the page, sending mail or counting a visit, happens
twice. `web.IsLiveJoin(ctx)` is true only on the join's run; skip them there:

```vuka
@web.Get("/welcome")
func Welcome(ctx context.Context, m *mail.Mailer) (ui.Node, error) {
	if !web.IsLiveJoin(ctx) {
		if err := m.Send(ctx, welcomeMail); err != nil {
			return nil, err
		}
	}
	return <WelcomePage />, nil
}
```

A navigation over the connection (`navigate`) is a visit of its own and
isn't a join. Web marks its own run, so a client sending `X-Vuka-Live`
can't make it true; events after the join see false too.

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
`disconnected`, `idle` or `closed` (style a banner on it). The document receives
`vk:patched` (`detail.ids`) after each render or patch and `vk:error`
(`detail.message`) for a handler's error or a failed message; errors are also
logged to the console.

**Errors.** What the browser reads of an error follows HTTP: a `*web.Error`'s
message (`web.BadRequest("a name, please")`, `web.Status(409, …)`) as it is,
and a field that doesn't parse; an event naming a handler the session no
longer has is `live: no handler "c3:75"`; any other error — a handler's,
`Mount`'s or `Update`'s, or a panic — is `Internal Server Error`. What the
browser doesn't get of an error (all of it, or the detail of a no-handler
error: whose handler it was, by Go type) goes to the log (`slog`, with the
session, the page, the handler and the component). In development
(`web.Dev()`, `VUKA_ENV=development`, `vuka run`) the browser — its console and
the toolbar — gets every error's full text.

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

@ui.Mount
func (c *Chat) mount(ctx context.Context) error { c.Subscribe("chat:" + c.Room); return nil }

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

**Limits.** A socket must join within `LiveJoinTimeout` (10s); a joined page
the browser sends nothing for `LiveIdleTimeout` (30m; broadcasts don't count)
closes with code 4002, dropping its session, and the runtime joins again at
once if the page is in view, else when it is shown. At most
`LiveMaxConns` (10,000) sockets are open, `LiveMaxConnsPerIP` (100) from one
client address (`web.ClientIP`, so through `TrustedProxies`) and `LiveMaxConnsPerPage` (4) with one page token; a socket
beyond a cap is refused (503, 429) and the runtime retries. A page is
checked when it mounts — its join and each navigation run its route through
the app's middleware and gates — and its events and broadcasts trust what it
mounted with. `LiveCheck`s run on each joined page every `LiveCheckEvery`
(30s): one failing closes the page with 4001, and it reloads;
`app.CloseLive(match)` closes the pages whose context `match` picks at once.
Each limit is an option; 0 lifts it.

**Security.** The token is the page's URI and an expiry (`LiveTokenTTL`,
12h by default), HMAC-SHA256-signed with the app's key: a connection can only
rebuild a page the app rendered, and it does so with the connecting
browser's own cookies, through the route's own middleware. Each app draws a
random key; replicas behind one hostname need a shared `web.LiveKey(key)`. A
bad or expired token closes the connection with code 4001 and the runtime
reloads the page (at most once in 10 seconds) for a fresh one. WebSocket
upgrades only from the page's own origin, unless `web.LiveOrigins(patterns…)`
allows more (`"*.example.com"`).

### Navigation

A paged list, tabs kept in the URL, a list and its detail pages: moving
between them shouldn't reload the document, drop the connection or lose
the scroll position of the page you come back to. `ui.Link` is a link a live
page follows over its connection:

```vuka
<ui.Link href="/books/7">Dune</ui.Link>                 // navigate: another page
<ui.Link href={shelfURL(s.page + 1)} patch>Next</ui.Link> // patch: this page, other parameters
```

It renders `<a href="…" data-vk-link>` (`="patch"`), so it stays a plain link
without JavaScript, on a page that isn't live, in a new tab (`target`, a
modifier key, the middle button) and to another origin — the runtime leaves
those clicks to the browser.

- **Patch** moves to another URL of the same page. The route runs again for
  the new URL (its middleware too). A Live page whose type has an `Update`
  overload taking a `web.URLParams` struct stays mounted with its state: its
  props are bound from the URL again, and that `Update` gets the new params
  first — bound the same way, from the path segment or query parameter of
  each field's name. Then the usual patch is sent. **The server decides,
  not the link**: a page without such an `Update` mounts again, `Mount`
  seeing the new URL, as a navigate. So **`Update` must repeat every check
  `Mount` makes** of what the URL names (that the visitor may see order
  `{id}`): an error it returns fails the patch, nothing stays mounted, and
  the browser loads the URL, the route answering as for any request.
- **Navigate** moves to another page: its route runs as a page load does
  (middleware, gates, dependencies, layout), its components mount on the
  same connection, and the old ones unmount. Under protocol v2 the page
  arrives as a change to the trees the connection holds: a shared layout
  isn't sent again.

```vuka
type ShelfParams struct {
	web.URLParams
	Page int // ?page=
}

@ui.Mount
func (s *Shelf) mount(ctx context.Context) error { s.show(s.Page); return nil } // the first URL

func (s *Shelf) Update(p ShelfParams) { s.show(p.Page) } // each patch
```

From a handler, `c.PushPatch(href)` and `c.PushNavigate(href)` move the page
once the reply is applied; in a chain, `ui.JS.Patch(href)` and
`ui.JS.Navigate(href)` do it from the browser. A URL whose route answers
anything but a page — a redirect, an error, JSON, another origin — is
loaded as usual, and so is any navigation while the page isn't connected,
or one that fails. Only paths and http and https URLs are followed: the
runtime refuses to load a `javascript:`, `data:` or other URL, wherever it
comes from (a command, a fallback, the server), and the server sends no
redirect to one (an app's open redirect to `javascript:` is an error).

In the browser: the address changes with `pushState`, and back and forward
patch or navigate to match (patch within one page's entries); a navigation
scrolls to the top or to the URL's `#hash`, back restores where the page was,
and focus moves to the new page's `[autofocus]` element, else its `<main>`,
else its first component. The page's `<title>` follows, and its `<head>`
elements the document lacks (stylesheets, scripts, meta tags) are added once;
scripts in the new page's body don't run. `<html data-vk-navigating>` is
set while a navigation is in flight, and `vk:navigate` fires on `window`
when one completes (`detail`: `{url, patch}`). Events aren't sent while the
page is on its way out, and a new page drops the optimistic pushes still
waiting for the old one, without their hooks. A reconnect joins the page the
connection moved to.

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
commands first, and a declined `Confirm` keeps the event from the server. The
attribute commands change no event handler attribute (`on…`) or `srcdoc`,
and set no URL attribute (`href`, `src`, `action`, `formaction`,
`xlink:href`, `poster`, `data`) to a `javascript:`, `data:` or `vbscript:`
URL: `ui.JS.SetAttr` and its kin panic on such a name when the chain is
built and sanitize such a value as an `href` in markup is, and the runtime
refuses either in commands from elsewhere (an `Exec`'s attribute).

**Pushed commands.** `c.PushJS(chain)` in a handler, an `Update` or `Mount`
rides the reply (`"js":[{"id","ops"}]` on `render` and `patch`, both protocol
versions) and runs after it is applied, on the component's root element.

**Reconciliation.** What a command changed — a class, an attribute, the
`style` display — stays across the server's patches while the server keeps
rendering that attribute as it did when the command ran: an open menu stays
open while the list in it updates. When the server renders a different value,
the server wins and the browser's change is dropped. A running transition's
classes survive a patch until it ends.

**Optimistic updates.** A like should count at once and a deleted row
should go at once, without waiting for the server — and come back if the
server refuses. The steps before a `Push` in a chain are its predictions:
they show immediately, and the push's reply settles them.

```vuka
<button
	class={likedClass(p.liked)}
	onClick={ui.JS.AddClass(ui.This, "liked").SetText(ui.Ref("likes"), fmt.Sprint(p.likes+1)).
		Push(p.Like).OnError(ui.JS.Show(ui.Ref("like-error")))}
>
	Like
</button>
<b ref="likes">{p.likes}</b>
<p ref="like-error" hidden>Couldn't save your like.</p>
```

- **Success** (a reply without an error): the predictions stay, and
  reconciliation goes on as for any command — wherever the server renders
  something else (the count with everyone else's likes), the server wins.
  The `OnSuccess` chain runs.
- **Failure** (the handler returned an error, an `error` message, no reply
  within the push's timeout — 10s by default — or the connection dropping
  before the reply): each prediction is undone, latest first, except where
  the server has rendered a different value since or a later command
  changed it. The `OnError` chain runs, and `vk:error` fires as for any
  error.
  A push made while the page isn't connected isn't sent (the runtime
  doesn't queue events) and fails at once.
- `Push(h, ui.KeepOnError())` never undoes (the hooks still run);
  `ui.Timeout(d)` sets how long a push waits. A reply that comes after its
  push timed out is applied as usual, without running hooks again.

What counts: classes, attributes, the `style` display (`Show`/`Hide`, a
`hidden` attribute), the text `SetText` sets — reconciled like an attribute:
it stays while the server renders the same text — and a field's value or
checked state from `SetValue`. Each push in a chain settles only the steps
since the previous push; a push a branch skips ends its segment too. Hooks
run on the element the chain ran for, or the component's root once that
element is gone; a server-pushed chain's pushes work the same way. While
a push waits, its element has `data-vk-pending`; after a failure it has
`data-vk-failed` until its next push — hooks for CSS:

```css
[data-vk-pending] { opacity: .6; }
[data-vk-failed] { outline: 1px solid crimson; }
```

**Calling JavaScript.** A chart to redraw, confetti on a save, a date
picker to open: `Call("name", args…)` calls a page function with
`ctx` — `{el, event, root, target(spec)}` — and the arguments, which may be
reads of the page at that moment (`ui.Ref("q").Value()`, `.Checked()`,
`.Attr("name")`). Register functions with `vuka.register`; the first line lets a
script run before the runtime (its registrations wait in `vuka.q`):

```html
<script>
  window.vuka ||= { q: [], register(name, fn) { this.q.push([name, fn]); } };
  vuka.register("confetti", (ctx, n) => fire(ctx.el, n));
</script>
```

Otherwise the name is a dotted path on `window` (`"Picker.refresh"`, called
with `Picker` as `this`), never evaluated as code. A returned promise holds
the rest of the chain until it settles; `false` stops the chain as a
declined `Confirm` does. An unknown function, a throw or a rejection stops
the chain and fires `vk:error` on the element (`detail`: `{error, fn}`);
when it happens among a push's predictions, that push fails without being
sent — its predictions are undone and its `OnError` chain runs. (A chain that
waits on a promise doesn't hold back a `data-vk-on-…` handler on the same
element; put the `Push` in the chain.)

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
| `redirect` | sent for a join the route redirects; the connection then closes normally (1000); never to a URL that isn't http or https (an `error` instead) |
| `navigate` | runs the URL's route (`X-Vuka-Live: navigate`) and makes its page the session's; the `patch` reply carries `url` and `token`, the new page's join token; a route that doesn't answer with a page gets `redirect` (its `Location`, else the URL); `patch: true` asks to keep the page mounted, which the server does only for an `Update` taking `web.URLParams`; a failed `Mount`, `Update` or render answers `error`, nothing staying mounted |
| close 4001 | invalid or expired token, or a `LiveCheck` failed: reload |
| close 4002 | the page sat idle (`LiveIdleTimeout`): reconnect once the page is in view |
| close 1008 | no `join` within `LiveJoinTimeout` |
| close 4004 | the route isn't a live page: the error message precedes it; no retry |
| close 1001 | the server is stopping: reconnect |
| request header `X-Vuka-Live: join` | on the route's run for a join (`navigate` for a navigation's) |

A broadcast arriving before `join` is dropped (the join renders fresh).

## Client state

`ui.State` keeps a value in the browser — a menu's open flag, a tab, a
filter — and the page follows it with no round trip. The runtime (live.js)
evaluates the bindings and runs the writes, live page or not:

```vuka
var theme = ui.State("light") // the page's: one value

func Menu() ui.Node {
	open := ui.State(false) // each Menu's own
	return <div>
		<button aria-expanded={open} onClick={open.Toggle()}>Menu</button>
		<ui.Show when={open}>
			<ul class="transition data-closed:opacity-0">…</ul>
		</ui.Show>
	</div>
}

func Tabs() ui.Node {
	tab := ui.State("env")
	f := ui.State(Filters{Sort: "name"})
	return <section>
		<button aria-selected={tab == "env"} onClick={tab.Set("env")}>Env</button>
		<button onClick={tab.Set("logs").Set(f.Kind, "")}>Logs</button>
		<div hidden={tab != "logs" || f.Kind == ""}>…</div>
		<Count kind={f.Kind} /> {/* the same state */}
	</section>
}
```

A live page's renders leave what the browser set alone; a component sets a
state from the server with `c.PushJS(tab.Set("logs"))`. Guide:
[client state](https://vuka-lang.github.io/ui/state); example:
`examples/live/pages/state`.

## Dependency injection

A service is a struct with a constructor whose parameters are its own
dependencies, registered in `main`:

```go
type Todos struct {
	db   *DB
	mu   sync.Mutex
	rows []Todo
}

func NewTodos(db *DB) *Todos { return &Todos{db: db} }

app := web.New()
app.Service(NewTodos)
app.Provide(OpenDB) // constructors and ready values of any type
```

The container holds constructors — `func(deps…) T` or `func(deps…) (T, error)`
— and ready values (anything that isn't a function). Every value is a lazy
singleton per app; dependencies match by exact type (provide a constructor
returning the interface to inject an interface). Missing providers, cycles
and duplicates are Build errors. A `@web.Controller` struct is constructed
by the constructor Vuka writes, which takes its pointer and interface
fields (or by its own `New`).

### Parameters, by type

Hooks and handlers declare what they need by parameter type, and each
parameter is filled from the one source its type names — the same rule for
a route's handler, a controller's method, a live page's hooks (`@ui.Mount`,
`@ui.OnChange`, `@ui.Loads`) and event handlers, a layout's or guard's
`Load`, and a middleware's constructor:

| Parameter | Filled from |
|---|---|
| `context.Context` | the request's, or the live session's |
| a struct, or pointer to one | the form or JSON body (a route), the event's form fields (a live handler); `validate:` tags checked, 422 |
| `url.Values` | the form's fields, as sent |
| a string, number, bool or `TextUnmarshaler` | a route: the path segment of its name, else the query parameter; a live handler: the event's value |
| `*http.Request`, `http.ResponseWriter` | the request (a live page's: the one it joined with) |
| `*auth.Identity` (a `ParamFrom` type) | the signed-in visitor |
| a type `app.Service` or `web.Provide` registered | the container |

A parameter nothing fills is a Build error with the file, the line and its
type — E0407 for a pointer or interface nothing provides, E0405 for a type
a request or an event can't carry:

```
error[E0407]: GET / (live page pages.Index): Index.Add's 2nd parameter, *sql.DB, is not provided
 --> index.vuka.html:41
  = help: register it in main with app.Service(NewX), or web.Provide(NewX)
```

A live page's exported methods are checked as event handlers: a method
taking two values from the page's own Go (`p.Toggle(id, done)` in a
closure) is no event, and only a missing service is reported for it. A
scalar's name comes from the compiler, which records it
(`vuka.RegisterParams`); a handler registered from Go binds its scalars to
the path's segments in order, the last scalar the last segment, and takes
the query in a struct.

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

## Crons

A function runs on a schedule with `@web.Cron`. It takes a
`context.Context` and dependencies, like a handler, and returns nothing or
an error:

```vuka
@web.Cron("@every 10m")
func forgetUnconfirmed(ctx context.Context, owners *Owners) error { … }

@web.Cron("CRON_TZ=Africa/Dar_es_Salaam 0 7 * * 1-5")
func (r *Reports) Morning(ctx context.Context) error { … }   // a method of a service

app.Cron("digest", "0 8 * * *", sendDigest)                  // from Go
```

| spec | |
|---|---|
| `*/15 * * * *` | five fields: minute, hour, day of the month, month, day of the week; `*`, `a-b`, `a,b`, `/step`, `JAN`, `MON`; Sunday is 0 or 7 |
| `@hourly`, `@daily` (`@midnight`), `@weekly`, `@monthly`, `@yearly` | the usual shorthands |
| `@every 30s` | at each multiple of the duration — :00 and :30 here, on every replica and after a restart |
| `CRON_TZ=Europe/Paris 0 9 * * *` | the fields read in that zone; else the process's local time |

When both day fields are restricted a day matching either runs. Across
daylight-saving changes a time the clock skips runs at the jump, and a
time it passes twice runs once — unless the hour field is `*`, when the
job keeps its rhythm by the clock. Package `web/cron` parses specs and
computes the next run on its own.

`app.Start` schedules the crons after building everything they need; `Stop`
stops scheduling, waits for the runs in progress until its context is done,
then cancels their context. A run never overlaps the previous one: a tick
that finds it still running is skipped and logged. Missed ticks (the
machine slept) aren't run late. A panic is recovered and logged with its
stack, and errors go to `slog` with the cron's name. An invalid spec, a
parameter that isn't provided or a cron declared twice is a Build error:

```
web: @web.Cron("61 * * * *") on main.cleanUp at jobs.vuka:12: cron: "61 * * * *": minute: 61 is out of range 0-59
```

A cron's name is its function's (`forgetUnconfirmed`; `jobs.cleanUp` from
another package; `Reports.Morning`). `app.RunCron(ctx, name)` runs one now
and returns its error, for tests and commands; `web.NoCrons()` serves
without scheduling them.

**Several replicas.** Each process schedules its crons, so every replica
runs every tick. Provide a `web.CronLock` and only the replica that claims a
tick runs it:

```go
type CronLock interface {
	Claim(ctx context.Context, name string, tick time.Time) (bool, error)
}
```

Every replica asks for the same name and tick, so a lock is a unique key —
an `INSERT` of `(name, tick)` into a table with a unique index (`false` on a
conflict), or Redis's `SET cron:name:tick 1 NX EX 3600`. Without one the app
uses `web.NewMemoryCronLock()`, which claims each tick once in its process.

**Testing.** `crontest.Run(app, name)` runs a cron with its dependencies.
`crontest.NewClock(t)` is a virtual clock (`web.CronClock(clock)`): nothing
fires until the test moves it.

```go
clock := crontest.NewClock(time.Date(2026, 1, 5, 6, 59, 0, 0, time.UTC))
app := web.New(web.CronClock(clock), web.Provide(NewReports))
app.Start(ctx)
clock.WaitTimers(1)          // the scheduler waits for 07:00
clock.Advance(time.Minute)   // the cron runs
crontest.Wait(app)           // and has finished
```

## Mail

`web/mail` sends email. Provide a `*mail.Mailer` and inject it:

```vuka
app := web.New(mail.Provide())   // configured from the environment

@web.Post("/signup")
func SignUp(ctx context.Context, in Signup, m *mail.Mailer) error {
	return m.Send(ctx, mail.Message{
		To:      []string{in.Email},
		Subject: "Welcome to Pets",
		HTML:    <WelcomeEmail name={in.Name} />,   // JSX or templ; the text part is made from it
	})
}
```

| variable | |
|---|---|
| `MAIL_DRIVER` | `log` (the default: log the message, send nothing) or `smtp` |
| `MAIL_HOST`, `MAIL_PORT` | the SMTP server; the port defaults to 587, 465 with `tls`, 25 with `none` |
| `MAIL_USERNAME`, `MAIL_PASSWORD` | PLAIN authentication, when set |
| `MAIL_ENCRYPTION` | `starttls` (the default; fails when the server can't), `tls` (from the start), `none` (local catchers) |
| `MAIL_FROM`, `MAIL_FROM_NAME` | the sender when a message names none |

`mail.Provide(mail.Config{…})` configures it in code instead (also a
`Transport` of your own, a TLS config, timeouts). A `Message` has `From`,
`To`, `Cc`, `Bcc`, `ReplyTo`, `Subject`, `Text`, `HTML` (a `ui.Node`;
`ui.Safe(s)` for a string), `Headers` and `Attachments`. Text and HTML go
as `multipart/alternative`, attachments in `multipart/mixed`, non-ASCII
headers and file names encoded. Addresses are parsed, and a line break in
any header value is refused before anything is sent (`*mail.InvalidError`).
SMTP is `net/smtp` with a dial timeout and a deadline; no other dependency.

`m.SendLater(ctx, msg)` checks and renders the message now and sends it in
the background, retrying a temporary failure (a 4xx answer, a failed
connection) 3 times, waiting 2s, 4s, 8s; the last failure is logged. The
app's `Stop` waits for the queue within its context. The queue holds at
most `Config.Queue` messages (1000 by default); past it, `SendLater`
returns `mail.ErrQueueFull`.

**Testing.** The log driver keeps what it was given: `m.Sent()` (each with
its rendered `HTML`, `Text`, `Recipients` and `Raw` MIME). `mailtest.New()`
is a quiet one to provide; `mailtest.Last(t, m)`, `mailtest.To(t, m, addr)`
read it, waiting for `SendLater` first.

A live page's route runs again when its socket joins: mail sent from it
goes once with `if !web.IsLiveJoin(ctx) { … }` around the send (see
"Side effects in a live page's handler").

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

## Tailwind

Write Tailwind classes anywhere — JSX, `.templ` and `.html` views, the class
names a JS command adds — and they are styled, with no setup:

```vuka
@web.Get("/")
func Home() ui.Node {
	return <Layout title="Home">
		<button class="rounded bg-emerald-500 px-4 py-2 text-white"
			onClick={ui.JS.AddClass(ui.Ref("note"), "opacity-50")}>Save</button>
		<p ref="note" class="mt-2 text-sm text-gray-500">Unsaved changes</p>
	</Layout>
}
```

The `vuka` command builds the stylesheet with the
[Tailwind CLI](https://tailwindcss.com/docs/installation/tailwind-cli)
(`brew install tailwindcss`, or `npm i -D @tailwindcss/cli`) from the module's
`.vuka`, `.templ`, `.html` and `.go` files and those of the dependencies that
ship components (github.com/vuka-lang/ui/v2), and every full page the app serves
links it: `<link rel="stylesheet" href="/_vuka/app.css?v=…">` before
`</head>`, unless the page links it already. Fragments, JSON and live updates
don't.

- **`vuka run`** keeps it rebuilt (`tailwindcss --watch`, into
  `.vuka/tailwind/`) and the app serves it from there; open pages swap in each
  new build without reloading, live state kept.
- **`vuka build`** builds it once, minified, and embeds it in the binary:
  served as `/_vuka/app.css?v=<hash>`, cached for good; revalidated by ETag
  without the hash. `vuka gen` writes the file that embeds it
  (`vuka_tailwind_gen.go` in the main package) for plain `go build`.

Your own theme, plugins or components go in `app.css` or `styles/app.css` at
the module root — any file with `@import "tailwindcss"`; it replaces the
built-in one, which is just that line. Elsewhere, name it:
`web.New(web.Tailwind("assets/site.css"))` (a string literal: the `vuka`
command reads it from the source).

Without the `vuka` command, `web.Dev()` runs the watcher for `go run .`
([below](#go-run)).

`web.NoTailwind()` turns it all off, as does `VUKA_TAILWIND=off` for the
`vuka` command. Without the Tailwind CLI, `vuka run` says how to install it
and serves pages unstyled; `vuka build` fails.

`web.RegisterCSS(name, css)` serves another stylesheet at `/_vuka/<name>`;
the file `vuka build` generates registers `app.css` with it.

## go run

The `vuka` command is a convenience; a plain `go run .` gets live CSS and the
frontend's dev server from one option:

```go
app := web.New(web.Dev(), web.Frontend(dist, "frontend/dist"))
app.Run("")
```

```sh
VUKA_DEV=1 go run .        # or VUKA_ENV=development
```

In development `Start` (`Run`) runs `tailwindcss --watch` on the app's
stylesheet (as `vuka run` does, into `.vuka/tailwind/`) and each frontend's
Vite dev server (`node_modules/.bin/vite` in its project, the Vite plugin
copied in), and the app serves through them: pages swap in each new
stylesheet, and load their modules from Vite. Their output is logged with
`tailwind:` and `vite:` prefixes. `Stop` ends them — SIGTERM to each one's
process group, SIGKILL after 2s — and a dev server whose app is gone,
however it ended, exits by itself.

What is missing isn't installed for you: the app serves the last build and
logs one line saying how to get live CSS and the dev server. Nothing runs
twice — not under `vuka run`, which runs both itself, nor beside a Vite dev
server or a `VUKA_TAILWIND_CSS` watcher already running. In a test binary
`web.Dev()` turns on development (error messages) and starts nothing; in a
production binary it does nothing at all, so `main` keeps it.

## Styles and scripts

A component's own stylesheet and script — a keyframe animation, a canvas
drawn on mount — are declared beside it, and the `vuka` command compiles
them:

```vuka
@web.Live("/counter",
	web.Layout(counterPage, web.Style("layout.css")),
	web.Style("counter.css", "parts/*.css"),
	web.Script("counter.ts"),
)
type Counter struct { … }
```

- `web.Style(paths…)`, `web.Script(paths…)`, `` web.CSS(`…`) `` and
  `` web.JS(`…`) `` are options of `@web.Live` (the component's), of
  `web.Layout` and `web.LiveLayout` (the layout's own markup) and of
  `web.New` (the app's, unscoped). `web.Global` among the arguments makes one
  unscoped. Paths are string literals relative to the `.vuka` file; globs
  work.
- **Scoped CSS.** Every element a scoped component's markup writes carries
  `data-vk-s="<scope>"`, fixed markup the transpiler adds (a templ layout's
  too), and its selectors are rewritten to match only those: `:global(…)`
  and `:root` stay unscoped, `:deep(…)` reaches into child components,
  `:scope` is the root, keyframes are the component's own. They compile
  into the app's Tailwind `app.css`, so `@apply` and variants work.
- **Scripts** (TypeScript or JavaScript, bundled with esbuild inside the
  `vuka` command) are one module per component under `/_vuka/c/`; every
  full page of an app with scripts loads a small loader
  (`/_vuka/c/loader.js`), which imports a component's module the first time
  it renders and runs its hooks: `mounted(el, ctx)` on load, join or
  arrival, `updated` after a patch that keeps it, `destroyed` when it leaves
  (live navigation too). `ctx.push("Inc", payload)` calls a Go method of the
  component as an event — only those its scripts name in `ctx.push("…")`
  literals, which the `vuka` command lists in the manifest and the app
  allows (`live.AllowPush`); any other name is refused, `ctx.on(name, fn)` receives `c.PushEvent(name, payload)`,
  `ctx.ref(name)` finds its `ref` element. `ui.JS.Call("bump")` reaches the
  component's own methods before `vuka.register`'s and `window`'s.
- `vuka run` keeps them compiled in `.vuka/assets` (`VUKA_ASSETS`): a saved
  stylesheet restyles open pages, a saved script loads on the next mount.
  `vuka build` embeds them with a generated `vuka_assets_gen.go`, which
  calls `web.RegisterAssets(manifest)` and `web.RegisterScript(name, js)`.
- The development toolbar's Assets panel lists the page's scoped
  components and their files.

Options given from Go (`app.Live(…, web.Style(…))`) do nothing: the `vuka`
command reads them from `.vuka` files.

## Development toolbar

Why is this page slow, which statements did it run, what did that click
do? In development every full page answers on the spot: a handle on its
right edge shows the request's time and statement count, and opens a
drawer of panels for that request.

- **Request**: method, path, the route's pattern, the handler (its name,
  file and line), the status, the arguments the handler was called with
  and where each came from (path, query, body, dependency), and the
  request's and response's headers (cookies and credentials hidden).
- **SQL**: each statement in order, with its arguments, rows and time as
  a bar. A statement run more than once is amber; run three times or more
  with different arguments it is named as an N+1, one statement per row
  of an earlier result. A failed or refused statement is red.
- **Timeline**: the app's and the route's middleware, each call decorator
  (`@timed`), the handler, the render and the statements, as bars against
  the request's time.
- **Logs**: what `slog`'s default logger wrote for the request — with its
  context (`slog.InfoContext(ctx, …)`) or from the request's goroutine.
  The console gets it as before.
- **Live**: on a live page, each message its connection carried — the
  join, each event (`Counter.Inc (click)`), navigation and broadcast —
  with its time, statements, reply size and what it ran. A message that
  failed is red, and so are failures only the browser saw (a push timing
  out, a dropped connection).
- **Crons**: the app's crons, their next and last runs, and a **Run now**
  button. Each run is a record of its own, listed under *Cron runs*:
  its statements, timeline, logs and error, like a request's.
- **Mail**: with `web/mail`, the messages the request (or cron run) sent
  or queued — headers, attachments, state — and the HTML in a sandboxed
  preview.

The list at the top holds the page and each `fetch` and XHR call it made
since it loaded: every response of a development app carries its record's
id in `X-Vuka-Toolbar`. Under `vuka run -watch` the handle follows the
next build: `building… 1.2s` while it compiles (the last good build still
answering), `build failed` with the compiler's output in the drawer.

It is on under `vuka run`, `web.Dev()` or `VUKA_ENV=development`, and off
in a test binary (so tests see the pages the app serves), with
`web.NoToolbar()`, and in any other app: no recording, no routes, no
header, no hooks. The toolbar draws itself in a shadow DOM, so the page's
styles and its own don't meet, light or dark as the system prefers. The
page's HTML is the same but for its script tag (in `<head>`, beside the
stylesheet's link); live updates never carry it. The app keeps its latest
100 requests in memory and serves them under `/_vuka/toolbar/`.

**Statements.** [orm](https://github.com/vuka-lang/orm)'s `ormweb` feeds
the SQL panel by itself. Another database layer reports its statements
with `web.NoteStatement(ctx, web.Statement{SQL, Args, DB, Start,
Duration, Rows, Err})`; `app.Toolbar()` says whether to bother.

**Panels of your own.** `web.Note(ctx, panel, v)` records a value for a
panel from request code — a no-op outside development, so it can stay —
and `web.AddPanel(name, render)` draws it:

```vuka
func (c *Cache) Get(ctx context.Context, key string) ([]byte, bool) {
	v, ok := c.m[key]
	web.Note(ctx, "Cache", Lookup{key, ok})
	return v, ok
}

web.AddPanel("Cache", func(notes []any) ui.Node {
	return <ul>{for _, n := range notes { <li>{n.(Lookup).Key}: {n.(Lookup).Hit}</li> }}</ul>
})
```

A panel noted without `AddPanel` lists its values. A panel's markup may
use the toolbar's classes: `kv` and `grid` tables, `dim`, `errtext`,
`status`, and `preview` for an `<iframe>` (the Mail panel's).

## Authentication

[`web/auth`](auth) signs users in and guards the app. The app writes one
interface — where users and their passwords live — and every route, live
page and live event then needs a sign-in unless marked public:

```vuka
type Users struct{ db *DB } // registered in main: app.Service(NewUsers)

func (u *Users) FindLogin(ctx context.Context, login string) (*auth.Identity, string, error) { … } // + the encoded password
func (u *Users) Load(ctx context.Context, id string) (*auth.Identity, error) { … }

@web.Get("/login")
@auth.Public
func Login(r *http.Request) ui.Node { return <auth.LoginPage request={r} /> }

@web.Get("/orders")
@auth.Requires("orders.read")
func Orders(me *auth.Identity, store *Store) Result[[]Order] { return store.For(me.ID) }

@auth.Requires("orders.delete")
func (b *Board) Clear(ctx context.Context) error { … }  // a live event: refused to anyone else, however reached

func main() {
	web.New(auth.Module(auth.Config{Endpoints: "/auth"})).Run("")
}
```

Cookie sessions for pages and live pages (rotated on sign-in, HttpOnly,
SameSite=Lax, Secure under HTTPS), opaque bearer tokens for APIs
(`POST /auth/token`), JWTs issued elsewhere, Argon2id hashes (bcrypt ones
upgraded at login; another system's encodings read by `Config.Hashers`), one error for an unknown login and a wrong password,
throttled failures, "remember me" (`RememberMe`), `auth.RevokeUser` closing the user's live pages, and
`authtest` for tests. A live event's gate is checked before its handler
runs, and again by each gated method it reaches — given the page's context,
on any goroutine. A gated call no identity accounts for is refused
(`LaxGates` lets one outside any request run); `main` and jobs mark their
context with `auth.System(ctx)`, and crons are marked by themselves. A
failed login takes at least `FailedLoginTime` (300ms), whatever failed, and
throttling counts the account a login finds, not only its spelling. A live
page's sign-in is checked when the page mounts (its join, each navigation),
its events trusting the identity it mounted with; a sign-in ended through
auth (`SignOut`, `Revoke`, `RevokeUser`, a new sign-in, a session found
expired) closes its pages at once, and one ended behind auth's back (a store
row deleted, another process) is caught within `web.LiveCheckEvery` (30s),
when its identity is also loaded again if `LiveRecheck` (60s) has passed. Its sessions and tokens are kept in memory unless the
app gives it stores. An app's own login handler re-renders its page on a
failure (see Forms that fail) with `auth.LoginMessage(err)`;
`auth.LoginPage` keeps the posted login and `next`. See the [auth guide](https://vuka-lang.github.io/web/auth)
and [`examples/auth`](examples/auth).

**CSRF.** `web.CSRF()` (which `auth.Module` turns on) answers 403 to a POST,
PUT, PATCH or DELETE a browser sends from another site's page, read from
`Sec-Fetch-Site` and `Origin` by net/http's `CrossOriginProtection`: no
token in forms. Requests with `Authorization: Bearer` pass, as browsers
never add one. Live connections check their origin themselves.

**Hooks for libraries.** What `auth` builds on, open to any library:

| | |
|---|---|
| `web.WrapRoutes(name, f)` | `f(web.RouteInfo)` returns middleware for each route (its method, path, attributes, whether it is a page or a Live page, and a Live page's type); an attribute implementing `RouteWrapper() string` fails Build when no `WrapRoutes` of that name is installed |
| `web.Attrs(attrs…)` | typed attributes for a route or `App.Live` page added in code |
| `web.ParamFrom(f)` | parameters of `f`'s result type bind with `f(r)` |
| `web.Implementation[I](app)` | the provided value implementing interface `I` |
| `web.LiveCheck(f)` | `f(ctx)` on each joined live page every `LiveCheckEvery` (30s); an error closes it (4001) and the page reloads |
| `app.CloseLive(match)` | closes (4001, reload) the joined live pages whose context `match(ctx)` picks, at once: a library ending a session closes its pages |
| `web.GuardLiveEvents(f)` | `f(ctx, web.LiveHandler)` before each live event's handler runs, with the page's context: the handler's name, its component's type, its receiver type and method, its declaration's attributes (Vuka's registry: `vuka.FuncOf`), whether it is a function literal; an error refuses the event as a handler's error would |
| `web.CurrentLiveEvent()` | the live event the goroutine handles: its context, its handler's name, `Handling()` (its handler, or what the handler calls, runs; not the render after it), and `Refuse(err)` — for a decorator of a handler without a `context.Context` |
| `web.LiveOrigin()` | on a goroutine a live connection's goroutine started (one a handler starts), that page's context; nil elsewhere |
| `web.IsLiveJoin(ctx)` | whether the route runs again for a live page's join |
| `web.TrackRequests()`, `web.CurrentRequest()` | whether the calling goroutine serves a request (a live connection's included), and the context of the route it runs |
| `web.CronContext(f)` | every cron runs with its context passed through `f` |
| `web.ClientIP(r)`, `web.IsHTTPS(r)` | the client's address and scheme, through `web.TrustedProxies` only |
| `web.NoteRequest(ctx, note)` | a row in the toolbar's Request panel |
| `app.WriteError(w, r, err, html)` | answer as a handler's error is answered |

## Frontends

A single-page app — React, Vue, Svelte, anything that builds to static files —
is served from the binary beside the app's routes:

```vuka
//go:embed all:frontend/dist
var dist embed.FS

app := web.New(web.Frontend(dist, "frontend/dist"))
```

| | |
|---|---|
| a path naming a file (`/assets/index-B3x9kQa1.js`, `/robots.txt`) | that file, or 404 |
| any other GET path (`/`, `/pets/7`) | `index.html`, so client-side routes survive a reload |
| the app's routes, live pages, `/_vuka/` | matched first |
| `.vite/` | never served |

Files Vite's manifest (`.vite/manifest.json`) lists as build output, with a
content hash in their names, are cached for good
(`Cache-Control: immutable`); everything else, `index.html` included, is
revalidated by its ETag and answered with a 304 when unchanged. Outside
development a root without `index.html` fails `Build` ("build it first").

`web.FrontendAt("/admin")` serves it under a prefix (build it with Vite's
`base: "/admin/"`); `web.FrontendEnv("APP_NAME", "API_URL")` passes those
variables' values (the environment's, `.env`'s) to the page:
`index.html` sets them before any module runs, and `import.meta.env.APP_NAME`
reads them. Only the keys named reach the browser.

**Development.** In development (`web.Dev()`, `VUKA_ENV=development`,
`vuka run`) the files are read from disk, and a running Vite dev server takes
over: the Vite plugin in [`vite/vuka.js`](vite/vuka.js) (`vuka()` in
`vite.config`) writes `<dist>/.vite/vuka-hot.json` with the dev server's
origin and pid, and the app's pages load their modules from it — the browser
stays on the app's address and gets hot module replacement, with no proxy to
configure. Other files redirect to the dev server, which has the current
`public/` ones. A hot file whose process is gone is ignored. With neither a
build nor a dev server, pages say how to build. `vuka run` starts the
frontend's Vite (and installs its dependencies first), `vuka build` runs
`vite build` before embedding; both copy the plugin into
`frontend/vuka/vite.js`. Without the `vuka` command, `web.Dev()` starts the
dev server for `go run .` ([above](#go-run)). A frontend without a `package.json` is served as it
is. The development toolbar is added to the page; the app's Tailwind
stylesheet isn't. An example is in [`examples/spa`](examples/spa).

## Islands

A Vue, React or Svelte component placed in a server-rendered page hydrates
on its own; the rest of the page ships no JavaScript. Import the component
into a `.vuka` file and its props, read from its TypeScript, are type-checked
like any component's; a function prop takes a live view method:

```vuka
import Week "./web/islands/Week.vue"

<Week days={p.days} onBook={p.Book} client:visible />
```

A page can be a component: `@web.Live` on an `@ui.Island` type embedding
`ui.Live` serves the component as a live page, `@web.Get` on one without it
as a stateless page. Through `useLive` (from `vuka:live`, typed from the Go
struct) the component reads the exported fields (`live.rows.value`), sets
the ones tagged `bind:` (`v-model="live.q.value"`, debounced; the server
runs `Changed` after) and calls the exported methods (`live.cancel(id)`).
A live page's sets and calls are live events; a stateless page's are each a
signed POST that carries the page's props back — signed for the page and
the visitor's sign-in, so no one else can replay them. `validate:` tags on
a bound field, and on a method's struct arguments at any depth, are checked
before anything is applied or run: a failure answers 422 with each field's
message (`fields: {"q": "at most 50 characters"}`, `level.code`), shown as
`live.q.error` or the method's `error.fields`. `ui.Layout` in the
decorator is the page's layout (over `web.Layout` and `web.LiveLayout`): a
`func(ui.Node) ui.Node`, or a `func(ctx, ui.Node) ui.Node` reading the
request; several nest.

```vuka
@web.Live("/orders")
@ui.Island("./web/components/Orders.vue", ui.Layout(layouts.Page("Orders")))
type Orders struct {
	ui.Live
	Q    string  `json:"q" url:"q" bind:"q"`
	Rows []Order `json:"rows"`
	db   *Store
}

func (o *Orders) Changed(ctx context.Context) error           { … }
func (o *Orders) Cancel(ctx context.Context, id int64) error { … }
```

The guide: [Islands](https://vuka-lang.github.io/web/islands). An
example is in [`examples/islands`](examples/islands).

## Static files

`app.Static("/assets", fsys)` serves an `fs.FS` (an `embed.FS` sub-tree,
`os.DirFS(…)`). A directory is its `index.html`, else 404 — never a
listing — and dotfiles and dot directories (`.env`, `.git/`) are never
served, but `.well-known/`. `web.Frontend` serves the same way.

Link to a file with `web.Asset`, giving the path it is served at:

```html
<img src={web.Asset("/static/images/logo.png")} alt="Logo" />
```

Under `vuka run` that is the path as written. In a binary `vuka build` made
it is the file's content-hashed name (`/static/images/logo.3f9a1c2b.png`),
served with `Cache-Control: public, max-age=31536000, immutable`; the plain
name keeps working. `vuka build` and `vuka vet` check each literal path
against the mounts of embedded directories. `url(…)`s in the stylesheets a
mount serves, and in the app's Tailwind CSS, name the hashed files too.
`web.AssetURL(ctx, path)` is the absolute URL, for emails: the request's
scheme and host, or `web.BaseURL("https://example.com")`'s where there is no
request.

## Security headers

Every response carries, unless its handler sets them itself:

| header | default |
|---|---|
| `X-Content-Type-Options` | `nosniff` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |
| `X-Frame-Options` | `DENY` |
| `Content-Security-Policy` | `frame-ancestors 'none'` (`'self'` with `SAMEORIGIN`) |
| `Strict-Transport-Security` | `max-age=31536000`, on HTTPS requests only |

`web.Headers(web.SecurityHeaders{FrameOptions: "SAMEORIGIN", HSTS: -1})`
changes them (`"-"` leaves one out; a negative `HSTS` too), and
`web.NoSecurityHeaders()` turns them off.

**Behind a proxy**, `web.TrustedProxies("10.0.0.0/8")` names it: from those
addresses only, `web.ClientIP(r)` reads the client's address from
`X-Forwarded-For` (else `Forwarded`) — the nearest one that isn't a trusted
proxy — and `X-Forwarded-Proto: https` counts as HTTPS. Without it,
`web.ClientIP(r)` is the connection's peer and those headers are ignored.
Login throttling and the per-address live connection cap count by it.

## Plain Go

Everything works from Go without decorators, with the calls they are:
`@web.Get("/pets/{id}")` on `ShowPet` is `web.Routes.Get("/pets/{id}",
ShowPet)` at init, after `vuka.RegisterParams(ShowPet, "id", "store")`
recording the names reflection can't see, and an `App`'s methods of the
same names register on that app alone. A handler registered from Go binds
its scalars to the path's segments in order, the last scalar the last
segment, and takes the query in a struct; structs, request values and
dependencies bind by type as usual.

```go
app := web.New()
app.Service(NewStore)
app.Get("/pets/{id}", ShowPet, auth.Required()) // func ShowPet(id int, s *Store)
app.Post("/pets", func(in NewPet, s *Store) (Pet, error) { … })
app.Get("/page", page, web.Template(vuka.File("views/page.html")), web.Use(mw))
app.Live("/board", NewBoard)
app.Live("/applicants", NewApplicants, ui.Island(ui.Layout(layouts.Page("Applicants"))))
app.Controller("/admin/pets", NewPetAdmin)
app.Get("/{id}", (*PetAdmin).Show)
```

A page's constructor builds each page's component, its parameters of
provided types injected and the others zero. Options are `RouteOption`s:
web's own, an auth gate, `ui.Island`, or any value carrying typed attributes.
From Go, `vuka.File("…")` reads from disk relative to the working directory.

Generated code only makes things faster. Live components written as nodes
(`ui.El`, no JSX) patch as whole components: the same page in the browser,
larger replies ([ui](https://github.com/vuka-lang/ui#live-patches-without-render-tracking)).
An island's component works untyped without `vuka-live.d.ts`: `useLive()`
comes from the island build; only `defineProps<T>()` of its types needs the
file.

## Testing

```
go test ./...                     # the package
vuka test ./...                   # also examples/ and internal/vukatest, written in Vuka
```

For an app's crons and mail, see `crontest` and `mailtest` above.

The browser tests (`examples/live`, `TestBrowserReconnect`, `TestBrowserJS…`)
drive a headless Chrome over the DevTools protocol (`internal/chrome`) and
skip when none is found; `VUKA_CHROME` names the binary. The runtime's JS
commands and navigation also run in node over a small DOM
(`testdata/livejs/js.js`, `TestLiveJSCommands`), skipped without node.

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

## Versions

From v0.50.0, vuka, ui, web and the `vuka` command are released together
under one version; require them at the same version (`vuka upgrade` moves an
app between versions). See
[Versions and compatibility](https://vuka-lang.github.io/project/versions).

## License

MIT
