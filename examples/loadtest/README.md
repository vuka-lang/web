# Live-page load test

A repeatable load test of live pages, and the gate every change to the live
runtime's scale is judged by. A server (`main.vuka`, `debug.go`, `pages/`,
`components/`, `feed/`, `store/`) with two scenarios, and a Go client
(`client/`) behaving as the browser's runtime does: it GETs a page for its
`data-vk-token`, dials `/_vuka/live?t=`, joins with `{"type":"join","ref":1,"v":2}`,
waits for `render`, and then sends events, matching replies by ref, or just
receives. The server lifts the caps with `web.LiveMaxConns(0)` and
`web.LiveMaxConnsPerIP(0)`.

| Scenario | Pages | What it measures |
|---|---|---|
| `pg` | `/`: a dashboard of 25 instances over Postgres, one refresh running five queries | events against a database, with a `LiveCheck` querying it |
| `mem` | `/counter`: a button and its rows. `/feed`: the same rows, subscribed to a topic, showing the last tick broadcast on it | the runtime alone: renders cost the same every time and read nothing outside the process |

`?rows=` sets the rows of a `mem` page (20 by default, 500 at most), each an
instance, so a page's cost is about its row count; `?topic=` the topic a feed
page subscribes to.

## Running it

It's a module of its own, so the framework's module doesn't need the database
driver. From this directory:

```sh
mkdir -p build
vuka build -o build/server .
go build -o build/loadclient ./client

# the runtime alone: ramp, events, broadcast, storm, slow clients
SCENARIO=mem ./run.sh 10000 out/mem-10k

# the dashboard over Postgres
createdb loadtest
export LOADTEST_DB='postgres://you@127.0.0.1:5432/loadtest?sslmode=disable&pool_max=20&pool_max_idle=20'
./run.sh 1000 out/pg-1000             # latencies
./run.sh 10000 out/pg-10000 profile   # also CPU, heap, goroutine, block and mutex profiles
dropdb loadtest
```

`vuka build` leaves a copy of the module in `build/`, which is why the client
is `build/loadclient`. The module requires the framework through a `replace`
of `../..`; when that checkout needs a newer `ui` than the one released,
build with a `go.work` (outside the repository) that `use`s this directory and
`replace`s `github.com/vuka-lang/ui/v2` with a checkout.

`run.sh` waits for the machine's load to fall below `MAX_LOAD` (up to ten
minutes), raises the open-file limit and stops if it can't reach what the run
needs, starts the server with `GODEBUG=gctrace=1`, runs the client, and stops
the server if it passes `MAX_RSS_KB` (7 GB) or swap grows by more than
`MAX_SWAP_GROWTH_MB`. Its environment is at the top of the script; the client's
flags (`./build/loadclient -h`) go in `CLIENT_ARGS`, the server's in
`SERVER_ARGS`.

A run is, in order:

1. **Ramp.** Users join `RATE` a second, `STEP` at a time. After each step
   settles (`SETTLE`) the client samples the server twice, as it is and after
   it collects and returns memory (`/debug/metrics?gc=1`), plus its own memory.
   The slope of the server's memory over the connections of the steps, the
   empty server included, is what a connection costs: `ramp.bytes_per_connection`.
2. **Warm-up**, then **events**: the interactive users (`-interactive`, a
   tenth by default) click once per `EVERY` for `MEASURE`.
3. **Broadcast.** `-broadcasts` broadcasts to the topic, one at a time, each
   waiting for every feed user still connected. A broadcast's page shows the
   server's clock at the send, so each frame says how long it took to arrive:
   `to_last_ms` is the time from `Broadcast` to the *last* subscriber of each,
   over the broadcasts; `to_first_ms` to the first.
4. **Storm.** The topic is broadcast to at `-storm-rate` a second for
   `-storm-for` while the interactive users, whose pages aren't subscribed,
   keep clicking: `storm.interactive.latency_ms` against the events phase is
   what the storm costs the others. `storm.subscriber_latency_ms`,
   `frames_per_broadcast_per_subscriber` (below 1 once broadcasts merge) and
   `subscribers_behind_by_seq` are the subscribers' side.
5. **Drain.** The slow users read at last.

`-phases` picks among `events,broadcast,storm`. The `pg` scenario runs `events`.

## Slow users

`-slow` (0.02 by default in `mem`) is the fraction of users that don't keep up.
They offer no compression, so what the server writes is what crosses the socket,
and have a 4 KB receive buffer. In `delay` mode (the default) each reads one
frame per `-slow-delay` (15 s by default: the server gives up on a write blocked for 10 s,
so a socket whose buffers are full should end in a close). In `stop` mode they
read nothing until the drain, and then find whether and how the server closed
them: the server drops such a socket at its ping timeout, whatever it was sent.

Whether a slow user is closed depends on how much it was sent: the kernel's
buffers hold the first of it (`storm.frame_bytes_avg` times the frames it
received tells how much that was). Slow users are a class of their own in every
count: a broadcast doesn't expect them, and `closes` and `failures` are split by
class (`normal`, `interactive`, `slow`), each by its WebSocket status and reason,
with when in the run the client saw them (`at_seconds`; `timeline` names the
phases). A slow user only learns of a close when it reads, so for when the
server closed it read `live_conns` in the storm's `server_samples`. The drain
reads what the server had queued for each, then the close: `connection reset`
when the server closed with data unread, a WebSocket status when it sent one.

## Thirty thousand connections on one machine

- **Ephemeral ports.** One client address dialling one server address has about
  16,000 ports (49152 to 65535 on macOS), and closed connections hold theirs in
  `TIME_WAIT`. The server listens on `-ports` consecutive ports from `-addr`, the
  client dials them in turn, and `run.sh` takes one per 8,000 sockets (`PORTS`):
  30,000 sockets use four, about 7,500 each. No loopback aliases and no `sudo`.
- **Descriptors.** Client and server each hold a descriptor per socket.
  `run.sh` raises the soft limit to the hard limit (or `kern.maxfilesperproc`),
  checks it is above the sockets plus 512, and fails saying so if it isn't; the
  client raises its own as well.
- **The listen backlog** (`kern.ipc.somaxconn`, 128 on macOS) bounds how fast
  users can join: keep `RATE` to a few hundred a second.
- **The client mustn't be the bottleneck.** A user costs the client about 35 KB
  and one or two goroutines; frames of subscribers are scanned, not decoded.
  `client.lag_ms` (and `client_lag_ms` in each phase) is the client's own
  scheduling delay, a 10 ms timer's lateness: if it grows, the slow replies are
  the client's. `client.self` is its memory.

## Results

Each run's directory holds:

| File | What |
|---|---|
| `result.json` | everything below |
| `conditions.txt` | load and swap before and after, the open-file limit, the ports |
| `samples.txt` | RSS, swap and load every 5 s |
| `server.log` | the gctrace |
| `*.pprof`, `goroutine.txt` | with `profile`, taken 10 s into the events window |

`result.json`:

| Key | What |
|---|---|
| `machine`, `config` | CPU, cores, RAM, OS, Go, descriptor limit; every flag: the reference machine, recorded with the numbers |
| `classes` | users per class |
| `ramp` | `steps` (each: connections, server before and after collecting, client memory), `bytes_per_connection` (RSS, heap and stack, goroutines, the client's RSS: the slope and last-minus-first) |
| `latency_ms`, `events`, `events_per_second`, `slow_per_second`, `server_*` | the events phase |
| `broadcast` | `to_last_ms`, `to_first_ms` (quantiles over the broadcasts), `per_broadcast`, `per_subscriber_ms`, `dispatch_ms` per broadcast |
| `storm` | as above, with the server's samples |
| `slow` | the drain |
| `closes`, `failures` | by class, by status; `failures` are users that never joined (an HTTP 503 or 429 at the dial shows there) |
| `client` | its lag over the run, its memory |
| `server_*` samples | `rss_bytes`, `goroutines`, `heap_*`, `stack_inuse`, `live_conns`, `gc_pause_ms` (since start) and `gc_pause_window_ms` (since the last sample) |

Latencies of the frames of subscribers are of a histogram of eighths of a power
of two: quantiles within about 12%, bucket upper bounds. Event latencies are exact.

## Reading the numbers

Say what the machine was doing with every number: `conditions.txt` has the load
and swap, and a client and server on one machine compete for it. Watch for
swapping. Check `client.lag_ms` is small before blaming the server. A broadcast
timed to its last subscriber includes the client's time to read that frame, so
at tens of thousands of subscribers it's partly the client's: compare it with
the lag. Block and mutex profiling cost something, so compare latencies only
between runs without `profile`.

`/debug/metrics` (the server's second port, 18099) holds what the client
samples, `?gc=1` collects first, `?mark=1` adds the GC pauses since the last
mark; `/debug/broadcast?topic=` sends one tick and `/debug/seq` says how many
have been sent. They're the harness's own: the app of a real deployment has none.
